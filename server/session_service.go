package chserver

import (
	"context"
	"github.com/cloudradar-monitoring/rport/server/csr"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cloudradar-monitoring/rport/server/ports"
	chshare "github.com/cloudradar-monitoring/rport/share"
)

type SessionService struct {
	repo            *csr.ClientSessionRepository
	portDistributor *ports.PortDistributor
}

// NewSessionService returns a new instance of client session service.
func NewSessionService(
	portDistributor *ports.PortDistributor,
	repo *csr.ClientSessionRepository,
) *SessionService {
	return &SessionService{
		portDistributor: portDistributor,
		repo:            repo,
	}
}

func (s *SessionService) Count() (int, error) {
	return s.repo.Count()
}

func (s *SessionService) GetActiveByID(id string) (*csr.ClientSession, error) {
	return s.repo.GetActiveByID(id)
}

func (s *SessionService) GetAll() ([]*csr.ClientSession, error) {
	return s.repo.GetAll()
}

func (s *SessionService) StartClientSession(
	ctx context.Context, sid string, sshConn ssh.Conn,
	req *chshare.ConnectionRequest, user *chshare.User, clog *chshare.Logger,
) (*csr.ClientSession, error) {
	session := &csr.ClientSession{
		ID:         sid,
		Name:       req.Name,
		Tags:       req.Tags,
		OS:         req.OS,
		Hostname:   req.Hostname,
		Version:    req.Version,
		IPv4:       req.IPv4,
		IPv6:       req.IPv6,
		Address:    sshConn.RemoteAddr().String(),
		Tunnels:    make([]*csr.Tunnel, 0),
		Connection: sshConn,
		Context:    ctx,
		User:       user,
		Logger:     clog,
	}

	_, err := s.StartSessionTunnels(session, req.Remotes, csr.TunnelACL{})
	if err != nil {
		return nil, err
	}

	err = s.repo.Save(session)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// StartSessionTunnels returns a new tunnel for each requested remote or nil if error occurred
func (s *SessionService) StartSessionTunnels(session *csr.ClientSession, remotes []*chshare.Remote, acl csr.TunnelACL) ([]*csr.Tunnel, error) {
	err := s.portDistributor.Refresh()
	if err != nil {
		return nil, err
	}

	tunnels := make([]*csr.Tunnel, 0, len(remotes))
	for _, remote := range remotes {
		if !remote.IsLocalSpecified() {
			port, err := s.portDistributor.GetRandomPort()
			if err != nil {
				return nil, err
			}
			remote.LocalPort = strconv.Itoa(port)
			remote.LocalHost = "0.0.0.0"
		}

		t, err := session.StartTunnel(remote, acl)
		if err != nil {
			return nil, err
		}
		tunnels = append(tunnels, t)
	}
	return tunnels, nil
}

func (s *SessionService) Terminate(session *csr.ClientSession) error {
	if s.repo.KeepLostClients == nil {
		return s.repo.Delete(session)
	}

	now := time.Now().UTC()
	session.Disconnected = &now
	return s.repo.Save(session)
}
