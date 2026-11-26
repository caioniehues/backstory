// Package search is the Search seam: one way to find Records by text or by
// another Record, filtered before ranking. Recall's text anchor and the
// note tool's related-Decisions check both go through it. Hits carry ids
// only; callers fetch the Records they need.
//
// A text query is always hybrid: the full-text side and the vector side each
// rank candidates and Reciprocal Rank Fusion merges them. The vector side
// embeds the query text through an injected Embedder; without one, or when
// it fails or misses the deadline, the query is FTS-only and Result.Reason
// says why. A Record query ("Records near this one") is vector-only and uses
// the Record's stored vector.
package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/RidgetopAi/backstory/internal/store"
)

// Tunables. Named here, never inlined.
const (
	// RRFK is Reciprocal Rank Fusion's k: a Hit scores 1/(RRFK + rank) for
	// each side that ranked it (rank counted from 1).
	RRFK = 60
	// FusionDepth is how many candidates each side ranks for fusion, as a
	// multiple of the Query's Limit, so a Record both sides rank just past
	// the Limit can still make the fused cut.
	FusionDepth = 3
	// QueryEmbedDeadline is how long a query waits for the Embedder before
	// falling back to FTS-only (#19 measured a warm query at 6.2 ms p95).
	QueryEmbedDeadline = 100 * time.Millisecond
	// CosineFloor is the default cosine similarity below which a vector hit
	// is dropped, so unrelated Records are never "near". Provisional: the
	// GPU proof (#44) sets it from the real model; tests inject their own
	// with WithCosineFloor.
	CosineFloor = 0.5
)

// Result.Reason values. A text query's reason means the vector side did not
// contribute, or contributed partially; its hits are still the text side's.
const (
	// ReasonEmbeddingsOff: no Embedder is configured.
	ReasonEmbeddingsOff = "embeddings off"
	// ReasonUnavailablePrefix starts the reason when the Embedder failed or
	// missed QueryEmbedDeadline; the cause follows.
	ReasonUnavailablePrefix = "embeddings unavailable: "
	// ReasonPartialPrefix starts the reason when some Records under the
	// Query's filters have no vector for the current fingerprint yet; the
	// count follows.
	ReasonPartialPrefix = "embeddings partial: "
	// ReasonNoRecordVector: a Record query's Record has no vector for the
	// current fingerprint, so its Neighbours can't be looked up.
	ReasonNoRecordVector = "no vector for this Record yet"
)

// Via says which side of the search found a Hit.
type Via string

const (
	ViaText   Via = "text"
	ViaVector Via = "vector"
	ViaBoth   Via = "both"
)

// Query is either Text or a RecordID ("Records near this one"), plus the
// filters applied before ranking and a limit. Tombstoned Records are always
// excluded.
type Query struct {
	Text     string
	RecordID string

	// ProjectKey is the Project searched when Scope is nil.
	ProjectKey string
	// Scope, when set, searches a Work location's scope instead.
	Scope *store.LocationScope
	// Kinds keeps only Records of these kinds; empty keeps every kind.
	Kinds []store.RecordKind
	// CurrentOnly also excludes superseded Records.
	CurrentOnly bool
	// Limit caps the hits; zero or less returns none.
	Limit int
	// KeyTermsOnly searches by Text's distinctive terms whatever its length
	// (store.SearchFilter.KeyTermsOnly). It shapes the text side only; the
	// vector side embeds the whole Text.
	KeyTermsOnly bool
}

// Hit is one Record a Query found.
type Hit struct {
	RecordID string
	Via      Via
	// Similarity is the cosine similarity when the vector side found the
	// Record, 0 otherwise.
	Similarity float64
}

// Result is a Query's hits, best first, and why the vector side did not
// contribute fully ("" when it did).
type Result struct {
	Hits   []Hit
	Reason string
}

// Searcher finds Records.
type Searcher interface {
	Search(ctx context.Context, q Query) (Result, error)
}

// Embedder is the vector side's view of the embedding runtime. Both calls
// run under QueryEmbedDeadline; the Searcher abandons a call that outlives
// it, so an implementation should still honour ctx to free its resources.
type Embedder interface {
	// Fingerprint is the current embedding fingerprint: which model, prompt
	// template, backend and dimension the stored vectors must have been
	// made under to be compared. Record queries use it.
	Fingerprint(ctx context.Context) (string, error)
	// EmbedQuery embeds query text and returns the vector with the
	// fingerprint it was made under.
	EmbedQuery(ctx context.Context, text string) (fingerprint string, vec []float32, err error)
}

// Option configures New.
type Option func(*searcher)

// WithEmbedder gives the Searcher a vector side. Without it embeddings are
// off and every query is FTS-only.
func WithEmbedder(e Embedder) Option { return func(s *searcher) { s.emb = e } }

// WithCosineFloor replaces CosineFloor.
func WithCosineFloor(floor float64) Option { return func(s *searcher) { s.floor = floor } }

// WithDeadline replaces QueryEmbedDeadline.
func WithDeadline(d time.Duration) Option { return func(s *searcher) { s.deadline = d } }

// New returns a Searcher over st.
func New(st *store.Store, opts ...Option) Searcher {
	s := &searcher{st: st, floor: CosineFloor, deadline: QueryEmbedDeadline}
	for _, o := range opts {
		o(s)
	}
	return s
}

type searcher struct {
	st       *store.Store
	emb      Embedder
	floor    float64
	deadline time.Duration
}

var errTextAndRecord = errors.New("search: a query is text or a Record id, not both")

func (s *searcher) Search(ctx context.Context, q Query) (Result, error) {
	if q.Text != "" && q.RecordID != "" {
		return Result{}, errTextAndRecord
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if q.RecordID != "" {
		return s.searchRecord(ctx, q)
	}
	return s.searchText(ctx, q)
}

func (q Query) filter(limit int) store.SearchFilter {
	return store.SearchFilter{
		ProjectKey: q.ProjectKey, Scope: q.Scope, Kinds: q.Kinds, CurrentOnly: q.CurrentOnly, Limit: limit,
		KeyTermsOnly: q.KeyTermsOnly,
	}
}

// searchText runs both sides and fuses them. Only the text side can fail
// the query; anything wrong on the vector side becomes the Reason.
func (s *searcher) searchText(ctx context.Context, q Query) (Result, error) {
	depth := q.Limit * FusionDepth
	var vector <-chan vectorSide
	if s.emb != nil && q.Limit > 0 && strings.TrimSpace(q.Text) != "" {
		vector = s.startVectorSide(ctx, q, depth)
	}
	textIDs, err := s.st.SearchText(q.Text, q.filter(depth))
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	var vs vectorSide
	switch {
	case s.emb == nil:
		vs.reason = ReasonEmbeddingsOff
	case vector != nil:
		vs = <-vector
	}
	return Result{Hits: fuse(textIDs, vs.hits, q.Limit), Reason: vs.reason}, nil
}

type vectorSide struct {
	hits   []store.VectorHit
	reason string
}

// startVectorSide embeds q.Text and scans for its nearest vectors in the
// background, so the embedding overlaps the text side.
func (s *searcher) startVectorSide(ctx context.Context, q Query, depth int) <-chan vectorSide {
	out := make(chan vectorSide, 1)
	go func() {
		var fp string
		var vec []float32
		err := s.withDeadline(ctx, func(ctx context.Context) error {
			var err error
			fp, vec, err = s.emb.EmbedQuery(ctx, q.Text)
			return err
		})
		if err == nil && (fp == "" || len(vec) == 0) {
			err = errors.New("the embedder returned no vector")
		}
		if err != nil {
			out <- vectorSide{reason: s.unavailable(err)}
			return
		}
		hits, reason, err := s.nearest(fp, vec, q.filter(depth), "")
		if err != nil {
			reason = ReasonUnavailablePrefix + err.Error()
		}
		out <- vectorSide{hits: hits, reason: reason}
	}()
	return out
}

// searchRecord returns q.RecordID's Neighbours from its stored vector.
func (s *searcher) searchRecord(ctx context.Context, q Query) (Result, error) {
	if s.emb == nil {
		return Result{Reason: ReasonEmbeddingsOff}, nil
	}
	if q.Limit <= 0 {
		return Result{}, nil
	}
	var fp string
	err := s.withDeadline(ctx, func(ctx context.Context) error {
		var err error
		fp, err = s.emb.Fingerprint(ctx)
		return err
	})
	if err == nil && fp == "" {
		err = errors.New("the embedder has no fingerprint")
	}
	if err != nil {
		return Result{Reason: s.unavailable(err)}, nil
	}
	vec, ok, err := s.st.RecordVector(q.RecordID, fp)
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	if !ok {
		return Result{Reason: ReasonNoRecordVector}, nil
	}
	// One extra, since the Record itself is its own nearest vector.
	hits, reason, err := s.nearest(fp, vec, q.filter(q.Limit+1), q.RecordID)
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	res := Result{Reason: reason}
	for _, h := range hits {
		if len(res.Hits) == q.Limit {
			break
		}
		res.Hits = append(res.Hits, Hit{RecordID: h.RecordID, Via: ViaVector, Similarity: h.Similarity})
	}
	return res, nil
}

// nearest is the vector adapter: the store's scan, minus exclude and every
// hit below the cosine floor, and the partial reason when the scan could
// not see every Record.
func (s *searcher) nearest(fp string, vec []float32, f store.SearchFilter, exclude string) ([]store.VectorHit, string, error) {
	scan, err := s.st.NearestVectors(fp, vec, f)
	if err != nil {
		return nil, "", err
	}
	var hits []store.VectorHit
	for _, h := range scan.Hits {
		if h.RecordID != exclude && h.Similarity >= s.floor {
			hits = append(hits, h)
		}
	}
	return hits, partialReason(scan.Unembedded), nil
}

func partialReason(unembedded int) string {
	switch unembedded {
	case 0:
		return ""
	case 1:
		return ReasonPartialPrefix + "1 Record not embedded yet"
	default:
		return fmt.Sprintf("%s%d Records not embedded yet", ReasonPartialPrefix, unembedded)
	}
}

// errDeadline is what withDeadline returns when call outlived s.deadline.
var errDeadline = errors.New("deadline")

// withDeadline runs call under s.deadline and returns as soon as it passes,
// even if call ignores its context: a slow Embedder can't hold up a query.
func (s *searcher) withDeadline(ctx context.Context, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	select {
	case err := <-done:
		if errors.Is(err, context.DeadlineExceeded) {
			return errDeadline
		}
		return err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errDeadline
		}
		return ctx.Err()
	}
}

// unavailable is the Reason for an Embedder failure.
func (s *searcher) unavailable(err error) string {
	if errors.Is(err, errDeadline) {
		return fmt.Sprintf("%squery embedding missed the %s deadline", ReasonUnavailablePrefix, s.deadline)
	}
	return ReasonUnavailablePrefix + err.Error()
}

// fuse merges the text side's ranking (ids, best first) and the vector
// side's by Reciprocal Rank Fusion, keeping the best limit. Ties go to the
// text side's better rank, so an exact-token match beats a vector hit of
// equal standing, then to the vector side's.
func fuse(textIDs []string, vecHits []store.VectorHit, limit int) []Hit {
	type fused struct {
		hit      Hit
		score    float64
		textRank int
		vecRank  int
	}
	const unranked = int(^uint(0) >> 1)
	byID := map[string]*fused{}
	var all []*fused
	get := func(id string) *fused {
		if f, ok := byID[id]; ok {
			return f
		}
		f := &fused{hit: Hit{RecordID: id}, textRank: unranked, vecRank: unranked}
		byID[id] = f
		all = append(all, f)
		return f
	}
	for i, id := range textIDs {
		f := get(id)
		f.textRank = i + 1
		f.score += 1.0 / float64(RRFK+i+1)
		f.hit.Via = ViaText
	}
	for i, h := range vecHits {
		f := get(h.RecordID)
		f.vecRank = i + 1
		f.score += 1.0 / float64(RRFK+i+1)
		f.hit.Similarity = h.Similarity
		if f.hit.Via == ViaText {
			f.hit.Via = ViaBoth
		} else {
			f.hit.Via = ViaVector
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.textRank != b.textRank {
			return a.textRank < b.textRank
		}
		return a.vecRank < b.vecRank
	})
	var hits []Hit
	for _, f := range all {
		if len(hits) == limit {
			break
		}
		hits = append(hits, f.hit)
	}
	return hits
}
