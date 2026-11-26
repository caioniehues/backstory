package recall

import (
	"fmt"

	"github.com/RidgetopAi/backstory/internal/graph"
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
// one, else its Project. graph.Scope.Has is the rule for what is inside it.
func anchorScope(anchor Anchor) graph.Scope {
	return graph.Scope{ProjectKey: anchor.ProjectKey, Location: anchor.Scope}
}

// closerNeighbour is the budget's Neighbour tier, after direct hits and
// edge-reached items (outranks): it reports whether a reaches a Neighbour
// closer to its seed than b does. A Neighbour outranks anything that is
// neither.
func closerNeighbour(a, b *graph.Reached) bool {
	aNeighbour, bNeighbour := a.Via == graph.ViaNeighbour, b.Via == graph.ViaNeighbour
	if aNeighbour && bNeighbour {
		return a.Similarity > b.Similarity
	}
	return aNeighbour && !bNeighbour
}

// neighbourLabel is how a Neighbour was reached, e.g. "Neighbour of
// `a1b2c3d4` (0.82)": the seed it is close to and the cosine similarity.
func neighbourLabel(r *graph.Reached) string {
	return fmt.Sprintf("Neighbour of `%s` (%.2f)", shortID(r.From), r.Similarity)
}
