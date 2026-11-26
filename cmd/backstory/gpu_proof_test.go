//go:build gpu

// The GPU proof (#44): the whole embedding path against the real llama.cpp +
// Vulkan runtime and EmbeddingGemma 2 Q8_0 as #19 pinned them, on this
// Device's RX 7800 XT. It needs the card, so it is hand-run and never part
// of `make check`:
//
//	make gpu-proof
//	go test -count=1 -v -tags backstorytest,gpu -run TestGPUProof ./cmd/backstory
//
// Everything runs on a throwaway store under throwaway HOME and XDG dirs,
// with its own daemon on its own socket: the Human's store, settings and
// running daemon are never touched. By default the store holds generated
// Records. BACKSTORY_GPU_STORE=PATH runs on a copy of the store at PATH
// instead (taken read-only with VACUUM INTO; PATH is never opened for
// writing), and that is how the cosine floor is measured on real Records.
//
// Knobs, all optional:
//
//	BACKSTORY_GPU_RUNTIME  llama-embedding (default /usr/bin/llama-embedding)
//	BACKSTORY_GPU_MODEL    the Q8_0 GGUF (default ~/models/embeddinggemma-2/embeddinggemma-2-Q8_0.gguf)
//	BACKSTORY_GPU_PCI      the card's PCI address (default 0000:03:00.0)
//	BACKSTORY_GPU_STORE    a store to copy instead of generating Records
//	BACKSTORY_GPU_OUT      a folder for the per-pair similarities (ids and
//	                       numbers only, never Record text); without it they
//	                       are not written
//
// The test's own output carries counts, timings and distributions only:
// no Record text and no Record ids.
package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/mcp"
	"github.com/RidgetopAi/backstory/internal/search"
	"github.com/RidgetopAi/backstory/internal/store"
)

// The pins from #19.
const (
	gpuPinnedModelSHA256 = "2188ac1deca4b77dffefd603c2776a9d76d9d74ec01841392982ebb840b09135"
	gpuPinnedBuild       = "build 12359, commit b1b770d90d"
	gpuDefaultPCI        = "0000:03:00.0"
	gpuCardName          = "AMD Radeon RX 7800 XT"
)

// gpuWarmQueries is how many warm queries are timed, after gpuWarmups.
const (
	gpuWarmups     = 5
	gpuWarmQueries = 50
)

// gpuMinCardVRAM is the least VRAM the warm llama-server must hold on the
// card (the Q8_0 weights alone are ~0.3 GB; #19 measured 235 MB at -np 1),
// and gpuMaxOtherMemory the most it may hold on any other device (opening
// the iGPU's node costs ~12 MB of GTT; the weights there would be ~0.3 GB).
const (
	gpuMinCardVRAM    = 128 << 20
	gpuMaxOtherMemory = 64 << 20
)

type gpuConfig struct {
	runtime, model, pci, storeCopy, out string
}

func gpuConfigFromEnv(t *testing.T) gpuConfig {
	t.Helper()
	home := realHome
	if u, err := user.Current(); err == nil && home == "" {
		home = u.HomeDir
	}
	c := gpuConfig{
		runtime:   envOr("BACKSTORY_GPU_RUNTIME", "/usr/bin/llama-embedding"),
		model:     envOr("BACKSTORY_GPU_MODEL", filepath.Join(home, "models", "embeddinggemma-2", "embeddinggemma-2-Q8_0.gguf")),
		pci:       envOr("BACKSTORY_GPU_PCI", gpuDefaultPCI),
		storeCopy: os.Getenv("BACKSTORY_GPU_STORE"),
		out:       os.Getenv("BACKSTORY_GPU_OUT"),
	}
	for _, p := range []string{c.runtime, c.model} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the GPU proof needs the real runtime and model: %v", err)
		}
	}
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// gpuEnv is a child env whose HOME and XDG dirs are all fresh temp dirs and
// whose transcript roots are empty, so nothing the daemon or a command
// does reaches the Human's own files.
func gpuEnv(t *testing.T) (env []string, dataDir, runtimeDir string) {
	t.Helper()
	root := t.TempDir()
	dir := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dataDir, runtimeDir = dir("data"), dir("run")
	ancestry, err := json.Marshal(noHarnessAncestry)
	if err != nil {
		t.Fatal(err)
	}
	env = testXDGEnv("XDG_DATA_HOME="+dataDir, "XDG_RUNTIME_DIR="+runtimeDir,
		"XDG_STATE_HOME="+dir("state"), "XDG_CONFIG_HOME="+dir("config"))
	env = envWithout(env, "HOME", "XDG_CACHE_HOME", "BACKSTORY_WORKSPACE_DIRS", "BACKSTORY_PROJECT_ROOTS",
		"BACKSTORY_CLAUDE_ROOT", "BACKSTORY_CODEX_ROOT", "BACKSTORY_PI_ROOT", "HERMES_HOME")
	env = append(env, "HOME="+dir("home"), "XDG_CACHE_HOME="+dir("cache"),
		"BACKSTORY_CLAUDE_ROOT="+dir("claude"), "BACKSTORY_CODEX_ROOT="+dir("codex"),
		"BACKSTORY_PI_ROOT="+dir("pi"), "HERMES_HOME="+dir("hermes"),
		fakeAncestryEnvVar+"="+string(ancestry))
	return env, dataDir, runtimeDir
}

// runGPUCLI runs `backstory args...` with env and returns its output and
// how long it took.
func runGPUCLI(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int, took time.Duration) {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // bin is the binary this test just built
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	took = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("backstory %s: %v", strings.Join(args, " "), err)
	}
	return out.String(), errb.String(), code, took
}

// TestGPUProof is #44's acceptance criteria, in order.
func TestGPUProof(t *testing.T) {
	cfg := gpuConfigFromEnv(t)
	logLoad(t, "at start")
	checkGPUPins(t, cfg)
	bin := buildBackstory(t)
	env, dataDir, runtimeDir := gpuEnv(t)
	dbPath := filepath.Join(dataDir, "backstory", "backstory.db")

	realStore := cfg.storeCopy != ""
	if realStore {
		copyStoreReadOnly(t, cfg.storeCopy, dbPath)
		t.Logf("store: a copy of BACKSTORY_GPU_STORE (real Records)")
	} else {
		seedGPUStore(t, dbPath)
		t.Logf("store: generated Records (no BACKSTORY_GPU_STORE given)")
	}
	eligible := gpuEligible(t, dbPath)
	t.Logf("store: %d eligible Records (decision, outcome, handoff, note; not Tombstoned)", eligible)
	if eligible == 0 {
		t.Fatal("the store has no eligible Records; the proof would prove nothing")
	}

	// 1. embed --on, matched by the card's PCI address.
	stdout, stderr, code, took := runGPUCLI(t, bin, env, "embed", "--on",
		"--runtime", cfg.runtime, "--model", cfg.model, "--device", cfg.pci)
	if code != 0 {
		t.Fatalf("CRITERION embed --on: FAIL (exit %d)\n%s%s", code, stdout, stderr)
	}
	fp := lineValue(stdout, "fingerprint: ")
	onLine := strings.SplitN(stdout, "\n", 2)[0]
	switch {
	case !strings.HasPrefix(onLine, "embeddings on: "+gpuCardName):
		t.Errorf("CRITERION embed --on: FAIL: %q does not name the %s", onLine, gpuCardName)
	case fp != embed.Fingerprint(gpuPinnedModelSHA256, "vulkan", embed.Dim):
		t.Errorf("CRITERION embed --on: FAIL: fingerprint %q, want the pinned model on vulkan at %d", fp, embed.Dim)
	default:
		t.Logf("CRITERION embed --on: PASS in %v: %q, fingerprint %s", took.Round(time.Millisecond), onLine, fp)
	}

	// 2. The resolved device is the card, not the iGPU.
	checkResolvedDevice(t, cfg, dbPath)

	// 3. A real batch through --catch-up.
	stdout, stderr, code, took = runGPUCLI(t, bin, env, "embed", "--catch-up")
	last := lastLine(stdout)
	want := fmt.Sprintf("caught up: %d of %d embedded, 0 skipped", eligible, eligible)
	if code != 0 || last != want {
		t.Errorf("CRITERION catch-up: FAIL (exit %d): last line %q, want %q\n%s%s", code, last, want, stdout, stderr)
	} else {
		t.Logf("CRITERION catch-up: PASS: %d Records in %v (%.0f Records/s, %d progress lines, CLI start and device probe included)",
			eligible, took.Round(time.Millisecond), float64(eligible)/took.Seconds(), strings.Count(stdout, "\n")-1)
	}

	// 4. The daemon, with its warm query server.
	daemon, sock, log := startGPUDaemon(t, bin, env, runtimeDir)
	client := mcp.DaemonEmbedder{SocketPath: sock}
	ready := time.Now()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(50 * time.Millisecond) {
		if _, _, err := client.EmbedQuery(context.Background(), "is the query server warm yet"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the query server never answered: %v\ndaemon log:\n%s", err, log())
		}
	}
	t.Logf("query server: answering %v after the daemon started (device probe and model load included)", time.Since(ready).Round(time.Millisecond))

	// 5. status: on, the fingerprint, embedded == eligible.
	stdout, stderr, code, _ = runGPUCLI(t, bin, env, "status")
	line := linePrefixed(stdout, "embeddings: ")
	want = fmt.Sprintf("embeddings: on (%s), %d embedded, 0 queued, 0 skipped", embed.ShortFingerprint(fp), eligible)
	if code != 0 || line != want {
		t.Errorf("CRITERION status: FAIL (exit %d): %q, want %q\n%s%s", code, line, want, stdout, stderr)
	} else {
		t.Logf("CRITERION status: PASS: %q", line)
	}

	// 6. Warm queries under the deadline, on the card.
	logLoad(t, "before the warm queries")
	checkWarmQueries(t, client, fp)
	checkServerOnCard(t, daemon.Process.Pid, cfg.pci)
	checkHybridSearch(t, dbPath, client)

	// 7. The cosine floor's distributions.
	measureCosineFloor(t, dbPath, fp, cfg.out, realStore)

	if err := daemon.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

// logLoad logs the load average: the timings below are only comparable
// with #19's on a quiet machine.
func logLoad(t *testing.T, when string) {
	t.Helper()
	load, _ := os.ReadFile("/proc/loadavg")
	fields := strings.Fields(string(load))
	if len(fields) > 3 {
		fields = fields[:3]
	}
	t.Logf("machine %s: load average %s on %d CPUs", when, strings.Join(fields, " "), runtime.NumCPU())
}

// checkGPUPins checks the runtime build and the model file against #19.
func checkGPUPins(t *testing.T, cfg gpuConfig) {
	t.Helper()
	out, err := exec.Command(cfg.runtime, "--version").CombinedOutput() //nolint:gosec // the Human's runtime
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", cfg.runtime, err, out)
	}
	if !strings.Contains(string(out), gpuPinnedBuild) {
		t.Errorf("PIN runtime: FAIL: %s --version says %q, want %s (#19)", cfg.runtime, strings.TrimSpace(string(out)), gpuPinnedBuild)
	} else {
		t.Logf("PIN runtime: %s (%s)", cfg.runtime, gpuPinnedBuild)
	}
	hash, err := embed.ModelHash(memSettings{}, cfg.model)
	if err != nil {
		t.Fatal(err)
	}
	if hash != gpuPinnedModelSHA256 {
		t.Fatalf("PIN model: FAIL: %s has sha256 %s, want %s (#19)", cfg.model, hash, gpuPinnedModelSHA256)
	}
	t.Logf("PIN model: %s, sha256 %s", cfg.model, hash)
}

// memSettings is a throwaway embed.Settings for hashing the model outside
// any store.
type memSettings map[string]string

func (m memSettings) GetSetting(k string) (string, bool, error) { v, ok := m[k]; return v, ok, nil }
func (m memSettings) SetSetting(k, v string) error              { m[k] = v; return nil }

// checkResolvedDevice resolves the stored setup as the drain and the query
// server do and checks it lands on the card, with the iGPU listed beside it
// (or the check would be empty).
func checkResolvedDevice(t *testing.T, cfg gpuConfig, dbPath string) {
	t.Helper()
	st, err := store.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	setup, on, err := embed.LoadSetup(st)
	if err != nil || !on {
		t.Fatalf("CRITERION device: FAIL: setup %+v on=%v: %v", setup, on, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	devs, err := embed.ListDevices(ctx, cfg.runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("devices listed:\n%s", embed.DeviceList(devs))
	dev, err := embed.ResolveDevice(ctx, setup)
	if err != nil {
		t.Fatalf("CRITERION device: FAIL: %v", err)
	}
	others := 0
	for _, d := range devs {
		if d.Backend == dev.Backend && d.Arg != dev.Arg {
			others++
		}
	}
	switch {
	case setup.Device != cfg.pci:
		t.Errorf("CRITERION device: FAIL: stored embed.device %q, want %q", setup.Device, cfg.pci)
	case dev.PCI != cfg.pci || dev.Backend != "vulkan" || !strings.HasPrefix(dev.Name, gpuCardName):
		t.Errorf("CRITERION device: FAIL: resolved %+v, want the %s at %s on vulkan", dev, gpuCardName, cfg.pci)
	case others == 0:
		t.Errorf("CRITERION device: FAIL: no other %s device is listed, so not picking the iGPU proves nothing", dev.Backend)
	default:
		t.Logf("CRITERION device: PASS: embed.device %q resolves to %s, %s (%s), backend %s; %d other %s device(s) listed and not used",
			setup.Device, dev.Arg, dev.Name, dev.PCI, dev.Backend, others, dev.Backend)
	}
}

// startGPUDaemon starts the throwaway daemon and waits for its socket.
func startGPUDaemon(t *testing.T, bin string, env []string, runtimeDir string) (*exec.Cmd, string, func() string) {
	t.Helper()
	cmd := exec.Command(bin, "daemon") //nolint:gosec // bin is the binary this test just built
	cmd.Env = env
	var out safeBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	sock := filepath.Join(runtimeDir, "backstory", "sock")
	waitForFile(t, sock, 10*time.Second)
	return cmd, sock, out.String
}

// checkWarmQueries times warm embed_query round trips (CLI side to the
// daemon, the daemon to llama-server and back) against the 100 ms deadline.
func checkWarmQueries(t *testing.T, client mcp.DaemonEmbedder, fp string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < gpuWarmups; i++ {
		if _, _, err := client.EmbedQuery(ctx, fmt.Sprintf("warm-up query number %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	took := make([]time.Duration, 0, gpuWarmQueries)
	for i := 0; i < gpuWarmQueries; i++ {
		q := fmt.Sprintf("why did we pick option %d for the %s", i, gpuTopics[i%len(gpuTopics)].subject)
		start := time.Now()
		qfp, vec, err := client.EmbedQuery(ctx, q)
		d := time.Since(start)
		if err != nil {
			t.Fatalf("CRITERION warm query: FAIL: query %d: %v", i, err)
		}
		if qfp != fp || len(vec) != embed.Dim {
			t.Fatalf("CRITERION warm query: FAIL: fingerprint %q and %d dims, want %q and %d", qfp, len(vec), fp, embed.Dim)
		}
		if n := l2(vec); math.Abs(n-1) > 1e-3 {
			t.Errorf("warm query %d: L2 norm %.4f, want 1", i, n)
		}
		took = append(took, d)
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	p50, p95, worst := took[len(took)/2], took[(len(took)*95+99)/100-1], took[len(took)-1]
	summary := fmt.Sprintf("%d warm queries after %d warm-ups: min %v, p50 %v, p95 %v, max %v (deadline %v)",
		len(took), gpuWarmups, round(took[0]), round(p50), round(p95), round(worst), search.QueryEmbedDeadline)
	if worst >= search.QueryEmbedDeadline {
		t.Errorf("CRITERION warm query: FAIL: %s", summary)
	} else {
		t.Logf("CRITERION warm query: PASS: %s", summary)
	}
}

func round(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }

// checkServerOnCard reads the daemon's llama-server's DRM memory per device
// from /proc: the weights must sit on the card, and no other device may hold
// more than a node's opening cost.
func checkServerOnCard(t *testing.T, daemonPid int, pci string) {
	t.Helper()
	pid := childNamed(t, daemonPid, embed.ServerBinary)
	if pid == 0 {
		t.Errorf("CRITERION device (warm server): FAIL: no %s child of the daemon", embed.ServerBinary)
		return
	}
	mem := drmMemory(t, pid)
	var parts []string
	for _, dev := range sortedKeys(mem) {
		parts = append(parts, fmt.Sprintf("%s vram %d MiB gtt %d MiB", dev, mem[dev].vram>>20, mem[dev].gtt>>20))
	}
	bad := mem[pci].vram < gpuMinCardVRAM
	for dev, m := range mem {
		if dev != pci && m.vram+m.gtt > gpuMaxOtherMemory {
			bad = true
		}
	}
	if bad {
		t.Errorf("CRITERION device (warm server): FAIL: %s holds %s; want >= %d MiB VRAM on %s and < %d MiB on any other device",
			embed.ServerBinary, strings.Join(parts, "; "), gpuMinCardVRAM>>20, pci, gpuMaxOtherMemory>>20)
	} else {
		t.Logf("CRITERION device (warm server): PASS: %s (pid %d) holds %s", embed.ServerBinary, pid, strings.Join(parts, "; "))
	}
}

// childNamed is the pid of parent's child whose comm is name, or 0.
func childNamed(t *testing.T, parent int, name string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat") //nolint:gosec // a /proc path built from a pid
		if err != nil {
			continue
		}
		s := string(b)
		lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if lp < 0 || rp < lp {
			continue
		}
		fields := strings.Fields(s[rp+1:])
		if s[lp+1:rp] == name && len(fields) > 1 && fields[1] == strconv.Itoa(parent) {
			return pid
		}
	}
	return 0
}

type drmMem struct{ vram, gtt int64 }

var drmLine = regexp.MustCompile(`^drm-(pdev|resident-vram|resident-gtt):\s+(\S+)(?:\s+KiB)?$`)

// drmMemory is pid's resident VRAM and GTT per PCI device, from the DRM
// fdinfo of its open device nodes (each node counted once).
func drmMemory(t *testing.T, pid int) map[string]drmMem {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fdinfo", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]drmMem{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a /proc path
		if err != nil {
			continue
		}
		var pdev string
		var m drmMem
		for _, l := range strings.Split(string(b), "\n") {
			sm := drmLine.FindStringSubmatch(l)
			if sm == nil {
				continue
			}
			kib, _ := strconv.ParseInt(sm[2], 10, 64)
			switch sm[1] {
			case "pdev":
				pdev = sm[2]
			case "resident-vram":
				m.vram = kib << 10
			case "resident-gtt":
				m.gtt = kib << 10
			}
		}
		if pdev == "" {
			continue
		}
		// Several fds can share one DRM client; keep the largest view.
		if cur, ok := out[pdev]; !ok || m.vram+m.gtt > cur.vram+cur.gtt {
			out[pdev] = m
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkHybridSearch runs one text query through the Searcher the CLI uses,
// with the default floor and deadline, and wants no fallback reason.
func checkHybridSearch(t *testing.T, dbPath string, client mcp.DaemonEmbedder) {
	t.Helper()
	st, err := store.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	q := search.Query{Text: "which storage engine did we choose and why", Limit: 10}
	if keys := projectKeys(t, st); len(keys) > 0 {
		q.ProjectKey = keys[0]
	}
	start := time.Now()
	res, err := search.New(st, search.WithEmbedder(client)).Search(context.Background(), q)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	var vector int
	for _, h := range res.Hits {
		if h.Via != search.ViaText {
			vector++
		}
	}
	if res.Reason != "" {
		t.Errorf("hybrid search: FAIL: reason %q, want the vector side in full", res.Reason)
	} else {
		t.Logf("hybrid search: PASS: %d hits (%d from the vector side, floor %.2f) in %v, no fallback reason",
			len(res.Hits), vector, search.CosineFloor, round(took))
	}
}

// projectKeys is the store's Projects, most Records first.
func projectKeys(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT project_key FROM records WHERE project_key IS NOT NULL
		GROUP BY project_key ORDER BY COUNT(*) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	return keys
}

// copyStoreReadOnly takes a consistent copy of the store at src into dst:
// src is opened read-only and only ever read (VACUUM INTO), then the copy
// loses every Device-local setting that could reach outside the test
// (backups, sync Peers, an embedding setup) before anything opens it.
func copyStoreReadOnly(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	db, err := sql.Open("sqlite", "file:"+src+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		_ = db.Close()
		t.Fatalf("copy the store read-only: %v", err)
	}
	_ = db.Close()
	if _, err := store.VerifyBackup(dst); err != nil {
		t.Fatal(err)
	}
	cp, err := sql.Open("sqlite", "file:"+dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cp.Close() }()
	for _, stmt := range []string{
		`DELETE FROM settings WHERE key LIKE 'embed.%' OR key LIKE 'backup%'`,
		`DELETE FROM sync_peers`,
	} {
		if _, err := cp.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// gpuEligible counts the Records the drain must embed, by its own SQL
// rather than the store's: every non-Tombstoned decision, outcome, handoff
// or note.
func gpuEligible(t *testing.T, dbPath string) int {
	t.Helper()
	st, err := store.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM records
		WHERE kind IN ('decision','outcome','handoff','note') AND tombstoned_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// gpuTopic is one generated subject: a choice, the alternative it beat and
// why. Every generated Record is about one topic, so Records of a topic are
// related and Records of different topics are not.
type gpuTopic struct{ subject, choice, rejected, reason string }

var gpuTopics = []gpuTopic{
	{"build cache", "a content-addressed folder", "a shared network drive", "rebuilds must stay reproducible offline"},
	{"storage engine", "an embedded SQL database", "a document server", "one file is easier to back up"},
	{"release pipeline", "signed tags", "nightly snapshots", "users install only reviewed versions"},
	{"logging format", "one JSON object per line", "free-form text", "the log must be machine-readable"},
	{"config file", "TOML", "YAML", "indentation errors kept breaking deploys"},
	{"test fixtures", "golden files", "inline literals", "large expected outputs are easier to review as files"},
	{"error reporting", "wrapped errors with context", "error codes", "a message must say which step failed"},
	{"date handling", "UTC everywhere with local display", "local time in storage", "daylight saving shifted the ordering"},
	{"thumbnail images", "WebP at two sizes", "full-size PNGs", "the gallery page loaded too slowly"},
	{"keyboard shortcuts", "a single leader key", "chorded modifiers", "the chords clashed with the window manager"},
	{"database migrations", "forward-only numbered files", "an ORM's auto-migrate", "schema changes must be reviewable"},
	{"font rendering", "grayscale antialiasing", "subpixel rendering", "colour fringes showed on rotated screens"},
	{"backup schedule", "a daily snapshot kept for a week", "continuous replication", "a bad write would replicate instantly"},
	{"package manager", "the distro's packages", "a language-specific installer", "updates should come with the system"},
	{"audio pipeline", "a 48 kHz fixed rate", "per-device rates", "resampling glitches came from switching rates"},
	{"retry policy", "exponential backoff capped at ten minutes", "a fixed one-second retry", "a dead service was flooded with requests"},
	{"code formatting", "the standard formatter on save", "a custom style guide", "style debates were wasting review time"},
	{"search index", "full-text search with ranking", "substring scans", "scans got slow past a few thousand rows"},
	{"window layout", "a tiling layout with one master pane", "floating windows", "windows kept overlapping the terminal"},
	{"power profile", "balanced by default with a performance toggle", "always performance", "the fans were loud while idle"},
	{"commit messages", "a package prefix and an issue number", "free-form subjects", "the history was hard to search"},
	{"secret storage", "the system keyring", "a dotfile with tokens", "a dotfile leaked into a public repository"},
	{"plotting library", "a declarative chart grammar", "hand-drawn canvas code", "every new chart took a day"},
	{"dependency updates", "a weekly batch with a changelog read", "updating on every release", "surprise breakages hit on Fridays"},
}

// gpuTemplates phrase one topic's Decision several ways.
var gpuTemplates = []string{
	"We use %[2]s for the %[1]s instead of %[3]s, because %[4]s.",
	"Decided: the %[1]s is %[2]s. %[3]s was rejected since %[4]s.",
	"For the %[1]s we settled on %[2]s; %[3]s came up, but %[4]s.",
	"The %[1]s stays on %[2]s. Do not switch back to %[3]s: %[4]s.",
	"Chose %[2]s over %[3]s for the %[1]s. Reason: %[4]s.",
	"Keep %[2]s as the %[1]s. Moving to %[3]s was considered and dropped because %[4]s.",
	"%[2]s is now how we handle the %[1]s, replacing %[3]s, since %[4]s.",
	"After trying %[3]s for the %[1]s we went with %[2]s, because %[4]s.",
}

// gpuProject is the generated store's one Project.
const gpuProject = "gpu-proof-project"

// seedGPUStore fills a fresh store with generated Records: per topic, one
// Decision per template (each superseding the one before, all about the
// topic's path) and one Handoff. No Record text comes from anywhere real.
func seedGPUStore(t *testing.T, dbPath string) {
	t.Helper()
	st, err := store.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertProject(store.Project{Key: gpuProject, Toplevel: gpuProject, FirstSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Hour).UnixNano()
	n := 0
	insert := func(kind, text, about, next string) string {
		n++
		ts++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		var nextArg any
		if next != "" {
			nextArg = next
		}
		if _, err := st.DB().Exec(`INSERT INTO records
			(id, ts, kind, tier, text, about, session_id, project_key, evidence, outcome, promoter, expires_at, tombstoned_at, git_branch, next)
			VALUES (?, ?, ?, 'human-declared', ?, ?, NULL, ?, '[]', NULL, NULL, NULL, NULL, NULL, ?)`,
			id, ts, kind, text, about, gpuProject, nextArg); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, tp := range gpuTopics {
		about := fmt.Sprintf(`["internal/topic%02d/%s.go"]`, i, strings.ReplaceAll(tp.subject, " ", "_"))
		prev := ""
		for _, tpl := range gpuTemplates {
			id := insert("decision", fmt.Sprintf(tpl, tp.subject, tp.choice, tp.rejected, tp.reason), about, "")
			if prev != "" {
				if _, err := st.DB().Exec(`INSERT INTO edges (from_id, to_id, type, declared_by) VALUES (?, ?, 'supersedes', 'human')`, id, prev); err != nil {
					t.Fatal(err)
				}
			}
			prev = id
		}
		insert("handoff", fmt.Sprintf("Moved the %s to %s and left %s in place for one release.", tp.subject, tp.choice, tp.rejected),
			about, fmt.Sprintf("remove %s once nothing reads it", tp.rejected))
	}
}

// floorRecord is one embedded Record as the floor measurement sees it: no
// text, only what relates it to others.
type floorRecord struct {
	id, kind, project string
	about             []string
	vec               []float32
}

// floorPair is one compared pair.
type floorPair struct {
	a, b, why string
	sim       float64
}

// Floor-measurement sizes: the unrelated sample, and the least of each
// kind of pair worth summarising.
const (
	floorUnrelatedSample = 5000
	floorMinRelated      = 20
	floorMinUnrelated    = 100
)

// measureCosineFloor compares the document-document cosine similarity of
// related Records (a supersedes, possibly_supersedes or informs edge, or
// Decisions sharing an `about` path) against random pairs of Decisions with
// neither, and reports, for each candidate floor, how many related pairs it
// keeps and how many unrelated pairs it drops. On generated Records it is a
// check of the measurement only, and names no floor.
func measureCosineFloor(t *testing.T, dbPath, fp, outDir string, realRecords bool) {
	t.Helper()
	label := "real Records"
	if !realRecords {
		label = "GENERATED Records, a harness check only: these numbers are not a floor"
	}
	st, err := store.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	recs := floorRecords(t, st, fp)
	byID := map[string]*floorRecord{}
	var decisions []*floorRecord
	for _, r := range recs {
		byID[r.id] = r
		if r.kind == "decision" {
			decisions = append(decisions, r)
		}
	}

	key := func(a, b string) string {
		if a > b {
			a, b = b, a
		}
		return a + " " + b
	}
	linked := map[string]bool{}
	var related []floorPair
	rows, err := st.DB().Query(`SELECT from_id, to_id, type FROM edges`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var from, to, typ string
		if err := rows.Scan(&from, &to, &typ); err != nil {
			t.Fatal(err)
		}
		linked[key(from, to)] = true
		a, b := byID[from], byID[to]
		if a == nil || b == nil || from == to {
			continue
		}
		switch typ {
		case "supersedes", "possibly_supersedes", "informs":
			related = append(related, floorPair{a: from, b: to, why: typ, sim: cosine(a.vec, b.vec)})
		}
	}
	_ = rows.Close()
	seen := map[string]bool{}
	for _, p := range related {
		seen[key(p.a, p.b)] = true
	}
	sharesAbout := func(a, b *floorRecord) bool {
		for _, x := range a.about {
			for _, y := range b.about {
				if x == y {
					return true
				}
			}
		}
		return false
	}
	for i, a := range decisions {
		for _, b := range decisions[i+1:] {
			if k := key(a.id, b.id); !seen[k] && sharesAbout(a, b) {
				seen[k] = true
				related = append(related, floorPair{a: a.id, b: b.id, why: "about", sim: cosine(a.vec, b.vec)})
			}
		}
	}

	var unrelated []floorPair
	if len(decisions) > 1 {
		rng := rand.New(rand.NewPCG(44, 44)) //nolint:gosec // a fixed-seed sample, so a rerun compares the same pairs
		tries := 0
		for len(unrelated) < floorUnrelatedSample && tries < 20*floorUnrelatedSample {
			tries++
			a, b := decisions[rng.IntN(len(decisions))], decisions[rng.IntN(len(decisions))]
			k := key(a.id, b.id)
			if a == b || linked[k] || seen[k] || sharesAbout(a, b) {
				continue
			}
			seen[k] = true
			why := "same-project"
			if a.project != b.project {
				why = "cross-project"
			}
			unrelated = append(unrelated, floorPair{a: a.id, b: b.id, why: why, sim: cosine(a.vec, b.vec)})
		}
	}

	t.Logf("COSINE FLOOR (%s): document-document cosine over %d embedded Records (%d Decisions)", label, len(recs), len(decisions))
	t.Logf("  related pairs: %d (%s)", len(related), countWhy(related))
	t.Logf("  unrelated Decision pairs: %d (%s), no edge and no shared about path, sampled with a fixed seed", len(unrelated), countWhy(unrelated))
	t.Logf("  related:   %s", quantiles(related))
	t.Logf("  unrelated: %s", quantiles(unrelated))
	if outDir != "" {
		writeFloorPairs(t, outDir, related, unrelated)
	}
	if len(related) < floorMinRelated || len(unrelated) < floorMinUnrelated {
		t.Logf("  TOO FEW PAIRS to set a floor (need %d related and %d unrelated): the floor is not measured", floorMinRelated, floorMinUnrelated)
		return
	}
	t.Logf("  floor  related-kept  unrelated-dropped")
	bestJ, bestFloor := -1.0, 0.0
	for f := 0.20; f <= 0.951; f += 0.05 {
		kept, dropped := share(related, func(s float64) bool { return s >= f }), share(unrelated, func(s float64) bool { return s < f })
		t.Logf("  %.2f   %5.1f%%        %5.1f%%", f, 100*kept, 100*dropped)
	}
	if !realRecords {
		return
	}
	for f := 0.0; f <= 1.0001; f += 0.01 {
		kept, dropped := share(related, func(s float64) bool { return s >= f }), share(unrelated, func(s float64) bool { return s < f })
		if j := kept + dropped - 1; j > bestJ {
			bestJ, bestFloor = j, f
		}
	}
	kept, dropped := share(related, func(s float64) bool { return s >= bestFloor }), share(unrelated, func(s float64) bool { return s < bestFloor })
	t.Logf("  best separation (Youden's J = kept + dropped - 1, 0.01 steps): floor %.2f keeps %.1f%% of related pairs and drops %.1f%% of unrelated (J %.3f)",
		bestFloor, 100*kept, 100*dropped, bestJ)
}

// floorRecords loads every embedded, non-Tombstoned Record of an embedded
// kind under fp.
func floorRecords(t *testing.T, st *store.Store, fp string) []*floorRecord {
	t.Helper()
	rows, err := st.DB().Query(`SELECT r.id, r.kind, COALESCE(r.project_key, ''), r.about, e.vec
		FROM records r JOIN record_embeddings e ON e.record_id = r.id
		WHERE e.model = ? AND e.dim = ? AND r.tombstoned_at IS NULL
		AND r.kind IN ('decision','outcome','handoff','note')`, fp, embed.Dim)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*floorRecord
	for rows.Next() {
		var r floorRecord
		var about string
		var blob []byte
		if err := rows.Scan(&r.id, &r.kind, &r.project, &about, &blob); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal([]byte(about), &r.about)
		r.vec = make([]float32, len(blob)/4)
		for i := range r.vec {
			r.vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:]))
		}
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func l2(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func countWhy(ps []floorPair) string {
	c := map[string]int{}
	for _, p := range ps {
		c[p.why]++
	}
	var parts []string
	for _, k := range sortedKeys(c) {
		parts = append(parts, fmt.Sprintf("%s %d", k, c[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// quantiles summarises ps's similarities.
func quantiles(ps []floorPair) string {
	if len(ps) == 0 {
		return "no pairs"
	}
	s := make([]float64, len(ps))
	var sum float64
	for i, p := range ps {
		s[i] = p.sim
		sum += p.sim
	}
	sort.Float64s(s)
	q := func(p float64) float64 { return s[int(math.Round(p*float64(len(s)-1)))] }
	return fmt.Sprintf("n %d, min %.3f, p5 %.3f, p10 %.3f, p25 %.3f, median %.3f, p75 %.3f, p90 %.3f, p95 %.3f, max %.3f, mean %.3f",
		len(s), s[0], q(0.05), q(0.10), q(0.25), q(0.5), q(0.75), q(0.90), q(0.95), s[len(s)-1], sum/float64(len(s)))
}

func share(ps []floorPair, ok func(float64) bool) float64 {
	if len(ps) == 0 {
		return 0
	}
	n := 0
	for _, p := range ps {
		if ok(p.sim) {
			n++
		}
	}
	return float64(n) / float64(len(ps))
}

// writeFloorPairs writes every compared pair (ids, reason, similarity) as
// TSV into outDir, 0600. No Record text.
func writeFloorPairs(t *testing.T, outDir string, related, unrelated []floorPair) {
	t.Helper()
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("set\twhy\tsimilarity\ta\tb\n")
	for _, set := range []struct {
		name  string
		pairs []floorPair
	}{{"related", related}, {"unrelated", unrelated}} {
		for _, p := range set.pairs {
			fmt.Fprintf(&b, "%s\t%s\t%.6f\t%s\t%s\n", set.name, p.why, p.sim, p.a, p.b)
		}
	}
	path := filepath.Join(outDir, "cosine-floor-pairs-"+time.Now().Format("20060102-150405")+".tsv")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("  per-pair similarities written to BACKSTORY_GPU_OUT (%d rows)", len(related)+len(unrelated))
}

// lineValue is the rest of the first line of s starting with prefix.
func lineValue(s, prefix string) string {
	return strings.TrimPrefix(linePrefixed(s, prefix), prefix)
}

// linePrefixed is the first line of s starting with prefix, or "".
func linePrefixed(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
