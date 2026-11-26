package embed

import (
	"context"
	"fmt"
	"sync"

	"github.com/RidgetopAi/backstory/internal/search"
)

// RecordEmbedder is a search.Embedder for Record text: it embeds the text
// as a document, the way the drain does (DocumentPrefix, through the batch
// runtime, on stdin), under the fingerprint the drain writes. The note
// tool's related-Decisions check uses it to search by a Decision that has
// no vector yet, so Record text never reaches the warm query server (#37
// user story 45). Each call is one llama-embedding run.
//
// It follows the setup: each call reads it, answers search.ErrEmbeddingsOff
// while embeddings are off, and resolves the device again when the setup
// has changed (or the last run failed). It is safe for concurrent use.
type RecordEmbedder struct {
	Settings Settings

	mu    sync.Mutex
	setup Setup
	batch *Batch
	fp    string
}

var _ search.Embedder = (*RecordEmbedder)(nil)

// Fingerprint is the fingerprint the batch runtime's vectors are made
// under for the current setup.
func (r *RecordEmbedder) Fingerprint(ctx context.Context) (string, error) {
	_, fp, err := r.open(ctx)
	return fp, err
}

// EmbedQuery embeds text as a document. It honours ctx: the runtime is
// killed when ctx ends.
func (r *RecordEmbedder) EmbedQuery(ctx context.Context, text string) (string, []float32, error) {
	b, fp, err := r.open(ctx)
	if err != nil {
		return "", nil, err
	}
	vecs, err := b.Embed(ctx, []string{text})
	if err != nil {
		r.forget(b)
		return "", nil, err
	}
	if len(vecs) != 1 {
		return "", nil, fmt.Errorf("embed: %d vectors for one document", len(vecs))
	}
	return fp, vecs[0], nil
}

// open is the Batch for the current setup and its fingerprint, made anew
// when the setup has changed.
func (r *RecordEmbedder) open(ctx context.Context) (*Batch, string, error) {
	setup, on, err := LoadSetup(r.Settings)
	if err != nil {
		return nil, "", err
	}
	if !on {
		return nil, "", search.ErrEmbeddingsOff
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.batch != nil && setup == r.setup {
		return r.batch, r.fp, nil
	}
	b, err := NewBatch(ctx, setup)
	if err != nil {
		return nil, "", err
	}
	fp, err := FingerprintFor(r.Settings, setup.Model, b.Device())
	if err != nil {
		return nil, "", err
	}
	r.setup, r.batch, r.fp = setup, b, fp
	return b, fp, nil
}

// forget drops b after a failed run, so the next call resolves the device
// again.
func (r *RecordEmbedder) forget(b *Batch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.batch == b {
		r.batch = nil
	}
}
