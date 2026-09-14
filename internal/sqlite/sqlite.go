// Package sqlite opens the single SQLite database the PIM process uses.
package sqlite

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // pure Go, no cgo
)

// Open opens path in WAL mode with a busy timeout, so concurrent reads do not
// block on the occasional write. ":memory:" gives a private in-memory database
// (used by tests).
func Open(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := "file:" + path + "?" + q.Encode()
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if path == ":memory:" {
		// Every connection to ":memory:" is a different database.
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	return db, nil
}
