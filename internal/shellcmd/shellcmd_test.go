package shellcmd_test

import (
	"testing"

	"github.com/RidgetopAi/backstory/internal/shellcmd"
)

func TestAnalyseFindsTheTestRunnerAndWhetherItsExitIsObserved(t *testing.T) {
	cases := []struct {
		cmd                     string
		head                    string
		isTest, observed, piped bool
	}{
		{cmd: "go test ./...", head: "go test ./...", isTest: true, observed: true},
		{cmd: "cd x && make check", head: "make check", isTest: true, observed: true},
		{cmd: "go test ./... | tail -5", head: "go test ./...", isTest: true, piped: true},
		{cmd: "set -o pipefail; go test ./... | tail -5", head: "go test ./...", isTest: true, observed: true},
		{cmd: "go test ./... ; echo done", head: "go test ./...", isTest: true},
		{cmd: "bash -lc 'pytest -q'", head: "pytest -q", isTest: true, observed: true},
		{cmd: "echo 'go test'", head: "echo 'go test'"},
		{cmd: "ls -la", head: "ls -la"},
	}
	for _, c := range cases {
		got := shellcmd.Analyse(c.cmd)
		if got.Head != c.head || got.IsTest != c.isTest || got.Observed != c.observed || got.Piped != c.piped {
			t.Errorf("Analyse(%q) = %+v, want head %q isTest %v observed %v piped %v", c.cmd, got, c.head, c.isTest, c.observed, c.piped)
		}
	}
}

func TestKeyIgnoresFlagsAndRedirections(t *testing.T) {
	if a, b := shellcmd.Key("python -m unittest"), shellcmd.Key("python -m unittest -v 2>&1 | tail"); a != b || a != "python unittest" {
		t.Errorf("Key: %q and %q, want both %q", a, b, "python unittest")
	}
	if a, b := shellcmd.Key("go test ./a"), shellcmd.Key("go test ./b"); a == b {
		t.Errorf("Key: different packages share key %q", a)
	}
}

func TestExitUnobservedOnlyForATestRunWhoseExitIsNotItsOwn(t *testing.T) {
	cases := map[string]bool{
		"go test ./...":             false,
		"go test ./... | tail":      true,
		"go test ./... || true":     true,
		"ls | tail":                 false, // not a test run
		"make check && echo passed": false,
	}
	for cmd, want := range cases {
		if got := shellcmd.ExitUnobserved(cmd); got != want {
			t.Errorf("ExitUnobserved(%q) = %v, want %v", cmd, got, want)
		}
	}
}
