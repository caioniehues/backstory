package store

import "time"

// SetClock sets the clock a Draft's expiry is judged against (ExpiredDraft
// and Search's own filter), replacing any earlier one; nil restores
// time.Now. Tests pin it.
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		s.clock.Store(nil)
		return
	}
	s.clock.Store(&now)
}

// now is the store's clock.
func (s *Store) now() time.Time {
	if f := s.clock.Load(); f != nil {
		return (*f)()
	}
	return time.Now()
}

// ExpiredDraft reports whether rec is an expired Draft: an inferred Record
// whose own expiry is at or before the store's clock. An expired Draft is a
// stale guess: never a search result, and left out of a walk that reaches
// it. A Record of another Tier past its expiry, such as a Claim, is not one.
func (s *Store) ExpiredDraft(rec Record) bool {
	return rec.Tier == TierInferred && rec.ExpiresAt != nil && !rec.ExpiresAt.After(s.now())
}

// notExpiredDraft is ExpiredDraft's rule, negated, as a condition over
// records r; its one arg is the store's clock.
const notExpiredDraft = "NOT (r.tier = 'inferred' AND r.expires_at IS NOT NULL AND r.expires_at <= ?)"
