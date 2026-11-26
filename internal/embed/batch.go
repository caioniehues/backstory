package embed

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// Dim is the vector length EmbeddingGemma 2 gives (no Matryoshka
	// truncation, #20).
	Dim = 768

	// ContextTokens is the model's context (#19). Each document must fit
	// in one batch of it, so the runtime runs with -b and -ub at this size:
	// at llama-embedding's default 2048 a long Record fails the whole batch.
	ContextTokens = 8192

	// MaxDocumentBytes is the byte budget a document is first cut to,
	// keeping its head: store.MaxRecordTextBytes (16 KB), so a Record's text
	// fits whole. It stays under ContextTokens at 2 or more bytes per token,
	// which prose and code take; only denser text (long hex, say) can
	// overflow it, and that gets SafeDocumentBytes instead.
	MaxDocumentBytes = 16 * 1024

	// SafeDocumentBytes is the budget a lone document that overflowed the
	// context is cut to for its one retry. Every token covers at least one
	// byte, so this much text, DocumentPrefix and the two special tokens
	// stay under ContextTokens whatever the text.
	SafeDocumentBytes = ContextTokens - 64

	// separatorDraws is how many random separators a batch tries before
	// refusing its text.
	separatorDraws = 4
)

// ErrNoGPUOffload is a run whose log shows no GPU device in use.
var ErrNoGPUOffload = errors.New("embed: the run shows no GPU offload")

// ErrContextOverflow is a run the runtime refused because a document has
// more tokens than fit in one batch of ContextTokens.
var ErrContextOverflow = errors.New("embed: a document overflows the context")

// overflowLine is the runtime's error for a document over its batch
// (llama-embedding: "number of tokens in input line (N) exceeds batch size
// (8192)").
var overflowLine = regexp.MustCompile(`number of tokens in input line \(\d+\) exceeds batch size`)

// Batch is the batch embedder: one llama-embedding run per batch, with the
// documents on stdin. It holds the device resolved when it was made, and
// every run checks from the runtime's log that it used that device.
type Batch struct {
	setup     Setup
	device    Device
	separator func() (string, error)
}

// CheckFiles refuses, with the reason, a runtime that isn't an executable
// file or a model that isn't a file.
func CheckFiles(s Setup) error {
	if fi, err := os.Stat(s.Runtime); err != nil {
		return fmt.Errorf("embed: runtime %s: %w", s.Runtime, err)
	} else if fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("embed: runtime %s is not an executable file", s.Runtime)
	}
	if fi, err := os.Stat(s.Model); err != nil {
		return fmt.Errorf("embed: model %s: %w", s.Model, err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("embed: model %s is not a file", s.Model)
	}
	return nil
}

// NewBatch checks s's files (CheckFiles) and resolves its device. It
// refuses, with the reason, when a file is missing or the device doesn't
// resolve.
func NewBatch(ctx context.Context, s Setup) (*Batch, error) {
	if err := CheckFiles(s); err != nil {
		return nil, err
	}
	dev, err := ResolveDevice(ctx, s)
	if err != nil {
		return nil, err
	}
	return &Batch{setup: s, device: dev, separator: randomSeparator}, nil
}

// Device is the device b runs on.
func (b *Batch) Device() Device { return b.device }

// Embed returns one Dim-long vector per document, in order. Each document
// gets DocumentPrefix and is cut to MaxDocumentBytes from its head. A lone
// document the runtime refuses as over the context (ErrContextOverflow) is
// run once more cut to SafeDocumentBytes, so its head is embedded rather
// than the Record dropped; in a batch of several the error is returned, and
// the drain retries each alone. The text goes to the runtime only over
// stdin: never in argv, never in a file.
func (b *Batch) Embed(ctx context.Context, docs []string) ([][]float32, error) {
	vecs, err := b.run(ctx, docs, MaxDocumentBytes)
	if len(docs) == 1 && errors.Is(err, ErrContextOverflow) {
		return b.run(ctx, docs, SafeDocumentBytes)
	}
	return vecs, err
}

// run is one runtime run over docs, each cut to budget bytes.
func (b *Batch) run(ctx context.Context, docs []string, budget int) ([][]float32, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = truncateHead(d, budget)
	}
	sep, err := batchSeparator(texts, b.separator)
	if err != nil {
		return nil, err
	}
	var stdin strings.Builder
	for i, t := range texts {
		if i > 0 {
			stdin.WriteString(sep)
		}
		stdin.WriteString(DocumentPrefix)
		stdin.WriteString(t)
	}

	ctxTokens := strconv.Itoa(ContextTokens)
	args := append(runtimeArgs(b.setup.Model, b.device), "-b", ctxTokens, "-ub", ctxTokens, "-lv", logVerbosity,
		"--embd-output-format", "json", "--embd-separator", sep, "-f", "/dev/stdin")
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, b.setup.Runtime, args...) //nolint:gosec // runtime and model are the Human's chosen files; no Record text in args
	cmd.Stdin = strings.NewReader(stdin.String())
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == nil && overflowLine.Match(stderr.Bytes()) {
			err = errors.Join(ErrContextOverflow, err)
		}
		return nil, fmt.Errorf("embed: %s: %w%s", b.setup.Runtime, err, errorLines(stderr.Bytes()))
	}
	if err := b.checkDevice(stderr.Bytes()); err != nil {
		return nil, err
	}
	return parseVectors(stdout.Bytes(), len(docs))
}

// runtimeArgs starts every llama.cpp command line the batch embedder and
// the query server run: the model, the resolved device, every layer on it
// unless it is the CPU, and -fit off.
func runtimeArgs(model string, dev Device) []string {
	args := []string{"-m", model, "-dev", dev.Arg}
	if dev.Backend != cpuDevice.Backend {
		args = append(args, "-ngl", "all")
	}
	return append(args, "-fit", "off")
}

// checkDevice checks from the run's log that it used b's device.
func (b *Batch) checkDevice(stderr []byte) error {
	if b.device.Backend == cpuDevice.Backend {
		return nil
	}
	used := usedDevices(stderr)
	if len(used) == 0 {
		return fmt.Errorf("%w on %s (%s)", ErrNoGPUOffload, b.device.Arg, b.device.Name)
	}
	for _, u := range used {
		if u.Arg != b.device.Arg || (b.device.PCI != "" && u.PCI != b.device.PCI) {
			return fmt.Errorf("embed: the run used %s (%s) (%s), not the matched %s", u.Arg, u.Name, u.PCI, b.device.Arg)
		}
	}
	return nil
}

// parseVectors reads --embd-output-format json: n vectors, in index order,
// each Dim long.
func parseVectors(stdout []byte, n int) ([][]float32, error) {
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("embed: reading the runtime's output: %w", err)
	}
	if len(out.Data) != n {
		return nil, fmt.Errorf("embed: the runtime gave %d vectors for %d documents", len(out.Data), n)
	}
	vecs := make([][]float32, n)
	for i, d := range out.Data {
		if d.Index != i {
			return nil, fmt.Errorf("embed: vector %d has index %d", i, d.Index)
		}
		if len(d.Embedding) != Dim {
			return nil, fmt.Errorf("embed: the runtime gave %d-dimensional vectors, want %d", len(d.Embedding), Dim)
		}
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// truncateHead is s cut to at most max bytes, keeping the head and whole
// runes.
func truncateHead(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// batchSeparator draws a separator that no text contains. Record text has
// newlines, so the runtime's default "\n" would split one Record into many.
func batchSeparator(texts []string, draw func() (string, error)) (string, error) {
	for range separatorDraws {
		sep, err := draw()
		if err != nil {
			return "", err
		}
		clean := !strings.Contains(DocumentPrefix, sep)
		for _, t := range texts {
			if strings.Contains(t, sep) {
				clean = false
				break
			}
		}
		if clean {
			return sep, nil
		}
	}
	return "", errors.New("embed: every separator drawn occurs in the batch's text")
}

// randomSeparator is a fresh one-line separator of 128 random bits.
func randomSeparator() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("embed: drawing a separator: %w", err)
	}
	return "<backstory-" + hex.EncodeToString(b[:]) + ">", nil
}
