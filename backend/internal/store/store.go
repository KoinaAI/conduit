package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const (
	configStateKey = "config_state"
	sqliteHeader   = "SQLite format 3\x00"
)

// FileStore keeps the public type name stable while switching the persistence
// backend from a JSON file to SQLite.
type FileStore struct {
	path  string
	db    *sql.DB
	mu    sync.RWMutex
	state model.State
}

// Open initializes the SQLite-backed store and imports legacy JSON state when
// the configured path still contains the previous file-store format.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	legacyState, err := loadLegacyJSON(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}

	store := &FileStore{
		path: path,
		db:   db,
	}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.load(legacyState); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close flushes the SQLite WAL before closing the backing database handle.
func (s *FileStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE);`)
	err := s.db.Close()
	s.db = nil
	return err
}

// Ping verifies that the backing SQLite connection is still healthy.
func (s *FileStore) Ping(ctx context.Context) error {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return errors.New("store is closed")
	}
	return db.PingContext(ctx)
}

func loadLegacyJSON(path string) (*model.State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if bytes.HasPrefix(data, []byte(sqliteHeader)) {
		return nil, nil
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil
	}

	var state model.State
	if err := json.Unmarshal(trimmed, &state); err != nil {
		return nil, fmt.Errorf("legacy state import failed: %w", err)
	}
	state.Normalize()

	backupPath := path + ".legacy.json"
	if _, statErr := os.Stat(backupPath); errors.Is(statErr, os.ErrNotExist) {
		if err := os.WriteFile(backupPath, append(trimmed, '\n'), 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return &state, nil
}

func (s *FileStore) initSchema() error {
	ddl := `
CREATE TABLE IF NOT EXISTS metadata (
  key TEXT PRIMARY KEY,
  value BLOB NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS request_records (
  id TEXT PRIMARY KEY,
  started_at TEXT NOT NULL,
  protocol TEXT NOT NULL,
  route_alias TEXT NOT NULL,
  status_code INTEGER NOT NULL,
  stream INTEGER NOT NULL,
  provider_name TEXT NOT NULL,
  account_id TEXT NOT NULL,
  gateway_key_id TEXT NOT NULL,
  payload BLOB NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_request_records_started_at ON request_records(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_records_route_alias ON request_records(route_alias);
CREATE INDEX IF NOT EXISTS idx_request_records_gateway_key ON request_records(gateway_key_id);

CREATE TABLE IF NOT EXISTS request_attempts (
  request_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  started_at TEXT NOT NULL,
  payload BLOB NOT NULL,
  PRIMARY KEY (request_id, sequence)
);

CREATE INDEX IF NOT EXISTS idx_request_attempts_started_at ON request_attempts(started_at DESC);
`
	_, err := s.db.Exec(ddl)
	return err
}

func (s *FileStore) load(legacyState *model.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.loadConfigLocked()
	if err != nil {
		return err
	}
	if existing == nil {
		initial := model.DefaultState()
		if legacyState != nil {
			initial = legacyState.Clone()
		}
		initial.Normalize()
		if err := s.persistStateLocked(initial, true); err != nil {
			return err
		}
		s.state = initial
		return nil
	}

	existing.Normalize()
	history, err := s.loadRequestHistoryLocked(1000)
	if err != nil {
		return err
	}
	existing.RequestHistory = history
	existing.Normalize()
	s.state = *existing
	return nil
}

func (s *FileStore) loadConfigLocked() (*model.State, error) {
	row := s.db.QueryRow(`SELECT value FROM metadata WHERE key = ?`, configStateKey)
	var raw []byte
	switch err := row.Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}

	var state model.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *FileStore) loadRequestHistoryLocked(limit int) ([]model.RequestRecord, error) {
	query := `SELECT payload FROM request_records ORDER BY started_at DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []model.RequestRecord{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record model.RequestRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(records)
	return records, nil
}

// Snapshot returns a deep copy of the current configuration plus recent history.
func (s *FileStore) Snapshot() model.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Clone()
}

// RoutingSnapshot returns the hot-path subset.
func (s *FileStore) RoutingSnapshot() model.RoutingState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.RoutingSnapshot()
}

// Update applies a mutation to the persisted snapshot.
func (s *FileStore) Update(fn func(*model.State) error) (model.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.state.Clone()
	if err := fn(&next); err != nil {
		return model.State{}, err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Normalize()
	if err := s.persistConfigLocked(next); err != nil {
		return model.State{}, err
	}
	s.state = next
	return next.Clone(), nil
}

// Replace replaces the full snapshot.
func (s *FileStore) Replace(next model.State) (model.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next = next.Clone()
	next.UpdatedAt = time.Now().UTC()
	next.Normalize()
	if err := s.persistStateLocked(next, true); err != nil {
		return model.State{}, err
	}
	s.state = next
	return next.Clone(), nil
}

// AppendRequestRecord appends one request record and its upstream attempts.
func (s *FileStore) AppendRequestRecord(record model.RequestRecord, attempts []model.RequestAttemptRecord, maxItems int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	recordPayload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(
		`INSERT OR REPLACE INTO request_records (id, started_at, protocol, route_alias, status_code, stream, provider_name, account_id, gateway_key_id, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.StartedAt.UTC().Format(time.RFC3339Nano),
		record.Protocol,
		record.RouteAlias,
		record.StatusCode,
		boolToInt(record.Stream),
		record.ProviderName,
		record.AccountID,
		record.GatewayKeyID,
		recordPayload,
	); err != nil {
		return err
	}

	if _, err = tx.Exec(`DELETE FROM request_attempts WHERE request_id = ?`, record.ID); err != nil {
		return err
	}
	for _, attempt := range attempts {
		payload, marshalErr := json.Marshal(attempt)
		if marshalErr != nil {
			err = marshalErr
			return err
		}
		if _, err = tx.Exec(
			`INSERT INTO request_attempts (request_id, sequence, started_at, payload) VALUES (?, ?, ?, ?)`,
			attempt.RequestID,
			attempt.Sequence,
			attempt.StartedAt.UTC().Format(time.RFC3339Nano),
			payload,
		); err != nil {
			return err
		}
	}

	if maxItems > 0 {
		if _, err = tx.Exec(
			`DELETE FROM request_records
			  WHERE id IN (
			    SELECT id FROM request_records
			    ORDER BY started_at DESC
			    LIMIT -1 OFFSET ?
			  )`,
			maxItems,
		); err != nil {
			return err
		}
		if _, err = tx.Exec(
			`DELETE FROM request_attempts WHERE request_id NOT IN (SELECT id FROM request_records)`,
		); err != nil {
			return err
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	next := s.state.Clone()
	next.RequestHistory = append(slices.Clone(s.state.RequestHistory), record)
	next.RequestHistory = model.TrimHistory(next.RequestHistory, maxItems)
	next.UpdatedAt = time.Now().UTC()
	next.Normalize()
	s.state = next
	return nil
}

// RequestAttempts returns the recorded upstream attempts for one request.
func (s *FileStore) RequestAttempts(requestID string) ([]model.RequestAttemptRecord, error) {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return nil, errors.New("store is closed")
	}
	rows, err := db.Query(`SELECT payload FROM request_attempts WHERE request_id = ? ORDER BY sequence ASC`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attempts := []model.RequestAttemptRecord{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var attempt model.RequestAttemptRecord
		if err := json.Unmarshal(raw, &attempt); err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *FileStore) persistStateLocked(state model.State, replaceHistory bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = persistConfigTx(tx, state); err != nil {
		return err
	}
	if replaceHistory {
		if _, err = tx.Exec(`DELETE FROM request_attempts`); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM request_records`); err != nil {
			return err
		}
		for _, record := range state.RequestHistory {
			payload, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				err = marshalErr
				return err
			}
			if _, err = tx.Exec(
				`INSERT INTO request_records (id, started_at, protocol, route_alias, status_code, stream, provider_name, account_id, gateway_key_id, payload)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				record.ID,
				record.StartedAt.UTC().Format(time.RFC3339Nano),
				record.Protocol,
				record.RouteAlias,
				record.StatusCode,
				boolToInt(record.Stream),
				record.ProviderName,
				record.AccountID,
				record.GatewayKeyID,
				payload,
			); err != nil {
				return err
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *FileStore) persistConfigLocked(state model.State) error {
	return persistConfigDB(s.db, state)
}

func persistConfigDB(db *sql.DB, state model.State) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = persistConfigTx(tx, state); err != nil {
		return err
	}
	return tx.Commit()
}

func persistConfigTx(tx *sql.Tx, state model.State) error {
	configOnly := state.Clone()
	configOnly.RequestHistory = []model.RequestRecord{}
	configOnly.Normalize()
	payload, err := json.Marshal(configOnly)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO metadata (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		configStateKey,
		payload,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
