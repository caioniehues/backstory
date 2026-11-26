package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DrainLeaseKey is the Device-local settings row that makes the embedding
// drain one per Device: "<holder pid> <expiry, unix ns>".
const DrainLeaseKey = "embed.drain_lease"

// DrainLease is who holds the drain lease, and until when.
type DrainLease struct {
	// Holder is the holding process's pid.
	Holder int
	// Expires is when the lease lapses unless the holder renews it.
	Expires time.Time
}

// TakeDrainLease takes the drain lease for holder until now+ttl, or renews
// it when holder already has it. ok is false, with the live lease returned,
// when another holder's lease hasn't expired: the caller defers. Read and
// write are one transaction, so two drainers racing can't both get it.
func (s *Store) TakeDrainLease(holder int, now time.Time, ttl time.Duration) (DrainLease, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DrainLease{}, false, fmt.Errorf("store: take drain lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, held, err := readDrainLease(tx, now)
	if err != nil {
		return DrainLease{}, false, err
	}
	if held && cur.Holder != holder {
		return cur, false, nil
	}
	l := DrainLease{Holder: holder, Expires: now.Add(ttl)}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		DrainLeaseKey, strconv.Itoa(holder)+" "+strconv.FormatInt(l.Expires.UnixNano(), 10)); err != nil {
		return DrainLease{}, false, fmt.Errorf("store: take drain lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DrainLease{}, false, fmt.Errorf("store: take drain lease: %w", err)
	}
	return l, true, nil
}

// ReleaseDrainLease gives the lease up if holder has it, so the next
// drainer needn't wait for it to expire. Another holder's lease is left.
func (s *Store) ReleaseDrainLease(holder int) error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key = ? AND value LIKE ?`,
		DrainLeaseKey, strconv.Itoa(holder)+" %"); err != nil {
		return fmt.Errorf("store: release drain lease: %w", err)
	}
	return nil
}

// DrainLease is the lease as of now, held false when nobody holds it or it
// has expired.
func (s *Store) DrainLease(now time.Time) (DrainLease, bool, error) {
	return readDrainLease(s.db, now)
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readDrainLease(q queryRower, now time.Time) (DrainLease, bool, error) {
	var v string
	err := q.QueryRow(`SELECT value FROM settings WHERE key = ?`, DrainLeaseKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return DrainLease{}, false, nil
	}
	if err != nil {
		return DrainLease{}, false, fmt.Errorf("store: read drain lease: %w", err)
	}
	pid, exp, ok := strings.Cut(v, " ")
	holder, perr := strconv.Atoi(pid)
	ns, eerr := strconv.ParseInt(exp, 10, 64)
	if !ok || perr != nil || eerr != nil {
		// An unreadable lease protects nothing: treat it as free.
		return DrainLease{}, false, nil
	}
	l := DrainLease{Holder: holder, Expires: time.Unix(0, ns)}
	return l, now.Before(l.Expires), nil
}
