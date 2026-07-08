// Package store persists probe history and service events in SQLite.
package store

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite"

	"labdeck/internal/engine"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite serializes writes; a single connection avoids
	// SQLITE_BUSY churn under concurrent probe recording.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS probe_history (
	check_id   TEXT    NOT NULL,
	service_id TEXT    NOT NULL,
	ts         INTEGER NOT NULL, -- unix seconds
	ok         INTEGER NOT NULL,
	latency_ms INTEGER NOT NULL,
	detail     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_history_service_ts ON probe_history(service_id, ts);
CREATE INDEX IF NOT EXISTS idx_history_check_ts   ON probe_history(check_id, ts);

CREATE TABLE IF NOT EXISTS events (
	service_id  TEXT    NOT NULL,
	ts          INTEGER NOT NULL,
	from_status TEXT    NOT NULL,
	to_status   TEXT    NOT NULL,
	reason      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
`

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) RecordProbe(serviceID string, r engine.ProbeRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO probe_history(check_id, service_id, ts, ok, latency_ms, detail) VALUES(?,?,?,?,?,?)`,
		r.CheckID, serviceID, r.Time.Unix(), boolInt(r.OK), r.Latency.Milliseconds(), r.Detail,
	)
	return err
}

func (s *Store) RecordEvent(t engine.Transition) error {
	_, err := s.db.Exec(
		`INSERT INTO events(service_id, ts, from_status, to_status, reason) VALUES(?,?,?,?,?)`,
		t.ServiceID, t.Time.Unix(), string(t.From), string(t.To), t.Reason,
	)
	return err
}

type Event struct {
	ServiceID  string    `json:"service_id"`
	Time       time.Time `json:"time"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Reason     string    `json:"reason"`
}

func (s *Store) RecentEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(
		`SELECT service_id, ts, from_status, to_status, reason FROM events ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ServiceID, &ts, &e.FromStatus, &e.ToStatus, &e.Reason); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DayBucket is one day of aggregated uptime for a service.
type DayBucket struct {
	Date    string  `json:"date"` // YYYY-MM-DD (UTC)
	UpRatio float64 `json:"up_ratio"`
	Samples int     `json:"samples"`
}

// DailyUptime returns per-day up-ratio over the trailing `days` days.
// Days with no samples are omitted; the frontend renders them as "no data".
func (s *Store) DailyUptime(serviceID string, days int) ([]DayBucket, error) {
	since := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := s.db.Query(
		`SELECT date(ts, 'unixepoch') AS day, AVG(ok), COUNT(*)
		 FROM probe_history WHERE service_id = ? AND ts >= ?
		 GROUP BY day ORDER BY day`, serviceID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayBucket{}
	for rows.Next() {
		var b DayBucket
		if err := rows.Scan(&b.Date, &b.UpRatio, &b.Samples); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LatencyPoint is a single probe sample for detail charts.
type LatencyPoint struct {
	Time      time.Time `json:"time"`
	OK        bool      `json:"ok"`
	LatencyMS int64     `json:"latency_ms"`
	Detail    string    `json:"detail,omitempty"`
}

func (s *Store) RecentHistory(serviceID string, hours, limit int) ([]LatencyPoint, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.db.Query(
		`SELECT ts, ok, latency_ms, detail FROM probe_history
		 WHERE service_id = ? AND ts >= ? ORDER BY ts DESC LIMIT ?`, serviceID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencyPoint{}
	for rows.Next() {
		var p LatencyPoint
		var ts int64
		var ok int
		if err := rows.Scan(&ts, &ok, &p.LatencyMS, &p.Detail); err != nil {
			return nil, err
		}
		p.Time = time.Unix(ts, 0)
		p.OK = ok == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// Retain deletes history and events older than the retention window.
func (s *Store) Retain(days int) error {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	if _, err := s.db.Exec(`DELETE FROM probe_history WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	return err
}

// RetainLoop runs Retain once per hour until ctx is done.
func (s *Store) RetainLoop(ctx context.Context, days int) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.Retain(days)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
