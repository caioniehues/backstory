package drain

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/store"
)

// fakeTimers stands in for time.After: the test fires the poll and flush
// waits by sending on them, and every other wait is a backoff, recorded
// and fired by sending on retry.
type fakeTimers struct {
	poll, flush, retry chan time.Time
	mu                 sync.Mutex
	backoffs           []time.Duration
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{poll: make(chan time.Time), flush: make(chan time.Time), retry: make(chan time.Time)}
}

func (f *fakeTimers) after(d time.Duration) <-chan time.Time {
	switch d {
	case PollInterval:
		return f.poll
	case FlushInterval:
		return f.flush
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backoffs = append(f.backoffs, d)
	return f.retry
}

func (f *fakeTimers) asked() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.backoffs)
}

// fire sends on c, failing the test if Run isn't waiting on it.
func fire(t *testing.T, c chan time.Time, what string) {
	t.Helper()
	select {
	case c <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatalf("Run was not waiting on the %s", what)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startRun(t *testing.T, e *env, timers *fakeTimers) *Drain {
	t.Helper()
	cfg := e.config()
	cfg.After = timers.after
	d := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return d
}

// The daemon's drain wakes at start, after the daemon's own writes (one
// batch per flush, however many writes), and on the poll, which is how it
// sees writes from another process.
func TestRunWakesAtStartAfterWritesAndOnThePoll(t *testing.T) {
	e := newEnv(t)
	e.insertN(2)
	timers := newFakeTimers()
	d := startRun(t, e, timers)
	eventually(t, "the start pass", func() bool { return e.counts().Queued == 0 })

	e.st.OnRecordWrite(d.Wake)
	written := e.insertN(3)
	fire(t, timers.flush, "flush after the writes")
	eventually(t, "the written Records' vectors", func() bool { return e.counts().Queued == 0 })
	if got := e.emb.batchSizes(); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("batches = %v, want [2 3]: three writes in one flush make one batch", got)
	}
	for _, id := range written {
		if !e.hasVector(id) {
			t.Errorf("%s not embedded", id)
		}
	}

	cli, err := store.Open(e.path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	id, err := cli.InsertRecord(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityHuman}, Kind: store.KindNote, Text: "written by the CLI"})
	if err != nil {
		t.Fatal(err)
	}
	fire(t, timers.poll, "poll")
	eventually(t, "the CLI-written Record's vector", func() bool { return e.hasVector(id) })
}

// A full batch queued by writes is embedded without waiting for the flush.
func TestRunEmbedsAFullBatchWithoutWaitingForTheFlush(t *testing.T) {
	e := newEnv(t)
	timers := newFakeTimers()
	d := startRun(t, e, timers)
	e.st.OnRecordWrite(d.Wake)
	eventually(t, "the start pass", func() bool { return e.openCount() == 1 })
	e.insertN(BatchSize)
	eventually(t, "the full batch", func() bool { return e.counts().Queued == 0 })
}

// A failing runtime backs off exponentially from FirstBackoff, capped at
// MaxBackoff. A wake during the backoff doesn't retry, but a changed setup
// (the Human fixed the runtime) retries at once.
func TestRunBacksOffExponentiallyCappedAtMax(t *testing.T) {
	e := newEnv(t)
	e.insertN(2)
	e.emb.fail = func([]string) error { return errRuntime }
	timers := newFakeTimers()
	d := startRun(t, e, timers)

	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 320 * time.Second, 10 * time.Minute, 10 * time.Minute}
	eventually(t, "the first backoff", func() bool { return len(timers.asked()) == 1 })
	for i := 1; i < len(want); i++ {
		fire(t, timers.retry, "backoff")
		eventually(t, "the next backoff", func() bool { return len(timers.asked()) == i+1 })
	}
	if got := timers.asked(); !slices.Equal(got, want) {
		t.Fatalf("backoffs = %v\nwant       %v", got, want)
	}

	calls := e.emb.callCount()
	e.st.OnRecordWrite(d.Wake)
	e.insertN(1)
	time.Sleep(50 * time.Millisecond)
	if e.emb.callCount() != calls {
		t.Fatal("a wake during the backoff retried the failing runtime")
	}

	e.emb.mu.Lock()
	e.emb.fail = nil
	e.emb.mu.Unlock()
	e.turnOn("/opt/llama-fixed/llama-embedding")
	d.Wake()
	eventually(t, "the retry after the setup changed", func() bool { return e.counts().Queued == 0 })
}
