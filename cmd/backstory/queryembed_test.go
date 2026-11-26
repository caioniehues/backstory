package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/drain"
	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/mcp"
	"github.com/RidgetopAi/backstory/internal/store"
)

// queryRecallProject is the Project the query-embedding tests seed one
// Decision in, so a text recall has an FTS hit to fall back on.
const queryRecallProject = "query-embed-proj"

// seedQueryStore creates the store under dataDir with one Decision that
// mentions "quokka" and, when runtime is set, embeddings on with it on the
// dGPU.
func seedQueryStore(t *testing.T, dataDir, runtime string) {
	t.Helper()
	st, err := store.Open(filepath.Join(dataDir, "backstory", "backstory.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertProject(store.Project{Key: queryRecallProject, Toplevel: queryRecallProject, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	mustInsertQueryRecord(t, st, "dddd4444-0000-4000-8000-000000000004", "We keep the quokka cache warm.")
	if runtime == "" {
		return
	}
	model := filepath.Join(t.TempDir(), "embeddinggemma-2-Q8_0.gguf")
	if err := os.WriteFile(model, []byte("GGUF stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := embed.SaveSetup(st, embed.Setup{Runtime: runtime, Model: model, Device: "AMD Radeon RX 7800 XT"}); err != nil {
		t.Fatal(err)
	}
}

func mustInsertQueryRecord(t *testing.T, st *store.Store, id, text string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO records
		(id, ts, kind, tier, text, about, session_id, project_key, evidence, outcome, promoter, expires_at, tombstoned_at, git_branch)
		VALUES (?, ?, 'decision', 'human-declared', ?, '[]', NULL, ?, '[]', NULL, NULL, NULL, NULL, NULL)`,
		id, time.Now().UnixNano(), text, queryRecallProject); err != nil {
		t.Fatal(err)
	}
}

// stubRuntimeDir builds the llama-server stub beside a link to the
// llama-embedding stub and returns the link: the daemon runs the
// llama-server next to the stored runtime. The stubs list one GPU.
func stubRuntimeDir(t *testing.T) (runtime string, env []string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, embed.ServerBinary), "../../internal/embed/testdata/llamaserver") //nolint:gosec // fixed source path, temp-dir output
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the llama-server stub: %v\n%s", err, out)
	}
	stub, err := filepath.Abs(embedStub)
	if err != nil {
		t.Fatal(err)
	}
	runtime = filepath.Join(dir, "llama-embedding")
	if err := os.Symlink(stub, runtime); err != nil {
		t.Fatal(err)
	}
	devices := filepath.Join(dir, "devices")
	if err := os.WriteFile(devices, []byte(embedDGPU+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime, []string{"STUB_DEVICES=" + devices, "STUB_SERVER_STARTS=" + filepath.Join(dir, "starts")}
}

// recallNotice runs `backstory recall --json` for query in-process
// against dataDir and runtimeDir and returns its notice and item ids.
func recallNotice(t *testing.T, dataDir, runtimeDir, query string) (string, []string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	stdout, stderr, code := runRecallCLI(t, dataDir, "--project", queryRecallProject, "--json", query)
	if code != 0 {
		t.Fatalf("recall: exit %d: %s", code, stderr)
	}
	var out struct {
		Notice string `json:"notice"`
		Items  []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("recall --json: %v: %s", err, stdout)
	}
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	return out.Notice, ids
}

// With embeddings on but no daemon running, CLI recall is FTS-only and
// says why.
func TestRecallFallsBackToFTSWhenNoDaemonRuns(t *testing.T) {
	dataDir := t.TempDir()
	seedQueryStore(t, dataDir, "/nonexistent/llama-embedding")
	notice, ids := recallNotice(t, dataDir, t.TempDir(), "quokka")
	if notice != "embeddings unavailable: daemon unreachable" {
		t.Errorf("notice = %q, want the daemon unreachable", notice)
	}
	if len(ids) != 1 || ids[0] != "dddd4444-0000-4000-8000-000000000004" {
		t.Errorf("items = %v, want the FTS hit", ids)
	}
}

// With embeddings off, CLI recall says so (and needs no daemon).
func TestRecallSaysEmbeddingsOffWhenOff(t *testing.T) {
	dataDir := t.TempDir()
	seedQueryStore(t, dataDir, "")
	if notice, ids := recallNotice(t, dataDir, t.TempDir(), "quokka"); notice != "embeddings off" || len(ids) != 1 {
		t.Errorf("notice = %q, items %v; want embeddings off and the FTS hit", notice, ids)
	}
}

// The daemon runs the warm query server while embeddings are on, answers
// embed_query from it within its deadline, CLI recall gets its vector side
// through the daemon, and stopping the daemon stops the server.
func TestDaemonAnswersEmbedQueryFromItsWarmServer(t *testing.T) {
	bin := buildBackstory(t)
	runtime, stubEnv := stubRuntimeDir(t)
	dataDir, runtimeDir := t.TempDir(), t.TempDir()
	seedQueryStore(t, dataDir, runtime)

	env := testXDGEnv("XDG_RUNTIME_DIR="+runtimeDir, "XDG_DATA_HOME="+dataDir)
	env = append(envWithout(env, "HOME", "BACKSTORY_WORKSPACE_DIRS"), "HOME="+t.TempDir())
	b, err := json.Marshal(noHarnessAncestry)
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, fakeAncestryEnvVar+"="+string(b), "STUB_SERVER_DELAY=2s", "STUB_SERVER_SLOW=too slow")
	env = append(env, stubEnv...)
	cmd := exec.Command(bin, "daemon") //nolint:gosec // bin is the binary this test just built
	cmd.Env = env
	var out safeBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	waitDone := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(waitDone) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waitDone
	})
	sock := filepath.Join(runtimeDir, "backstory", "sock")
	waitForFile(t, sock, 2*time.Second)

	client := mcp.DaemonEmbedder{SocketPath: sock}
	query := "why did we drop the cache"
	var vec []float32
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var err error
		if _, vec, err = client.EmbedQuery(context.Background(), query); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("embed_query never answered: %v\ndaemon log:\n%s", err, out.String())
		}
	}
	if want := len(embed.QueryPrefix + query); len(vec) != embed.Dim || vec[0] != float32(want) {
		t.Errorf("vector = len %d starting %v, want %d long starting %d", len(vec), vec[0], embed.Dim, want)
	}
	embedSock := filepath.Join(runtimeDir, "backstory", "embed.sock")
	if fi, err := os.Stat(embedSock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("query server socket: %v %v, want a 0600 socket under XDG_RUNTIME_DIR", fi, err)
	}

	start := time.Now()
	_, _, err = client.EmbedQuery(context.Background(), "too slow")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Errorf("slow query: %v after %v, want the daemon's %v deadline", err, time.Since(start), mcp.EmbedQueryDeadline)
	}

	// The drain (#42) embeds the Decision. The query server's fingerprint
	// must be byte-identical to the one the drain writes vectors under, or
	// the vector scan matches no rows.
	fp, err := client.Fingerprint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "backstory", "backstory.db")
	waitForDrained(t, dbPath, fp, out.String)

	// "marsupial" matches no word of the Decision, so only the vector side
	// can find it: from the CLI over embed_query, and in the daemon's own
	// Searcher in-process. No reason means nothing was missing.
	if notice, ids := recallNotice(t, dataDir, runtimeDir, "marsupial"); notice != "" || len(ids) != 1 || ids[0] != "dddd4444-0000-4000-8000-000000000004" {
		t.Errorf("CLI recall: notice %q, items %v; want the Decision found by its vector, no reason", notice, ids)
	}
	if msg, ids := daemonRecall(t, sock, "marsupial"); msg != "" || len(ids) != 1 {
		t.Errorf("the daemon's recall: message %q, items %v; want the Decision found by its vector", msg, ids)
	}

	starts, err := os.ReadFile(strings.TrimPrefix(stubEnv[1], "STUB_SERVER_STARTS=")) //nolint:gosec // the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(starts)))
	if err != nil {
		t.Fatalf("starts file %q: %v", starts, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
		if waitErr != nil {
			t.Errorf("daemon exit after SIGTERM: %v, want 0\n%s", waitErr, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon did not exit on SIGTERM\n%s", out.String())
	}
	waitForGone(t, pid)
	if _, err := os.Stat(embedSock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("query server socket after the daemon stopped: %v, want it removed", err)
	}
}

// daemonRecall asks the daemon's recall method (the agent path, with
// the daemon's own Searcher) for query in queryRecallProject.
func daemonRecall(t *testing.T, sock, query string) (string, []string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	params, err := json.Marshal(mcp.RecallParams{Query: query, Project: queryRecallProject})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(mcp.DaemonRequest{Method: "recall", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp mcp.DaemonResponse
	var res mcp.RecallResult
	if err := json.Unmarshal(reply, &resp); err != nil || resp.Error != nil {
		t.Fatalf("recall: %s (%v)", reply, err)
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range res.Items {
		ids = append(ids, it.ID)
	}
	return res.Message, ids
}

// waitForDrained waits until the drain has published fp as its fingerprint
// and embedded every queued Record under it.
func waitForDrained(t *testing.T, dbPath, fp string, log func() string) {
	t.Helper()
	st, err := store.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var (
		rep    drain.Report
		counts store.EmbedCounts
	)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if rep, _, err = drain.ReadReport(st); err != nil {
			t.Fatal(err)
		}
		if counts, err = st.EmbedCounts(fp); err != nil {
			t.Fatal(err)
		}
		if rep.Fingerprint == fp && counts.Embedded == 1 && counts.Queued == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the drain's fingerprint %q, counts %+v under the query server's %q; want them identical and the Decision embedded\ndaemon log:\n%s",
				rep.Fingerprint, counts, fp, log())
		}
	}
}

// waitForGone waits for pid to exit (it is the daemon's child, reparented
// once the daemon is gone).
func waitForGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && strings.Contains(string(b), ") Z ") { //nolint:gosec // a /proc path built from a pid
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("llama-server pid %d still runs after the daemon stopped", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
