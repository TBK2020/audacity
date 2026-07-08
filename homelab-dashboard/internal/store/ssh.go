package store

import (
	"database/sql"
	"errors"
	"time"
)

const sshSchema = `
CREATE TABLE IF NOT EXISTS credentials (
	id         TEXT PRIMARY KEY,
	type       TEXT NOT NULL,             -- password | key
	username   TEXT NOT NULL DEFAULT '',
	nonce      BLOB NOT NULL,
	secret     BLOB NOT NULL,             -- AES-256-GCM sealed
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS host_keys (
	host_id     TEXT PRIMARY KEY,
	fingerprint TEXT NOT NULL,            -- SHA256:… of the host public key
	added_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ssh_sessions (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id      TEXT NOT NULL,
	target       TEXT NOT NULL,
	username     TEXT NOT NULL,
	remote_addr  TEXT NOT NULL DEFAULT '',
	started_at   INTEGER NOT NULL,
	ended_at     INTEGER,
	bytes_in     INTEGER NOT NULL DEFAULT 0,
	bytes_out    INTEGER NOT NULL DEFAULT 0,
	close_reason TEXT NOT NULL DEFAULT ''
);
`

var ErrNotFound = errors.New("not found")

type Credential struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	// Nonce/Secret stay server-side; JSON views never include them.
	Nonce  []byte `json:"-"`
	Secret []byte `json:"-"`
}

func (s *Store) SaveCredential(c Credential) error {
	_, err := s.db.Exec(
		`INSERT INTO credentials(id, type, username, nonce, secret, created_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET type=excluded.type, username=excluded.username,
		   nonce=excluded.nonce, secret=excluded.secret`,
		c.ID, c.Type, c.Username, c.Nonce, c.Secret, time.Now().Unix(),
	)
	return err
}

func (s *Store) GetCredential(id string) (Credential, error) {
	var c Credential
	var ts int64
	err := s.db.QueryRow(
		`SELECT id, type, username, nonce, secret, created_at FROM credentials WHERE id = ?`, id,
	).Scan(&c.ID, &c.Type, &c.Username, &c.Nonce, &c.Secret, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.CreatedAt = time.Unix(ts, 0)
	return c, err
}

func (s *Store) ListCredentials() ([]Credential, error) {
	rows, err := s.db.Query(`SELECT id, type, username, created_at FROM credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		var c Credential
		var ts int64
		if err := rows.Scan(&c.ID, &c.Type, &c.Username, &ts); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(ts, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCredential(id string) error {
	res, err := s.db.Exec(`DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HostKey returns the pinned fingerprint for a host ("" if first connect).
func (s *Store) HostKey(hostID string) (string, error) {
	var fp string
	err := s.db.QueryRow(`SELECT fingerprint FROM host_keys WHERE host_id = ?`, hostID).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fp, err
}

func (s *Store) PinHostKey(hostID, fingerprint string) error {
	_, err := s.db.Exec(
		`INSERT INTO host_keys(host_id, fingerprint, added_at) VALUES(?,?,?)
		 ON CONFLICT(host_id) DO NOTHING`,
		hostID, fingerprint, time.Now().Unix(),
	)
	return err
}

type SSHSession struct {
	ID          int64      `json:"id"`
	HostID      string     `json:"host_id"`
	Target      string     `json:"target"`
	Username    string     `json:"username"`
	RemoteAddr  string     `json:"remote_addr"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	BytesIn     int64      `json:"bytes_in"`
	BytesOut    int64      `json:"bytes_out"`
	CloseReason string     `json:"close_reason"`
}

func (s *Store) StartSSHSession(hostID, target, username, remoteAddr string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO ssh_sessions(host_id, target, username, remote_addr, started_at)
		 VALUES(?,?,?,?,?)`,
		hostID, target, username, remoteAddr, time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) EndSSHSession(id, bytesIn, bytesOut int64, reason string) error {
	_, err := s.db.Exec(
		`UPDATE ssh_sessions SET ended_at=?, bytes_in=?, bytes_out=?, close_reason=? WHERE id=?`,
		time.Now().Unix(), bytesIn, bytesOut, reason, id,
	)
	return err
}

func (s *Store) RecentSSHSessions(limit int) ([]SSHSession, error) {
	rows, err := s.db.Query(
		`SELECT id, host_id, target, username, remote_addr, started_at, ended_at,
		        bytes_in, bytes_out, close_reason
		 FROM ssh_sessions ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SSHSession{}
	for rows.Next() {
		var ss SSHSession
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&ss.ID, &ss.HostID, &ss.Target, &ss.Username, &ss.RemoteAddr,
			&started, &ended, &ss.BytesIn, &ss.BytesOut, &ss.CloseReason); err != nil {
			return nil, err
		}
		ss.StartedAt = time.Unix(started, 0)
		if ended.Valid {
			t := time.Unix(ended.Int64, 0)
			ss.EndedAt = &t
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}
