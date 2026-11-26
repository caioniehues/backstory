package recall

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/RidgetopAi/backstory/internal/graph"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// withNeighbours is limits plus graph.NeighboursPerSeed Neighbours per seed
// from scope, the anchor's own: the walk widens by meaning only inside
// what the anchor reads (#32), though declared edges cross scope.
func withNeighbours(limits graph.Limits, scope graph.Scope) graph.Limits {
	limits.Neighbours = graph.NeighboursPerSeed
	limits.Scope = scope
	return limits
}

// anchorScope is what a text anchor reads: its location scope when it has
// one, else its Project under the key the store files it as.
func anchorScope(st *store.Store, anchor Anchor) graph.Scope {
	return graph.Scope{ProjectKey: st.CanonicalProjectKey(anchor.ProjectKey), Location: anchor.Scope}
}

// textNeighbours widens a text anchor's hits by meaning: the Neighbours of
// seeds, its top hits by search rank, inside its scope (graph.Walk). Every
// hit stays a direct hit, even one close to another. The result is in
// sequence order, and n's reason gains the walk's, each reason once.
func textNeighbours(st *store.Store, searcher search.Searcher, anchor Anchor, hits []found, seeds []string, n notices, workspaces []string) ([]found, notices, error) {
	g, err := graph.Walk(context.Background(), graph.Source{Store: st, Searcher: searcher, Workspaces: workspaces},
		seeds, withNeighbours(graph.Limits{}, anchorScope(st, anchor)))
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
	}
	n.reason = joinReasons(n.reason, g.Reason)

	ids := make([]string, 0, len(hits))
	seen := map[string]bool{}
	for _, h := range hits {
		ids = append(ids, h.rec.ID)
		seen[h.rec.ID] = true
	}
	widened := map[string]graph.Node{}
	for _, node := range g.Nodes {
		if seen[node.Record.ID] || node.Reached == nil || node.Status == graph.StatusTombstoned {
			continue
		}
		widened[node.Record.ID] = node
		ids = append(ids, node.Record.ID)
	}
	if len(widened) == 0 {
		return hits, n, nil
	}

	recs, err := st.RecordsByIDs(ids)
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
	}
	out := make([]found, len(recs))
	for i, rec := range recs {
		out[i] = found{rec: rec}
		if node, ok := widened[rec.ID]; ok {
			out[i].reached, out[i].handoff = node.Reached, node.Handoff
		}
	}
	return out, n, nil
}

// joinReasons is reasons ("; "-joined) plus each of more's not already
// there.
func joinReasons(reasons, more string) string {
	var all []string
	for _, r := range strings.Split(reasons+"; "+more, "; ") {
		if r != "" && !slices.Contains(all, r) {
			all = append(all, r)
		}
	}
	return strings.Join(all, "; ")
}

// neighbourLabel is how a Neighbour was reached, e.g. "Neighbour of
// `a1b2c3d4` (0.82)": the seed it is close to and the cosine similarity.
func neighbourLabel(r *graph.Reached) string {
	return fmt.Sprintf("Neighbour of `%s` (%.2f)", shortID(r.From), r.Similarity)
}
