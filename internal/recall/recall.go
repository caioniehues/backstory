// Package recall answers "what's the backstory on X": an anchor (a
// project, a record, or free text) resolves to an ordered, trust-annotated
// set of ledger items, rendered at one of three altitudes under a token
// budget (PLAN.md §Phase 4, the recall_thread model). It is the engine the
// MCP recall tool, the CLI, the This Week view and export are meant to
// share — none of which this package wires up; it reads the store and
// returns data, nothing else.
package recall

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RidgetopAi/backstory/internal/block"
	"github.com/RidgetopAi/backstory/internal/graph"
	"github.com/RidgetopAi/backstory/internal/handoff"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// maxProjectRecords bounds a project anchor's own store query, independent
// of the token budget that trims the rendered result afterward — the same
// pattern internal/mcp/recall.go's v0 stub used for decisions
// (maxRecallDecisions): a project with thousands of records must not load
// them all before the budget ever gets a chance to cut the list down.
const maxProjectRecords = 500

// maxTextResults bounds a free-text anchor's own search the same way.
const maxTextResults = 200

// textAnchorSeeds is how many of a text anchor's hits, best first by search
// rank, its Ledger walk starts from (#32: "Text anchors widen from their
// top 10 search hits").
const textAnchorSeeds = 10

// AnchorKind is the kind of anchor Build resolves to a starting record set.
type AnchorKind string

const (
	AnchorProject AnchorKind = "project"
	AnchorRecord  AnchorKind = "record"
	AnchorText    AnchorKind = "text"
)

// Anchor is recall's starting point. Only the fields the Kind uses are
// read; ProjectAnchor, RecordAnchor and TextAnchor build a well-formed
// value each rather than callers setting fields by hand.
type Anchor struct {
	Kind       AnchorKind
	ProjectKey string
	RecordID   string
	Text       string
	// Scope, when set on a project or text anchor, widens "the project's
	// records" to everything belonging to a location (store.LocationScope):
	// the repo-key records plus the workspace-homed ones — a repo session's
	// handoff is filed under the workspace key (task ed31b744). Nil keeps
	// the bare ProjectKey reading.
	Scope *store.LocationScope
	// Asker, when set on a text anchor, is the calling Session's folder and
	// branch: a query that matches nothing falls back to the Resume handoff
	// the SessionStart block there offers (handoff.ResumeFor). Nil (no
	// caller folder, as for the CLI) falls back to the location's newest
	// Current Handoff.
	Asker *handoff.Reader
}

// WithScope returns a with its records read through ls.
func (a Anchor) WithScope(ls store.LocationScope) Anchor {
	a.Scope = &ls
	return a
}

// AskedFrom returns a as asked by the Session reading at r.
func (a Anchor) AskedFrom(r handoff.Reader) Anchor {
	a.Asker = &r
	return a
}

// ProjectAnchor anchors on a project: its whole ledger (every record kind,
// including tombstoned ones), newest first by sequence.
func ProjectAnchor(projectKey string) Anchor {
	return Anchor{Kind: AnchorProject, ProjectKey: projectKey}
}

// RecordAnchor anchors on a single record, named by its full id or a short
// prefix unique to it, plus the records the Ledger walk reaches from it
// (graph.Walk, graph.DefaultHops of any edge type, either direction), and
// its Neighbours in its Project.
func RecordAnchor(idOrPrefix string) Anchor {
	return Anchor{Kind: AnchorRecord, RecordID: idOrPrefix}
}

// TextAnchor anchors on a free-text search, scoped to projectKey, plus the
// records the Ledger walk reaches from its top textAnchorSeeds hits: along
// declared edges, and their Neighbours.
func TextAnchor(projectKey, query string) Anchor {
	return Anchor{Kind: AnchorText, ProjectKey: projectKey, Text: query}
}

// Altitude is how much of each item Build renders.
type Altitude string

const (
	// AltitudeHeadline renders one line per item: short id, kind, tier,
	// status, first line of text. ~500 tokens.
	AltitudeHeadline Altitude = "headline"
	// AltitudeSummary renders a short body per item. ~2k tokens.
	AltitudeSummary Altitude = "summary"
	// AltitudeFull renders each item's full text, up to the budget.
	AltitudeFull Altitude = "full"
)

// Status is a recall item's provenance status, derived only from the
// record's own tombstone column and its supersedes/contradicts edges —
// never inferred any other way.
type Status string

const (
	StatusCurrent      Status = "current"
	StatusSuperseded   Status = "superseded"
	StatusTombstoned   Status = "tombstoned"
	StatusContradicted Status = "contradicted"
	// StatusPossiblyStale is a handoff-only status (decision bcc9fa54):
	// positive evidence — a contradiction, a later same-project record, or a
	// later timeline event — arrived after the handoff's event_cursor with
	// no later `confirm affirm` informing it since. It takes precedence over
	// StatusContradicted for handoff records (store.HandoffFreshness folds
	// that same contradiction in as its own reason, affirm-aware in a way
	// the plain StatusContradicted check below never is); no other kind can
	// carry it.
	StatusPossiblyStale Status = "possibly-stale"
	// StatusExpired marks a record whose own ExpiresAt has passed: it was
	// declared valid only until then, so it must never read as current.
	StatusExpired Status = "expired"
)

// now is the clock annotate judges ExpiresAt against; a var so tests can pin it.
var now = time.Now

// Item is one ledger record in a recall result.
type Item struct {
	ID   string
	Kind store.RecordKind
	// TS is when the record was written (not when it was inserted: a synced
	// record is inserted on arrival).
	TS time.Time
	// Tier is the record's stored tier, verbatim — never computed from
	// anything else (PLAN.md §Phase 4: "never computed from anything but
	// the stored tier").
	Tier store.Tier
	// OriginDeviceName names the paired device that wrote the record, ""
	// for one written on this device.
	OriginDeviceName string
	// GitBranch is the branch the record was written on, "" when unknown.
	GitBranch string
	// Text is the record's body, empty when Status is StatusTombstoned
	// (SCHEMA.md invariant 1: recall omits a tombstoned record's text,
	// keeps its edges).
	Text string
	// Next is a handoff's one-line next step, empty for any other kind, for
	// a handoff with none, and when Status is StatusTombstoned.
	Next string
	// Edges are every edge touching this record, both directions, every
	// type, unaffected by Status — in particular still populated when
	// Status is StatusTombstoned, per SCHEMA.md invariant 1 ("keeps its
	// edges").
	Edges []store.Edge

	Status Status
	// SupersededByID is set only when Status is StatusSuperseded: the id of
	// the record that supersedes this one, from the incoming `supersedes`
	// edge.
	SupersededByID string
	// ContradictionEvidence is set only when Status is StatusContradicted:
	// the timeline event ids the contradicting record cited as its own
	// evidence, reached only via a `contradicts` edge into this record.
	ContradictionEvidence []int64
	// ExpiresAt is the record's own expiry, when it declared one.
	ExpiresAt *time.Time

	// StaleReasons is set only when Status is StatusPossiblyStale: every
	// reason handoff.Of found, each carrying its own evidence
	// ids.
	StaleReasons []store.FreshnessReason

	// Done is set for a handoff the human marked done (handoff.Of);
	// DoneAt is when. A done handoff is never possibly stale and is not a
	// resume instruction.
	Done   bool
	DoneAt time.Time

	// Git is the record's write-time repo HEAD compared with the repo's HEAD
	// now; nil when the record carries no git_head (or is tombstoned).
	Git *GitStamp

	// ProjectKey is the Project the record belongs to.
	ProjectKey string
	// OtherProject is set on a walk-reached item from outside the anchor's
	// Project (the anchor record's, or a text anchor's Project or location
	// scope): the walk crosses scope, and the item is labelled with its
	// Project.
	OtherProject bool
	// Reached is how a record or text anchor's walk reached the item
	// (graph.Walk), by an edge or as a Neighbour; nil for a direct hit: the
	// anchor record itself, every text anchor hit, and every project anchor
	// item.
	Reached *graph.Reached
	// Handoff is handoff's status for a Handoff that isn't Tombstoned,
	// whatever its recall Status; nil for any other item.
	Handoff *handoff.Status
	// ConfirmedBy is every Done, Affirm or Promote confirming the record
	// that isn't Tombstoned, oldest first; never nil.
	ConfirmedBy []store.Confirmation
}

// Result is Build's return value: the items that made the altitude's
// budget cut, in the order rendered, and the rendered text itself. Items
// always matches what Rendered shows — Build never trims one without the
// other.
type Result struct {
	Items    []Item
	Rendered string
	// Notice is set for a text anchor: "no match for <query>; latest handoff
	// shown" when nothing matched and recall fell back to the location's
	// latest handoff (and it made the budget), then the Searcher's reasons
	// when it gave any (e.g. "embeddings off"), for its search and for the
	// Neighbours, each once, joined by "; ". A record anchor's is the
	// Searcher's reason for its Neighbours. The reason is there even when
	// Items is empty. Empty for project anchors.
	Notice string
}

// Build resolves anchor to an ordered set of records — walking edges for a
// record anchor — annotates each with its trust tier and status, and
// renders the result at altitude under budgetTokens, using the same
// estimator the SessionStart block budgets against (block.EstimateTokens).
// It only reads st; a text anchor's matches come from searcher. workspaces is the caller's already-resolved workspace
// directory list (task 482b2320, decision f3fa04c7's clause 7): recall
// never resolves workspace dirs itself (no project.DefaultWorkspaceDirs
// call, no env read) — it only needs the list to pass through to
// handoff.Of when annotating a handoff's possibly-stale status.
func Build(st *store.Store, searcher search.Searcher, anchor Anchor, altitude Altitude, budgetTokens int, workspaces []string) (Result, error) {
	found, said, err := resolve(st, searcher, anchor, workspaces)
	if err != nil {
		return Result{}, err
	}

	ids := make([]string, len(found))
	for i, f := range found {
		ids[i] = f.rec.ID
	}
	confirmations, err := st.ConfirmationsOf(ids)
	if err != nil {
		return Result{}, fmt.Errorf("recall: %w", err)
	}

	stamper := newGitStamper(st)
	items := make([]Item, len(found))
	for i, f := range found {
		item, err := annotateWith(st, f.rec, workspaces, f.handoff)
		if err != nil {
			return Result{}, err
		}
		if item.Status != StatusTombstoned {
			if item.Git, err = stamper.stamp(f.rec); err != nil {
				return Result{}, err
			}
		}
		item.Reached, item.OtherProject = f.reached, f.otherProject
		item.ConfirmedBy = confirmations[f.rec.ID]
		items[i] = item
	}

	res := fit(items, altitude, budgetTokens)
	var lines []string
	if said.fallback != "" && len(res.Items) > 0 {
		lines = append(lines, said.fallback)
	}
	if said.reason != "" {
		lines = append(lines, said.reason)
	}
	res.Notice = strings.Join(lines, "; ")
	return res, nil
}

// notices is what resolve has to say about how it found the records: the
// latest-Handoff fallback notice and the Searcher's reason, each "" when
// there is none.
type notices struct {
	fallback string
	reason   string
}

// resolved is one record resolve found, with what the Ledger walk already
// knows about it: how it was reached, whether it is from another Project
// than the anchor's, and its Handoff status (nil when not computed).
type resolved struct {
	rec          store.Record
	reached      *graph.Reached
	otherProject bool
	handoff      *handoff.Status
}

// direct wraps records that are direct hits.
func direct(recs []store.Record) []resolved {
	out := make([]resolved, len(recs))
	for i, rec := range recs {
		out[i] = resolved{rec: rec}
	}
	return out
}

// resolve turns anchor into an ordered record set, before status
// annotation or altitude rendering: project and record anchors are already
// in sequence order (RecordsForProjectAll / graph.Walk); a text anchor's
// search hits are re-ordered into sequence order the same way, so every
// anchor kind's output obeys the one ordering rule (SCHEMA.md invariant
// 10: order by sequence, never ts).
func resolve(st *store.Store, searcher search.Searcher, anchor Anchor, workspaces []string) ([]resolved, notices, error) {
	switch anchor.Kind {
	case AnchorProject:
		recs, err := projectRecords(st, anchor)
		return direct(recs), notices{}, err
	case AnchorRecord:
		return resolveRecordAnchor(st, searcher, anchor.RecordID, workspaces)
	case AnchorText:
		return resolveTextAnchor(st, searcher, anchor, workspaces)
	default:
		return nil, notices{}, fmt.Errorf("recall: unknown anchor kind %q", anchor.Kind)
	}
}

// projectRecords reads anchor's whole ledger (scoped to its location when it
// has one), newest first by sequence.
func projectRecords(st *store.Store, anchor Anchor) ([]store.Record, error) {
	var recs []store.Record
	var err error
	if anchor.Scope != nil {
		recs, err = st.RecordsForLocation(*anchor.Scope, maxProjectRecords)
	} else {
		recs, err = st.RecordsForProjectAll(anchor.ProjectKey, maxProjectRecords)
	}
	if err != nil {
		return nil, fmt.Errorf("recall: project anchor: %w", err)
	}
	return recs, nil
}

// resolveRecordAnchor resolves idOrPrefix to a record, then walks the
// Ledger from it (graph.Walk, graph.DefaultHops, plus the record's
// Neighbours in its Project) and returns what the walk reached, newest first
// by sequence, minus Tombstoned Records: recall never shows one (#32),
// though the walk goes on through it. The record itself is always shown,
// with its status, even an expired Draft or a confirm Record, since
// graph.Walk keeps every seed; what the walk reaches leaves those out. The
// notice carries the Searcher's reason when Neighbours are missing. An
// anchor that resolves to no record
// (unknown id, or an ambiguous short prefix) returns an empty set, not an
// error: recall answering "nothing here" is this anchor's honest empty
// state.
func resolveRecordAnchor(st *store.Store, searcher search.Searcher, idOrPrefix string, workspaces []string) ([]resolved, notices, error) {
	root, ok, err := st.FindRecordByIDPrefix(idOrPrefix)
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: record anchor: %w", err)
	}
	if !ok {
		return nil, notices{}, nil
	}

	// A record anchor's scope is its root Record's Project, never a Work
	// location: a record id names no location, so its Neighbours come only
	// from that Project, and an item outside it is labelled with its own.
	scope := graph.Scope{ProjectKey: root.ProjectKey}
	limits := withNeighbours(graph.Limits{Hops: graph.DefaultHops}, scope)
	g, err := graph.Walk(context.Background(), graph.Source{Store: st, Searcher: searcher, Workspaces: workspaces},
		[]string{root.ID}, limits)
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: record anchor: %w", err)
	}
	var out []resolved
	for _, node := range g.Nodes {
		if node.Status == graph.StatusTombstoned {
			continue
		}
		out = append(out, resolved{
			rec: node.Record, reached: node.Reached, handoff: node.Handoff,
			otherProject: !scope.Has(st, node.Record),
		})
	}
	return out, notices{reason: g.Reason}, nil
}

// handoffKindWords are the query words that name the handoff record kind
// itself: a query containing one matches every handoff at the location, since
// an agent asking for "resume" or the "handoff" wants the handoff whatever
// words it happens to contain.
var handoffKindWords = map[string]bool{"handoff": true, "handoffs": true, "resume": true}

// resolveTextAnchor takes searcher's hits for the query (a Handoff's next
// text included), adds every handoff at the location when the query names
// the handoff kind, then widens: the Ledger walk (graph.Walk,
// graph.DefaultHops, plus each seed's Neighbours inside the anchor's
// scope) from the top textAnchorSeeds hits by search rank adds what it
// reaches, minus Tombstoned Records. Every hit and every added handoff
// stays a direct hit, even one the walk reaches. A superseded Record is
// never a direct hit, only reached by an edge (#32). The result is in
// sequence order. When nothing matches it falls back to the asking
// Session's Resume handoff, else (no Asker) the location's newest Current
// Handoff (handoff.Newest), and says so in the returned notice, which also
// carries the searcher's reasons, for its search and for the Neighbours,
// each once.
func resolveTextAnchor(st *store.Store, searcher search.Searcher, anchor Anchor, workspaces []string) ([]resolved, notices, error) {
	res, err := searcher.Search(context.Background(), search.Query{
		Text: anchor.Text, ProjectKey: anchor.ProjectKey, Scope: anchor.Scope, Limit: maxTextResults, CurrentOnly: true,
	})
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
	}
	ids := make([]string, 0, len(res.Hits))
	seen := map[string]bool{}
	for _, h := range res.Hits {
		ids = append(ids, h.RecordID)
		seen[h.RecordID] = true
	}
	seeds := ids[:min(len(ids), textAnchorSeeds)]

	said := notices{reason: res.Reason}
	wantHandoffs := namesHandoffKind(anchor.Text)
	if wantHandoffs || len(ids) == 0 {
		all, err := projectRecords(st, anchor)
		if err != nil {
			return nil, notices{}, err
		}
		if wantHandoffs {
			for _, rec := range all {
				if rec.Kind != store.KindHandoff || rec.TombstonedAt != nil || seen[rec.ID] {
					continue
				}
				if sup, err := st.IsSuperseded(rec.ID); err != nil {
					return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
				} else if sup {
					continue
				}
				seen[rec.ID] = true
				ids = append(ids, rec.ID)
			}
		}
		if len(ids) == 0 {
			latest, ok, err := standIn(st, anchor, all)
			if err != nil {
				return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
			}
			if ok {
				seen[latest.ID] = true
				ids = append(ids, latest.ID)
				said.fallback = fmt.Sprintf("no match for %s; latest handoff shown", anchor.Text)
			}
		}
	}

	scope := anchorScope(anchor)
	g, err := graph.Walk(context.Background(), graph.Source{Store: st, Searcher: searcher, Workspaces: workspaces},
		seeds, withNeighbours(graph.Limits{Hops: graph.DefaultHops}, scope))
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
	}
	said.reason = search.JoinReasons(said.reason, g.Reason)
	widened := map[string]graph.Node{}
	for _, node := range g.Nodes {
		if seen[node.Record.ID] || node.Status == graph.StatusTombstoned {
			continue
		}
		widened[node.Record.ID] = node
		ids = append(ids, node.Record.ID)
	}

	recs, err := st.RecordsByIDs(ids)
	if err != nil {
		return nil, notices{}, fmt.Errorf("recall: text anchor: %w", err)
	}
	out := make([]resolved, len(recs))
	for i, rec := range recs {
		out[i] = resolved{rec: rec}
		if node, ok := widened[rec.ID]; ok {
			out[i].reached, out[i].handoff = node.Reached, node.Handoff
			out[i].otherProject = !scope.Has(st, rec)
		}
	}
	return out, said, nil
}

// standIn is a no-match text anchor's Handoff: the asking Session's Resume
// handoff, else the newest Current one among the location's records.
func standIn(st *store.Store, anchor Anchor, all []store.Record) (store.Record, bool, error) {
	if anchor.Asker != nil {
		res, err := handoff.ResumeFor(st, *anchor.Asker)
		return res.Handoff, res.Found, err
	}
	return handoff.Newest(st, all)
}

// namesHandoffKind reports whether query has a word naming the handoff kind.
func namesHandoffKind(query string) bool {
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if handoffKindWords[t] {
			return true
		}
	}
	return false
}

// Annotate is annotate's exported form: the single status code path, shared
// with callers (the `records` CLI) that list records outside a recall
// narrative but must label them exactly as recall does.
func Annotate(st *store.Store, rec store.Record, workspaces []string) (Item, error) {
	return annotateWith(st, rec, workspaces, nil)
}

// annotateWith builds an Item from rec: tier carried verbatim, status derived
// in a fixed precedence — tombstoned first (text omitted per SCHEMA.md
// invariant 1, so nothing downstream can leak it back in), then superseded,
// then contradicted, else current. A record can technically satisfy more
// than one condition (e.g. a tombstoned record superseded before deletion);
// this precedence picks the single status callers see, always the
// strongest one, rather than leaving it ambiguous which wins.
//
// A Handoff's status (handoff.Of) is hs when the caller already has it,
// else computed here, and set on Item.Handoff whatever the recall status.
func annotateWith(st *store.Store, rec store.Record, workspaces []string, hs *handoff.Status) (Item, error) {
	edges, err := st.EdgesTouching(rec.ID)
	if err != nil {
		return Item{}, fmt.Errorf("recall: annotate %s: %w", rec.ID, err)
	}

	item := Item{
		ID:               rec.ID,
		Kind:             rec.Kind,
		TS:               rec.TS,
		Tier:             rec.Tier,
		OriginDeviceName: rec.OriginDeviceName,
		GitBranch:        rec.GitBranch,
		Text:             rec.Text,
		Next:             rec.Next,
		Status:           StatusCurrent,
		Edges:            edges,
		ProjectKey:       rec.ProjectKey,
	}
	if rec.ExpiresAt != nil {
		item.ExpiresAt = rec.ExpiresAt
	}
	if rec.TombstonedAt != nil {
		item.Status = StatusTombstoned
		item.Text = ""
		item.Next = ""
		return item, nil
	}

	if rec.Kind == store.KindHandoff {
		if hs == nil {
			status, err := handoff.Of(st, rec, workspaces)
			if err != nil {
				return Item{}, fmt.Errorf("recall: annotate %s: %w", rec.ID, err)
			}
			hs = &status
		}
		item.Handoff = hs
	}

	if by, ok := store.SupersederOf(rec.ID, edges); ok {
		item.Status = StatusSuperseded
		item.SupersededByID = by
		return item, nil
	}

	if rec.ExpiresAt != nil && !rec.ExpiresAt.After(now()) {
		item.Status = StatusExpired
		return item, nil
	}

	// A handoff's contradiction signal is folded into HandoffFreshness's own
	// reasons (FreshnessContradicted), affirm-aware in a way the plain
	// StatusContradicted check below is not, so a handoff never falls
	// through to that check at all — flagged or not.
	if item.Handoff != nil {
		item.Done, item.DoneAt = item.Handoff.Done, item.Handoff.DoneAt
		if item.Handoff.PossiblyStale() {
			item.Status = StatusPossiblyStale
			item.StaleReasons = item.Handoff.Reasons
		}
		return item, nil
	}

	for _, e := range edges {
		if e.Type == store.EdgeContradicts && e.ToID == rec.ID {
			evidenceRec, err := st.GetRecord(e.FromID)
			if err != nil {
				return Item{}, fmt.Errorf("recall: annotate %s: contradiction source: %w", rec.ID, err)
			}
			item.Status = StatusContradicted
			item.ContradictionEvidence = evidenceRec.Evidence
			return item, nil
		}
	}

	return item, nil
}

// fit renders items at altitude, keeping only as many as fit budgetTokens
// (block.EstimateTokens, the same estimator the SessionStart block budgets
// against). Survivors are picked by priority (outranks: direct hits, then
// edge-reached items by fewest hops, then Neighbours by closeness, ties in
// the given order) and rendered in the given order, which is sequence
// order (#32). It adds them
// one at a time, re-rendering the kept set, and stops before the first
// that would push the estimate over budget, so a lower-priority item never
// pushes out a higher one, Result.Rendered's own estimate is always <=
// budgetTokens, and Result.Items is exactly the set that rendering
// reflects.
func fit(items []Item, altitude Altitude, budgetTokens int) Result {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return outranks(items[order[a]].Reached, items[order[b]].Reached) })

	sep := altitudeSeparator(altitude)
	lines := make([]string, len(items))
	kept := make([]bool, len(items))
	rendered, last := "", -1
	for _, i := range order {
		lines[i] = renderItem(items[i], altitude)
		var candidate string
		switch {
		case last < 0:
			candidate = lines[i]
		case i > last:
			// Every kept item comes before this one: append, as a project
			// anchor (all direct hits) always does.
			candidate = rendered + sep + lines[i]
		default:
			kept[i] = true
			candidate = joinKept(lines, kept, sep)
			kept[i] = false
		}
		if block.EstimateTokens(candidate) > budgetTokens {
			break
		}
		rendered, kept[i], last = candidate, true, max(last, i)
	}
	res := Result{Rendered: rendered}
	for i, item := range items {
		if kept[i] {
			res.Items = append(res.Items, item)
		}
	}
	return res
}

// outranks reports whether an item reached by a goes into the budget
// before one reached by b: a direct hit (nil) first, then an edge-reached
// item, fewer hops first, then a Neighbour, closer first (#32). Anything
// else comes after those.
func outranks(a, b *graph.Reached) bool {
	if (a == nil) != (b == nil) {
		return a == nil
	}
	if a == nil {
		return false
	}
	aEdge, bEdge := a.Via == graph.ViaEdge, b.Via == graph.ViaEdge
	if aEdge && bEdge {
		return a.Hops < b.Hops
	}
	if aEdge != bEdge {
		return aEdge
	}
	return closerNeighbour(a, b)
}

// joinKept joins the kept lines, in order, with sep.
func joinKept(lines []string, kept []bool, sep string) string {
	var parts []string
	for i, line := range lines {
		if kept[i] {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, sep)
}

func altitudeSeparator(altitude Altitude) string {
	if altitude == AltitudeHeadline {
		return "\n"
	}
	return "\n\n"
}

// shortIDLen is how many leading characters of a record id headline
// rendering shows — enough to eyeball-distinguish items in a list, short
// enough that "one line per item" (DONE WHEN clause 2) stays true even for
// a long first line of text.
const shortIDLen = 8

func shortID(id string) string {
	if len(id) <= shortIDLen {
		return id
	}
	return id[:shortIDLen]
}

// summaryBodyChars bounds the summary altitude's per-item body length —
// "short bodies of key records" (PLAN.md §Phase 4) — before the overall
// token budget ever gets a chance to cut items.
const summaryBodyChars = 240

// renderItem renders one item at altitude: headline is always exactly one
// line (firstLine strips any embedded newline, and no other part of the
// line can contain one), summary truncates the body, full does not.
func renderItem(item Item, altitude Altitude) string {
	header := fmt.Sprintf("%s · %s · %s · %s", shortID(item.ID), item.Kind, item.Tier, statusLabel(item))
	if item.OriginDeviceName != "" {
		header += " · from " + item.OriginDeviceName
	}
	if item.Kind == store.KindHandoff && item.GitBranch != "" {
		header += " · on branch " + item.GitBranch
	}
	if item.Git != nil {
		header += " · " + gitLabel(item.Git)
	}
	if item.OtherProject {
		header += " · project " + item.ProjectKey
	}
	reached := reachedLabel(item.Reached)
	if altitude == AltitudeHeadline {
		out := header
		if reached != "" {
			out += " · " + reached
		}
		if fl := firstLine(item.Text); fl != "" {
			out += " · " + fl
		}
		if nl := firstLine(item.Next); nl != "" {
			out += " · Next: " + nl
		}
		return out
	}
	if reached != "" {
		header += "\n" + reached
	}
	switch altitude {
	case AltitudeSummary:
		return withNext(renderBody(header, item.Text, summaryBodyChars), item.Next)
	case AltitudeFull:
		return withNext(renderBody(header, item.Text, 0), item.Next)
	default:
		return header
	}
}

// reachedLabel is how an item was reached, e.g. "← informs `a1b2c3d4`":
// the arrow points along the edge relative to the item, so "←" is From ->
// item and "→" is item -> From. A Neighbour's is neighbourLabel. "" for a
// direct hit.
func reachedLabel(r *graph.Reached) string {
	if r != nil && r.Via == graph.ViaNeighbour {
		return neighbourLabel(r)
	}
	if r == nil || r.Via != graph.ViaEdge {
		return ""
	}
	arrow := "←"
	if r.Direction == graph.DirectionIn {
		arrow = "→"
	}
	return arrow + " " + string(r.Type) + " `" + shortID(r.From) + "`"
}

// withNext appends a handoff's next step on its own "Next:" line.
func withNext(body, next string) string {
	if next == "" {
		return body
	}
	return body + "\nNext: " + next
}

func renderBody(header, text string, maxChars int) string {
	if maxChars > 0 {
		r := []rune(text)
		if len(r) > maxChars {
			text = string(r[:maxChars]) + "…"
		}
	}
	if text == "" {
		return header
	}
	return header + "\n" + text
}

// StatusLabel is the engine's own human-readable label for item's status.
func StatusLabel(item Item) string { return statusLabel(item) }

func statusLabel(item Item) string {
	if item.Done {
		return "done (marked done by you on " + item.DoneAt.UTC().Format("2006-01-02") + ")"
	}
	switch item.Status {
	case StatusSuperseded:
		return "superseded by " + shortID(item.SupersededByID)
	case StatusTombstoned:
		return "tombstoned"
	case StatusContradicted:
		return "contradicted (evidence " + joinInt64s(item.ContradictionEvidence) + ")"
	case StatusPossiblyStale:
		return "possibly stale: " + handoff.Status{Reasons: item.StaleReasons}.Label() +
			" (evidence " + strings.Join(staleEvidence(item.StaleReasons), ",") + ")"
	case StatusExpired:
		return "expired " + item.ExpiresAt.UTC().Format(time.RFC3339)
	case StatusCurrent:
		return "current"
	default:
		return "current"
	}
}

// staleEvidence is handoff.Status.Evidence in reason order with each record
// id shortened, as the rest of the label shows record ids.
func staleEvidence(reasons []store.FreshnessReason) []string {
	var ids []string
	for _, r := range reasons {
		for _, id := range r.RecordIDs {
			ids = append(ids, shortID(id))
		}
		ids = append(ids, intsToStrs(r.EventIDs)...)
	}
	return ids
}

func intsToStrs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}

func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i]
	}
	return text
}

func joinInt64s(ids []int64) string {
	strs := make([]string, len(ids))
	for i, v := range ids {
		strs[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(strs, ",")
}
