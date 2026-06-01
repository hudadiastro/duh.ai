package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id          TEXT PRIMARY KEY,
  source      TEXT NOT NULL,
  kind        TEXT NOT NULL,
  occurred_at INTEGER NOT NULL,
  title       TEXT NOT NULL,
  url         TEXT,
  project     TEXT,
  status      TEXT,
  meta        TEXT NOT NULL DEFAULT '{}',
  ingested_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_by_time   ON events(occurred_at DESC);
CREATE INDEX IF NOT EXISTS events_by_source ON events(source, occurred_at DESC);

CREATE TABLE IF NOT EXISTS summaries (
  key         TEXT PRIMARY KEY,
  body        TEXT NOT NULL,
  generated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS refresh_log (
  source       TEXT PRIMARY KEY,
  last_run_at  INTEGER NOT NULL,
  status       TEXT NOT NULL,
  message      TEXT
);
`

type Event struct {
	ID         string
	Source     string // claude_code | github | jira
	Kind       string // session | commit | ticket
	OccurredAt time.Time
	Title      string
	URL        string
	Project    string
	Status     string
	Meta       map[string]any
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := ensureDir(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpsertEvent(ctx context.Context, e Event) error {
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	meta, _ := json.Marshal(e.Meta)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO events (id, source, kind, occurred_at, title, url, project, status, meta, ingested_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  title=excluded.title,
  url=excluded.url,
  project=excluded.project,
  status=excluded.status,
  meta=excluded.meta`,
		e.ID, e.Source, e.Kind, e.OccurredAt.Unix(), e.Title, e.URL, e.Project, e.Status, string(meta), time.Now().Unix())
	return err
}

func (s *Store) ListEvents(ctx context.Context, from, to time.Time, sources []string) ([]Event, error) {
	q := `SELECT id, source, kind, occurred_at, title, COALESCE(url,''), COALESCE(project,''), COALESCE(status,''), meta
	      FROM events WHERE occurred_at >= ? AND occurred_at < ?`
	args := []any{from.Unix(), to.Unix()}
	if len(sources) > 0 {
		q += " AND source IN ("
		for i, s := range sources {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, s)
		}
		q += ")"
	}
	q += " ORDER BY occurred_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Event, 0, 64)
	for rows.Next() {
		var e Event
		var ts int64
		var metaStr string
		if err := rows.Scan(&e.ID, &e.Source, &e.Kind, &ts, &e.Title, &e.URL, &e.Project, &e.Status, &metaStr); err != nil {
			return nil, err
		}
		e.OccurredAt = time.Unix(ts, 0).In(time.Local)
		if metaStr != "" {
			_ = json.Unmarshal([]byte(metaStr), &e.Meta)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type DayBucket struct {
	Date   time.Time
	Source string
	Count  int
}

func (s *Store) DailyCounts(ctx context.Context, from, to time.Time) ([]DayBucket, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT strftime('%Y-%m-%d', datetime(occurred_at, 'unixepoch', 'localtime')) AS day,
       source, COUNT(*) FROM events
WHERE occurred_at >= ? AND occurred_at < ?
GROUP BY day, source
ORDER BY day ASC`,
		from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DayBucket
	for rows.Next() {
		var dayStr, src string
		var c int
		if err := rows.Scan(&dayStr, &src, &c); err != nil {
			return nil, err
		}
		d, _ := time.ParseInLocation("2006-01-02", dayStr, time.Local)
		out = append(out, DayBucket{Date: d, Source: src, Count: c})
	}
	return out, rows.Err()
}

func (s *Store) SaveSummary(ctx context.Context, key, body string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO summaries(key, body, generated_at) VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET body=excluded.body, generated_at=excluded.generated_at`,
		key, body, time.Now().Unix())
	return err
}

func (s *Store) GetSummary(ctx context.Context, key string) (string, time.Time, error) {
	var body string
	var ts int64
	err := s.db.QueryRowContext(ctx, `SELECT body, generated_at FROM summaries WHERE key=?`, key).Scan(&body, &ts)
	if err == sql.ErrNoRows {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return body, time.Unix(ts, 0), nil
}

func (s *Store) RecordRefresh(ctx context.Context, source, status, msg string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO refresh_log(source, last_run_at, status, message) VALUES(?, ?, ?, ?)
ON CONFLICT(source) DO UPDATE SET last_run_at=excluded.last_run_at, status=excluded.status, message=excluded.message`,
		source, time.Now().Unix(), status, msg)
	return err
}

type RefreshStatus struct {
	Source    string
	LastRunAt time.Time
	Status    string
	Message   string
}

func (s *Store) RefreshStatuses(ctx context.Context) ([]RefreshStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, last_run_at, status, COALESCE(message,'') FROM refresh_log ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefreshStatus
	for rows.Next() {
		var r RefreshStatus
		var ts int64
		if err := rows.Scan(&r.Source, &ts, &r.Status, &r.Message); err != nil {
			return nil, err
		}
		r.LastRunAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" {
		return nil
	}
	return mkdirAll(dir)
}
