package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/store"
)

// The stub llama-embedding internal/embed's tests use; see its header for
// the STUB_* knobs. No GPU, model or network.
const embedStub = "../../internal/embed/testdata/llama-embedding"

const (
	embedIGPU = "Vulkan0|AMD Ryzen 5 7600X 6-Core Processor (RADV RAPHAEL_MENDOCINO)|0000:13:00.0"
	embedDGPU = "Vulkan1|AMD Radeon RX 7800 XT (RADV NAVI32)|0000:03:00.0"
)

type embedEnv struct {
	dbPath, runtime, model string
}

// newEmbedEnv is a store plus the stub runtime listing devices, and a model
// file.
func newEmbedEnv(t *testing.T, devices ...string) embedEnv {
	t.Helper()
	_, dbPath := backupEnv(t)
	runtime, err := filepath.Abs(embedStub)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	model := filepath.Join(dir, "embeddinggemma-2-Q8_0.gguf")
	if err := os.WriteFile(model, []byte("GGUF stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_DEVICES", "")
	if len(devices) > 0 {
		p := filepath.Join(dir, "devices")
		if err := os.WriteFile(p, []byte(strings.Join(devices, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("STUB_DEVICES", p)
	}
	return embedEnv{dbPath: dbPath, runtime: runtime, model: model}
}

func (e embedEnv) on(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	return runAutoCLI(t, append([]string{"embed", "--on", "--runtime", e.runtime, "--model", e.model}, extra...)...)
}

func (e embedEnv) setup(t *testing.T) (embed.Setup, bool) {
	t.Helper()
	st, err := store.Open(e.dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, on, err := embed.LoadSetup(st)
	if err != nil {
		t.Fatal(err)
	}
	return s, on
}

func TestEmbedIsOffByDefault(t *testing.T) {
	newEmbedEnv(t, embedDGPU)
	code, out, errb := runAutoCLI(t, "embed")
	if code != 0 || !strings.Contains(out, "embeddings: off") {
		t.Fatalf("embed: %d %q %q", code, out, errb)
	}
}

func TestEmbedOnSavesTheSetupAfterATestEmbed(t *testing.T) {
	e := newEmbedEnv(t, embedIGPU, embedDGPU)
	code, out, errb := e.on(t, "--device", "0000:03:00.0")
	if code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	if !strings.Contains(out, "|vulkan|768") || !strings.Contains(out, "AMD Radeon RX 7800 XT") {
		t.Errorf("--on output lacks the device or fingerprint: %q", out)
	}
	s, on := e.setup(t)
	if want := (embed.Setup{Runtime: e.runtime, Model: e.model, Device: "0000:03:00.0"}); !on || s != want {
		t.Fatalf("saved %+v on=%v, want %+v", s, on, want)
	}
	if code, out, _ := runAutoCLI(t, "embed"); code != 0 || !strings.Contains(out, "embeddings: on") || !strings.Contains(out, "device: 0000:03:00.0") {
		t.Fatalf("embed after --on: %d %q", code, out)
	}
}

// With one GPU listed, --device may be left out; the device's name is saved,
// never its backend index.
func TestEmbedOnWithOneGPUSavesItsName(t *testing.T) {
	e := newEmbedEnv(t, embedDGPU)
	if code, out, errb := e.on(t); code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	if s, _ := e.setup(t); s.Device != "AMD Radeon RX 7800 XT (RADV NAVI32)" {
		t.Fatalf("saved device %q", s.Device)
	}
}

func TestEmbedOnRefusesAndSavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		devices []string
		args    func(e embedEnv) []string
		want    []string
	}{
		{name: "missing runtime", devices: []string{embedDGPU},
			args: func(e embedEnv) []string { return []string{"--runtime", e.runtime + "-nope", "--model", e.model} },
			want: []string{"runtime", "llama-embedding-nope"}},
		{name: "missing model", devices: []string{embedDGPU},
			args: func(e embedEnv) []string { return []string{"--runtime", e.runtime, "--model", e.model + ".nope"} },
			want: []string{"model", ".gguf.nope"}},
		{name: "wrong dim", devices: []string{embedDGPU}, env: map[string]string{"STUB_DIM": "1024"},
			want: []string{"1024", "768"}},
		{name: "cpu without --allow-cpu", devices: []string{embedDGPU}, env: map[string]string{"STUB_CPU": "1"},
			want: []string{"no GPU offload", "--allow-cpu"}},
		{name: "no GPU listed, no --allow-cpu",
			want: []string{"no GPU", "--allow-cpu"}},
		{name: "two GPUs, no --device", devices: []string{embedIGPU, embedDGPU},
			want: []string{"--device", "Vulkan0: AMD Ryzen 5 7600X", "Vulkan1: AMD Radeon RX 7800 XT"}},
		{name: "device doesn't resolve", devices: []string{embedIGPU, embedDGPU},
			args: func(e embedEnv) []string {
				return []string{"--runtime", e.runtime, "--model", e.model, "--device", "0000:09:00.0"}
			},
			want: []string{"no device matches", "Vulkan1: AMD Radeon RX 7800 XT"}},
		{name: "backend index", devices: []string{embedIGPU, embedDGPU},
			args: func(e embedEnv) []string {
				return []string{"--runtime", e.runtime, "--model", e.model, "--device", "Vulkan1"}
			},
			want: []string{"backend index"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEnv(t, tc.devices...)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			args := []string{"embed", "--on", "--runtime", e.runtime, "--model", e.model}
			if tc.args != nil {
				args = append([]string{"embed", "--on"}, tc.args(e)...)
			}
			code, out, errb := runAutoCLI(t, args...)
			if code == 0 {
				t.Fatalf("accepted: %q", out)
			}
			for _, w := range append(tc.want, "nothing saved") {
				if !strings.Contains(errb, w) {
					t.Errorf("stderr lacks %q: %q", w, errb)
				}
			}
			if s, on := e.setup(t); on || s != (embed.Setup{}) {
				t.Fatalf("saved %+v on=%v after a refusal", s, on)
			}
		})
	}
}

func TestEmbedOnAllowCPU(t *testing.T) {
	t.Run("silent CPU run", func(t *testing.T) {
		e := newEmbedEnv(t, embedDGPU)
		t.Setenv("STUB_CPU", "1")
		if code, out, errb := e.on(t, "--allow-cpu"); code != 0 || !strings.Contains(out, "|cpu|768") {
			t.Fatalf("--on --allow-cpu: %d %q %q", code, out, errb)
		}
		if s, on := e.setup(t); !on || s.Device != embed.DeviceCPU {
			t.Fatalf("saved %+v on=%v, want device cpu", s, on)
		}
	})
	t.Run("no GPU listed", func(t *testing.T) {
		e := newEmbedEnv(t)
		if code, out, errb := e.on(t, "--allow-cpu"); code != 0 {
			t.Fatalf("--on --allow-cpu: %d %q %q", code, out, errb)
		}
		if s, _ := e.setup(t); s.Device != embed.DeviceCPU {
			t.Fatalf("saved device %q, want cpu", s.Device)
		}
	})
}

func TestEmbedOffClearsTheSwitchAndKeepsVectors(t *testing.T) {
	e := newEmbedEnv(t, embedDGPU)
	if code, out, errb := e.on(t); code != 0 {
		t.Fatalf("--on: %d %q %q", code, out, errb)
	}
	id := seedDeletableRecord(t, e.dbPath, "proj-vec", "has a vector")
	st, err := store.Open(e.dbPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpsertRecordEmbedding(id, "fp", []float32{1, 0}); err != nil || !ok {
		t.Fatalf("upsert: %v %v", ok, err)
	}
	_ = st.Close()

	if code, out, errb := runAutoCLI(t, "embed", "--off"); code != 0 || !strings.Contains(out, "embeddings off") {
		t.Fatalf("--off: %d %q %q", code, out, errb)
	}
	if _, on := e.setup(t); on {
		t.Fatal("still on after --off")
	}
	db, err := sql.Open("sqlite", "file:"+e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM record_embeddings`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("vectors after --off: %d (%v), want 1 kept", n, err)
	}
}

func TestEmbedFlagErrors(t *testing.T) {
	newEmbedEnv(t, embedDGPU)
	for _, args := range [][]string{
		{"embed", "--on", "--off"},
		{"embed", "--on"},
		{"embed", "--off", "--model", "/m"},
		{"embed", "stray"},
	} {
		if code, _, _ := runAutoCLI(t, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}
