package embed_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RidgetopAi/backstory/internal/embed"
	"github.com/RidgetopAi/backstory/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "backstory.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// sha256 of "title: none | text: \ntask: search result | query: " is
// ecc4bd80…: the version is named by both prefixes, so changing either
// changes it, and this literal with it.
func TestTemplateVersionNamesBothPrefixes(t *testing.T) {
	if embed.TemplateVersion != "tpl-ecc4bd80" {
		t.Fatalf("TemplateVersion = %q: a prompt prefix changed; vectors made with the old one must not match", embed.TemplateVersion)
	}
}

func TestFingerprintIsModelTemplateBackendAndDim(t *testing.T) {
	sha := "2188ac1deca4b77dffefd603c2776a9d76d9d74ec01841392982ebb840b09135"
	got := embed.Fingerprint(sha, "vulkan", 768)
	if want := sha + "|tpl-ecc4bd80|vulkan|768"; got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
	if embed.Fingerprint(sha, "rocm", 768) == got {
		t.Fatal("a backend switch keeps the fingerprint")
	}
}

// The model's hash is cached in settings by path, size and mtime: the same
// file isn't hashed again, and a changed one is.
func TestModelHashIsCachedByPathSizeAndMtime(t *testing.T) {
	st := openStore(t)
	model := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(model, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	if err := os.Chtimes(model, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	const shaABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const shaXYZ = "3608bca1e44ea6c4d268eb6db02260269892c0b42b86bbf1e77a6fa16c3c9282"
	hash := func() string {
		t.Helper()
		h, err := embed.ModelHash(st, model)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if h := hash(); h != shaABC {
		t.Fatalf("first hash = %s, want sha256(abc)", h)
	}

	// Same size, same mtime: the cache answers, so the stale hash shows.
	if err := os.WriteFile(model, []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(model, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if h := hash(); h != shaABC {
		t.Fatalf("unchanged size and mtime rehashed: %s", h)
	}

	// A new mtime is a changed file.
	later := mtime.Add(time.Second)
	if err := os.Chtimes(model, later, later); err != nil {
		t.Fatal(err)
	}
	if h := hash(); h != shaXYZ {
		t.Fatalf("after mtime change: %s, want sha256(xyz)", h)
	}

	// A new size is a changed file.
	if err := os.WriteFile(model, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(model, later, later); err != nil {
		t.Fatal(err)
	}
	if h := hash(); h != "88d4266fd4e6338d13b845fcf289579d209c897823b9217da3e161936f031589" {
		t.Fatalf("after size change: %s, want sha256(abcd)", h)
	}

	// Another path with the same size and mtime is hashed, not taken from
	// the cache.
	other := filepath.Join(t.TempDir(), "other.gguf")
	if err := os.WriteFile(other, []byte("wxyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, later, later); err != nil {
		t.Fatal(err)
	}
	if h, err := embed.ModelHash(st, other); err != nil || strings.HasPrefix(h, "88d4266f") {
		t.Fatalf("other path: %s, %v; want its own hash", h, err)
	}
}

func TestSetupIsSavedAndTurnedOff(t *testing.T) {
	st := openStore(t)
	if _, on, err := embed.LoadSetup(st); err != nil || on {
		t.Fatalf("fresh store: on=%v err=%v; embeddings are off by default", on, err)
	}
	want := embed.Setup{Runtime: "/usr/bin/llama-embedding", Model: "/m/e.gguf", Device: "0000:03:00.0"}
	if err := embed.SaveSetup(st, want); err != nil {
		t.Fatal(err)
	}
	for key, v := range map[string]string{"embed.runtime": want.Runtime, "embed.model": want.Model, "embed.device": want.Device} {
		if got, ok, _ := st.GetSetting(key); !ok || got != v {
			t.Errorf("%s = %q (set %v), want %q", key, got, ok, v)
		}
	}
	got, on, err := embed.LoadSetup(st)
	if err != nil || !on || got != want {
		t.Fatalf("load = %+v on=%v err=%v", got, on, err)
	}
	if err := embed.TurnOff(st); err != nil {
		t.Fatal(err)
	}
	if _, on, err := embed.LoadSetup(st); err != nil || on {
		t.Fatalf("after TurnOff: on=%v err=%v", on, err)
	}
}
