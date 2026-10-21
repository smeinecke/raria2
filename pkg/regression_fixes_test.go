package raria2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server that rejects Range requests must not cause files to be dropped
// under --accept-mime: the sniffer falls back to a plain GET.
func TestSniffFallsBackToPlainGetOnRangeReject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			// No Content-Type on purpose so the GET fallback is exercised.
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.Header.Get("Range") != "" {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			// ZIP magic so DetectContentType reports application/zip.
			_, _ = w.Write([]byte("PK\x03\x04 fake-zip-payload"))
		}
	}))
	defer server.Close()

	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	r := New(baseURL)
	r.OutputPath = tempDir(t)
	r.DryRun = true
	r.DisableRetries = true
	r.FiltersConfig().AcceptMime = map[string]struct{}{"application/zip": {}}

	r.downloadResource(1, server.URL+"/test.zip")
	assert.Len(t, r.downloadEntries, 1, "file should be queued once the plain-GET sniff succeeds")
}
