package integration

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ftpUser = "testuser"
	ftpPass = "testpass"
)

// fsDriver is a minimal ftpserverlib MainDriver serving an afero filesystem.
type fsDriver struct {
	fs       afero.Fs
	settings *ftpserver.Settings
}

func (d *fsDriver) GetSettings() (*ftpserver.Settings, error) { return d.settings, nil }

func (d *fsDriver) ClientConnected(ftpserver.ClientContext) (string, error) {
	return "test FTP server", nil
}

func (d *fsDriver) ClientDisconnected(ftpserver.ClientContext) {}

func (d *fsDriver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != ftpUser || pass != ftpPass {
		return nil, errors.New("authentication failed")
	}
	return d.fs, nil
}

func (d *fsDriver) GetTLSConfig() (*tls.Config, error) {
	return nil, errors.New("TLS not configured")
}

// startFTPServer serves dir over FTP on a random port and returns the listener address.
func startFTPServer(t *testing.T, dir string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	driver := &fsDriver{
		fs: afero.NewBasePathFs(afero.NewOsFs(), dir),
		settings: &ftpserver.Settings{
			Listener:                 listener,
			PassiveTransferPortRange: &ftpserver.PortRange{Start: 25100, End: 25120},
		},
	}

	server := ftpserver.NewFtpServer(driver)
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Stop() })

	return listener.Addr().String()
}

func TestFTPCrawl(t *testing.T) {
	addr := startFTPServer(t, fixtureDir)

	// Credentials ride in the URL userinfo; the crawler must pass them to the
	// FTP client or every login would be anonymous and fail here.
	entries := runCrawl(t, "ftp://"+ftpUser+":"+ftpPass+"@"+addr+"/", nil)

	require.NotEmpty(t, entries)
	assertFixtureFiles(t, entries, "")
}

// A directory holding a single file named like the directory itself produces
// the same single-entry LIST response as LISTing the file directly. The
// crawler must disambiguate via the parent listing and still crawl it.
func TestFTPSameNameDirIsCrawled(t *testing.T) {
	addr := startFTPServer(t, fixtureDir)

	entries := runCrawl(t, "ftp://"+ftpUser+":"+ftpPass+"@"+addr+"/samename", nil)

	require.NotEmpty(t, entries)
	urls := entryURLs(entries)
	assert.Contains(t, urls, "ftp://"+ftpUser+":"+ftpPass+"@"+addr+"/samename/samename")
}
