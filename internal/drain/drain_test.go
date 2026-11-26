package drain

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/store"
)

const testFP = "sha|tpl-test|vulkan|3"

// fakeEmbedder is the batch embedder: every document gets [len, 1, 0], and
// each call's documents are recorded. fail, when set, decides whether a
// call fails; during, when set, runs inside each call.
type fakeEmbedder struct {
	mu     sync.Mutex
	calls  [][]string
	fail   func(docs []string) error
	during func(call int, docs []string)
}

func (f *fakeEmbedder) Embed(_ context.Context, docs []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(docs))
	call, fail, during := len(f.calls), f.fail, f.during
	f.mu.Unlock()
	if during != nil {
		during(call, docs)
	}
	if fail != nil {
		if err := fail(docs); err != nil {
			return nil, err
		}
	}
	out := make([][]float32, len(docs))
	for i, d := range docs {
		out[i] = []float32{float32(len(d)), 1, 0}
	}
	return out, nil
}

func (f *fakeEmbedder) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n []int
	for _, c := range f.calls {
		n = append(n, len(c))
	}
	return n
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type env struct {
	t     *testing.T
	st    *store.Store
	path  string
	emb   *fakeEmbedder
	opens int
	// openErr, when set, makes Open fail.
	openErr error
	mu      sync.Mutex
}

// newEnv is a real migrated store with embeddings switched on (the setup's
// files are never run: the Opener hands out the fake).
func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backstory.db")
	st, err := store.Open(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{t: t, st: st, path: path, emb: &fakeEmbedder{}}
	e.turnOn("/opt/llama/llama-embedding")
	return e
}

func (e *env) turnOn(runtime string) {
	e.t.Helper()
	if err := embed.SaveSetup(e.st, embed.Setup{Runtime: runtime, Model: "/m/gemma.gguf", Device: "RX 7800 XT"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) open(context.Context, embed.Setup) (Embedder, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opens++
	if e.openErr != nil {
		return nil, "", e.openErr
	}
	return e.emb, testFP, nil
}

func (e *env) openCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opens
}

func (e *env) config() Config {
	return Config{Store: e.st, Holder: 4242, Open: e.open}
}

func (e *env) insert(kind store.RecordKind, text string) string {
	e.t.Helper()
	id, err := e.st.InsertRecord(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityAgent}, Kind: kind, Text: text})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) insertN(n int) []string {
	e.t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = e.insert(store.KindNote, fmt.Sprintf("note number %d", i))
	}
	return ids
}

func (e *env) counts() store.EmbedCounts {
	e.t.Helper()
	c, err := e.st.EmbedCounts(testFP)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) hasVector(id string) bool {
	e.t.Helper()
	_, ok, err := e.st.RecordVector(id, testFP)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func pass(t *testing.T, d *Drain) (Outcome, error) {
	t.Helper()
	return d.Pass(context.Background())
}

// A pass embeds the whole queue in BatchSize batches, each document the
// Record's text, and stores every vector under the fingerprint.
func TestPassEmbedsTheQueueInBatches(t *testing.T) {
	e := newEnv(t)
	e.insertN(BatchSize*2 + 3)
	e.insert(store.KindDecision, "a decision")
	if _, err := e.st.InsertRecord(store.InsertRecordParams{Identity: store.Identity{Kind: store.IdentityAgent},
		Kind: store.KindHandoff, Text: "paused here", Next: "ship it"}); err != nil {
		t.Fatal(err)
	}
	var progress []store.EmbedCounts
	cfg := e.config()
	cfg.Progress = func(p Progress) { progress = append(progress, p.Counts) }
	out, err := pass(t, New(cfg))
	if out != Drained || err != nil {
		t.Fatalf("Pass = %v, %v; want Drained", out, err)
	}
	if got, want := e.emb.batchSizes(), []int{BatchSize, BatchSize, 5}; !slices.Equal(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	if !slices.Contains(e.emb.calls[0], "paused here\n\nship it") {
		t.Errorf("the newest Handoff's document is missing from the first batch: %q", e.emb.calls[0][:2])
	}
	if c := e.counts(); c != (store.EmbedCounts{Embedded: BatchSize*2 + 5}) {
		t.Fatalf("counts = %+v, want everything embedded", c)
	}
	if len(progress) != 3 || progress[2].Queued != 0 || progress[0].Embedded != BatchSize {
		t.Fatalf("progress = %+v", progress)
	}
	if _, held, _ := e.st.DrainLease(time.Now()); held {
		t.Error("the lease is still held after the pass")
	}
}

// One drain per Device: a pass defers to another holder's live lease
// without opening the runtime, and takes an expired one over.
func TestPassDefersToALiveLeaseAndTakesOverAnExpiredOne(t *testing.T) {
	e := newEnv(t)
	e.insertN(3)
	now := time.Now()
	if _, ok, err := e.st.TakeDrainLease(7, now, time.Minute); !ok || err != nil {
		t.Fatal("could not take the other drainer's lease")
	}
	cfg := e.config()
	clock := now
	cfg.Now = func() time.Time { return clock }
	d := New(cfg)
	if out, err := pass(t, d); out != Deferred || err != nil {
		t.Fatalf("Pass under a live lease = %v, %v; want Deferred", out, err)
	}
	if e.opens != 0 || e.emb.callCount() != 0 {
		t.Fatalf("a deferring pass opened the runtime (%d) or embedded (%d)", e.opens, e.emb.callCount())
	}
	clock = now.Add(time.Minute + time.Second)
	if out, err := pass(t, d); out != Drained || err != nil {
		t.Fatalf("Pass after the lease expired = %v, %v; want Drained", out, err)
	}
	if c := e.counts(); c.Queued != 0 {
		t.Fatalf("counts = %+v after taking the lease over", c)
	}
}

// The lease is renewed after each batch, so a long pass keeps it.
func TestPassRenewsTheLeaseAfterEachBatch(t *testing.T) {
	e := newEnv(t)
	e.insertN(BatchSize * 4)
	clock := time.Now()
	cfg := e.config()
	cfg.Now = func() time.Time { return clock }
	e.emb.during = func(int, []string) {
		l, held, err := e.st.DrainLease(clock)
		if err != nil || !held || l.Holder != 4242 {
			t.Errorf("mid-pass lease = %+v, %v, %v; want held by the drain", l, held, err)
		}
		clock = clock.Add(LeaseTTL / 2)
	}
	if out, err := pass(t, New(cfg)); out != Drained || err != nil {
		t.Fatalf("Pass = %v, %v", out, err)
	}
	if e.emb.callCount() != 4 {
		t.Fatalf("%d batches, want 4", e.emb.callCount())
	}
}

// A Record Tombstoned while its batch is embedding gets no vector: the
// conditional upsert refuses it, and the rest of the batch is stored.
func TestARecordTombstonedMidBatchGetsNoVector(t *testing.T) {
	e := newEnv(t)
	ids := e.insertN(4)
	e.emb.during = func(call int, _ []string) {
		if call == 1 {
			if err := e.st.TombstoneRecord(ids[2], store.Identity{Kind: store.IdentityHuman}); err != nil {
				t.Error(err)
			}
		}
	}
	if out, err := pass(t, New(e.config())); out != Drained || err != nil {
		t.Fatalf("Pass = %v, %v", out, err)
	}
	if e.hasVector(ids[2]) {
		t.Fatal("the Record Tombstoned mid-batch got a vector")
	}
	var n int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM record_embeddings WHERE record_id = ?`, ids[2]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("record_embeddings rows for the Tombstoned Record = %d, %v", n, err)
	}
	for _, id := range []string{ids[0], ids[1], ids[3]} {
		if !e.hasVector(id) {
			t.Errorf("%s has no vector", id)
		}
	}
}

// Embeddings off: a pass does nothing, and --off mid-pass stops the drain
// before its next batch.
func TestOffStopsTheDrain(t *testing.T) {
	e := newEnv(t)
	e.insertN(BatchSize * 3)
	if err := embed.TurnOff(e.st); err != nil {
		t.Fatal(err)
	}
	d := New(e.config())
	if out, err := pass(t, d); out != Off || err != nil || e.opens != 0 {
		t.Fatalf("Pass while off = %v, %v (opens %d); want Off, nothing opened", out, err, e.opens)
	}

	e.turnOn("/opt/llama/llama-embedding")
	e.emb.during = func(call int, _ []string) {
		if call == 1 {
			if err := embed.TurnOff(e.st); err != nil {
				t.Error(err)
			}
		}
	}
	if out, err := pass(t, d); out != Off || err != nil {
		t.Fatalf("Pass turned off mid-way = %v, %v; want Off", out, err)
	}
	if n := e.emb.callCount(); n != 1 {
		t.Fatalf("%d batches embedded, want 1 (--off stops the next)", n)
	}
}

var errRuntime = errors.New("embed: llama-embedding: exit status 1: E main: GPU hang")

// A Record that keeps failing alone, while the rest of its batch embeds, is
// retried up to MaxRecordFailures times, then skipped and counted: it no
// longer blocks the queue or reaches the runtime. The report carries the
// count and the last failure.
func TestARecordThatKeepsFailingIsSkippedAndCounted(t *testing.T) {
	e := newEnv(t)
	good := e.insertN(3)
	poison := e.insert(store.KindDecision, "poison pill")
	e.emb.fail = func(docs []string) error {
		if slices.Contains(docs, "poison pill") {
			return errRuntime
		}
		return nil
	}
	d := New(e.config())
	for i := 0; i < MaxRecordFailures+2; i++ {
		_, _ = pass(t, d)
	}
	for _, id := range good {
		if !e.hasVector(id) {
			t.Errorf("good Record %s has no vector", id)
		}
	}
	if e.hasVector(poison) {
		t.Fatal("the poison Record has a vector")
	}
	poisonTries := 0
	for _, c := range e.emb.calls {
		if slices.Contains(c, "poison pill") {
			poisonTries++
		}
	}
	// The first try is in the full batch, then once alone per failure.
	if poisonTries != MaxRecordFailures+1 {
		t.Fatalf("the poison Record reached the runtime %d times, want %d", poisonTries, MaxRecordFailures+1)
	}
	if d.Skipped() != 1 {
		t.Fatalf("Skipped = %d, want 1", d.Skipped())
	}
	r, ok, err := ReadReport(e.st)
	if err != nil || !ok {
		t.Fatalf("ReadReport = %v, %v", ok, err)
	}
	if r.Skipped != 1 || !strings.Contains(r.LastFailure, "GPU hang") || r.LastFailureAt.IsZero() || r.Fingerprint != testFP {
		t.Fatalf("report = %+v", r)
	}
}

// When nothing in a batch embeds, even alone, the runtime is failing, not
// the Records: none of them is counted toward a skip.
func TestABrokenRuntimeSkipsNoRecord(t *testing.T) {
	e := newEnv(t)
	e.insertN(5)
	e.emb.fail = func([]string) error { return errRuntime }
	d := New(e.config())
	for i := 0; i < MaxRecordFailures+2; i++ {
		if out, err := pass(t, d); out != Failed || !errors.Is(err, errRuntime) {
			t.Fatalf("Pass = %v, %v; want Failed with the runtime's error", out, err)
		}
	}
	if d.Skipped() != 0 || e.counts().Queued != 5 {
		t.Fatalf("skipped %d, counts %+v; want 0 skipped, all 5 queued", d.Skipped(), e.counts())
	}
}

// A runtime that breaks after its first batch fails the pass without
// counting the next batch's Records toward a skip.
func TestARuntimeThatBreaksMidPassSkipsNoRecord(t *testing.T) {
	e := newEnv(t)
	e.insertN(BatchSize + 4)
	e.emb.fail = func([]string) error {
		if e.emb.callCount() > 1 {
			return errRuntime
		}
		return nil
	}
	d := New(e.config())
	for i := 0; i < MaxRecordFailures+1; i++ {
		if out, _ := pass(t, d); out != Failed {
			t.Fatalf("Pass = %v, want Failed", out)
		}
	}
	if d.Skipped() != 0 || e.counts() != (store.EmbedCounts{Embedded: BatchSize, Queued: 4}) {
		t.Fatalf("skipped %d, counts %+v; want 0 skipped, the first batch embedded", d.Skipped(), e.counts())
	}
}

// A runtime that won't open (a device that doesn't resolve, a missing
// file) fails the pass and is reported as the setup error for status.
func TestASetupThatWontOpenIsReported(t *testing.T) {
	e := newEnv(t)
	e.insertN(2)
	e.openErr = errors.New(`embed: no device matches "RX 7800 XT"`)
	if out, err := pass(t, New(e.config())); out != Failed || err == nil {
		t.Fatalf("Pass = %v, %v; want Failed", out, err)
	}
	r, _, err := ReadReport(e.st)
	if err != nil || !strings.Contains(r.SetupError, "no device matches") || r.LastFailureAt.IsZero() {
		t.Fatalf("report = %+v, %v", r, err)
	}
	e.openErr = nil
	if out, err := pass(t, New(e.config())); out != Drained || err != nil {
		t.Fatalf("Pass = %v, %v", out, err)
	}
	if r, _, _ := ReadReport(e.st); r.SetupError != "" || r.Fingerprint != testFP {
		t.Fatalf("report after a good open = %+v; want the setup error cleared", r)
	}
}
