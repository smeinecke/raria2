package raria2

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeAria2c installs a shell script as `aria2c` on PATH.
func writeFakeAria2c(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aria2c")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProbeAria2StdinStreamingPatched(t *testing.T) {
	// Emits a dispatch marker while stdin stays open - like a patched
	// aria2 that consumes -i - entries incrementally.
	writeFakeAria2c(t, `cat >/dev/null &
echo "Download aborted. URI=http://127.0.0.1:1/x"
wait`)
	if !probeAria2StdinStreaming() {
		t.Fatal("expected patched-style output to be detected as streaming")
	}
}

func TestProbeAria2StdinStreamingUnpatched(t *testing.T) {
	// Blocks reading stdin until EOF and prints nothing - like an
	// unpatched aria2 frozen in fgets.
	writeFakeAria2c(t, `cat >/dev/null`)
	if probeAria2StdinStreaming() {
		t.Fatal("expected silent blocked process NOT to be detected as streaming")
	}
}

func TestProbeAria2StdinStreamingMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if probeAria2StdinStreaming() {
		t.Fatal("missing binary must not be detected as streaming")
	}
}

func TestSessionEntryLimitAutoPatched(t *testing.T) {
	old := aria2StreamingProbe
	defer func() { aria2StreamingProbe = old }()
	aria2StreamingProbe = func() bool { return true }

	am := &Aria2Manager{Aria2EntriesPerSession: 100, Aria2StreamingAuto: true}
	if got := am.sessionEntryLimit(); got != 0 {
		t.Fatalf("patched auto mode should disable restarts, got %d", got)
	}
	// Cached: a second call must not re-probe.
	calls := 0
	aria2StreamingProbe = func() bool { calls++; return false }
	if got := am.sessionEntryLimit(); got != 0 || calls != 0 {
		t.Fatalf("session limit must be resolved once, got %d calls %d", got, calls)
	}
}

func TestSessionEntryLimitAutoUnpatched(t *testing.T) {
	old := aria2StreamingProbe
	defer func() { aria2StreamingProbe = old }()
	aria2StreamingProbe = func() bool { return false }

	am := &Aria2Manager{Aria2EntriesPerSession: 100, Aria2StreamingAuto: true}
	if got := am.sessionEntryLimit(); got != 100 {
		t.Fatalf("unpatched auto mode should keep the configured limit, got %d", got)
	}
}

func TestSessionEntryLimitNoAutoNoProbe(t *testing.T) {
	old := aria2StreamingProbe
	defer func() { aria2StreamingProbe = old }()
	aria2StreamingProbe = func() bool {
		t.Fatal("probe must not run when Aria2StreamingAuto is off")
		return false
	}

	am := &Aria2Manager{Aria2EntriesPerSession: 42}
	if got := am.sessionEntryLimit(); got != 42 {
		t.Fatalf("configured session size must be honored, got %d", got)
	}
}
