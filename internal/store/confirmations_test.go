package store

import "testing"

func mustConfirm(t *testing.T, s *Store, p ConfirmParams) string {
	t.Helper()
	if p.ProjectKey == "" {
		p.ProjectKey = freshnessProjectKey
	}
	id, err := s.Confirm(p)
	if err != nil {
		t.Fatalf("Confirm %s: %v", p.Action, err)
	}
	return id
}

// Each confirm Record informing a target, not Tombstoned, is named by the
// action the store wrote it under: Done, Affirm or Promote. A
// Contradiction is not a confirmation, a Tombstoned confirm no longer
// confirms anything, and an Agent's affirm whose text happens to read like
// Done's marker is still an affirm. An id with no confirmations has an
// empty entry, never nil.
func TestConfirmationsOfNamesEachConfirmByItsStoredAction(t *testing.T) {
	s, sessionA := newFreshnessFixture(t)
	sessionB := mustStartSessionInProject(t, s, freshnessProjectKey)
	h := mustInsertHandoff(t, s, sessionA, []string{"a.go"})
	draft, err := s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityInference}, Kind: KindNote, Text: "a guess",
		SessionID: sessionA, ProjectKey: freshnessProjectKey,
	})
	if err != nil {
		t.Fatalf("InsertRecord draft: %v", err)
	}
	unconfirmed := mustInsertNoteAbout(t, s, sessionA, nil)

	affirm := mustConfirm(t, s, ConfirmParams{Identity: Identity{Kind: IdentityAgent}, Action: ConfirmAffirm, RecordID: h.ID, SessionID: sessionA})
	lookalike := mustConfirm(t, s, ConfirmParams{Identity: Identity{Kind: IdentityAgent}, Action: ConfirmAffirm, RecordID: h.ID, SessionID: sessionA, Text: DoneMarkerText})
	forgotten := mustConfirm(t, s, ConfirmParams{Identity: Identity{Kind: IdentityAgent}, Action: ConfirmAffirm, RecordID: h.ID, SessionID: sessionA})
	if err := s.TombstoneRecord(forgotten, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatalf("TombstoneRecord: %v", err)
	}
	_ = mustContradict(t, s, sessionA, h.ID)
	done := mustDone(t, s, h.ID)
	promote := mustConfirm(t, s, ConfirmParams{Identity: Identity{Kind: IdentityAgent}, Action: ConfirmPromote, RecordID: draft, SessionID: sessionB})

	got, err := s.ConfirmationsOf([]string{h.ID, draft, unconfirmed})
	if err != nil {
		t.Fatalf("ConfirmationsOf: %v", err)
	}
	type want struct {
		id   string
		typ  ConfirmAction
		tier Tier
	}
	check := func(target string, wants []want) {
		t.Helper()
		cs := got[target]
		if len(cs) != len(wants) {
			t.Fatalf("ConfirmationsOf[%s] = %+v, want %d confirmations", target, cs, len(wants))
		}
		for i, w := range wants {
			c := cs[i]
			if c.ID != w.id || c.Type != w.typ || c.Tier != w.tier || c.TS.IsZero() {
				t.Errorf("ConfirmationsOf[%s][%d] = %+v, want id %s type %s tier %s and a ts", target, i, c, w.id, w.typ, w.tier)
			}
		}
	}
	check(h.ID, []want{
		{affirm, ConfirmAffirm, TierAgentDeclared},
		{lookalike, ConfirmAffirm, TierAgentDeclared},
		{done, ConfirmDone, TierHumanDeclared},
	})
	check(draft, []want{{promote, ConfirmPromote, TierAgentDeclared}})
	if cs := got[unconfirmed]; cs == nil || len(cs) != 0 {
		t.Errorf("ConfirmationsOf[unconfirmed] = %#v, want empty, not nil", cs)
	}
}
