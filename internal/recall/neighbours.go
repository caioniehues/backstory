package recall

import (
	"fmt"
	"slices"
	"strings"

	"github.com/RidgetopAi/backstory/internal/graph"
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
