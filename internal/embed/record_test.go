package embed_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/search"
)

// The record embedder embeds a Record's text the way the drain does: as a
// document (DocumentPrefix), through the batch runtime on stdin, never in
// argv, under the fingerprint the drain writes. The query server is never
// started or asked.
func TestRecordEmbedderEmbedsTextAsADocumentOnStdin(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	argv := stubArgv(t)
	r := &embed.RecordEmbedder{Settings: e.settings}
	text := "we keep the quokka cache warm\nso recall stays fast"

	fp, vec, err := r.EmbedQuery(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if want := float32(len(embed.DocumentPrefix + text)); len(vec) != embed.Dim || vec[0] != want || vec[1] != 2 {
		t.Errorf("vector = len %d %v..., want %d long starting %v bytes, 2 lines", len(vec), vec[:2], embed.Dim, want)
	}
	want, err := embed.FingerprintFor(e.settings, e.setup.Model, embed.Device{Backend: "vulkan"})
	if err != nil {
		t.Fatal(err)
	}
	if fp != want {
		t.Errorf("fingerprint = %q, want the drain's %q", fp, want)
	}
	if got, err := r.Fingerprint(context.Background()); err != nil || got != want {
		t.Errorf("Fingerprint = %q, %v; want %q", got, err, want)
	}
	if joined := strings.Join(argv(), "\n"); strings.Contains(joined, "quokka") {
		t.Errorf("argv carries the Record's text: %q", joined)
	}
	if starts := e.lines("starts"); len(starts) != 0 {
		t.Errorf("a llama-server was started %d times; the record embedder runs the batch runtime only", len(starts))
	}
	if inputs := e.lines("inputs"); len(inputs) != 0 {
		t.Errorf("the query server was sent %q", inputs)
	}
}

// With embeddings off it embeds nothing and says so; switched on again it
// follows the new setup.
func TestRecordEmbedderFollowsEmbedOnAndOff(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	r := &embed.RecordEmbedder{Settings: e.settings}
	if _, _, err := r.EmbedQuery(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := embed.TurnOff(e.settings); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.EmbedQuery(context.Background(), "while off"); !errors.Is(err, search.ErrEmbeddingsOff) {
		t.Fatalf("EmbedQuery while off: %v, want search.ErrEmbeddingsOff", err)
	}
	if _, err := r.Fingerprint(context.Background()); !errors.Is(err, search.ErrEmbeddingsOff) {
		t.Fatalf("Fingerprint while off: %v, want search.ErrEmbeddingsOff", err)
	}

	cpu := e.setup
	cpu.Device = embed.DeviceCPU
	if err := embed.SaveSetup(e.settings, cpu); err != nil {
		t.Fatal(err)
	}
	fp, _, err := r.EmbedQuery(context.Background(), "on the CPU now")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp, "|cpu|") {
		t.Errorf("fingerprint after the setup changed = %q, want the CPU's", fp)
	}
}

// A device that doesn't resolve is an error with the reason, not a vector.
func TestRecordEmbedderRefusesADeviceThatDoesNotResolve(t *testing.T) {
	e := newServerEnv(t, "RX 9070")
	r := &embed.RecordEmbedder{Settings: e.settings}
	if _, _, err := r.EmbedQuery(context.Background(), "anything"); err == nil || errors.Is(err, search.ErrEmbeddingsOff) {
		t.Fatalf("EmbedQuery on an unresolved device: %v, want the device error", err)
	}
}
