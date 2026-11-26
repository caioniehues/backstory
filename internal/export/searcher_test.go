package export

import (
	"context"
	"errors"
	"testing"

	"github.com/RidgetopAi/backstory/internal/search"
)

// failingSearcher is a Searcher whose every query fails, as a broken one
// would, and counts how often it was asked.
type failingSearcher struct{ calls int }

func (f *failingSearcher) Search(context.Context, search.Query) (search.Result, error) {
	f.calls++
	return search.Result{}, errors.New("searcher broken")
}

// Build takes the caller's Searcher (the CLI's asks the daemon for query
// embeddings, #43) and renders the same mirror with it: the mirror is a
// project anchor, which reads the ledger and never searches, so a missing
// daemon can't change or break an export.
func TestBuildWithTheCallersSearcherRendersTheSameMirror(t *testing.T) {
	st := newTestStore(t)
	buildExportFixtureStore(t, st)
	s := &failingSearcher{}

	got, err := Build(Params{Store: st, Searcher: s, ProjectKey: exportFixtureProject, Altitude: AltitudeSummary, Now: exportFixtureNow})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	compareGolden(t, "summary.golden", got)
	if s.calls != 0 {
		t.Errorf("Build searched %d times, want 0", s.calls)
	}
}
