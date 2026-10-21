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

// ftp://host/dir where /dir contains a single file named "dir" must be treated
// as a directory, not as a file download.
func TestFTPSingleEntrySameNameDirIsCrawled(t *testing.T) {
	base := mustParseURL(t, "ftp://example.com/dir")
	r := newTestClient(base)
	r.OutputPath = tempDir(t)
	r.urlCache = NewURLCache("")
	r.MaxDepth = -1
	r.ftpList = func(ctx context.Context, u *url.URL) ([]ftpListingEntry, error) {
		switch u.Path {
		case "/dir":
			// Ambiguous listing: could be a file named "dir" or a directory
			// holding one file named "dir".
			return []ftpListingEntry{{name: "dir"}}, nil
		case "/":
			return []ftpListingEntry{{name: "dir", isDir: true}, {name: "other.txt"}}, nil
		default:
			return nil, errNotHTML
		}
	}

	r.subDownloadUrls(context.Background(), 0, base.String())
	urls := make([]string, 0, len(r.downloadEntries))
	for _, e := range r.downloadEntries {
		urls = append(urls, e.URL)
	}
	assert.Contains(t, urls, "ftp://example.com/dir/dir",
		"the same-named child file inside the directory should be discovered")
}

// A genuine file (parent listing confirms file type) must still be treated as
// a file, i.e. the disambiguation must not break the file case.
func TestFTPSingleEntrySameNameFileIsDownloaded(t *testing.T) {
	base := mustParseURL(t, "ftp://example.com/dir")
	r := newTestClient(base)
	r.OutputPath = tempDir(t)
	r.urlCache = NewURLCache("")
	r.ftpList = func(ctx context.Context, u *url.URL) ([]ftpListingEntry, error) {
		switch u.Path {
		case "/dir":
			return []ftpListingEntry{{name: "dir"}}, nil
		case "/":
			return []ftpListingEntry{{name: "dir"}, {name: "other.txt"}}, nil
		default:
			return nil, errNotHTML
		}
	}

	r.subDownloadUrls(context.Background(), 0, base.String())
	if assert.Len(t, r.downloadEntries, 1) {
		assert.Equal(t, "ftp://example.com/dir", r.downloadEntries[0].URL)
	}
}

// A batch entry with control characters must be skipped, not abort the run.
func TestBatchDownloadSkipsInvalidEntry(t *testing.T) {
	batchFile := tempDir(t) + "/batch.txt"
	am := NewAria2Manager()
	am.WriteBatch = batchFile

	entries := make(chan aria2URLEntry, 3)
	entries <- aria2URLEntry{URL: "https://example.com/good1.bin"}
	entries <- aria2URLEntry{URL: "https://example.com/evil\x01.bin"}
	entries <- aria2URLEntry{URL: "https://example.com/good2.bin"}
	close(entries)

	require.NoError(t, am.ExecuteBatchDownload(context.Background(), &RAria2{}, entries))

	data, err := os.ReadFile(batchFile)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "https://example.com/good1.bin")
	assert.Contains(t, content, "https://example.com/good2.bin")
	assert.NotContains(t, content, "evil")
}

// A sink-level (non-validation) write error must still abort the run.
func TestBatchDownloadStillPropagatesSinkErrors(t *testing.T) {
	am := NewAria2Manager()
	var wrote []aria2URLEntry
	am.sinkFactory = func(context.Context, *RAria2) (downloadSink, error) {
		return &stubDownloadSink{
			onWrite: func(e aria2URLEntry) error {
				wrote = append(wrote, e)
				if len(wrote) == 2 {
					return fmt.Errorf("disk full")
				}
				return nil
			},
		}, nil
	}

	entries := make(chan aria2URLEntry, 3)
	entries <- aria2URLEntry{URL: "https://example.com/a"}
	entries <- aria2URLEntry{URL: "https://example.com/b"}
	entries <- aria2URLEntry{URL: "https://example.com/c"}
	close(entries)

	assert.Error(t, am.ExecuteBatchDownload(context.Background(), &RAria2{}, entries))
}

// Default FTP/FTPS ports must canonicalize like http/https ones do.
func TestCanonicalURLFTPDefaultPorts(t *testing.T) {
	assert.Equal(t, canonicalURL("ftp://example.com/"), canonicalURL("ftp://example.com:21/"))
	assert.Equal(t, canonicalURL("ftps://example.com/"), canonicalURL("ftps://example.com:990/"))
	assert.NotEqual(t, canonicalURL("ftp://example.com/"), canonicalURL("ftp://example.com:2121/"))
}

// The detection marker must match any SFTPGo major version, not just 2.x.
func TestDetectSFTPGoV3(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/login" {
			_, _ = w.Write([]byte("<html><script>$('title').text('SFTPGo 3.1 WebClient');</script></html>"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true

	detected, err := r.DetectSFTPGo(context.Background())
	assert.NoError(t, err)
	assert.True(t, detected)
}

// The stored session cookie must be a clean "name=value" pair; cookie
// attributes (Path/Expires) do not belong in a Cookie header.
func TestSFTPGoSessionCookieIsNameValueOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/client/login" {
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: "jwt", Value: "initialtoken", Path: "/web/client"})
				_, _ = w.Write([]byte(`<input type="hidden" name="_form_token" value="tok" />`))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "jwt", Value: "sessiontoken", Path: "/web/client", MaxAge: 3600})
			w.Header().Set("Location", "/web/client/files")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/web/client/files?path=/")
	r := New(base)
	r.DisableRetries = true
	r.SFTPGoUsername = "u"
	r.SFTPGoPassword = "p"

	require.NoError(t, r.SFTPGoLogin(context.Background()))
	assert.Equal(t, "jwt=sessiontoken", r.SFTPGoSessionCookie)
}
