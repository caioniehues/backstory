package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// With embeddings on, the daemon embeds the queue in the background as soon
// as it starts, through the real batch path (the stub runtime), even with
// Capture paused, and publishes its status for `backstory status`.
func TestDaemonDrainsTheQueueWhileCaptureIsPaused(t *testing.T) {
	bin := buildBackstory(t)
	runtimeDir, dataDir := t.TempDir(), t.TempDir()
	devices := filepath.Join(t.TempDir(), "devices")
	if err := os.WriteFile(devices, []byte(embedDGPU+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub, err := filepath.Abs(embedStub)
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "gemma.gguf")
	if err := os.WriteFile(model, []byte("GGUF stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := testXDGEnv("XDG_RUNTIME_DIR="+runtimeDir, "XDG_DATA_HOME="+dataDir, "STUB_DEVICES="+devices)
	env = append(envWithout(env, "HOME", "BACKSTORY_WORKSPACE_DIRS"), "HOME="+t.TempDir())
	cli := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...) //nolint:gosec // bin is the binary this test just built
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("backstory %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	cli("embed", "--on", "--runtime", stub, "--model", model)
	cli("capture", "off")

	dbPath := filepath.Join(dataDir, "backstory", "backstory.db")
	ids := []string{
		seedDeletableRecord(t, dbPath, "proj-daemon", "a note the daemon embeds"),
		seedDeletableRecord(t, dbPath, "proj-daemon", "another one"),
	}

	cmd := exec.Command(bin, "daemon") //nolint:gosec // bin is the binary this test just built
	cmd.Env = env
	var out safeBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		fps := vectorFingerprints(t, dbPath)
		if strings.HasSuffix(fps[ids[0]], "|vulkan|768") && strings.HasSuffix(fps[ids[1]], "|vulkan|768") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not embed the queue; vectors %v\ndaemon output:\n%s", fps, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, code := runStatusSubprocess(t, bin, env)
	if code != 0 || !strings.Contains(status, "embeddings: on (") || !strings.Contains(status, "|vulkan|768), ") ||
		!strings.Contains(status, " 0 queued, 0 skipped") {
		t.Errorf("backstory status after the daemon drained: exit %d\n%s", code, status)
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		exited = true
		if err != nil {
			t.Fatalf("daemon exit after SIGTERM: %v\n%s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon did not exit after SIGTERM\n%s", out.String())
	}
}
