package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// searchEnv is a fresh store with two Projects, each with one Session.
type searchEnv struct {
	t        *testing.T
	s        *Store
	sessionA string
	sessionB string
}

func newSearchEnv(t *testing.T) *searchEnv {
	t.Helper()
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	mustUpsertProject(t, s, "proj-a")
	mustUpsertProject(t, s, "proj-b")
	return &searchEnv{t: t, s: s,
		sessionA: mustStartSessionInProject(t, s, "proj-a"),
		sessionB: mustStartSessionInProject(t, s, "proj-b")}
}

func (e *searchEnv) insert(project string, kind RecordKind, text, next string) string {
	e.t.Helper()
	session := e.sessionA
	if project == "proj-b" {
		session = e.sessionB
	}
	id, err := e.s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: kind, Text: text, Next: next,
		SessionID: session, ProjectKey: project,
	})
	if err != nil {
		e.t.Fatalf("InsertRecord: %v", err)
	}
	return id
}

func (e *searchEnv) search(text string, f SearchFilter) []string {
	e.t.Helper()
	ids, err := e.s.SearchText(text, f)
	if err != nil {
		e.t.Fatalf("SearchText(%q): %v", text, err)
	}
	return ids
}

// The filters narrow the candidates before the limit is applied: records
// outside the filter that would outrank the one inside it never take its
// place, so a narrow filter never comes back short.
func TestSearchTextFiltersBeforeRanking(t *testing.T) {
	e := newSearchEnv(t)
	for i := 0; i < 20; i++ {
		e.insert("proj-b", KindDecision, fmt.Sprintf("widget widget widget %d", i), "")
		e.insert("proj-a", KindNote, fmt.Sprintf("widget widget widget note %d", i), "")
	}
	weak := e.insert("proj-a", KindDecision, "a long decision that mentions a widget only once among many other words", "")
	strong := e.insert("proj-a", KindDecision, "widget widget superseded", "")
	replacement := e.insert("proj-a", KindDecision, "unrelated replacement", "")
	if err := e.s.LinkEdge(replacement, strong, EdgeSupersedes, "human"); err != nil {
		t.Fatal(err)
	}

	got := e.search("widget", SearchFilter{ProjectKey: "proj-a", Kinds: []RecordKind{KindDecision}, CurrentOnly: true, Limit: 1})
	if !slices.Equal(got, []string{weak}) {
		t.Fatalf("got %v, want [%s] (proj-a's one current widget decision)", got, weak)
	}

	got = e.search("widget", SearchFilter{ProjectKey: "proj-a", Kinds: []RecordKind{KindDecision}, Limit: 5})
	if !slices.Equal(got, []string{strong, weak}) {
		t.Fatalf("without CurrentOnly got %v, want [%s %s] (superseded included, best match first)", got, strong, weak)
	}

	ls, err := e.s.LocationScopeForKey("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	got = e.search("widget", SearchFilter{Scope: &ls, Kinds: []RecordKind{KindDecision}, CurrentOnly: true, Limit: 1})
	if !slices.Equal(got, []string{weak}) {
		t.Fatalf("scoped: got %v, want [%s]", got, weak)
	}
}

func TestSearchTextExcludesTombstoned(t *testing.T) {
	e := newSearchEnv(t)
	gone := e.insert("proj-a", KindNote, "sandbox credentials rotated", "")
	kept := e.insert("proj-a", KindNote, "sandbox rebuilt", "")
	if err := e.s.TombstoneRecord(gone, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	// A tombstoned row whose text and next the scrub never reached is still
	// never a hit: the filter, not the scrub, is what excludes it.
	if _, err := e.s.DB().Exec(`INSERT INTO records
		(id, ts, kind, tier, text, next, about, session_id, project_key, evidence, tombstoned_at)
		VALUES ('cccc0001-0000-4000-8000-000000000001', ?, 'handoff', 'agent-declared', 'sandbox leftover', 'sandbox next', '[]', NULL, 'proj-a', '[]', ?)`,
		time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if got := e.search("sandbox", SearchFilter{ProjectKey: "proj-a", Limit: 10}); !slices.Equal(got, []string{kept}) {
		t.Fatalf("got %v, want only [%s]", got, kept)
	}
}

// A Handoff whose next step names the query is a hit even though its text
// does not; a tombstoned one, or one outside the filter, is not.
func TestSearchTextFindsHandoffByNext(t *testing.T) {
	e := newSearchEnv(t)
	byText := e.insert("proj-a", KindNote, "the migration is half done", "")
	byNext := e.insert("proj-a", KindHandoff, "wired the parser", "finish the Migration script")
	e.insert("proj-a", KindHandoff, "unrelated handoff", "something else")
	e.insert("proj-b", KindHandoff, "other project", "finish the migration there")
	gone := e.insert("proj-a", KindHandoff, "forgotten", "migration secrets")
	if err := e.s.TombstoneRecord(gone, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}

	got := e.search("migration", SearchFilter{ProjectKey: "proj-a", Limit: 10})
	if !slices.Equal(got, []string{byText, byNext}) {
		t.Fatalf("got %v, want [%s %s] (text match, then the handoff found by next)", got, byText, byNext)
	}
	ls, err := e.s.LocationScopeForKey("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.search("migration", SearchFilter{Scope: &ls, Kinds: []RecordKind{KindHandoff}, Limit: 10}); !slices.Equal(got, []string{byNext}) {
		t.Fatalf("scoped handoffs: got %v, want [%s]", got, byNext)
	}
	if got := e.search("migration", SearchFilter{ProjectKey: "proj-a", Kinds: []RecordKind{KindNote}, Limit: 10}); !slices.Equal(got, []string{byText}) {
		t.Fatalf("notes only: got %v, want [%s]", got, byText)
	}
}

// Short text is quoted token by token, so short words still match; long text
// is reduced to its distinctive terms, so filler words match nothing.
func TestSearchTextQueryRuleDependsOnLength(t *testing.T) {
	e := newSearchEnv(t)
	postgres := e.insert("proj-a", KindDecision, "use Postgres for the store", "")
	qml := e.insert("proj-a", KindDecision, "render the panel with QML", "")
	f := SearchFilter{ProjectKey: "proj-a", Limit: 10}

	if got := e.search("the", f); len(got) != 2 {
		t.Errorf("short %q: got %v, want both records", "the", got)
	}
	if got := e.search("drop Postgres, keep the file", f); !slices.Equal(got, []string{postgres}) {
		t.Errorf("long text: got %v, want only [%s] (not %s via %q)", got, postgres, qml, "the")
	}
}

func TestSearchTextKeepsExactTokensAndNeverErrors(t *testing.T) {
	e := newSearchEnv(t)
	id := e.insert("proj-a", KindNote, "investigate wobble-party crash e3f9a1", "")
	f := SearchFilter{ProjectKey: "proj-a", Limit: 10}
	for _, q := range []string{"wobble-party", "WOBBLE-PARTY", `"wobble-party"`, "e3f9a1", "wobble", "wobble-party crash", "why did wobble-party crash at e3f9a1 today"} {
		if got := e.search(q, f); !slices.Equal(got, []string{id}) {
			t.Errorf("%q: got %v, want [%s]", q, got, id)
		}
	}
	for _, q := range []string{"", "   ", `"`, "'", "a:b", "foo*", "(x, x)", "^x", `say "hi`, "NOT", "AND OR", "NEAR(a b)", "---",
		`"*" ( NEAR AND : " and some more words`} {
		if _, err := e.s.SearchText(q, f); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

// DecisionsSharingAbout ranks current Decisions in the Project by how many
// about paths they share, most first, then newest.
func TestDecisionsSharingAbout(t *testing.T) {
	e := newSearchEnv(t)
	decide := func(project, text string, about ...string) string {
		t.Helper()
		session := e.sessionA
		if project == "proj-b" {
			session = e.sessionB
		}
		id, err := e.s.InsertRecord(InsertRecordParams{
			Identity: Identity{Kind: IdentityAgent}, Kind: KindDecision, Text: text, About: about,
			SessionID: session, ProjectKey: project,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	one := decide("proj-a", "one shared", "a.go", "z.go")
	two := decide("proj-a", "two shared", "a.go", "b.go")
	newerOne := decide("proj-a", "newer one shared", "b.go")
	decide("proj-a", "none shared", "c.go")
	decide("proj-b", "other project", "a.go", "b.go")
	sup := decide("proj-a", "superseded", "a.go", "b.go")
	if err := e.s.LinkEdge(newerOne, sup, EdgeSupersedes, "human"); err != nil {
		t.Fatal(err)
	}
	gone := decide("proj-a", "tombstoned", "a.go", "b.go")
	if err := e.s.TombstoneRecord(gone, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: KindNote, Text: "a note", About: []string{"a.go", "b.go"},
		SessionID: e.sessionA, ProjectKey: "proj-a",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := e.s.DecisionsSharingAbout("proj-a", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{two, newerOne, one}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, _ := e.s.DecisionsSharingAbout("proj-a", nil); len(got) != 0 {
		t.Fatalf("no about paths: got %v, want none", got)
	}
}

// sessionScope is a scope holding only sessionIDs, for fixtures filed under
// no Project.
func sessionScope(sessionIDs ...string) *LocationScope {
	ls := LocationScope{SessionIDs: map[string]bool{}}
	for _, id := range sessionIDs {
		ls.SessionIDs[id] = true
	}
	return &ls
}

// Records that match equally are ordered newest first by ts. The earlier
// record is inserted first, so rowid order agrees with ts order and only the
// ts tiebreak can reverse it.
func TestSearchTextTiebreaksEqualRankNewestFirst(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	sessionID := mustStartSession(t, s)
	earlier := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	earlierID := insertRecordFixture(t, s, sessionID, earlier, "tiebreak probe shared text")
	laterID := insertRecordFixture(t, s, sessionID, earlier.Add(time.Hour), "tiebreak probe shared text")

	got, err := s.SearchText("tiebreak", SearchFilter{Scope: sessionScope(sessionID), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{laterID, earlierID}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v (equal-rank ties broken newest first)", got, want)
	}
}

// KeyTermsOnly reduces even short text to its distinctive terms, so a short
// Decision's filler words match nothing.
func TestSearchTextKeyTermsOnly(t *testing.T) {
	e := newSearchEnv(t)
	sqlite := e.insert("proj-a", KindDecision, "keep SQLite as the store", "")
	e.insert("proj-a", KindDecision, "use Postgres for reports", "")
	if got := e.search("Use SQLite", SearchFilter{ProjectKey: "proj-a", Limit: 10}); len(got) != 2 {
		t.Fatalf("short text by length: got %v, want both (%q matches %q)", got, "use", "use Postgres")
	}
	if got := e.search("Use SQLite", SearchFilter{ProjectKey: "proj-a", KeyTermsOnly: true, Limit: 10}); !slices.Equal(got, []string{sqlite}) {
		t.Fatalf("KeyTermsOnly: got %v, want only [%s]", got, sqlite)
	}
}

// insertDraft writes an inferred Note (a Draft) in proj-a expiring at
// expires.
func (e *searchEnv) insertDraft(text string, expires time.Time) string {
	e.t.Helper()
	id, err := e.s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityInference}, Kind: KindNote, Text: text,
		SessionID: e.sessionA, ProjectKey: "proj-a", ExpiresAt: &expires,
	})
	if err != nil {
		e.t.Fatalf("InsertRecord draft: %v", err)
	}
	return id
}

// An expired Draft is a stale guess, so the text match leaves it out before
// ranking; a Draft that hasn't expired yet still matches.
func TestSearchTextExcludesExpiredDrafts(t *testing.T) {
	e := newSearchEnv(t)
	e.insertDraft("lantern lantern lantern guess", time.Now().Add(-time.Hour))
	fresh := e.insertDraft("lantern guess still fresh", time.Now().Add(time.Hour))
	kept := e.insert("proj-a", KindNote, "a lantern note among many other words", "")

	got := e.search("lantern", SearchFilter{ProjectKey: "proj-a", Limit: 1})
	if !slices.Equal(got, []string{fresh}) {
		t.Fatalf("limit 1: got %v, want [%s] (the expired Draft never takes the slot)", got, fresh)
	}
	got = e.search("lantern", SearchFilter{ProjectKey: "proj-a", Limit: 10})
	if !slices.Equal(got, []string{fresh, kept}) {
		t.Fatalf("got %v, want [%s %s] (no expired Draft)", got, fresh, kept)
	}
}

// ExpiredDraft is the one Draft-expiry rule, on the store's clock: an
// inferred Record whose own expiry is at or before now. Search leaves out
// exactly the Records it names. A Record of another Tier past its expiry,
// such as a Claim, is not an expired Draft.
func TestExpiredDraftIsSearchsRuleOnTheStoresClock(t *testing.T) {
	e := newSearchEnv(t)
	real := time.Now()
	e.s.SetClock(func() time.Time { return real.Add(2 * time.Hour) })
	lapsed := e.insertDraft("beacon guess, lapsed by the store's clock", real.Add(time.Hour))
	fresh := e.insertDraft("beacon guess, fresh by the store's clock", real.Add(3*time.Hour))
	claim, err := e.s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: KindClaim, Text: "beacon claim, past its expiry",
		SessionID: e.sessionA, ProjectKey: "proj-a", ExpiresAt: &real,
	})
	if err != nil {
		t.Fatalf("InsertRecord claim: %v", err)
	}

	found := e.search("beacon", SearchFilter{ProjectKey: "proj-a", Limit: 10})
	for id, want := range map[string]bool{lapsed: true, fresh: false, claim: false} {
		rec, err := e.s.GetRecord(id)
		if err != nil {
			t.Fatalf("GetRecord: %v", err)
		}
		if got := e.s.ExpiredDraft(rec); got != want {
			t.Errorf("ExpiredDraft(%q) = %v, want %v", rec.Text, got, want)
		}
		if searched := slices.Contains(found, id); searched == want {
			t.Errorf("%q searched = %v, want %v (Search applies ExpiredDraft's rule)", rec.Text, searched, !want)
		}
	}
}
