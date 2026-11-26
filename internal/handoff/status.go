// Package handoff decides a Handoff's status: Done, Possibly stale (with the
// reasons and the Evidence each cites), or neither, and words those reasons
// (Status.Label, Line). It is the one place the SessionStart block, This
// Week and recall ask, so the three surfaces agree.
package handoff

import (
	"fmt"
	"strconv"
	"time"

	"github.com/RidgetopAi/backstory/internal/store"
)

// Status is a Handoff's status. A Done Handoff is never Possibly stale.
type Status struct {
	// Done: the Human marked the Handoff Done (store.HandoffDone); DoneAt is
	// when, from the latest live done record.
	Done   bool
	DoneAt time.Time
	// Reasons is every reason the Handoff is Possibly stale, in
	// store.HandoffFreshness's fixed order; empty when it is not.
	Reasons []store.FreshnessReason
}

// PossiblyStale reports whether any positive evidence flags the Handoff.
func (s Status) PossiblyStale() bool { return len(s.Reasons) > 0 }

// Evidence is every reason's Evidence (see Evidence), in reason order;
// never nil.
func (s Status) Evidence() []string {
	ids := []string{}
	for _, r := range s.Reasons {
		ids = append(ids, Evidence(r)...)
	}
	return ids
}

// Evidence is the ids reason r cites: its record ids, then its timeline
// event ids in decimal.
func Evidence(r store.FreshnessReason) []string {
	ids := make([]string, 0, len(r.RecordIDs)+len(r.EventIDs))
	ids = append(ids, r.RecordIDs...)
	for _, id := range r.EventIDs {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	return ids
}

// Of returns h's status. workspaces is the caller's already-resolved
// workspace directory list, passed through to store.HandoffFreshness.
func Of(st *store.Store, h store.Record, workspaces []string) (Status, error) {
	doneAt, done, err := st.HandoffDone(h.ID)
	if err != nil {
		return Status{}, fmt.Errorf("handoff: done %s: %w", h.ID, err)
	}
	if done {
		return Status{Done: true, DoneAt: doneAt}, nil
	}
	reasons, err := st.HandoffFreshness(h, workspaces)
	if err != nil {
		return Status{}, fmt.Errorf("handoff: freshness %s: %w", h.ID, err)
	}
	if reasons, err = dropClearedFailures(st, h, workspaces, reasons); err != nil {
		return Status{}, fmt.Errorf("handoff: cleared failures %s: %w", h.ID, err)
	}
	return Status{Reasons: reasons}, nil
}
