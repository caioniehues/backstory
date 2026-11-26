// Package drain keeps every eligible Record embedded: it embeds the queue
// (store.EmbedQueue) in batches through the batch embedder and stores each
// vector with the conditional upsert, outside any write transaction. The
// daemon runs one in the background (Run); `backstory embed --catch-up`
// runs passes in the foreground (Pass). A lease row keeps it one per Device.
package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/store"
)

const (
	// BatchSize is how many Records one runtime run embeds (#19: about
	// 0.45 s fixed plus 4.4 ms per Record).
	BatchSize = 64
	// FlushInterval is how long a wake waits for more writes before it
	// embeds a batch smaller than BatchSize.
	FlushInterval = 2 * time.Second
	// PollInterval is how often the daemon's drain looks at the queue, to
	// see Records written by the CLI and by Sync.
	PollInterval = 60 * time.Second
	// LeaseTTL is how long the drain lease lasts unless renewed; it is
	// renewed after each batch.
	LeaseTTL = 5 * time.Minute
	// MaxRecordFailures is how many times a Record may fail alone before
	// it is skipped. Skips are counted in memory only: there is no failure
	// table, and a restart tries them again.
	MaxRecordFailures = 3
)

// Embedder is the batch embedder: one vector per document, in order.
// *embed.Batch is one.
type Embedder interface {
	Embed(ctx context.Context, docs []string) ([][]float32, error)
}

// Opener makes the Embedder for setup, with the fingerprint its vectors are
// stored under. It is called at a pass's first batch, and again after a
// runtime failure or a setup change, so the device is resolved anew.
type Opener func(ctx context.Context, setup embed.Setup) (Embedder, string, error)

// OpenBatch is the real Opener: embed.NewBatch checks the files and
// resolves the device, and the fingerprint is embed.FingerprintFor that
// device.
func OpenBatch(s embed.Settings) Opener {
	return func(ctx context.Context, setup embed.Setup) (Embedder, string, error) {
		b, err := embed.NewBatch(ctx, setup)
		if err != nil {
			return nil, "", err
		}
		fp, err := embed.FingerprintFor(s, setup.Model, b.Device())
		if err != nil {
			return nil, "", err
		}
		return b, fp, nil
	}
}

// PassResult is how a pass ended.
type PassResult int

const (
	// Drained: the queue is empty, apart from skipped Records.
	Drained PassResult = iota
	// Off: embeddings are off (or were turned off mid-pass).
	Off
	// Deferred: another drainer holds the lease.
	Deferred
	// Failed: the runtime (or the store) failed; Run backs off.
	Failed
)

func (r PassResult) String() string {
	switch r {
	case Drained:
		return "drained"
	case Off:
		return "off"
	case Deferred:
		return "deferred"
	}
	return "failed"
}

// Progress is reported after each batch.
type Progress struct {
	// Counts is embedded vs queued under the drain's fingerprint.
	Counts store.EmbedCounts
	// Skipped counts the Records this drain has given up on.
	Skipped int
}

// Config is a Drain's collaborators. Only Store, Holder and Open are
// required.
type Config struct {
	Store *store.Store
	// Holder is the lease holder's pid.
	Holder int
	Open   Opener
	// Now defaults to time.Now.
	Now func() time.Time
	// After defaults to time.After. Run takes every wait (flush, poll,
	// backoff) from it.
	After func(time.Duration) <-chan time.Time
	// Logger, when set, gets a line per failure and skip.
	Logger *log.Logger
	// Progress, when set, is called after each batch.
	Progress func(Progress)
}

// Drain embeds the queue. Its per-Record failure counts and skips live in
// its memory only.
type Drain struct {
	cfg  Config
	wake chan struct{}

	mu       sync.Mutex
	emb      Embedder
	fp       string
	setup    embed.Setup
	failures map[string]int
	skipped  map[string]bool
	report   Report
	saved    Report
}

// New makes a Drain.
func New(cfg Config) *Drain {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	return &Drain{cfg: cfg, wake: make(chan struct{}, 1),
		failures: map[string]int{}, skipped: map[string]bool{}}
}

// Wake asks Run for a pass soon (after FlushInterval, or at once when a
// full batch is queued). It never blocks, so a writer can call it.
func (d *Drain) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Skipped counts the Records this drain has given up on.
func (d *Drain) Skipped() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.skipped)
}

// Pass drains the queue once: it reads the setup, takes the lease (or
// defers), embeds batch after batch until the queue is empty, and releases
// the lease. --off is checked before each batch. A batch that fails is
// retried one Record at a time: a Record that fails alone while others
// embed counts toward its skip; if nothing embeds the runtime is failing
// and the pass ends Failed with its error. An empty queue costs one query
// and takes no lease.
func (d *Drain) Pass(ctx context.Context) (PassResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.cfg.Store

	setup, on, err := embed.LoadSetup(st)
	if err != nil {
		return Failed, err
	}
	if !on {
		d.emb = nil
		return Off, nil
	}
	if d.emb != nil && setup != d.setup {
		d.emb = nil
	}
	if d.emb != nil {
		q, err := st.EmbedQueue(d.fp, 1, d.skippedIDs())
		if err != nil {
			return Failed, err
		}
		if len(q) == 0 {
			return Drained, nil
		}
	}

	if _, ok, err := st.TakeDrainLease(d.cfg.Holder, d.cfg.Now(), LeaseTTL); err != nil {
		return Failed, err
	} else if !ok {
		return Deferred, nil
	}
	defer func() { _ = st.ReleaseDrainLease(d.cfg.Holder) }()

	if d.emb == nil {
		emb, fp, err := d.cfg.Open(ctx, setup)
		if err != nil {
			d.report.SetupError = err.Error()
			d.failed(err)
			return Failed, err
		}
		if fp != d.fp {
			// Failures and skips belong to one fingerprint: a new model
			// or runtime gets to try every Record again.
			d.failures, d.skipped = map[string]int{}, map[string]bool{}
		}
		d.emb, d.fp, d.setup = emb, fp, setup
		d.report.Fingerprint, d.report.SetupError, d.report.Skipped = fp, "", len(d.skipped)
		d.save()
	}

	worked := false
	failedNow := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return Failed, err
		}
		if _, on, err := embed.LoadSetup(st); err != nil {
			return Failed, err
		} else if !on {
			d.emb = nil
			return Off, nil
		}
		exclude := d.skippedIDs()
		for id := range failedNow {
			exclude = append(exclude, id)
		}
		batch, err := st.EmbedQueue(d.fp, BatchSize, exclude)
		if err != nil {
			return Failed, err
		}
		if len(batch) == 0 {
			return Drained, nil
		}

		err = d.embed(ctx, batch)
		if err == nil {
			worked = true
		} else {
			if ctx.Err() != nil {
				return Failed, ctx.Err()
			}
			if errors.As(err, new(*storeError)) {
				return Failed, err
			}
			failed, alone := batch, 0
			if len(batch) > 1 {
				failed = nil
				for _, r := range batch {
					if one := d.embed(ctx, []store.QueuedRecord{r}); one != nil {
						if errors.As(one, new(*storeError)) {
							return Failed, one
						}
						failed = append(failed, r)
					} else {
						worked = true
						alone++
					}
				}
			}
			d.failed(err)
			if alone == 0 && len(batch) > 1 {
				// Nothing in the batch embedded, even alone: the runtime
				// is failing, not these Records.
				d.emb = nil
				return Failed, err
			}
			for _, r := range failed {
				failedNow[r.RecordID] = true
				d.failures[r.RecordID]++
				if d.failures[r.RecordID] >= MaxRecordFailures {
					d.skipped[r.RecordID] = true
					d.logf("embeddings: skipping Record %s after %d failures: %v", r.RecordID, d.failures[r.RecordID], err)
				}
			}
			d.report.Skipped = len(d.skipped)
			d.save()
			if !worked {
				d.emb = nil
				return Failed, err
			}
		}

		if _, ok, err := st.TakeDrainLease(d.cfg.Holder, d.cfg.Now(), LeaseTTL); err != nil {
			return Failed, err
		} else if !ok {
			return Deferred, nil
		}
		if d.cfg.Progress != nil {
			c, err := st.EmbedCounts(d.fp)
			if err != nil {
				return Failed, err
			}
			d.cfg.Progress(Progress{Counts: c, Skipped: len(d.skipped)})
		}
	}
}

// storeError is a failure to store a vector, as opposed to the runtime's.
type storeError struct{ err error }

func (e *storeError) Error() string { return e.err.Error() }
func (e *storeError) Unwrap() error { return e.err }

// embed runs one batch and stores its vectors. A Record Tombstoned while
// the batch ran is refused by the upsert and left out.
func (d *Drain) embed(ctx context.Context, batch []store.QueuedRecord) error {
	docs := make([]string, len(batch))
	for i, r := range batch {
		docs[i] = r.Document
	}
	vecs, err := d.emb.Embed(ctx, docs)
	if err != nil {
		return err
	}
	if len(vecs) != len(batch) {
		return fmt.Errorf("embed: %d vectors for %d documents", len(vecs), len(batch))
	}
	for i, r := range batch {
		if _, err := d.cfg.Store.UpsertRecordEmbedding(r.RecordID, d.fp, vecs[i]); err != nil {
			return &storeError{err}
		}
	}
	return nil
}

func (d *Drain) skippedIDs() []string {
	ids := make([]string, 0, len(d.skipped))
	for id := range d.skipped {
		ids = append(ids, id)
	}
	return ids
}

// failed records err as the last failure.
func (d *Drain) failed(err error) {
	d.report.LastFailure = err.Error()
	d.report.LastFailureAt = d.cfg.Now()
	d.save()
}

func (d *Drain) logf(format string, args ...any) {
	if d.cfg.Logger != nil {
		d.cfg.Logger.Printf(format, args...)
	}
}

// Run is the daemon's drain: a pass at start, after each Wake (once
// FlushInterval has passed, or at once with a full batch queued) and every
// PollInterval. A Failed pass backs off by embed.Backoff's default rule (5 s
// doubling up to 10 min, the query server's too); wakes and polls wait for
// the backoff unless the setup has changed. It returns when ctx is done.
func (d *Drain) Run(ctx context.Context) {
	var flush, retry <-chan time.Time
	poll := d.cfg.After(PollInterval)
	var (
		failedSetup embed.Setup
		backoff     embed.Backoff
	)
	run := func() {
		flush = nil
		out, err := d.Pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if out != Failed {
			backoff.Reset()
			retry = nil
			return
		}
		delay := backoff.Next()
		d.logf("embeddings: %v; retrying in %s", err, delay)
		failedSetup, _, _ = embed.LoadSetup(d.cfg.Store)
		retry = d.cfg.After(delay)
	}
	setupChanged := func() bool {
		s, _, err := embed.LoadSetup(d.cfg.Store)
		return err == nil && s != failedSetup
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			switch {
			case retry != nil:
				if setupChanged() {
					run()
				}
			case d.batchQueued():
				run()
			case flush == nil:
				flush = d.cfg.After(FlushInterval)
			}
		case <-flush:
			run()
		case <-poll:
			poll = d.cfg.After(PollInterval)
			if retry == nil || setupChanged() {
				run()
			}
		case <-retry:
			run()
		}
	}
}

// batchQueued reports whether a full batch is waiting.
func (d *Drain) batchQueued() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.emb == nil {
		return false
	}
	q, err := d.cfg.Store.EmbedQueue(d.fp, BatchSize, d.skippedIDs())
	return err == nil && len(q) >= BatchSize
}

// ReportKey is the Device-local settings row where the drain holding the
// lease publishes its Report for `backstory status`.
const ReportKey = "embed.drain_report"

// Report is what the drain last published: no Record ids, only counts and
// the runtime's error text.
type Report struct {
	// Fingerprint is the fingerprint the drain embeds under.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Skipped counts the Records it has given up on.
	Skipped int `json:"skipped,omitempty"`
	// LastFailure and LastFailureAt are its most recent failure.
	LastFailure   string    `json:"last_failure,omitempty"`
	LastFailureAt time.Time `json:"last_failure_at,omitzero"`
	// SetupError is why the runtime didn't open, if it didn't: a missing
	// file, or a device that doesn't resolve.
	SetupError string `json:"setup_error,omitempty"`
}

// ReadReport reads the published Report, ok false when none is.
func ReadReport(s embed.Settings) (Report, bool, error) {
	v, ok, err := s.GetSetting(ReportKey)
	if err != nil || !ok || v == "" {
		return Report{}, false, err
	}
	var r Report
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return Report{}, false, fmt.Errorf("drain: report: %w", err)
	}
	return r, true, nil
}

// ReadReportCounts reads the published Report and counts the Records
// embedded and queued under its fingerprint. counted is false, with zero
// counts, while no drainer has published a fingerprint; the Report (its
// setup error, say) still comes back.
func ReadReportCounts(st *store.Store) (r Report, c store.EmbedCounts, counted bool, err error) {
	r, ok, err := ReadReport(st)
	if err != nil || !ok || r.Fingerprint == "" {
		return r, store.EmbedCounts{}, false, err
	}
	if c, err = st.EmbedCounts(r.Fingerprint); err != nil {
		return r, store.EmbedCounts{}, false, err
	}
	return r, c, true, nil
}

// save publishes the report when it has changed. A failure to write it is
// only logged: status is best-effort.
func (d *Drain) save() {
	if d.report == d.saved {
		return
	}
	b, err := json.Marshal(d.report)
	if err == nil {
		err = d.cfg.Store.SetSetting(ReportKey, string(b))
	}
	if err != nil {
		d.logf("embeddings: publish status: %v", err)
		return
	}
	d.saved = d.report
}
