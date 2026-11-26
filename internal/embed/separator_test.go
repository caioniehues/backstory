package embed

import (
	"strings"
	"testing"
)

// The separator is drawn at random, so only a fixed draw can collide: a
// batch whose text contains every separator drawn is refused, never split.
func TestBatchSeparatorNeverOccursInTheText(t *testing.T) {
	draws := 0
	fixed := func() (string, error) { draws++; return "<SEP>", nil }
	_, err := batchSeparator([]string{"clean", "has <SEP> inside"}, fixed)
	if err == nil || !strings.Contains(err.Error(), "separator") {
		t.Fatalf("err = %v, want a separator refusal", err)
	}
	if draws < 2 {
		t.Fatalf("drew %d separators, want a retry before refusing", draws)
	}

	seq := []string{"<A>", "<B>"}
	next := func() (string, error) { s := seq[0]; seq = seq[1:]; return s, nil }
	sep, err := batchSeparator([]string{"mentions <A>", "clean"}, next)
	if err != nil || sep != "<B>" {
		t.Fatalf("separator = %q, %v; want <B> after <A> collided", sep, err)
	}
}
