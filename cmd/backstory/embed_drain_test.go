package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/drain"
	"github.com/RidgetopAi/backstory/internal/store"
)

// openEnvStore opens the embed env's store, closed at cleanup.
func openEnvStore(t *testing.T, e embedEnv) *store.Store {
	t.Helper()
	st, err := store.Open(e.dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// vectorFingerprints is every stored vector's fingerprint, by Record id.
func vectorFingerprints(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT record_id, model FROM record_embeddings`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, model string
		if err := rows.Scan(&id, &model); err != nil {
			t.Fatal(err)
		}
		out[id] = model
	}
	return out
}

// --catch-up drains the queue in the foreground through the real batch
// path (the stub runtime), with progress, and pausing Capture doesn't stop
// it.
func TestEmbedCatchUpDrainsTheQueueWithProgress(t *testing.T) {
	e := newEmbedEnv(t, embedDGPU)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if code, out, errb := e.on(t); code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	ids := []string{
		seedDeletableRecord(t, e.dbPath, "proj-cu", "first note to embed"),
		seedDeletableRecord(t, e.dbPath, "proj-cu", "second note to embed"),
	}
	if code, out, errb := runAutoCLI(t, "capture", "off"); code != 0 {
		t.Fatalf("capture off: %d %q %q", code, out, errb)
	}

	code, out, errb := runAutoCLI(t, "embed", "--catch-up")
	if code != 0 {
		t.Fatalf("--catch-up: %d %q %q", code, out, errb)
	}
	fps := vectorFingerprints(t, e.dbPath)
	for _, id := range ids {
		if !strings.HasSuffix(fps[id], "|vulkan|768") {
			t.Errorf("Record %s vector fingerprint = %q, want one ending |vulkan|768", id, fps[id])
		}
	}
	for _, w := range []string{"embedded", "0 queued", "caught up"} {
		if !strings.Contains(out, w) {
			t.Errorf("--catch-up output lacks %q: %q", w, out)
		}
	}
	if _, held, _ := openEnvStore(t, e).DrainLease(time.Now()); held {
		t.Error("--catch-up left the drain lease held")
	}
}

// With the daemon holding the lease, --catch-up prints the daemon's
// progress until the queue is empty and never runs the runtime itself.
func TestEmbedCatchUpFollowsTheDaemonsDrain(t *testing.T) {
	e := newEmbedEnv(t, embedDGPU)
	if code, out, errb := e.on(t); code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	seedDeletableRecord(t, e.dbPath, "proj-cu", "the daemon embeds this")
	seedDeletableRecord(t, e.dbPath, "proj-cu", "and this")
	const fp = "abcdef0123456789|tpl-x|vulkan|768"
	st := openEnvStore(t, e)
	queue, err := st.EmbedQueue(fp, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := len(queue)
	daemonPid := os.Getpid() + 1
	if _, ok, err := st.TakeDrainLease(daemonPid, time.Now(), time.Hour); !ok || err != nil {
		t.Fatal("could not take the daemon's lease")
	}
	if err := st.SetSetting(drain.ReportKey, `{"fingerprint":"`+fp+`"}`); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("STUB_ARGV", argv)
	defer func(d time.Duration) { catchUpPoll = d }(catchUpPoll)
	catchUpPoll = 10 * time.Millisecond

	type result struct {
		code     int
		out, err string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errb := runAutoCLI(t, "embed", "--catch-up")
		done <- result{code, out, errb}
	}()
	// Long enough for --catch-up to defer and print the daemon's 0 of n.
	time.Sleep(300 * time.Millisecond)
	for _, r := range queue {
		vec := make([]float32, 768)
		vec[0] = 1
		if ok, err := st.UpsertRecordEmbedding(r.RecordID, fp, vec); !ok || err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("--catch-up did not return once the daemon emptied the queue")
	}
	if r.code != 0 {
		t.Fatalf("--catch-up: %d %q %q", r.code, r.out, r.err)
	}
	all := fmt.Sprintf("%d of %d embedded", n, n)
	if !strings.Contains(r.out, "the daemon") || !strings.Contains(r.out, "is draining: 0 of "+strconv.Itoa(n)) ||
		!strings.Contains(r.out, all) || !strings.Contains(r.out, "caught up") {
		t.Errorf("--catch-up output = %q, want the daemon's progress up to %s", r.out, all)
	}
	if _, err := os.Stat(argv); err == nil {
		t.Error("--catch-up ran the runtime while the daemon held the lease")
	}
}

func TestEmbedCatchUpRefusesWhileOff(t *testing.T) {
	newEmbedEnv(t, embedDGPU)
	code, _, errb := runAutoCLI(t, "embed", "--catch-up")
	if code != 1 || !strings.Contains(errb, "embeddings are off") {
		t.Fatalf("--catch-up while off: %d %q", code, errb)
	}
}

// --drop deletes every vector; Records stay. While embeddings are on it
// says the daemon will embed them again.
func TestEmbedDropDeletesEveryVector(t *testing.T) {
	e := newEmbedEnv(t, embedDGPU)
	if code, out, errb := e.on(t); code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	a := seedDeletableRecord(t, e.dbPath, "proj-drop", "alpha")
	b := seedDeletableRecord(t, e.dbPath, "proj-drop", "beta")
	st := openEnvStore(t, e)
	for _, id := range []string{a, b} {
		if ok, err := st.UpsertRecordEmbedding(id, "fp", []float32{1, 0}); !ok || err != nil {
			t.Fatal(err)
		}
	}
	code, out, errb := runAutoCLI(t, "embed", "--drop")
	if code != 0 || !strings.Contains(out, "dropped 2 vectors") || !strings.Contains(out, "--off") {
		t.Fatalf("--drop: %d %q %q", code, out, errb)
	}
	if n := len(vectorFingerprints(t, e.dbPath)); n != 0 {
		t.Fatalf("%d vectors left after --drop", n)
	}
	if r, err := st.GetRecord(a); err != nil || r.Text != "alpha" {
		t.Fatalf("Record after --drop = %+v, %v", r, err)
	}
}

func TestEmbedNewFlagsCombineWithNothingElse(t *testing.T) {
	newEmbedEnv(t, embedDGPU)
	for _, args := range [][]string{
		{"embed", "--drop", "--off"},
		{"embed", "--catch-up", "--on", "--runtime", "/r", "--model", "/m"},
		{"embed", "--catch-up", "--drop"},
	} {
		if code, _, _ := runAutoCLI(t, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

func embeddingsLine(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	printEmbeddingsLine(&out)
	return out.String()
}

// status's embeddings line: off/on, the short fingerprint, embedded vs
// queued, skipped, the last failure and its time, and why the device
// didn't resolve.
func TestStatusEmbeddingsLine(t *testing.T) {
	e := newEmbedEnv(t, embedDGPU)
	if got := embeddingsLine(t); got != "embeddings: off\n" {
		t.Fatalf("off: %q", got)
	}
	if code, out, errb := e.on(t); code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	if got := embeddingsLine(t); !strings.Contains(got, "embeddings: on") || !strings.Contains(got, "not drained yet") {
		t.Fatalf("on, before any drain: %q", got)
	}

	const fp = "2188ac1d0123456789abcdef|tpl-ecc4bd80|vulkan|768"
	st := openEnvStore(t, e)
	seeded, err := st.EmbedCounts(fp)
	if err != nil {
		t.Fatal(err)
	}
	a := seedDeletableRecord(t, e.dbPath, "proj-st", "embedded one")
	seedDeletableRecord(t, e.dbPath, "proj-st", "queued one")
	seedDeletableRecord(t, e.dbPath, "proj-st", "queued two")
	if ok, err := st.UpsertRecordEmbedding(a, fp, []float32{1}); !ok || err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 25, 14, 3, 0, 0, time.Local)
	report := `{"fingerprint":"` + fp + `","skipped":1,"last_failure":"embed: llama-embedding: exit status 1: E main: GPU hang","last_failure_at":"` + at.Format(time.RFC3339) + `"}`
	if err := st.SetSetting(drain.ReportKey, report); err != nil {
		t.Fatal(err)
	}
	got := embeddingsLine(t)
	for _, w := range []string{"embeddings: on", "2188ac1d|tpl-ecc4bd80|vulkan|768", "1 embedded", fmt.Sprintf("%d queued", seeded.Queued+2), "1 skipped",
		"last failure 2026-11-25 14:03", "GPU hang"} {
		if !strings.Contains(got, w) {
			t.Errorf("line lacks %q: %q", w, got)
		}
	}
	if strings.Contains(got, "0123456789abcdef") || strings.Count(got, "\n") != 1 {
		t.Errorf("line = %q, want one line with the fingerprint shortened", got)
	}

	if err := st.SetSetting(drain.ReportKey, `{"setup_error":"embed: no device matches \"RX 9070\":\n  Vulkan0: AMD Radeon RX 7800 XT","last_failure_at":"`+at.Format(time.RFC3339)+`"}`); err != nil {
		t.Fatal(err)
	}
	got = embeddingsLine(t)
	if !strings.Contains(got, `not embedding: embed: no device matches "RX 9070"`) || !strings.Contains(got, "Vulkan0: AMD Radeon RX 7800 XT") || strings.Count(got, "\n") != 1 {
		t.Errorf("setup error line = %q", got)
	}
}
