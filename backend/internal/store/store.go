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
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const (
	configStateKey = "config_state"
	sqliteHeader   = "SQLite format 3\x00"
)

type storageBackend string

const (
	backendSQLite   storageBackend = "sqlite"
	backendPostgres storageBackend = "postgres"
)

// FileStore keeps the public type name stable while switching the persistence
// backend from a JSON file to SQLite.
type FileStore struct {
	path    string
	backend storageBackend
	db      *sql.DB
	mu      sync.RWMutex
	state   model.State
}

// HealthCounts summarizes lightweight in-memory counts for health endpoints.
type HealthCounts struct {
	Providers           int
	Routes              int
	GatewayKeysTotal    int
	GatewayKeysActive   int
	Integrations        int
	PricingProfiles     int
	RequestHistoryItems int
}

// Open initializes the configured persistence backend and imports legacy JSON
// state when the SQLite path still contains the previous file-store format.
func Open(locator string) (*FileStore, error) {
	locator = strings.TrimSpace(locator)
	if locator == "" {
		return nil, errors.New("state path or database url is required")
	}

	backend, err := detectBackend(locator)
	if err != nil {
		return nil, err
	}

	var legacyState *model.State
	if backend == backendSQLite {
		if err := os.MkdirAll(filepath.Dir(locator), 0o755); err != nil {
			return nil, err
		}
		legacyState, err = loadLegacyJSON(locator)
		if err != nil {
			return nil, err
		}
	}

	db, err := openDB(backend, locator)
	if err != nil {
		return nil, err
	}

	store := &FileStore{
		path:    locator,
		backend: backend,
		db:      db,
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

func detectBackend(locator string) (storageBackend, error) {
	lower := strings.ToLower(strings.TrimSpace(locator))
	switch {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return backendPostgres, nil
	case lower != "":
		return backendSQLite, nil
	default:
		return "", errors.New("state path or database url is required")
	}
}

func openDB(backend storageBackend, locator string) (*sql.DB, error) {
	switch backend {
	case backendSQLite:
		db, err := sql.Open("sqlite", locator)
		if err != nil {
			return nil, err
		}
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
			_ = db.Close()
			return nil, err
		}
		return db, nil
	case backendPostgres:
		db, err := sql.Open("pgx", locator)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(16)
		db.SetMaxIdleConns(4)
		db.SetConnMaxIdleTime(5 * time.Minute)
		db.SetConnMaxLifetime(30 * time.Minute)
		return db, nil
	default:
		return nil, fmt.Errorf("unsupported backend %q", backend)
	}
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

func (s *FileStore) Backend() string {
	if s == nil {
		return ""
	}
	return string(s.backend)
}

func (s *FileStore) SupportsBackup() bool {
	return s != nil && s.backend == backendSQLite
}

// Backup creates a point-in-time SQLite snapshot under dir and removes older
// snapshots beyond the requested retention count.
func (s *FileStore) Backup(dir string, retain int) (string, error) {
	if s == nil {
		return "", errors.New("store is nil")
	}
	if !s.SupportsBackup() {
		return "", fmt.Errorf("backup is only supported for %s stores", backendSQLite)
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("backup directory is required")
	}
	if retain <= 0 {
		return "", errors.New("backup retention must be greater than 0")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	backupPath := filepath.Join(dir, backupFilename(s.path, time.Now().UTC()))

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return "", errors.New("store is closed")
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE);`); err != nil {
		return "", err
	}
	query := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(backupPath, "'", "''"))
	if _, err := s.db.Exec(query); err != nil {
		return "", err
	}
	if err := pruneBackups(dir, backupPrefix(s.path), retain); err != nil {
		return "", err
	}
	return backupPath, nil
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

// HealthCounts returns lightweight state counts without cloning the full
// configuration snapshot or request history.
func (s *FileStore) HealthCounts(now time.Time) HealthCounts {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := HealthCounts{
		Providers:           len(s.state.Providers),
		Routes:              len(s.state.ModelRoutes),
		GatewayKeysTotal:    len(s.state.GatewayKeys),
		Integrations:        len(s.state.Integrations),
		PricingProfiles:     len(s.state.PricingProfiles),
		RequestHistoryItems: len(s.state.RequestHistory),
	}
	for _, key := range s.state.GatewayKeys {
		if key.Enabled && !key.IsExpired(now) {
			counts.GatewayKeysActive++
		}
	}
	return counts
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

func backupFilename(statePath string, now time.Time) string {
	return fmt.Sprintf("%s-%s.db", backupPrefix(statePath), now.UTC().Format("20060102T150405.000000000Z"))
}

func backupPrefix(statePath string) string {
	base := filepath.Base(strings.TrimSpace(statePath))
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "conduit"
	}
	return base
}

func pruneBackups(dir, prefix string, retain int) error {
	if retain <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	matches := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix+"-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		matches = append(matches, filepath.Join(dir, name))
	}
	sort.Strings(matches)
	if len(matches) <= retain {
		return nil
	}
	for _, path := range matches[:len(matches)-retain] {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *FileStore) initSchema() error {
	ddl := sqliteSchemaDDL
	if s.backend == backendPostgres {
		ddl = postgresSchemaDDL
	}
	_, err := s.db.Exec(ddl)
	return err
}

const sqliteSchemaDDL = `
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

const postgresSchemaDDL = `
CREATE TABLE IF NOT EXISTS metadata (
  key TEXT PRIMARY KEY,
  value BYTEA NOT NULL,
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
  payload BYTEA NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_request_records_started_at ON request_records(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_records_route_alias ON request_records(route_alias);
CREATE INDEX IF NOT EXISTS idx_request_records_gateway_key ON request_records(gateway_key_id);

CREATE TABLE IF NOT EXISTS request_attempts (
  request_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  started_at TEXT NOT NULL,
  payload BYTEA NOT NULL,
  PRIMARY KEY (request_id, sequence)
);

CREATE INDEX IF NOT EXISTS idx_request_attempts_started_at ON request_attempts(started_at DESC);
`

func (s *FileStore) rebind(query string) string {
	if s == nil || s.backend != backendPostgres {
		return query
	}
	return rebindQuery(query)
}

func rebindQuery(query string) string {
	var builder strings.Builder
	builder.Grow(len(query) + 8)
	placeholder := 1
	for _, char := range query {
		if char == '?' {
			builder.WriteString(fmt.Sprintf("$%d", placeholder))
			placeholder++
			continue
		}
		builder.WriteRune(char)
	}
	return builder.String()
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
	row := s.db.QueryRow(s.rebind(`SELECT value FROM metadata WHERE key = ?`), configStateKey)
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
	rows, err := s.db.Query(s.rebind(query), args...)
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
	recordUpsert := `INSERT OR REPLACE INTO request_records (id, started_at, protocol, route_alias, status_code, stream, provider_name, account_id, gateway_key_id, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if s.backend == backendPostgres {
		recordUpsert = `INSERT INTO request_records (id, started_at, protocol, route_alias, status_code, stream, provider_name, account_id, gateway_key_id, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   started_at = excluded.started_at,
		   protocol = excluded.protocol,
		   route_alias = excluded.route_alias,
		   status_code = excluded.status_code,
		   stream = excluded.stream,
		   provider_name = excluded.provider_name,
		   account_id = excluded.account_id,
		   gateway_key_id = excluded.gateway_key_id,
		   payload = excluded.payload`
	}
	if _, err = tx.Exec(
		s.rebind(recordUpsert),
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

	if _, err = tx.Exec(s.rebind(`DELETE FROM request_attempts WHERE request_id = ?`), record.ID); err != nil {
		return err
	}
	for _, attempt := range attempts {
		payload, marshalErr := json.Marshal(attempt)
		if marshalErr != nil {
			err = marshalErr
			return err
		}
		if _, err = tx.Exec(
			s.rebind(`INSERT INTO request_attempts (request_id, sequence, started_at, payload) VALUES (?, ?, ?, ?)`),
			attempt.RequestID,
			attempt.Sequence,
			attempt.StartedAt.UTC().Format(time.RFC3339Nano),
			payload,
		); err != nil {
			return err
		}
	}

	if maxItems > 0 {
		trimQuery := `DELETE FROM request_records
			  WHERE id IN (
			    SELECT id FROM request_records
			    ORDER BY started_at DESC
			    LIMIT -1 OFFSET ?
			  )`
		if s.backend == backendPostgres {
			trimQuery = `DELETE FROM request_records
			  WHERE id IN (
			    SELECT id FROM request_records
			    ORDER BY started_at DESC
			    LIMIT ALL OFFSET ?
			  )`
		}
		if _, err = tx.Exec(
			s.rebind(trimQuery),
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
	rows, err := db.Query(s.rebind(`SELECT payload FROM request_attempts WHERE request_id = ? ORDER BY sequence ASC`), requestID)
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

	if err = persistConfigTx(tx, s.backend, state); err != nil {
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
				s.rebind(`INSERT INTO request_records (id, started_at, protocol, route_alias, status_code, stream, provider_name, account_id, gateway_key_id, payload)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
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
	return persistConfigDB(s.db, s.backend, state)
}

func persistConfigDB(db *sql.DB, backend storageBackend, state model.State) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = persistConfigTx(tx, backend, state); err != nil {
		return err
	}
	return tx.Commit()
}

func persistConfigTx(tx *sql.Tx, backend storageBackend, state model.State) error {
	configOnly := state.Clone()
	configOnly.RequestHistory = []model.RequestRecord{}
	configOnly.Normalize()
	payload, err := json.Marshal(configOnly)
	if err != nil {
		return err
	}
	query := `INSERT INTO metadata (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
	if backend == backendPostgres {
		query = rebindQuery(query)
	}
	_, err = tx.Exec(
		query,
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
