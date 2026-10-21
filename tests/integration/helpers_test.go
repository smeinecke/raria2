package integration

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	raria2 "github.com/denysvitali/raria2/pkg"
)

const fixtureDir = "fixtures/site"

// batchEntry is one download record from an aria2 input file.
type batchEntry struct {
	URL string
	Dir string
	Out string
}

// readBatchFile parses an aria2 batch file into entries.
func readBatchFile(t *testing.T, path string) []batchEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var entries []batchEntry
	for _, block := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
		var e batchEntry
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "  dir="):
				e.Dir = strings.TrimPrefix(line, "  dir=")
			case strings.HasPrefix(line, "  out="):
				e.Out = strings.TrimPrefix(line, "  out=")
			default:
				e.URL = line
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// runCrawl executes raria2 against seed, writes a batch file and returns the parsed entries.
func runCrawl(t *testing.T, seed string, configure func(r *raria2.RAria2)) []batchEntry {
	t.Helper()

	u, err := url.Parse(seed)
	require.NoError(t, err)

	outDir := t.TempDir()
	batchPath := filepath.Join(t.TempDir(), "batch.txt")

	r := raria2.New(u)
	r.OutputPath = outDir
	r.WriteBatch = batchPath
	r.DisableRetries = true
	if configure != nil {
		configure(r)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, r.RunWithContext(ctx))

	return readBatchFile(t, batchPath)
}

// entryURLs returns the URL of each batch entry.
func entryURLs(entries []batchEntry) []string {
	urls := make([]string, 0, len(entries))
	for _, e := range entries {
		urls = append(urls, e.URL)
	}
	return urls
}

// entryPath returns the effective site path for a download URL: the ?path=
// query parameter when present (SFTPGo web client URLs), else the URL path.
func entryPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	if p := u.Query().Get("path"); p != "" {
		return p
	}
	return u.Path
}

// assertFixtureFiles checks that the crawl queued the expected fixture files
// under base, that an emitted out= name (when present) matches the real file
// name, and that nested files keep their directory in dir=.
func assertFixtureFiles(t *testing.T, entries []batchEntry, base string) {
	t.Helper()

	byPath := make(map[string]batchEntry)
	for _, e := range entries {
		byPath[entryPath(t, e.URL)] = e
	}

	for _, rel := range []string{"/file1.txt", "/file2.bin", "/sub/file3.txt"} {
		e, ok := byPath[base+rel]
		if !assert.True(t, ok, "expected batch entry for %q, got %v", base+rel, entryURLs(entries)) {
			continue
		}
		if e.Out != "" {
			assert.Equal(t, filepath.Base(rel), e.Out, "entry for %q has wrong out=", base+rel)
		}
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "/" && dir != "." {
			assert.True(t, strings.HasSuffix(filepath.ToSlash(e.Dir), dir),
				"entry for %q should land in %q, got dir=%q", base+rel, dir, e.Dir)
		}
	}
}
