package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Each entry runs once, in order. Never edit an entry that has shipped, append a new one.
var migrations = []string{
	`CREATE TABLE boards (
		id          TEXT PRIMARY KEY,
		secret_hash BLOB NOT NULL,
		status      TEXT,
		seen        INTEGER NOT NULL
	)`,
}

type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// checkBoard reports whether secret belongs to the board. With register set, an unknown
// board is stored with this secret (its first connection).
func (s *Store) checkBoard(id, secret string, register bool) (bool, error) {
	sum := sha256.Sum256([]byte(secret))
	var stored []byte
	err := s.db.QueryRow("SELECT secret_hash FROM boards WHERE id = ?", id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		if !register {
			return false, nil
		}
		_, err := s.db.Exec("INSERT INTO boards (id, secret_hash, seen) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
			id, sum[:], time.Now().Unix())
		if err != nil {
			return false, err
		}
		return s.checkBoard(id, secret, false)
	}
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(stored, sum[:]) == 1, nil
}

// rekeyBoard stores a new secret, e.g. after a USB flash erased the board's NVS.
func (s *Store) rekeyBoard(id, secret string) error {
	sum := sha256.Sum256([]byte(secret))
	_, err := s.db.Exec("UPDATE boards SET secret_hash = ? WHERE id = ?", sum[:], id)
	return err
}

func (s *Store) loadStatus(id string) ([]byte, time.Time, error) {
	var status sql.NullString
	var seen int64
	err := s.db.QueryRow("SELECT status, seen FROM boards WHERE id = ?", id).Scan(&status, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var raw []byte
	if status.Valid {
		raw = []byte(status.String)
	}
	return raw, time.Unix(seen, 0), nil
}

func (s *Store) saveStatus(id string, status []byte, seen time.Time) error {
	_, err := s.db.Exec("UPDATE boards SET status = ?, seen = ? WHERE id = ?", string(status), seen.Unix(), id)
	return err
}

func (s *Store) deleteStale(before time.Time) (int64, error) {
	res, err := s.db.Exec("DELETE FROM boards WHERE seen < ?", before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
