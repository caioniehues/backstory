package embed

import "time"

// The runtime-failure backoff (#20: "back off exponentially, capped at about
// 10 min"). It is the one rule: the drain (#42) and the query server both
// back off by it.
const (
	BackoffBase = 5 * time.Second
	BackoffCap  = 10 * time.Minute
)

// Backoff is how long a failing runtime waits before its next try: Base,
// then double each consecutive failure, never above Cap. The drain backs
// off failed passes with it, the query server crashes and failed starts.
// The zero value uses
// BackoffBase and BackoffCap. It is not safe for concurrent use.
type Backoff struct {
	Base, Cap time.Duration
	failures  int
}

// Next is the wait after one more consecutive failure.
func (b *Backoff) Next() time.Duration {
	base, ceiling := b.Base, b.Cap
	if base <= 0 {
		base = BackoffBase
	}
	if ceiling <= 0 {
		ceiling = BackoffCap
	}
	d := base
	for i := 0; i < b.failures && d < ceiling; i++ {
		d *= 2
	}
	b.failures++
	return min(d, ceiling)
}

// Reset starts over after a success.
func (b *Backoff) Reset() { b.failures = 0 }
