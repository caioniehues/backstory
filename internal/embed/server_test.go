package embed_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/search"
)

// stubServerDir holds the built llama-server stub beside a link to the
// llama-embedding stub: the query server runs the llama-server next to the
// stored runtime.
var stubServerDir string

func TestMain(m *testing.M) {
	code, err := runWithStubServer(m)
	if err != nil {
		_, _ = os.Stderr.WriteString("embed tests: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(code)
}

func runWithStubServer(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "embed-stub-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(dir, embed.ServerBinary), "./testdata/llamaserver").CombinedOutput(); err != nil { //nolint:gosec // fixed source path, temp-dir output
		return 0, errors.New("go build the llama-server stub: " + err.Error() + "\n" + string(out))
	}
	stub, err := filepath.Abs(filepath.Join("testdata", "llama-embedding"))
	if err != nil {
		return 0, err
	}
	if err := os.Symlink(stub, filepath.Join(dir, "llama-embedding")); err != nil {
		return 0, err
	}
	stubServerDir = dir
	return m.Run(), nil
}

// serverEnv is one query server under test and the stub's record files.
type serverEnv struct {
	t        *testing.T
	settings *fakeSettings
	setup    embed.Setup
	socket   string
	files    string
}

// fakeSettings is a concurrency-safe Settings: the query server reads it
// from its own goroutine.
type fakeSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeSettings) GetSetting(key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[key]
	return v, ok, nil
}

func (f *fakeSettings) SetSetting(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = value
	return nil
}

// newServerEnv turns embeddings on for device (the dGPU unless given) with
// the stubs, and points the stub's record files into a temp dir.
func newServerEnv(t *testing.T, device string) *serverEnv {
	t.Helper()
	stubDevices(t, iGPU, dGPU)
	files := t.TempDir()
	for env, name := range map[string]string{"STUB_SERVER_ARGV": "argv", "STUB_SERVER_STARTS": "starts", "STUB_SERVER_INPUTS": "inputs"} {
		t.Setenv(env, filepath.Join(files, name))
	}
	e := &serverEnv{
		t:        t,
		settings: &fakeSettings{m: map[string]string{}},
		setup:    embed.Setup{Runtime: filepath.Join(stubServerDir, "llama-embedding"), Model: stubModel(t), Device: device},
		// A short dir: a unix socket path has a 108-byte limit.
		socket: filepath.Join(shortDir(t), "embed.sock"),
		files:  files,
	}
	if err := embed.SaveSetup(e.settings, e.setup); err != nil {
		t.Fatal(err)
	}
	return e
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "q")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (e *serverEnv) start(cfg embed.QueryServerConfig) *embed.QueryServer {
	e.t.Helper()
	cfg.Settings, cfg.Socket = e.settings, e.socket
	if cfg.Poll == 0 {
		cfg.Poll = 20 * time.Millisecond
	}
	if cfg.Backoff == nil {
		cfg.Backoff = &embed.Backoff{Base: 10 * time.Millisecond, Cap: 50 * time.Millisecond}
	}
	q := embed.StartQueryServer(cfg)
	e.t.Cleanup(q.Close)
	return q
}

// lines is one of the stub's record files, a line per element.
func (e *serverEnv) lines(name string) []string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.files, name)) //nolint:gosec // the test's own temp file
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// waitEmbed retries EmbedQuery until it answers or 5 s pass.
func waitEmbed(t *testing.T, q *embed.QueryServer, text string) (string, []float32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		fp, vec, err := q.EmbedQuery(ctx, text)
		cancel()
		if err == nil {
			return fp, vec
		}
		if time.Now().After(deadline) {
			t.Fatalf("EmbedQuery never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor polls cond until it holds or 5 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Only query text goes through the server, with the query prefix, and the
// vector comes back under the fingerprint the drain writes.
func TestQueryServerEmbedsQueryTextWithTheQueryPrefix(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	t.Setenv("STUB_SERVER_LOAD", "200ms")
	q := e.start(embed.QueryServerConfig{})
	query := "why did we drop the cache"
	// Until /health says the model is loaded, queries never reach it.
	var (
		fp  string
		vec []float32
		err error
	)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if fp, vec, err = q.EmbedQuery(context.Background(), query); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "the query server is starting") || time.Now().After(deadline) {
			t.Fatalf("while the model loads: err = %v, want the server starting", err)
		}
	}
	want := "task: search result | query: " + query
	if len(vec) != embed.Dim || vec[0] != float32(len(want)) {
		t.Fatalf("vector = len %d %v..., want %d long starting %d (the prefixed query's bytes)", len(vec), vec[:2], embed.Dim, len(want))
	}
	if got := e.lines("inputs"); len(got) != 1 || got[0] != strconv.Quote(want) {
		t.Errorf("the server got %q, want only %q", got, want)
	}
	hash, err := embed.ModelHash(e.settings, e.setup.Model)
	if err != nil {
		t.Fatal(err)
	}
	if wantFP := embed.Fingerprint(hash, "vulkan", embed.Dim); fp != wantFP {
		t.Errorf("fingerprint = %q, want %q", fp, wantFP)
	}
	if got, err := q.Fingerprint(context.Background()); err != nil || got != fp {
		t.Errorf("Fingerprint() = %q, %v; want %q", got, err, fp)
	}
}

// The server runs on the resolved device, on the private socket made 0600,
// with one slot, no prompt cache, no slots endpoint and logging off, and
// never with a slot save path.
func TestQueryServerRunsOnTheResolvedDeviceOnAPrivateSocket(t *testing.T) {
	e := newServerEnv(t, "0000:03:00.0")
	q := e.start(embed.QueryServerConfig{})
	waitEmbed(t, q, "anything")

	argv := e.lines("argv")
	for name, want := range map[string]string{
		"-dev": "Vulkan1", "--host": e.socket, "-np": "1", "--cache-ram": "0", "-m": e.setup.Model, "-ngl": "all",
	} {
		if got := flagValue(argv, name); got != want {
			t.Errorf("%s = %q, want %q (argv %q)", name, got, want, argv)
		}
	}
	for _, flag := range []string{"--embedding", "--log-disable", "--no-slots", "--no-webui"} {
		if !slices.Contains(argv, flag) {
			t.Errorf("argv lacks %s: %q", flag, argv)
		}
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "--slot-save-path") || strings.Contains(a, "anything") {
			t.Errorf("argv carries %q: %q", a, argv)
		}
	}
	fi, err := os.Stat(e.socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a 0600 socket", fi.Mode())
	}
}

// EmbedQuery honours its caller's deadline: a server that answers in time
// is used, one that doesn't is given up on at the deadline.
func TestQueryServerHonoursTheDeadline(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	t.Setenv("STUB_SERVER_DELAY", "2s")
	t.Setenv("STUB_SERVER_SLOW", "too slow")
	q := e.start(embed.QueryServerConfig{})
	waitEmbed(t, q, "warm up")

	ctx, cancel := context.WithTimeout(context.Background(), search.QueryEmbedDeadline)
	defer cancel()
	if _, vec, err := q.EmbedQuery(ctx, "in time"); err != nil || len(vec) != embed.Dim {
		t.Fatalf("under the %s deadline: %d-d vector, %v", search.QueryEmbedDeadline, len(vec), err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := q.EmbedQuery(ctx, "too slow")
	if took := time.Since(start); took > time.Second {
		t.Errorf("gave up after %v, want it cut at the 20ms deadline, not the 2s answer", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// A crashed server is restarted after the backoff, and queries in between
// fail with the reason instead of hanging.
func TestQueryServerRestartsACrashedServer(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	crash := filepath.Join(e.files, "crash")
	t.Setenv("STUB_SERVER_CRASH", crash)
	q := e.start(embed.QueryServerConfig{Backoff: &embed.Backoff{Base: 200 * time.Millisecond, Cap: time.Second}})
	waitEmbed(t, q, "before the crash")

	if err := os.WriteFile(crash, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.EmbedQuery(context.Background(), "the crashing query"); err == nil {
		t.Fatal("the query the server crashed on succeeded")
	}
	waitFor(t, "the crash to be noticed", func() bool {
		_, _, err := q.EmbedQuery(context.Background(), "during the backoff")
		return err != nil && strings.Contains(err.Error(), "the query server exited")
	})
	waitEmbed(t, q, "after the restart")
	if starts := e.lines("starts"); len(starts) != 2 {
		t.Errorf("the server started %d times, want 2", len(starts))
	}
}

// A server that can't start is retried on the backoff, not in a tight loop.
func TestQueryServerBacksOffAServerThatCannotStart(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	t.Setenv("STUB_SERVER_FAIL", "1")
	q := e.start(embed.QueryServerConfig{Backoff: &embed.Backoff{Base: 100 * time.Millisecond, Cap: 400 * time.Millisecond}})
	time.Sleep(time.Second)
	// Starts at about 0, 0.1, 0.3 and 0.7 s (1.1 s with slow runs): never a
	// tight loop.
	if n := len(e.lines("starts")); n < 3 || n > 5 {
		t.Errorf("%d starts in 1s with a 100ms..400ms backoff, want 3 to 5", n)
	}
	if _, _, err := q.EmbedQuery(context.Background(), "q"); err == nil || !strings.Contains(err.Error(), "exited while starting") {
		t.Errorf("err = %v, want the failed start", err)
	}
}

// A server that keeps crashing soon after it starts backs off further each
// time: only a run of StableRun or longer starts the backoff over.
func TestQueryServerBacksOffAServerThatKeepsCrashing(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	t.Setenv("STUB_SERVER_EXIT_AFTER", "50ms")
	e.start(embed.QueryServerConfig{Backoff: &embed.Backoff{Base: 100 * time.Millisecond, Cap: 400 * time.Millisecond}})
	time.Sleep(1200 * time.Millisecond)
	// Starts at about 0, 0.15, 0.4 and 0.85 s; backing off from Base after
	// every crash would start about 8 times.
	if n := len(e.lines("starts")); n < 3 || n > 5 {
		t.Errorf("%d starts in 1.2s of 50ms runs with a 100ms..400ms backoff, want 3 to 5", n)
	}
}

// Embeddings off: queries read search.ErrEmbeddingsOff and the running
// server is stopped. On again: it comes back.
func TestQueryServerFollowsEmbedOnAndOff(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	q := e.start(embed.QueryServerConfig{})
	waitEmbed(t, q, "on")
	pid := serverPID(t, e)

	if err := embed.TurnOff(e.settings); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "embeddings off", func() bool {
		_, _, err := q.EmbedQuery(context.Background(), "off")
		return errors.Is(err, search.ErrEmbeddingsOff)
	})
	waitFor(t, "the server to exit", func() bool { return !alive(pid) })
	if _, err := q.Fingerprint(context.Background()); !errors.Is(err, search.ErrEmbeddingsOff) {
		t.Errorf("Fingerprint err = %v, want ErrEmbeddingsOff", err)
	}

	if err := embed.SaveSetup(e.settings, e.setup); err != nil {
		t.Fatal(err)
	}
	waitEmbed(t, q, "on again")
}

// offStopsWithin is how soon `embed --off` must stop the warm server, with
// the default setup poll: the GPU is freed within a couple of seconds.
const offStopsWithin = 2 * time.Second

// With the default poll, `embed --off` stops the server's GPU work within
// a couple of seconds, not at the next slow poll.
func TestQueryServerStopsPromptlyOnEmbedOff(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	q := embed.StartQueryServer(embed.QueryServerConfig{Settings: e.settings, Socket: e.socket})
	t.Cleanup(q.Close)
	waitEmbed(t, q, "on")
	pid := serverPID(t, e)

	if err := embed.TurnOff(e.settings); err != nil {
		t.Fatal(err)
	}
	off := time.Now()
	for alive(pid) {
		if time.Since(off) > offStopsWithin {
			t.Fatalf("the server still runs %v after embed --off; want it stopped within %v", time.Since(off), offStopsWithin)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A changed setup restarts the server under it, with its fingerprint.
func TestQueryServerRestartsOnASetupChange(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	q := e.start(embed.QueryServerConfig{})
	if fp, _ := waitEmbed(t, q, "on the GPU"); !strings.HasSuffix(fp, "|vulkan|768") {
		t.Fatalf("fingerprint = %q, want the vulkan backend", fp)
	}
	e.setup.Device = embed.DeviceCPU
	if err := embed.SaveSetup(e.settings, e.setup); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the CPU fingerprint", func() bool {
		fp, err := q.Fingerprint(context.Background())
		return err == nil && strings.HasSuffix(fp, "|cpu|768")
	})
	if got := flagValue(e.lines("argv"), "-dev"); got != "none" {
		t.Errorf("-dev = %q after the change, want none", got)
	}
	if n := len(e.lines("starts")); n != 2 {
		t.Errorf("%d starts, want 2", n)
	}
}

// Embeddings start off: nothing runs, and queries say so.
func TestQueryServerStartsNothingWhileOff(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	if err := embed.TurnOff(e.settings); err != nil {
		t.Fatal(err)
	}
	q := e.start(embed.QueryServerConfig{})
	if _, _, err := q.EmbedQuery(context.Background(), "q"); !errors.Is(err, search.ErrEmbeddingsOff) {
		t.Errorf("err = %v, want ErrEmbeddingsOff", err)
	}
	time.Sleep(100 * time.Millisecond)
	if starts := e.lines("starts"); len(starts) != 0 {
		t.Errorf("the server started %d times while off", len(starts))
	}
}

// A device match that no longer resolves refuses to embed, with the
// reason, and never starts the server on another device.
func TestQueryServerRefusesADeviceThatDoesNotResolve(t *testing.T) {
	e := newServerEnv(t, "NVIDIA GeForce RTX 4090")
	q := e.start(embed.QueryServerConfig{})
	waitFor(t, "the refusal", func() bool {
		_, _, err := q.EmbedQuery(context.Background(), "q")
		return err != nil && strings.Contains(err.Error(), `no device matches "NVIDIA GeForce RTX 4090"`)
	})
	if starts := e.lines("starts"); len(starts) != 0 {
		t.Errorf("the server started %d times on an unresolved device", len(starts))
	}
}

// Close stops the server and removes its socket.
func TestQueryServerCloseStopsTheServer(t *testing.T) {
	e := newServerEnv(t, "AMD Radeon RX 7800 XT")
	q := e.start(embed.QueryServerConfig{})
	waitEmbed(t, q, "q")
	pid := serverPID(t, e)
	q.Close()
	if alive(pid) {
		t.Errorf("llama-server pid %d still runs after Close", pid)
	}
	if _, err := os.Stat(e.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket after Close: %v, want it removed", err)
	}
}

func serverPID(t *testing.T, e *serverEnv) int {
	t.Helper()
	starts := e.lines("starts")
	pid, err := strconv.Atoi(starts[len(starts)-1])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// alive reports whether pid runs and isn't a zombie.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // a /proc path built from a pid
	return err == nil && !strings.Contains(string(b), ") Z ")
}

func TestBackoffDoublesToItsCapAndResets(t *testing.T) {
	b := &embed.Backoff{Base: time.Second, Cap: 5 * time.Second}
	var got []time.Duration
	for range 5 {
		got = append(got, b.Next())
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	b.Reset()
	if d := b.Next(); d != time.Second {
		t.Errorf("after Reset: %v, want 1s", d)
	}
	// The defaults are the drain's (#42): 5 s first, doubling to 10 min.
	var zero embed.Backoff
	if d := zero.Next(); d != 5*time.Second {
		t.Errorf("the default first wait = %v, want 5s", d)
	}
	for range 20 {
		zero.Next()
	}
	if d := zero.Next(); d != 10*time.Minute {
		t.Errorf("the default cap = %v, want 10m (#20)", d)
	}
}
