// Package graph walks the Ledger: from seed Records out along declared
// edges, both directions, and returns the Records it reached, each with
// its status and how it was reached, plus the edges among them. It is the
// one walker recall and `graph --json` share (#32's resolution, #29's
// contract), so hops, de-duplication and status live here, not in either
// caller. It only reads the store.
package graph

import (
	"context"
	"fmt"
	"sort"

	"github.com/RidgetopAi/backstory/internal/handoff"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// DefaultHops is how far a record anchor's walk follows declared edges
// (#32: "2 hops, both directions, all six types").
const DefaultHops = 2

// Source is what Walk reads.
type Source struct {
	Store *store.Store
	// Searcher finds Neighbours, by its Record query.
	Searcher search.Searcher
	// Workspaces is the caller's already-resolved Workspace list, passed
	// through to Handoff status.
	Workspaces []string
}

// Limits bound a walk. The zero value returns the seeds alone.
type Limits struct {
	// Hops is how many declared edges, in either direction, a node may be
	// from its nearest seed.
	Hops int
	// Neighbours is how many Neighbours each seed may add; zero adds none.
	Neighbours int
	// Scope is the caller's scope, the only place Neighbours come from.
	Scope Scope
}

// Status is a node's status: one of the four `graph --json` names.
type Status string

const (
	StatusCurrent    Status = "current"
	StatusSuperseded Status = "superseded"
	StatusDraft      Status = "draft"
	StatusTombstoned Status = "tombstoned"
)

// Via says what kind of link reached a node.
type Via string

const (
	ViaEdge Via = "edge"
	// ViaNeighbour is a Neighbour link: close in meaning to a seed.
	ViaNeighbour Via = "neighbour"
)

// Direction is a reaching edge's direction relative to Reached.From.
type Direction string

const (
	// DirectionOut is From -> this node.
	DirectionOut Direction = "out"
	// DirectionIn is this node -> From.
	DirectionIn Direction = "in"
)

// Reached is how a node was first reached. An edge sets Type, Direction
// and Hops; a Neighbour sets Similarity.
type Reached struct {
	Via        Via
	Type       store.EdgeType
	Direction  Direction
	From       string
	Hops       int
	Similarity float64
}

// Node is one Record in a Graph.
type Node struct {
	// Record is the Record as stored; Text and Next are "" when Status is
	// StatusTombstoned.
	Record store.Record
	Status Status
	// Reached is nil for a seed.
	Reached *Reached
	// Handoff is handoff's status for a Handoff that isn't Tombstoned; nil
	// for any other node.
	Handoff *handoff.Status
	// ConfirmedBy is every live Done, Affirm or Promote confirming the
	// Record, oldest first; never nil.
	ConfirmedBy []store.Confirmation
}

// Graph is a walk's result.
type Graph struct {
	// Nodes are newest first by sequence (SCHEMA.md invariant 10).
	Nodes []Node
	// Edges are the declared edges with both ends among Nodes.
	Edges []store.Edge
	// NeighbourEdges join each seed to its Neighbours; never nil. They are
	// computed for this walk and never stored (ADR-0003).
	NeighbourEdges []NeighbourEdge
	// Reason is why Neighbours are missing or partial, from the Searcher
	// ("embeddings off", ...); "" when it gave none.
	Reason string
}

// Walk returns the seeds plus every Record within limits.Hops declared
// edges of one, either direction, any of the six types and across scope.
// A Record reachable more than one way is reached by its fewest hops.
// A seed id that names no Record is skipped. An expired Draft (store.ExpiredDraft) reached by
// an edge is left out, and the walk doesn't go through it.
//
// A confirm Record written by Done, Affirm or Promote is never a node, even
// as a seed, and the walk doesn't go through it: its target carries it in
// ConfirmedBy instead (#29's amendment). A Contradiction or Supersede
// confirm stays a node, with its real edge.
//
// With limits.Neighbours set, each seed then adds its Neighbours (see
// neighbours), which are never walked further.
func Walk(ctx context.Context, src Source, seeds []string, limits Limits) (Graph, error) {
	w := walker{st: src.Store, workspaces: src.Workspaces, edges: map[string][]store.Edge{}, reached: map[string]*Reached{}, folded: map[string]bool{}}

	frontier, err := w.admit(seeds, nil)
	if err != nil {
		return Graph{}, err
	}
	admittedSeeds := frontier
	for hop := 1; hop <= limits.Hops && len(frontier) > 0; hop++ {
		if err := ctx.Err(); err != nil {
			return Graph{}, fmt.Errorf("graph: walk: %w", err)
		}
		var next []string
		found := map[string]*Reached{}
		for _, from := range frontier {
			edges, err := w.edgesOf(from)
			if err != nil {
				return Graph{}, err
			}
			for _, e := range edges {
				other, dir := e.ToID, DirectionOut
				if e.ToID == from {
					other, dir = e.FromID, DirectionIn
				}
				if _, seen := w.reached[other]; seen || w.folded[other] {
					continue
				}
				if _, seen := found[other]; seen {
					continue
				}
				found[other] = &Reached{Via: ViaEdge, Type: e.Type, Direction: dir, From: from, Hops: hop}
				next = append(next, other)
			}
		}
		if frontier, err = w.admit(next, found); err != nil {
			return Graph{}, err
		}
	}
	if err := w.neighbours(ctx, src.Searcher, admittedSeeds, limits); err != nil {
		return Graph{}, err
	}
	return w.graph()
}

type walker struct {
	st         *store.Store
	workspaces []string
	edges      map[string][]store.Edge
	// reached holds every admitted node, nil for a seed; order is the
	// admission order.
	reached map[string]*Reached
	order   []string
	// folded holds the confirm Records met so far that are folded into
	// their target rather than admitted.
	folded map[string]bool
	// neighbourEdges and reason are what neighbours found.
	neighbourEdges []NeighbourEdge
	reason         string
}

// admit adds ids that name a Record as nodes, with how each was reached,
// and returns the ones admitted.
func (w *walker) admit(ids []string, how map[string]*Reached) ([]string, error) {
	recs, err := w.st.RecordsByIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("graph: walk: %w", err)
	}
	var admitted []string
	for _, rec := range recs {
		if _, seen := w.reached[rec.ID]; seen {
			continue
		}
		fold, err := w.confirms(rec)
		if err != nil {
			return nil, err
		}
		if fold {
			w.folded[rec.ID] = true
			continue
		}
		if how[rec.ID] != nil && w.st.ExpiredDraft(rec) {
			continue
		}
		w.reached[rec.ID] = how[rec.ID]
		w.order = append(w.order, rec.ID)
		admitted = append(admitted, rec.ID)
	}
	sort.Strings(admitted)
	return admitted, nil
}

// confirms reports whether rec is a confirm Record that confirms its
// target, Done, Affirm or Promote: the confirm actions that mint an
// `informs` edge out of it (store.ConfirmEdgeType).
func (w *walker) confirms(rec store.Record) (bool, error) {
	if rec.Kind != store.KindConfirm {
		return false, nil
	}
	edges, err := w.edgesOf(rec.ID)
	if err != nil {
		return false, err
	}
	for _, e := range edges {
		if e.Type == store.EdgeInforms && e.FromID == rec.ID {
			return true, nil
		}
	}
	return false, nil
}

// edgesOf is id's edges, both directions, in a fixed order, read once.
func (w *walker) edgesOf(id string) ([]store.Edge, error) {
	if es, ok := w.edges[id]; ok {
		return es, nil
	}
	es, err := w.st.EdgesTouching(id)
	if err != nil {
		return nil, fmt.Errorf("graph: walk: %w", err)
	}
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.FromID != b.FromID {
			return a.FromID < b.FromID
		}
		return a.ToID < b.ToID
	})
	w.edges[id] = es
	return es, nil
}

// graph builds the result from the admitted nodes.
func (w *walker) graph() (Graph, error) {
	recs, err := w.st.RecordsByIDs(w.order)
	if err != nil {
		return Graph{}, fmt.Errorf("graph: walk: %w", err)
	}
	confirmations, err := w.st.ConfirmationsOf(w.order)
	if err != nil {
		return Graph{}, fmt.Errorf("graph: walk: %w", err)
	}
	g := Graph{Nodes: make([]Node, 0, len(recs)), Edges: []store.Edge{}, NeighbourEdges: w.neighbourEdges, Reason: w.reason}
	if g.NeighbourEdges == nil {
		g.NeighbourEdges = []NeighbourEdge{}
	}
	listed := map[store.Edge]bool{}
	for _, rec := range recs {
		edges, err := w.edgesOf(rec.ID)
		if err != nil {
			return Graph{}, err
		}
		for _, e := range edges {
			_, fromIn := w.reached[e.FromID]
			_, toIn := w.reached[e.ToID]
			if fromIn && toIn && !listed[e] {
				listed[e] = true
				g.Edges = append(g.Edges, e)
			}
		}
		n := Node{Record: rec, Status: status(rec, edges), Reached: w.reached[rec.ID], ConfirmedBy: confirmations[rec.ID]}
		if n.ConfirmedBy == nil {
			n.ConfirmedBy = []store.Confirmation{}
		}
		if n.Status == StatusTombstoned {
			n.Record.Text, n.Record.Next = "", ""
		} else if rec.Kind == store.KindHandoff {
			hs, err := handoff.Of(w.st, rec, w.workspaces)
			if err != nil {
				return Graph{}, fmt.Errorf("graph: walk: %w", err)
			}
			n.Handoff = &hs
		}
		g.Nodes = append(g.Nodes, n)
	}
	return g, nil
}

// status is rec's status, from its tombstone column, the supersedes edges
// into it and its Tier, in that precedence. A promoted Draft is still a
// Draft (#29's amendment).
func status(rec store.Record, edges []store.Edge) Status {
	if rec.TombstonedAt != nil {
		return StatusTombstoned
	}
	for _, e := range edges {
		if e.Type == store.EdgeSupersedes && e.ToID == rec.ID {
			return StatusSuperseded
		}
	}
	if rec.Tier == store.TierInferred {
		return StatusDraft
	}
	return StatusCurrent
}
