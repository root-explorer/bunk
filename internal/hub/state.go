package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the hub's SQLite state: machines, pairing codes, ACLs.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the hub database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS machines (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  pubkey     TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS codes (
  code       TEXT PRIMARY KEY,
  owner      TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS acls (
  a          TEXT NOT NULL,
  b          TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (a, b)
);`); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertMachine registers or updates a machine.
func (s *Store) UpsertMachine(id, name, pubkey string) error {
	_, err := s.db.Exec(
		`INSERT INTO machines(id,name,pubkey,created_at) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, pubkey=excluded.pubkey`,
		id, name, pubkey, time.Now().Unix())
	return err
}

// GetMachine returns a machine's pubkey, or ("", nil) if unknown.
func (s *Store) GetMachine(id string) (string, error) {
	var pub string
	err := s.db.QueryRow(`SELECT pubkey FROM machines WHERE id=?`, id).Scan(&pub)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return pub, err
}

// Machines lists all registered machine ids.
func (s *Store) Machines() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM machines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CreateCode stores a one-time pairing code for owner with a TTL.
func (s *Store) CreateCode(owner, code string, ttl time.Duration) error {
	_, err := s.db.Exec(
		`INSERT INTO codes(code,owner,expires_at) VALUES(?,?,?)`,
		code, owner, time.Now().Add(ttl).Unix())
	return err
}

// ConsumeCode redeems a one-time code and returns its owner.
func (s *Store) ConsumeCode(code string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var owner string
	var expires int64
	err = tx.QueryRow(`SELECT owner, expires_at FROM codes WHERE code=?`, code).Scan(&owner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("invalid pairing code")
	}
	if err != nil {
		return "", err
	}
	if time.Now().Unix() > expires {
		tx.Exec(`DELETE FROM codes WHERE code=?`, code)
		return "", errors.New("pairing code expired")
	}
	if _, err := tx.Exec(`DELETE FROM codes WHERE code=?`, code); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return owner, nil
}

// AddACL authorizes a bidirectional peer link.
func (s *Store) AddACL(a, b string) error {
	if a > b {
		a, b = b, a
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO acls(a,b,created_at) VALUES(?,?,?)`,
		a, b, time.Now().Unix())
	return err
}

// HasACL reports whether a and b are authorized to talk.
func (s *Store) HasACL(a, b string) (bool, error) {
	if a > b {
		a, b = b, a
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM acls WHERE a=? AND b=?`, a, b).Scan(&n)
	return n > 0, err
}

// DeleteACL removes the peer link.
func (s *Store) DeleteACL(a, b string) error {
	if a > b {
		a, b = b, a
	}
	_, err := s.db.Exec(`DELETE FROM acls WHERE a=? AND b=?`, a, b)
	return err
}
