package db

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// DB wraps a SQLite database connection.
type DB struct {
	sql *sql.DB
}

// readWriteDSN configures every read-write handle. Several processes write
// this database - the daemon, independent repair wrappers, and CLI commands -
// so a transaction must take the write lock when it begins (_txlock=immediate)
// and wait for it under busy_timeout. A deferred transaction that reads first
// cannot wait when another process commits before its write: SQLite fails
// that write with SQLITE_BUSY at once, which used to fail runs outright.
const readWriteDSN = "_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(30000)&_txlock=immediate"

// busyRetryDelays bounds execRetryBusy: attempts after the first, each after
// busy_timeout has already been waited out once.
var busyRetryDelays = []time.Duration{time.Second, 3 * time.Second, 10 * time.Second}

// execRetryBusy runs a lifecycle write - a run's status, a step's terminal
// state - retrying while the database stays locked past busy_timeout. Losing
// one of these writes to a lock left runs stuck pending or failed with green
// work; a short retry is cheaper than either.
func (d *DB) execRetryBusy(query string, args ...any) (sql.Result, error) {
	result, err := d.sql.Exec(query, args...)
	for _, delay := range busyRetryDelays {
		if !isBusyErr(err) {
			break
		}
		time.Sleep(delay)
		result, err = d.sql.Exec(query, args...)
	}
	return result, err
}

func isBusyErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked"))
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+"?"+readWriteDSN)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schemaSQL); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	for _, stmt := range migrationStatements {
		if _, err := sqlDB.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
			sqlDB.Close()
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}
	return &DB{sql: sqlDB}, nil
}

// OpenReadOnly opens an existing database without creating or migrating it.
// It is used by pre-mutation authorization, where even schema repair would be
// an unacceptable side effect before the caller is classified.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// isDuplicateColumnErr reports whether err is SQLite's "duplicate column name"
// error, which ALTER TABLE ADD COLUMN emits when the column already exists.
// Treating this as a no-op keeps migrations idempotent without a version table.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.sql.Close()
}

// newID generates a new ULID with monotonic ordering.
func newID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// now returns the current unix timestamp in seconds.
func now() int64 {
	return time.Now().Unix()
}
