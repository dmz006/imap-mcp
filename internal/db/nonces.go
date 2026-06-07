package db

import (
	"database/sql"
	"errors"
	"time"
)

// NonceRepo provides replay protection for the inbound command channel.
// It implements the trust.NonceStore interface.
type NonceRepo struct{ db *sql.DB }

// SeenOrRecord returns true if (account, nonce) was already recorded. Otherwise
// it records it atomically and returns false. The INSERT's primary-key
// constraint makes the check race-free even under concurrent delivery.
func (r *NonceRepo) SeenOrRecord(account, nonce string, ts time.Time) (bool, error) {
	var cmdTS any
	if !ts.IsZero() {
		cmdTS = ts.Unix()
	}
	_, err := r.db.Exec(
		`INSERT INTO inbound_nonces (account, nonce, cmd_ts) VALUES (?, ?, ?)`,
		account, nonce, cmdTS,
	)
	if err == nil {
		return false, nil // freshly recorded
	}
	// Unique-constraint violation → already seen (replay).
	var sqliteErr interface{ Error() string }
	if errors.As(err, &sqliteErr) {
		// modernc.org/sqlite reports constraint violations in the message;
		// confirm it's the PK collision rather than a real error.
		var exists int
		if qerr := r.db.QueryRow(
			`SELECT 1 FROM inbound_nonces WHERE account = ? AND nonce = ?`, account, nonce,
		).Scan(&exists); qerr == nil {
			return true, nil
		}
	}
	return false, err
}

// PruneNonces removes nonce records older than the given age, bounding table
// growth. Records remain long enough to cover any reasonable replay window.
func (r *NonceRepo) PruneNonces(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan).Unix()
	_, err := r.db.Exec(`DELETE FROM inbound_nonces WHERE seen_at < ?`, cutoff)
	return err
}
