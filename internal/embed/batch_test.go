package embed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/embed"
)

// stubArgv makes the stub write its arguments to a file; the returned func
// reads them back, one per element.
func stubArgv(t *testing.T) func() []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argv")
	t.Setenv("STUB_ARGV", p)
	return func() []string {
		t.Helper()
		b, err := os.ReadFile(p) //nolint:gosec // the test's own temp file
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

func newBatch(t *testing.T, device string) *embed.Batch {
	t.Helper()
	b, err := embed.NewBatch(context.Background(), embed.Setup{Runtime: stubRuntime(t), Model: stubModel(t), Device: device})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// flagValue is the argument after name in argv, or "" when name is absent.
func flagValue(argv []string, name string) string {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// The stub's vectors are [input bytes, input lines, 0...].
func TestBatchSendsRecordTextOnStdinNeverInArgv(t *testing.T) {
	stubDevices(t, iGPU, dGPU)
	argv := stubArgv(t)
	b := newBatch(t, "0000:03:00.0")
	docs := []string{"we dropped the memo cache\n\nnext: wire the drain", "a second Record"}
	vecs, err := b.Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("%d vectors for 2 documents", len(vecs))
	}
	prefix := len("title: none | text: ")
	for i, want := range []struct{ bytes, lines float32 }{
		{float32(prefix + len(docs[0])), 3}, // the multi-line Record stays one input
		{float32(prefix + len(docs[1])), 1},
	} {
		if len(vecs[i]) != embed.Dim || vecs[i][0] != want.bytes || vecs[i][1] != want.lines {
			t.Errorf("vector %d = len %d %v..., want %d bytes, %v lines", i, len(vecs[i]), vecs[i][:2], int(want.bytes), want.lines)
		}
	}

	args := argv()
	joined := strings.Join(args, "\n")
	for _, secret := range []string{"memo cache", "second Record", "title: none"} {
		if strings.Contains(joined, secret) {
			t.Errorf("argv carries document text %q: %q", secret, args)
		}
	}
	for _, forbidden := range []string{"-p", "--prompt", "--log-disable"} {
		for _, a := range args {
			if a == forbidden {
				t.Errorf("argv has %s: %q", forbidden, args)
			}
		}
	}
	for flag, want := range map[string]string{
		"-f": "/dev/stdin", "--embd-output-format": "json", "-fit": "off", "-dev": "Vulkan1",
	} {
		if got := flagValue(args, flag); got != want {
			t.Errorf("%s = %q, want %q (argv %q)", flag, got, want, args)
		}
	}
}

func TestBatchSeparatorIsFreshPerBatch(t *testing.T) {
	stubDevices(t, dGPU)
	argv := stubArgv(t)
	b := newBatch(t, "AMD Radeon RX 7800 XT")
	if _, err := b.Embed(context.Background(), []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	first := flagValue(argv(), "--embd-separator")
	// A Record that happens to contain the last batch's separator is still
	// one input: the next batch draws another.
	vecs, err := b.Embed(context.Background(), []string{"one " + first + " two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	second := flagValue(argv(), "--embd-separator")
	if first == "" || first == second || strings.Contains(first, "\n") {
		t.Fatalf("separators %q then %q: want two different one-line separators", first, second)
	}
	if len(vecs) != 2 {
		t.Fatalf("%d vectors for 2 documents", len(vecs))
	}
}

func TestBatchTruncatesFromTheHead(t *testing.T) {
	stubDevices(t, dGPU)
	b := newBatch(t, "AMD Radeon RX 7800 XT")
	prefix := len("title: none | text: ")
	head := "line one\nline two\nline three\n" // 3 newlines in the head
	long := head + strings.Repeat("x", embed.MaxDocumentBytes) + "\n\n\n\n\n"
	// é is two bytes; an odd budget offset puts the cut inside one.
	runes := "a" + strings.Repeat("é", embed.MaxDocumentBytes)
	fits := strings.Repeat("y", 16*1024) // MaxRecordTextBytes fits whole
	vecs, err := b.Embed(context.Background(), []string{long, runes, fits})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := vecs[0][0], float32(prefix+embed.MaxDocumentBytes); got != want {
		t.Errorf("long document: %v bytes embedded, want %v", got, want)
	}
	if vecs[0][1] != 4 {
		t.Errorf("long document: %v lines embedded, want the head's 4", vecs[0][1])
	}
	if got := int(vecs[1][0]) - prefix; got > embed.MaxDocumentBytes || got < embed.MaxDocumentBytes-1 || (got-1)%2 != 0 {
		t.Errorf("multi-byte document cut to %d bytes, want whole runes within %d", got, embed.MaxDocumentBytes)
	}
	if got, want := vecs[2][0], float32(prefix+16*1024); got != want {
		t.Errorf("16 KB document: %v bytes embedded, want %v (whole)", got, want)
	}
}

func TestBatchRefusesAWrongDimension(t *testing.T) {
	stubDevices(t, dGPU)
	t.Setenv("STUB_DIM", "512")
	b := newBatch(t, "AMD Radeon RX 7800 XT")
	_, err := b.Embed(context.Background(), []string{"a Record"})
	if err == nil || !strings.Contains(err.Error(), "512") || !strings.Contains(err.Error(), "768") {
		t.Fatalf("err = %v, want a 512-vs-768 refusal", err)
	}
}

func TestBatchRefusesARunWithNoGPUOffload(t *testing.T) {
	stubDevices(t, dGPU)
	t.Setenv("STUB_CPU", "1")
	b := newBatch(t, "AMD Radeon RX 7800 XT")
	_, err := b.Embed(context.Background(), []string{"a Record"})
	if !errors.Is(err, embed.ErrNoGPUOffload) {
		t.Fatalf("err = %v, want ErrNoGPUOffload", err)
	}
}

func TestBatchOnTheCPU(t *testing.T) {
	stubDevices(t, dGPU)
	argv := stubArgv(t)
	b := newBatch(t, embed.DeviceCPU)
	if _, err := b.Embed(context.Background(), []string{"a Record"}); err != nil {
		t.Fatal(err)
	}
	if got := flagValue(argv(), "-dev"); got != "none" {
		t.Fatalf("-dev = %q, want none", got)
	}
	if b.Device().Backend != "cpu" {
		t.Fatalf("backend = %q", b.Device().Backend)
	}
}

// Text dense enough to overflow the context within the first budget (the
// stub's STUB_MAX_INPUT_BYTES) is retried, alone, cut to SafeDocumentBytes,
// so its head is embedded rather than the Record dropped. In a batch of
// several the run fails as the runtime does (the drain then retries each
// alone), and a lone document is cut harder only once.
func TestBatchRetriesALoneOverflowingDocumentCutHarder(t *testing.T) {
	stubDevices(t, dGPU)
	prefix := len(embed.DocumentPrefix)
	t.Setenv("STUB_MAX_INPUT_BYTES", "12000")
	b := newBatch(t, "AMD Radeon RX 7800 XT")
	dense := strings.Repeat("7f", 8*1024) // 16 KB that tokenizes densely

	vecs, err := b.Embed(context.Background(), []string{dense})
	if err != nil {
		t.Fatalf("a lone overflowing document: %v, want it embedded cut harder", err)
	}
	if got, want := vecs[0][0], float32(prefix+embed.SafeDocumentBytes); got != want {
		t.Errorf("embedded %v bytes, want %v (DocumentPrefix plus SafeDocumentBytes of the head)", got, want)
	}

	if _, err := b.Embed(context.Background(), []string{dense, "a short Record"}); !errors.Is(err, embed.ErrContextOverflow) {
		t.Errorf("a batch with an overflowing document: %v, want ErrContextOverflow", err)
	}

	t.Setenv("STUB_MAX_INPUT_BYTES", "100")
	if _, err := b.Embed(context.Background(), []string{dense}); !errors.Is(err, embed.ErrContextOverflow) {
		t.Errorf("a document that overflows even cut harder: %v, want ErrContextOverflow", err)
	}
}

// The harder cut can't overflow: every token covers at least one byte, so
// SafeDocumentBytes of text plus DocumentPrefix and the two special tokens
// is at most that many tokens, under ContextTokens. The first budget still
// takes a 16 KB Record (store.MaxRecordTextBytes) whole.
func TestDocumentBudgetsFitTheContext(t *testing.T) {
	if worst := embed.SafeDocumentBytes + len(embed.DocumentPrefix) + 2; worst > embed.ContextTokens {
		t.Errorf("SafeDocumentBytes %d: worst case %d tokens, over the %d-token context", embed.SafeDocumentBytes, worst, embed.ContextTokens)
	}
	if embed.MaxDocumentBytes < 16*1024 {
		t.Errorf("MaxDocumentBytes %d cuts a 16 KB Record", embed.MaxDocumentBytes)
	}
	if embed.MaxDocumentBytes > 2*embed.ContextTokens {
		t.Errorf("MaxDocumentBytes %d: over 2 bytes per token of the %d-token context", embed.MaxDocumentBytes, embed.ContextTokens)
	}
}

func TestBatchReportsTheRuntimesError(t *testing.T) {
	stubDevices(t, dGPU)
	t.Setenv("STUB_FAIL", "number of tokens in input line (9000) exceeds batch size (8192)")
	b := newBatch(t, "AMD Radeon RX 7800 XT")
	_, err := b.Embed(context.Background(), []string{"a Record"})
	if err == nil || !strings.Contains(err.Error(), "exceeds batch size") {
		t.Fatalf("err = %v, want the runtime's error line", err)
	}
}

func TestNewBatchRefusesAMissingRuntimeOrModel(t *testing.T) {
	stubDevices(t, dGPU)
	missing := filepath.Join(t.TempDir(), "nope")
	for _, s := range []embed.Setup{
		{Runtime: missing, Model: stubModel(t), Device: "AMD Radeon RX 7800 XT"},
		{Runtime: stubRuntime(t), Model: missing, Device: "AMD Radeon RX 7800 XT"},
		{Runtime: stubRuntime(t), Model: t.TempDir(), Device: "AMD Radeon RX 7800 XT"},
	} {
		_, err := embed.NewBatch(context.Background(), s)
		if err == nil {
			t.Errorf("%+v: no refusal", s)
		}
	}
	_, err := embed.NewBatch(context.Background(), embed.Setup{Runtime: stubRuntime(t), Model: stubModel(t), Device: "0000:09:00.0"})
	if err == nil || !strings.Contains(err.Error(), "no device matches") {
		t.Fatalf("unresolved device: err = %v", err)
	}
}
