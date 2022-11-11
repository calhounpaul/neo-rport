-- TODO: currently all existing sessions will be cleared. is this ok?
DROP TABLE IF EXISTS api_sessions;

CREATE TABLE api_sessions (
    session_id INTEGER PRIMARY KEY NOT NULL,
    token TEXT NOT NULL,
    expires_at DATETIME NOT NULL,
    username TEXT NOT NULL,
    last_access_at DATETIME NOT NULL,
    user_agent TEXT,
    ip_address TEXT
);

CREATE UNIQUE INDEX idx_token
  ON api_sessions (token);

CREATE INDEX idx_expires_at_time
	ON api_sessions (DATETIME(expires_at) DESC);

-- username may not be unique as the user may create new tokens before old ones are deleted/expired
CREATE INDEX idx_username
  ON api_sessions (username);
