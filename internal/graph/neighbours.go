package graph

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// NeighboursPerSeed is how many Neighbours a seed adds (#32: "top 5 per
// seed").
const NeighboursPerSeed = 5

// MaxNeighbourSeeds is the most seeds the walker makes per-seed Record
// queries for (#32: "up to 20 seeds"). A walk from more adds no
// Neighbours: the pairwise pass over the nodes' stored vectors that #32
// gives larger seed sets is #46's.
const MaxNeighbourSeeds = 20

// Scope is a caller's scope: a Work location's scope when Location is set,
// else the Project ProjectKey names.
type Scope struct {
	ProjectKey string
	Location   *store.LocationScope
}

// has reports whether rec is inside the scope.
func (s Scope) has(rec store.Record) bool {
	if s.Location != nil {
		return s.Location.HasRecord(rec)
	}
	return rec.ProjectKey == s.ProjectKey
}

// NeighbourEdge joins a seed to a Record close to it in meaning.
type NeighbourEdge struct {
	From       string
	To         string
	Similarity float64
}

// neighbours adds up to limits.Neighbours Neighbours for each seed from
// the Searcher's Record query, which uses the seed's stored vector. The
// walker checks each one itself, whatever the Searcher returned: at least
// search.CosineFloor close, inside limits.Scope, and live. A Neighbour is
// a new node reached from its closest seed, or joins a node already reached
// another way by its neighbour edge alone, and it is never walked further.
//
// The Searcher's reasons (no Embedder, the runtime down, a seed not yet
// embedded) go into w.reason, each once. Embeddings off or unavailable
// ends the queries, since no other seed would fare better.
func (w *walker) neighbours(ctx context.Context, searcher search.Searcher, seeds []string, limits Limits) error {
	if limits.Neighbours <= 0 || searcher == nil || len(seeds) > MaxNeighbourSeeds {
		return nil
	}
	found := map[string]*Reached{}
	joined := map[[2]string]bool{}
	var next, reasons []string
	for _, seed := range seeds {
		res, err := searcher.Search(ctx, search.Query{
			RecordID: seed, ProjectKey: limits.Scope.ProjectKey, Scope: limits.Scope.Location,
			CurrentOnly: true, Limit: limits.Neighbours,
		})
		if err != nil {
			return fmt.Errorf("graph: neighbours of %s: %w", seed, err)
		}
		if res.Reason != "" && !slices.Contains(reasons, res.Reason) {
			reasons = append(reasons, res.Reason)
		}
		if res.Reason == search.ReasonEmbeddingsOff || strings.HasPrefix(res.Reason, search.ReasonUnavailablePrefix) {
			break
		}
		hits, err := w.eligible(seed, res.Hits, limits)
		if err != nil {
			return err
		}
		for _, h := range hits {
			// Two seeds close to each other are joined once.
			if !joined[[2]string{h.RecordID, seed}] {
				joined[[2]string{seed, h.RecordID}] = true
				w.neighbourEdges = append(w.neighbourEdges, NeighbourEdge{From: seed, To: h.RecordID, Similarity: h.Similarity})
			}
			if _, seen := w.reached[h.RecordID]; seen {
				continue
			}
			if r, ok := found[h.RecordID]; ok && r.Similarity >= h.Similarity {
				continue
			} else if !ok {
				next = append(next, h.RecordID)
			}
			found[h.RecordID] = &Reached{Via: ViaNeighbour, From: seed, Similarity: h.Similarity}
		}
	}
	w.reason = strings.Join(reasons, "; ")
	_, err := w.admit(next, found)
	return err
}

// eligible is the closest limits.Neighbours of hits that can be seed's
// Neighbours, closest first.
func (w *walker) eligible(seed string, hits []search.Hit, limits Limits) ([]search.Hit, error) {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.RecordID
	}
	recs, err := w.st.RecordsByIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("graph: neighbours of %s: %w", seed, err)
	}
	byID := make(map[string]store.Record, len(recs))
	for _, rec := range recs {
		byID[rec.ID] = rec
	}

	hits = append([]search.Hit(nil), hits...)
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Similarity > hits[j].Similarity })
	var out []search.Hit
	for _, h := range hits {
		if len(out) == limits.Neighbours {
			break
		}
		rec, ok := byID[h.RecordID]
		if !ok || rec.ID == seed || h.Similarity < search.CosineFloor || !limits.Scope.has(rec) {
			continue
		}
		live, err := w.live(rec)
		if err != nil {
			return nil, err
		}
		if live {
			out = append(out, h)
		}
	}
	return out, nil
}

// now is the clock a Draft's expiry is judged against.
var now = time.Now

// live reports whether rec may be a Neighbour (#32: "Current Records and
// live Drafts only"): not Tombstoned, superseded or past its own expiry,
// and not a confirm Record folded into its target.
func (w *walker) live(rec store.Record) (bool, error) {
	edges, err := w.edgesOf(rec.ID)
	if err != nil {
		return false, err
	}
	if s := status(rec, edges); s != StatusCurrent && s != StatusDraft {
		return false, nil
	}
	if rec.ExpiresAt != nil && !rec.ExpiresAt.After(now()) {
		return false, nil
	}
	fold, err := w.confirms(rec)
	return !fold, err
}
