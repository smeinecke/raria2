package raria2

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/stretchr/testify/assert"
)

func TestFtpConnKey(t *testing.T) {
	r := &RAria2{HTTPTimeout: 5 * time.Second}

	u, _ := url.Parse("ftp://example.com/path/")
	key, addr, user, pass, _, implicitTLS := r.ftpConnKey(u)
	assert.Equal(t, "example.com:21", addr)
	assert.Equal(t, "anonymous", user)
	assert.Equal(t, "anonymous", pass)
	assert.False(t, implicitTLS)
	assert.Contains(t, key, "ftp://")
	assert.Contains(t, key, "anonymous:anonymous")

	u, _ = url.Parse("ftps://user:secret@example.com:990/path/")
	key2, addr2, user2, pass2, _, implicitTLS2 := r.ftpConnKey(u)
	assert.Equal(t, "example.com:990", addr2)
	assert.Equal(t, "user", user2)
	assert.Equal(t, "secret", pass2)
	assert.True(t, implicitTLS2)
	assert.Contains(t, key2, "ftps://")
	assert.Contains(t, key2, "user:secret")

	assert.NotEqual(t, key, key2)
}

func TestFtpConnKeyDefaultPorts(t *testing.T) {
	r := &RAria2{HTTPTimeout: 5 * time.Second}

	u, _ := url.Parse("ftp://example.com/")
	_, addr, _, _, _, _ := r.ftpConnKey(u)
	assert.Equal(t, "example.com:21", addr)

	u, _ = url.Parse("ftps://example.com/")
	_, addr, _, _, _, _ = r.ftpConnKey(u)
	assert.Equal(t, "example.com:990", addr)
}

func TestFtpConnKeyDifferentCredentials(t *testing.T) {
	r := &RAria2{HTTPTimeout: 5 * time.Second}

	u1, _ := url.Parse("ftp://user1:pass1@example.com/")
	u2, _ := url.Parse("ftp://user2:pass2@example.com/")
	key1, _, _, _, _, _ := r.ftpConnKey(u1)
	key2, _, _, _, _, _ := r.ftpConnKey(u2)
	assert.NotEqual(t, key1, key2)
}

func TestFtpPoolSize(t *testing.T) {
	r := &RAria2{Threads: 10}
	assert.Equal(t, 10, r.ftpPoolSize())

	r.Threads = 0
	assert.Equal(t, 5, r.ftpPoolSize())
}

func TestFtpConnPoolGetAndPut(t *testing.T) {
	pool := &ftpConnPool{
		poolSize: 3,
		dialAddr: "example.com:21",
		user:     "anonymous",
		pass:     "anonymous",
	}

	// Simulate pre-populated idle connections.
	entry1 := &ftpConnEntry{dialAddr: "example.com:21", conn: &stubFTPConn}
	entry2 := &ftpConnEntry{dialAddr: "example.com:21", conn: &stubFTPConn}
	pool.conns = []*ftpConnEntry{entry1, entry2}

	// Get should return idle connections (LIFO).
	got, err := pool.get(context.Background())
	assert.NoError(t, err)
	assert.Same(t, entry2, got)
	assert.Equal(t, 1, pool.active)

	// Return to pool.
	pool.put(got)
	assert.Equal(t, 0, pool.active)
	assert.Len(t, pool.conns, 2) // entry1 + returned entry2
}

func TestFtpConnPoolDiscardDoesNotReturn(t *testing.T) {
	pool := &ftpConnPool{
		poolSize: 3,
		dialAddr: "example.com:21",
	}

	entry := &ftpConnEntry{dialAddr: "example.com:21"}
	pool.active = 1

	pool.discard(entry)
	assert.Equal(t, 0, pool.active)
	assert.Len(t, pool.conns, 0)
}

func TestFtpConnPoolConcurrentAccess(t *testing.T) {
	pool := &ftpConnPool{
		poolSize: 5,
		dialAddr: "example.com:21",
		user:     "anonymous",
		pass:     "anonymous",
	}

	// Pre-populate with 5 idle connections.
	for i := 0; i < 5; i++ {
		pool.conns = append(pool.conns, &ftpConnEntry{
			dialAddr: "example.com:21",
			conn:     &stubFTPConn,
		})
	}

	var wg sync.WaitGroup
	var checkedOut atomic.Int32

	// Simulate 5 concurrent workers checking out and returning connections.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, err := pool.get(context.Background())
			assert.NoError(t, err)
			assert.NotNil(t, entry)
			checkedOut.Add(1)
			// Simulate work.
			time.Sleep(10 * time.Millisecond)
			pool.put(entry)
		}()
	}

	wg.Wait()
	assert.Equal(t, int32(5), checkedOut.Load())
	assert.Equal(t, 0, pool.active)
	assert.Len(t, pool.conns, 5)
}

func TestFtpConnPoolGetRespectsContextCancellation(t *testing.T) {
	pool := &ftpConnPool{
		poolSize: 1,
		dialAddr: "example.com:21",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pool.get(ctx)
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

func TestFtpConnPoolCloseAll(t *testing.T) {
	pool := &ftpConnPool{
		poolSize: 2,
		dialAddr: "example.com:21",
	}

	entry1 := &ftpConnEntry{dialAddr: "example.com:21"}
	entry2 := &ftpConnEntry{dialAddr: "example.com:21"}
	pool.conns = []*ftpConnEntry{entry1, entry2}

	pool.closeAll()
	assert.Len(t, pool.conns, 0)
}

func TestFtpConnPoolGetContextCancelled(t *testing.T) {
	r := &RAria2{HTTPTimeout: 5 * time.Second, Threads: 2}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u, _ := url.Parse("ftp://example.com/")
	_, _, err := r.ftpConnPoolGet(ctx, u)
	assert.Error(t, err)
}

func TestFtpConnPoolGetHonorsPoolSize(t *testing.T) {
	pool := &ftpConnPool{
		poolSize: 1,
		dialAddr: "example.com:21",
	}
	pool.conns = []*ftpConnEntry{{dialAddr: "example.com:21", conn: &stubFTPConn}}

	first, err := pool.get(context.Background())
	assert.NoError(t, err)
	assert.NotNil(t, first)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second, err := pool.get(ctx)
	assert.Error(t, err)
	assert.Nil(t, second)

	pool.put(first)
}

func TestFtpConnCacheCloseAllEmpty(t *testing.T) {
	r := &RAria2{}
	// Should not panic on nil pool map.
	r.ftpConnCacheCloseAll()
}

func TestFtpConnPoolPutNilEntry(t *testing.T) {
	pool := &ftpConnPool{poolSize: 1}
	// Should not panic.
	pool.put(nil)
	assert.Equal(t, 0, pool.active)
}

func TestFtpConnPoolDiscardNilEntry(t *testing.T) {
	pool := &ftpConnPool{poolSize: 1}
	// Should not panic.
	pool.discard(nil)
	assert.Equal(t, 0, pool.active)
}

// stubFTPConn is a zero-value used as a non-nil sentinel for pool tests.
// The FTP pool only checks conn != nil to decide whether to reuse an entry.
var stubFTPConn ftp.ServerConn
