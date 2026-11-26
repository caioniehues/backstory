package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"

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

// Has reports whether rec is inside the scope, every Project key read
// under the key st files it as: the location scope's own rule
// (store.LocationScope.HasRecord) when Location is set, else rec's Project
// is ProjectKey's. It is the one scope rule, for Neighbours and for
// recall's other-Project label alike.
func (s Scope) Has(st *store.Store, rec store.Record) bool {
	rec.ProjectKey = st.CanonicalProjectKey(rec.ProjectKey)
	if s.Location != nil {
		return s.Location.HasRecord(rec)
	}
	return rec.ProjectKey == st.CanonicalProjectKey(s.ProjectKey)
}

// NeighbourEdge joins a seed to a Record close to it in meaning.
type NeighbourEdge struct {
	From       string
	To         string
	Similarity float64
}

// neighbours adds up to limits.Neighbours Neighbours for each seed from
// the Searcher's Record query, which uses the seed's stored vector and
// drops every hit below the Searcher's own cosine floor. The walker checks
// the rest itself, whatever the Searcher returned: inside limits.Scope, and
// a Current Record or an unexpired Draft (canBeNeighbour). A Neighbour is a
// new node reached from its closest seed, or joins a node already reached
// another way by its neighbour edge alone, and it is never walked further.
//
// The Searcher's reasons (no Embedder, the runtime down, a seed not yet
// embedded) go into w.reason, each once. Embeddings off or unavailable
// ends the queries, since no other seed would fare better.
func (w *walker) neighbours(ctx context.Context, searcher search.Searcher, seeds []string, limits Limits) error {
	if limits.Neighbours <= 0 || searcher == nil || len(seeds) > MaxNeighbourSeeds {
		return nil
	}
	candidates := map[string]*Reached{}
	joined := map[[2]string]bool{}
	var next []string
	for _, seed := range seeds {
		res, err := searcher.Search(ctx, search.Query{
			RecordID: seed, ProjectKey: limits.Scope.ProjectKey, Scope: limits.Scope.Location,
			CurrentOnly: true, Limit: limits.Neighbours,
		})
		if err != nil {
			return fmt.Errorf("graph: neighbours of %s: %w", seed, err)
		}
		w.reason = search.JoinReasons(w.reason, res.Reason)
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
			if r, ok := candidates[h.RecordID]; ok && r.Similarity >= h.Similarity {
				continue
			} else if !ok {
				next = append(next, h.RecordID)
			}
			candidates[h.RecordID] = &Reached{Via: ViaNeighbour, From: seed, Similarity: h.Similarity}
		}
	}
	_, err := w.admit(next, candidates)
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
		if !ok || rec.ID == seed || !limits.Scope.Has(w.st, rec) {
			continue
		}
		ok, err := w.canBeNeighbour(rec)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// canBeNeighbour reports whether rec may be a Neighbour (#32: Current
// Records and unexpired Drafts only): not Tombstoned, superseded or an
// expired Draft (store.ExpiredDraft), and not a confirm Record folded into
// its target.
func (w *walker) canBeNeighbour(rec store.Record) (bool, error) {
	edges, err := w.edgesOf(rec.ID)
	if err != nil {
		return false, err
	}
	if s := status(rec, edges); s != StatusCurrent && s != StatusDraft {
		return false, nil
	}
	if w.st.ExpiredDraft(rec) {
		return false, nil
	}
	fold, err := w.isFoldedConfirm(rec)
	return !fold, err
}
