// Package store is the SQLite persistence layer: settings, repos, the build
// queue and history, artifacts, secrets, webhook deliveries and sessions.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS repos (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	full_name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	clone_url       TEXT NOT NULL DEFAULT '',
	webhook_secret  TEXT NOT NULL,
	token           TEXT NOT NULL DEFAULT '',
	timeout_minutes INTEGER NOT NULL DEFAULT 0,
	commit_status   INTEGER NOT NULL DEFAULT 1,
	enabled         INTEGER NOT NULL DEFAULT 1,
	created_at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS branches (
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	name    TEXT NOT NULL,
	PRIMARY KEY (repo_id, name)
);
CREATE TABLE IF NOT EXISTS builds (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id          INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	branch           TEXT NOT NULL,
	commit_sha       TEXT NOT NULL DEFAULT '',
	commit_message   TEXT NOT NULL DEFAULT '',
	author           TEXT NOT NULL DEFAULT '',
	triggered_by     TEXT NOT NULL,
	status           TEXT NOT NULL,
	message          TEXT NOT NULL DEFAULT '',
	retry_of         INTEGER NOT NULL DEFAULT 0,
	artifacts_pruned INTEGER NOT NULL DEFAULT 0,
	created_at       INTEGER NOT NULL,
	started_at       INTEGER NOT NULL DEFAULT 0,
	finished_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS builds_status ON builds(status, id);
CREATE INDEX IF NOT EXISTS builds_repo_branch ON builds(repo_id, branch, id);
CREATE TABLE IF NOT EXISTS artifacts (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
	path     TEXT NOT NULL,
	size     INTEGER NOT NULL,
	sha256   TEXT NOT NULL,
	UNIQUE (build_id, path)
);
CREATE TABLE IF NOT EXISTS secrets (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	value      TEXT NOT NULL,
	all_repos  INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secret_repos (
	secret_id INTEGER NOT NULL REFERENCES secrets(id) ON DELETE CASCADE,
	repo_id   INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	PRIMARY KEY (secret_id, repo_id)
);
CREATE TABLE IF NOT EXISTS deliveries (
	id          TEXT PRIMARY KEY,
	received_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	csrf       TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
`

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- settings ----

const (
	KeyMaxParallel    = "max_parallel_builds"
	KeyMaxWaiting     = "max_waiting_per_branch"
	KeyDefaultTimeout = "default_timeout_minutes"
	KeyRetention      = "retention_builds"
	KeySlackWebhook   = "slack_webhook_url"
	KeyBaseURL        = "base_url"
	KeyGitHubToken    = "github_token"
	KeyAdminPassHash  = "admin_password_hash"
)

var settingDefaults = map[string]string{
	KeyMaxParallel:    "1",
	KeyMaxWaiting:     "1",
	KeyDefaultTimeout: "30",
	KeyRetention:      "10",
}

// Setting returns the stored value, or the built-in default when unset.
func (s *Store) Setting(key string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return settingDefaults[key]
	}
	return v
}

// SettingInt returns a setting as a positive integer, falling back to the default.
func (s *Store) SettingInt(key string) int {
	n, err := strconv.Atoi(s.Setting(key))
	if err != nil || n < 1 {
		n, _ = strconv.Atoi(settingDefaults[key])
	}
	return n
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- webhook deliveries ----

// RecordDelivery stores a delivery ID and reports whether it was new.
func (s *Store) RecordDelivery(id string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO deliveries (id, received_at) VALUES (?, ?)`, id, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// PruneDeliveries forgets delivery IDs older than maxAge.
func (s *Store) PruneDeliveries(maxAge time.Duration) error {
	_, err := s.db.Exec(`DELETE FROM deliveries WHERE received_at < ?`, time.Now().Add(-maxAge).Unix())
	return err
}

// ---- sessions ----

func (s *Store) CreateSession(tokenHash, csrf string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, csrf, expires_at) VALUES (?, ?, ?)`,
		tokenHash, csrf, time.Now().Add(ttl).Unix())
	return err
}

// Session returns the CSRF token of a live session.
func (s *Store) Session(tokenHash string) (csrf string, ok bool) {
	err := s.db.QueryRow(`SELECT csrf FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now()).Scan(&csrf)
	return csrf, err == nil
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteSessionsExcept logs out every session other than keep ("" removes all).
func (s *Store) DeleteSessionsExcept(keep string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash <> ?`, keep)
	return err
}

func (s *Store) PruneSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now())
	return err
}
