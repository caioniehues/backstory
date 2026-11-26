package store

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

// The hook runs after each Record this Store commits, and not for a write
// that fails or one through another handle on the same file.
func TestOnRecordWriteRunsAfterEachCommittedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backstory.db")
	s := mustOpen(t, path)
	other := mustOpen(t, path)
	var n atomic.Int32
	s.OnRecordWrite(func() { n.Add(1) })

	insertFiller(t, s, "one")
	if n.Load() != 1 {
		t.Fatalf("hook ran %d times after one write, want 1", n.Load())
	}
	if _, err := s.InsertRecordWithEdges(InsertRecordParams{Identity: Identity{Kind: IdentityAgent}, Kind: KindDecision, Text: "x"},
		[]EdgeSpec{{OtherID: "no-such-record", Type: EdgeSupersedes, DeclaredBy: "test"}}); err == nil {
		t.Fatal("an edge to an unknown Record was accepted")
	}
	insertFiller(t, other, "through another handle")
	if n.Load() != 1 {
		t.Fatalf("hook ran %d times, want still 1 (failed write, other handle)", n.Load())
	}

	s.OnRecordWrite(nil)
	insertFiller(t, s, "after clearing")
	if n.Load() != 1 {
		t.Fatal("a cleared hook still ran")
	}
}
