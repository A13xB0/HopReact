// Package store is HopReact's SQLite persistence layer: schema migration
// plus every query the rest of the service runs.
//
// The one structural decision worth understanding before changing anything
// here: there are TWO *sql.DB handles over the same file. WAL gives SQLite
// one writer and many concurrent readers, but database/sql pools several
// connections and will happily have two of them try to write at once, which
// surfaces as an intermittent "database is locked" that only appears when a
// poll overlaps a page load. Capping the writer pool at a single connection
// turns that contention into a Go mutex wait instead of an error.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so the binary stays CGO_ENABLED=0
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// dbTx is a short alias for the transaction type the write helpers take.
// Purely readability: these signatures appear on nearly every write.
type dbTx = sql.Tx

// Clock is the service's source of "now", injectable so the alert logic and
// everything that stamps a row can be tested without sleeping.
type Clock func() time.Time

// Store owns the database handles.
type Store struct {
	// write serves every INSERT/UPDATE/DELETE and all migrations. Capped at
	// one connection — see the package comment.
	write *sql.DB
	// read serves queries. Opened read-only so a stray write in a handler
	// fails loudly here rather than contending with the poller.
	read *sql.DB

	Now Clock
}

// Open prepares dir, migrates the database and returns a ready Store.
//
// Order matters: the writer must open and migrate first. A mode=ro
// connection cannot create the -wal and -shm sidecar files, so opening the
// reader against a not-yet-created database fails.
func Open(dir string, now Clock) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, "hopreact.db")

	// Pragmas belong in the DSN, not a startup Exec: with a connection pool
	// an Exec configures only whichever connection happened to serve it.
	//
	// _txlock=immediate is the one that is easy to omit and expensive to
	// debug. It makes BeginTx emit BEGIN IMMEDIATE, taking the write lock
	// up front. Without it a transaction that reads and then writes can hit
	// SQLITE_BUSY_SNAPSHOT — which busy_timeout does NOT retry, so it fails
	// instantly and intermittently.
	//
	// foreign_keys is per-connection and defaults OFF, so it must be on
	// both handles or the ON DELETE CASCADE rules silently do nothing and
	// deleting an account leaves orphans behind forever.
	writeDSN := "file:" + path + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"

	write, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("store: opening writer: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)

	if err := write.Ping(); err != nil {
		write.Close()
		return nil, fmt.Errorf("store: connecting to %s: %w", path, err)
	}
	if err := migrate(write); err != nil {
		write.Close()
		return nil, err
	}

	readDSN := "file:" + path + "?mode=ro" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	read, err := sql.Open("sqlite", readDSN)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: opening reader: %w", err)
	}
	read.SetMaxOpenConns(8)
	read.SetMaxIdleConns(8)
	read.SetConnMaxLifetime(0)
	if err := read.Ping(); err != nil {
		write.Close()
		read.Close()
		return nil, fmt.Errorf("store: connecting reader: %w", err)
	}

	return &Store{write: write, read: read, Now: now}, nil
}

func (s *Store) Close() error {
	var first error
	if err := s.read.Close(); err != nil {
		first = err
	}
	if err := s.write.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// migrate applies any embedded migration newer than PRAGMA user_version.
// Numbered .sql files and an integer version, rather than a migration
// library — it is a dozen lines and matches the project's stdlib-first
// bias.
func migrate(db *sql.DB) error {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: listing migrations: %w", err)
	}
	sort.Strings(entries)

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: reading user_version: %w", err)
	}

	for i, name := range entries {
		n := i + 1
		if n <= version {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: reading %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: applying %s: %w", name, err)
		}
		// PRAGMA takes no bind parameters, hence the formatted literal —
		// n is a loop index, not input.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: setting user_version for %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit %s: %w", name, err)
		}
	}
	return nil
}

// tx runs fn in a write transaction, rolling back on error or panic.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			t.Rollback()
			panic(p)
		}
	}()
	if err := fn(t); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

// nullInt converts a zero time to NULL, so "never" and "at the epoch" stay
// distinguishable in the database.
func nullInt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func timeFrom(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
