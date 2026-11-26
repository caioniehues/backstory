package handoff

import (
	"fmt"
	"strings"

	"github.com/RidgetopAi/backstory/internal/store"
)

// failureCommandMaxRunes bounds the failed command a short label quotes: a
// long command line must not blow up a one-line marker or Needs You row.
const failureCommandMaxRunes = 60

// Label is the short rendering of s's Possibly stale reasons: one clause per
// reason, in reason order, joined by "; ", e.g. "2 later edits to files it
// names; cargo test failed (exit 101) after it". It cites no ids (Evidence
// does). The SessionStart block's marker, recall's status label and This
// Week's summary all show it; "" when s is not Possibly stale.
func (s Status) Label() string {
	clauses := make([]string, len(s.Reasons))
	for i, r := range s.Reasons {
		clauses[i] = labelClause(r)
	}
	return strings.Join(clauses, "; ")
}

func labelClause(r store.FreshnessReason) string {
	switch r.Kind {
	case store.FreshnessContradicted:
		return "contradicted by " + count(len(r.RecordIDs), "a later record", "later records")
	case store.FreshnessLaterRecord:
		return count(len(r.RecordIDs), "a later record shares", "later records share") + " a path it names"
	case store.FreshnessLaterActivity:
		return count(len(r.EventIDs), "a later edit to a file", "later edits to files") + " it names"
	case store.FreshnessLaterFailure:
		return failure(r.Failures, failureCommandMaxRunes, " after it")
	case store.FreshnessLaterUnrecordedWork:
		return fmt.Sprintf("%s edited %s and wrote no note",
			count(r.Sessions, "1 later session", "later sessions"), count(r.Files, "1 file", "files"))
	default:
		return string(r.Kind)
	}
}

// count is one when n is 1, else n followed by many.
func count(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// failure names the newest failure, its exit code and how many more there
// were: "make lint failed (exit 2)<after> (+1 more)". maxRunes > 0 cuts the
// command to that many runes and an ellipsis.
func failure(failures []store.FailureDetail, maxRunes int, after string) string {
	what := "a tool call"
	var f store.FailureDetail
	if len(failures) > 0 {
		f = failures[len(failures)-1]
	}
	if f.Command != "" {
		what = f.Command
	} else if f.Tool != "" {
		what = f.Tool
	}
	if r := []rune(what); maxRunes > 0 && len(r) > maxRunes {
		what = string(r[:maxRunes]) + "…"
	}
	out := what + " failed"
	if f.Exit != nil {
		out += fmt.Sprintf(" (exit %d)", *f.Exit)
	}
	out += after
	if len(failures) > 1 {
		out += fmt.Sprintf(" (+%d more)", len(failures)-1)
	}
	return out
}

// Line is the long rendering of one Possibly stale reason, for This Week's
// detailed reasons: nothing cut and no ids. A contradicted or later-record
// reason quotes the newest cited Record's first line ("contradicted by a
// later record: <line>"), and a later-record reason citing an Outcome quotes
// the newest Outcome instead ("outcome <short id>: <line>"): it is what makes
// the Handoff look finished. A failure names the whole command and its exit
// code. A reason citing several records or failures describes the newest
// and ends "(+N more)". It reads the cited Records from st.
func Line(st *store.Store, r store.FreshnessReason) (string, error) {
	switch r.Kind {
	case store.FreshnessContradicted, store.FreshnessLaterRecord:
		var newest, newestOutcome store.Record
		for _, id := range r.RecordIDs {
			rec, err := st.GetRecord(id)
			if err != nil {
				return "", fmt.Errorf("handoff: line %s: %w", r.Kind, err)
			}
			if rec.Kind == store.KindOutcome && (newestOutcome.ID == "" || !rec.TS.Before(newestOutcome.TS)) {
				newestOutcome = rec
			}
			if newest.ID == "" || !rec.TS.Before(newest.TS) {
				newest = rec
			}
		}
		text := firstLine(newest.Text)
		switch {
		case r.Kind == store.FreshnessContradicted:
			text = "contradicted by a later record: " + text
		case newestOutcome.ID != "":
			text = "outcome " + shortID(newestOutcome.ID) + ": " + firstLine(newestOutcome.Text)
		}
		if n := len(r.RecordIDs); n > 1 {
			text += fmt.Sprintf(" (+%d more)", n-1)
		}
		return text, nil
	case store.FreshnessLaterActivity:
		return count(len(r.EventIDs), "a later edit to a file", "later edits to files") + " the handoff names", nil
	case store.FreshnessLaterFailure:
		return failure(r.Failures, 0, ""), nil
	case store.FreshnessLaterUnrecordedWork:
		return fmt.Sprintf("%s edited %s and wrote no note",
			count(r.Sessions, "1 session", "sessions"), count(r.Files, "1 file", "files")), nil
	default:
		return string(r.Kind), nil
	}
}

func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}

// shortID is a Record id's first 8 characters, as This Week shows it.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
