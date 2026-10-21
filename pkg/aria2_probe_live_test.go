package raria2

// Live probe verification against real aria2 binaries. Run with:
//   go test -run TestAria2StreamingProbeLive -v ./pkg
// Skipped unless RARIA2_LIVE_PROBE is set, since it spawns real processes.
import (
	"os"
	"testing"
	"time"
)

func TestAria2StreamingProbeLive(t *testing.T) {
	if os.Getenv("RARIA2_LIVE_PROBE") == "" {
		t.Skip("set RARIA2_LIVE_PROBE=1 to run the live aria2 probe test")
	}

	// Unpatched stock aria2c: must NOT be detected as streaming.
	t.Setenv("PATH", "/usr/bin:/bin")
	start := time.Now()
	if probeAria2StdinStreaming() {
		t.Fatal("stock aria2c detected as streaming - probe false positive")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("unpatched probe took too long: %v", d)
	}

	// Patched static build: must be detected.
	t.Setenv("PATH", "/tmp/patched-bin:/usr/bin:/bin")
	if !probeAria2StdinStreaming() {
		t.Fatal("patched aria2c NOT detected as streaming - probe false negative")
	}
}
