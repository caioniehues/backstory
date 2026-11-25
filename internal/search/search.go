// Package search is the Search seam: one way to find Records by text or by
// another Record, filtered before ranking. Recall's text anchor and the
// note tool's related-Decisions check both go through it. Hits carry ids
// only; callers fetch the Records they need.
//
// Today only the full-text side exists, so text queries are FTS-only and
// Record queries find nothing; each says why in Result.Reason.
package search

import (
	"context"
	"errors"
	"fmt"

	"github.com/RidgetopAi/backstory/internal/store"
)

// ReasonEmbeddingsOff is Result.Reason when no vector side is configured.
const ReasonEmbeddingsOff = "embeddings off"

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
	// (store.SearchFilter.KeyTermsOnly).
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
// contribute ("" when it did).
type Result struct {
	Hits   []Hit
	Reason string
}

// Searcher finds Records.
type Searcher interface {
	Search(ctx context.Context, q Query) (Result, error)
}

// New returns a Searcher over st. It has the full-text side only.
func New(st *store.Store) Searcher {
	return &searcher{st: st}
}

type searcher struct {
	st *store.Store
}

var errTextAndRecord = errors.New("search: a query is text or a Record id, not both")

func (s *searcher) Search(ctx context.Context, q Query) (Result, error) {
	if q.Text != "" && q.RecordID != "" {
		return Result{}, errTextAndRecord
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	res := Result{Reason: ReasonEmbeddingsOff}
	if q.RecordID != "" {
		return res, nil
	}
	ids, err := s.st.SearchText(q.Text, store.SearchFilter{
		ProjectKey: q.ProjectKey, Scope: q.Scope, Kinds: q.Kinds, CurrentOnly: q.CurrentOnly, Limit: q.Limit,
		KeyTermsOnly: q.KeyTermsOnly,
	})
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	for _, id := range ids {
		res.Hits = append(res.Hits, Hit{RecordID: id, Via: ViaText})
	}
	return res, nil
}
