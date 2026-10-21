package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	raria2 "github.com/denysvitali/raria2/pkg"
)

// siteFiles returns the fixture site as container files rooted at containerRoot.
func siteFiles(containerRoot string) []testcontainers.ContainerFile {
	mount := func(name string) testcontainers.ContainerFile {
		return testcontainers.ContainerFile{
			HostFilePath:      filepath.Join(fixtureDir, name),
			ContainerFilePath: containerRoot + "/" + name,
			FileMode:          0o644,
		}
	}
	return []testcontainers.ContainerFile{
		mount("file1.txt"),
		mount("file2.bin"),
		mount("sub/file3.txt"),
	}
}

// startHTTPContainer starts a container and returns its http://host:port endpoint.
func startHTTPContainer(t *testing.T, req testcontainers.ContainerRequest) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(ctx)
	})

	endpoint, err := ctr.Endpoint(ctx, "http")
	require.NoError(t, err)
	return endpoint
}

// TestNginxAutoindex crawls a real nginx autoindex listing. nginx emits relative
// hrefs for files and trailing-slash hrefs for directories, which is the most
// common static-listing shape the HTML parser sees in the wild.
func TestNginxAutoindex(t *testing.T) {
	endpoint := startHTTPContainer(t, testcontainers.ContainerRequest{
		Image:        "nginx:1.27-alpine",
		ExposedPorts: []string{"80/tcp"},
		Files: append(siteFiles("/srv/site"), testcontainers.ContainerFile{
			// Serve a dedicated root: the default docroot ships index.html,
			// which would shadow the autoindex listing.
			Reader:            strings.NewReader("server {\n\tlisten 80;\n\troot /srv/site;\n\tlocation / {\n\t\tautoindex on;\n\t}\n}\n"),
			ContainerFilePath: "/etc/nginx/conf.d/default.conf",
			FileMode:          0o644,
		}),
		WaitingFor: wait.ForHTTP("/").WithPort("80/tcp").WithStartupTimeout(60 * time.Second),
	})

	entries := runCrawl(t, endpoint+"/", nil)

	require.NotEmpty(t, entries)
	assertFixtureFiles(t, entries, "")
}

// TestCopyparty crawls a real copyparty server. copyparty's modern UI is
// JS-driven, so the crawler has to follow the ?b=u basic-mode link to reach a
// static listing; the listing also contains ?zip/?tar archive links which must
// not be queued for download.
func TestCopyparty(t *testing.T) {
	endpoint := startHTTPContainer(t, testcontainers.ContainerRequest{
		Image:        "copyparty/ac:latest",
		ExposedPorts: []string{"3923/tcp"},
		Cmd:          []string{"-v", "/data:/w:r"},
		Files:        siteFiles("/data"),
		WaitingFor:   wait.ForHTTP("/").WithPort("3923/tcp").WithStartupTimeout(60 * time.Second),
	})

	entries := runCrawl(t, endpoint+"/w/", nil)

	require.NotEmpty(t, entries)
	assertFixtureFiles(t, entries, "/w")

	for _, e := range entries {
		assert.NotContains(t, e.URL, "zip=", "copyparty archive link must not be queued: %s", e.URL)
		assert.NotContains(t, e.URL, "tar=", "copyparty archive link must not be queued: %s", e.URL)
	}
}

// TestSFTPGo crawls a real SFTPGo instance running in portable mode. It
// exercises the full web-client flow: login page, form token, session cookie,
// the /web/client/dirs JSON API and the ?path= download URLs.
func TestSFTPGo(t *testing.T) {
	const (
		user = "ituser"
		pass = "itpass"
		port = "8080/tcp"
	)

	endpoint := startHTTPContainer(t, testcontainers.ContainerRequest{
		Image:        "drakkan/sftpgo:v2-alpine",
		ExposedPorts: []string{port},
		// portable mode serves a directory over HTTP including the web client
		Cmd: []string{
			"sftpgo", "portable",
			"-d", "/data",
			"-u", user,
			"-p", pass,
			"-g", "*",
			"--httpd-port", "8080",
		},
		Files:      siteFiles("/data"),
		WaitingFor: wait.ForHTTP("/healthz").WithPort(port).WithStartupTimeout(90 * time.Second),
	})

	entries := runCrawl(t, fmt.Sprintf("%s/web/client/files?path=/", endpoint), func(r *raria2.RAria2) {
		r.SFTPGoUsername = user
		r.SFTPGoPassword = pass
	})

	require.NotEmpty(t, entries)
	assertFixtureFiles(t, entries, "")
}
