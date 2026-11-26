package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func takeLease(t *testing.T, s *Store, holder int, now time.Time) (DrainLease, bool) {
	t.Helper()
	l, ok, err := s.TakeDrainLease(holder, now, time.Minute)
	if err != nil {
		t.Fatalf("TakeDrainLease(%d): %v", holder, err)
	}
	return l, ok
}

// One drain per Device: a second drainer defers to a live lease, the holder
// renews it, and once it expires another holder takes it over.
func TestDrainLeaseDefersRenewsAndIsTakenOverWhenExpired(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	t0 := time.Unix(1_800_000_000, 0)

	if _, held, err := s.DrainLease(t0); err != nil || held {
		t.Fatalf("fresh store: held = %v, %v; want no lease", held, err)
	}
	if l, ok := takeLease(t, s, 100, t0); !ok || l.Holder != 100 || !l.Expires.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first take = %+v, %v", l, ok)
	}
	if l, ok := takeLease(t, s, 200, t0.Add(30*time.Second)); ok || l.Holder != 100 {
		t.Fatalf("second drainer = %+v, %v; want it to defer to 100", l, ok)
	}
	if l, ok := takeLease(t, s, 100, t0.Add(50*time.Second)); !ok || !l.Expires.Equal(t0.Add(110*time.Second)) {
		t.Fatalf("renew = %+v, %v; want expiry pushed to now+ttl", l, ok)
	}
	if l, ok := takeLease(t, s, 200, t0.Add(100*time.Second)); ok || l.Holder != 100 {
		t.Fatalf("before the renewed expiry: %+v, %v; want 100 still holding", l, ok)
	}
	if l, ok := takeLease(t, s, 200, t0.Add(111*time.Second)); !ok || l.Holder != 200 {
		t.Fatalf("expired lease = %+v, %v; want 200 to take it over", l, ok)
	}
	if _, ok := takeLease(t, s, 100, t0.Add(112*time.Second)); ok {
		t.Fatal("the old holder renewed a lease it lost")
	}
	if l, held, _ := s.DrainLease(t0.Add(112 * time.Second)); !held || l.Holder != 200 {
		t.Fatalf("DrainLease = %+v, %v; want 200", l, held)
	}

	if err := s.ReleaseDrainLease(100); err != nil {
		t.Fatal(err)
	}
	if l, held, _ := s.DrainLease(t0.Add(112 * time.Second)); !held || l.Holder != 200 {
		t.Fatal("a non-holder's release cleared the lease")
	}
	if err := s.ReleaseDrainLease(200); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := s.DrainLease(t0.Add(112 * time.Second)); held {
		t.Fatal("the holder's release left the lease held")
	}
	if _, ok := takeLease(t, s, 300, t0.Add(113*time.Second)); !ok {
		t.Fatal("a released lease could not be taken")
	}
}

// Two processes racing for a free lease: exactly one gets it.
func TestDrainLeaseRaceHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backstory.db")
	mustOpen(t, path)
	now := time.Now()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for holder := 1; holder <= 8; holder++ {
		s := mustOpen(t, path)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.TakeDrainLease(holder, now, time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d drainers took the lease, want 1", wins)
	}
}
