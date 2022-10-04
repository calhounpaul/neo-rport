package chserver

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	rportplus "github.com/cloudradar-monitoring/rport/plus"
	"github.com/cloudradar-monitoring/rport/plus/capabilities/oauth"
	"github.com/cloudradar-monitoring/rport/server/api"
	errors2 "github.com/cloudradar-monitoring/rport/server/api/errors"
	chshare "github.com/cloudradar-monitoring/rport/share"
	"github.com/cloudradar-monitoring/rport/share/logger"
)

type twoFAResponse struct {
	SendTo         string `json:"send_to"`
	DeliveryMethod string `json:"delivery_method"`
	TotPKeyStatus  string `json:"totp_key_status"`
}

type loginResponse struct {
	Token *string        `json:"token"`  // null if 2fa is on
	TwoFA *twoFAResponse `json:"two_fa"` // null if 2fa is off
}

func (al *APIListener) handleGetLogin(w http.ResponseWriter, req *http.Request) {
	if al.config.PlusOAuthEnabled() {
		al.jsonErrorResponse(w, http.StatusForbidden, errors.New("built-in authorization disabled. please authorize via your configured authorization"))
		return
	}

	if al.config.API.AuthHeader != "" && req.Header.Get(al.config.API.AuthHeader) != "" {
		al.handleLogin(req.Header.Get(al.config.API.UserHeader), "", true /* skipPasswordValidation */, w, req)
		return
	}

	basicUser, basicPwd, basicAuthProvided := req.BasicAuth()
	if basicAuthProvided {
		al.handleLogin(basicUser, basicPwd, false, w, req)
		return
	}

	// TODO: consider to move this check from all API endpoints to middleware similar to https://github.com/cloudradar-monitoring/rport/pull/199/commits/4ca1ca9f56c557762d79a60ffc96d2de47f3133c
	// ban IP if it sends a lot of bad requests
	if !al.handleBannedIPs(req, false) {
		return
	}
	al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "auth is required")
}

func (al *APIListener) handleLogin(username, pwd string, skipPasswordValidation bool, w http.ResponseWriter, req *http.Request) {
	if al.bannedUsers.IsBanned(username) {
		al.jsonErrorResponseWithTitle(w, http.StatusTooManyRequests, ErrTooManyRequests.Error())
		return
	}

	if username == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "username is required")
		return
	}

	authorized, user, err := al.validateCredentials(username, pwd, skipPasswordValidation)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if !al.handleBannedIPs(req, authorized) {
		return
	}

	if !authorized {
		al.bannedUsers.Add(username)
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}

	if al.config.API.IsTwoFAOn() {
		sendTo, err := al.twoFASrv.SendToken(req.Context(), username, req.UserAgent(), chshare.RemoteIP(req))
		if err != nil {
			al.jsonError(w, err)
			return
		}

		tokenStr, err := al.createAuthToken(
			req.Context(),
			lifetime,
			username,
			Scopes2FaCheckOnly,
		)
		if err != nil {
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(loginResponse{
			Token: &tokenStr,
			TwoFA: &twoFAResponse{
				SendTo:         sendTo,
				DeliveryMethod: al.twoFASrv.MsgSrv.DeliveryMethod(),
			},
		}))
		return
	}

	if al.config.API.TotPEnabled {
		al.twoFASrv.SetTotPLoginSession(username, al.config.API.TotPLoginSessionTimeout)

		loginResp := loginResponse{
			TwoFA: &twoFAResponse{
				DeliveryMethod: "totp_authenticator_app",
			},
		}

		totP, err := GetUsersTotPCode(user)
		if err != nil {
			al.Logf(logger.LogLevelError, "failed to get TotP secret: %v", err)
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		scopes := Scopes2FaCheckOnly
		if totP == nil {
			// we allow access to totp-secret creation only if no totp secret was created before
			scopes = append(scopes, ScopesTotPCreateOnly...)
			loginResp.TwoFA.TotPKeyStatus = TotPKeyPending.String()
		} else {
			loginResp.TwoFA.TotPKeyStatus = TotPKeyExists.String()
		}

		tokenStr, err := al.createAuthToken(
			req.Context(),
			lifetime,
			username,
			scopes,
		)
		if err != nil {
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		loginResp.Token = &tokenStr
		al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(loginResp))
		return
	}

	tokenStr, err := al.createAuthToken(req.Context(), lifetime, username, ScopesAllExcluding2FaCheck)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	response := api.NewSuccessPayload(loginResponse{
		Token: &tokenStr,
	})
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) sendJWTToken(username string, w http.ResponseWriter, req *http.Request) {
	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}

	tokenStr, err := al.createAuthToken(req.Context(), lifetime, username, ScopesAllExcluding2FaCheck)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	response := api.NewSuccessPayload(loginResponse{
		Token: &tokenStr,
	})
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handlePostLogin(w http.ResponseWriter, req *http.Request) {
	if al.config.PlusOAuthEnabled() {
		al.jsonErrorResponse(w, http.StatusForbidden, errors.New("built-in authorization disabled. please authorize via your configured authorization"))
		return
	}

	username, pwd, err := parseLoginPostRequestBody(req)
	if err != nil {
		// ban IP if it sends a lot of bad requests
		if !al.handleBannedIPs(req, false) {
			return
		}
		al.jsonError(w, err)
		return
	}

	al.handleLogin(username, pwd, false, w, req)
}

// TODO: consider moving these definitions to an auth related package

const BuiltInAuthProviderName = "built-in"

// AuthProviderInfo contains the provider name and the uris to be used
// for either regular or device flow based authorization
type AuthProviderInfo struct {
	AuthProvider      string `json:"auth_provider"`
	SettingsURI       string `json:"settings_uri"`
	DeviceSettingsURI string `json:"device_settings_uri"`
}

// AuthSettings contains the auth info to be used by a regular web app
// type authorization
type AuthSettings struct {
	AuthProvider string           `json:"auth_provider"`
	LoginInfo    *oauth.LoginInfo `json:"details"`
}

// DeviceAuthSettings contains the auth info to be used by a CLI or
// similarly constrained app
type DeviceAuthSettings struct {
	AuthProvider string                 `json:"auth_provider"`
	LoginInfo    *oauth.DeviceLoginInfo `json:"details"`
}

func (al *APIListener) handleGetAuthProvider(w http.ResponseWriter, req *http.Request) {
	var response api.SuccessPayload

	if al.config.PlusOAuthEnabled() {
		OAuthProvider := AuthProviderInfo{
			AuthProvider:      al.config.OAuthConfig.Provider,
			SettingsURI:       allRoutesPrefix + authRoutesPrefix + authSettingsRoute,
			DeviceSettingsURI: allRoutesPrefix + authRoutesPrefix + authDeviceSettingsRoute,
		}
		response = api.NewSuccessPayload(OAuthProvider)
	} else {
		builtInAuthProvider := AuthProviderInfo{
			AuthProvider: BuiltInAuthProviderName,
			SettingsURI:  "",
		}
		response = api.NewSuccessPayload(builtInAuthProvider)
	}
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handleGetAuthSettings(w http.ResponseWriter, req *http.Request) {
	if !al.config.PlusOAuthEnabled() {
		al.jsonErrorResponse(w, http.StatusForbidden, rportplus.ErrPlusNotAvailable)
		return
	}

	plus := al.Server.plusManager
	capEx := plus.GetOAuthCapabilityEx()
	if capEx == nil {
		al.jsonErrorResponse(w, http.StatusForbidden, rportplus.ErrCapabilityNotAvailable(rportplus.PlusOAuthCapability))
		return
	}

	loginInfo, err := capEx.GetLoginInfo()
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	settings := AuthSettings{
		AuthProvider: al.config.OAuthConfig.Provider,
		LoginInfo:    loginInfo,
	}
	response := api.NewSuccessPayload(settings)
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handleGetAuthDeviceSettings(w http.ResponseWriter, req *http.Request) {
	if !al.config.PlusOAuthEnabled() {
		al.jsonErrorResponse(w, http.StatusForbidden, rportplus.ErrPlusNotAvailable)
		return
	}

	plus := al.Server.plusManager
	capEx := plus.GetOAuthCapabilityEx()
	if capEx == nil {
		al.jsonErrorResponse(w, http.StatusForbidden, rportplus.ErrCapabilityNotAvailable(rportplus.PlusOAuthCapability))
		return
	}

	loginInfo, err := capEx.GetLoginInfoForDevice(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	settings := DeviceAuthSettings{
		AuthProvider: al.config.OAuthConfig.Provider,
		LoginInfo:    loginInfo,
	}

	response := api.NewSuccessPayload(settings)
	al.writeJSONResponse(w, http.StatusOK, response)
}

func parseLoginPostRequestBody(req *http.Request) (string, string, error) {
	reqContentType := req.Header.Get("Content-Type")
	if reqContentType == "application/x-www-form-urlencoded" {
		err := req.ParseForm()
		if err != nil {
			return "", "", errors2.APIError{
				Err:        fmt.Errorf("failed to parse form: %v", err),
				HTTPStatus: http.StatusBadRequest,
			}
		}
		return req.PostForm.Get("username"), req.PostForm.Get("password"), nil
	}
	if reqContentType == "application/json" {
		type loginReq struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		var params loginReq
		err := parseRequestBody(req.Body, &params)
		if err != nil {
			return "", "", err
		}
		return params.Username, params.Password, nil
	}
	return "", "", errors2.APIError{
		Message:    fmt.Sprintf("unsupported content type: %s", reqContentType),
		HTTPStatus: http.StatusBadRequest,
	}
}

func parseTokenLifetime(req *http.Request) (time.Duration, error) {
	lifetimeStr := req.URL.Query().Get("token-lifetime")
	if lifetimeStr == "" {
		lifetimeStr = "0"
	}
	lifetime, err := strconv.ParseInt(lifetimeStr, 10, 0)
	if err != nil {
		return 0, fmt.Errorf("invalid token-lifetime : %s", err)
	}
	result := time.Duration(lifetime) * time.Second
	if result > maxTokenLifetime {
		return 0, fmt.Errorf("requested token lifetime exceeds max allowed %d", maxTokenLifetime/time.Second)
	}
	if result <= 0 {
		result = defaultTokenLifetime
	}
	return result, nil
}
