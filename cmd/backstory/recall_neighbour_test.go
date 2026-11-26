package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/graph"
	"github.com/RidgetopAi/backstory/internal/recall"
	"github.com/RidgetopAi/backstory/internal/store"
)

// A Neighbour's reached in recall --json is {"via":"neighbour","from",
// "similarity"} (#30's amendment), and a record anchor's notice says why
// Neighbours are missing.
func TestRecallJSONNeighbourReachedAndNotice(t *testing.T) {
	const from = "aaaa1111-0000-4000-8000-000000000001"
	result := recall.Result{Notice: "embeddings partial: 1 Record not embedded yet", Items: []recall.Item{{
		ID: "bbbb2222-0000-4000-8000-000000000002", Kind: store.KindNote, TS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Reached:     &graph.Reached{Via: graph.ViaNeighbour, From: from, Similarity: 0.82},
		ConfirmedBy: []store.Confirmation{},
	}}}
	var out bytes.Buffer
	if code := printRecallJSON(&out, "acme-widgets", recall.AltitudeSummary, result); code != 0 {
		t.Fatalf("printRecallJSON = %d", code)
	}
	var parsed struct {
		Notice string                       `json:"notice"`
		Items  []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out.String(), err)
	}
	if got, want := string(parsed.Items[0]["reached"]), `{"via":"neighbour","from":"`+from+`","similarity":0.82}`; got != want {
		t.Errorf("reached = %s, want %s", got, want)
	}
	if parsed.Notice != result.Notice {
		t.Errorf("notice = %q, want %q", parsed.Notice, result.Notice)
	}
}
