package raria2

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/sirupsen/logrus"
)

type ftpConnEntry struct {
	conn          *ftp.ServerConn
	lastUsed      time.Time
	dialAddr      string
	dialOpts      []ftp.DialOption
	user          string
	pass          string
	isImplicitTLS bool
}

func (e *ftpConnEntry) logFields() logrus.Fields {
	fields := logrus.Fields{
		"addr": e.dialAddr,
		"user": e.user,
	}
	if e.isImplicitTLS {
		fields["implicit_tls"] = true
	}
	return fields
}

// ftpConnPool holds a pool of reusable FTP connections for a single server.
// Connections are checked out by workers and returned after use, allowing
// multiple workers to perform FTP operations concurrently.
type ftpConnPool struct {
	mu       sync.Mutex
	conns    []*ftpConnEntry // idle connections
	active   int             // number of checked-out connections
	poolSize int

	// connection template
	dialAddr      string
	dialOpts      []ftp.DialOption
	user          string
	pass          string
	isImplicitTLS bool
}

func (p *ftpConnPool) logFields() logrus.Fields {
	fields := logrus.Fields{
		"addr": p.dialAddr,
		"user": p.user,
	}
	if p.isImplicitTLS {
		fields["implicit_tls"] = true
	}
	return fields
}

// get returns an idle connection from the pool, or creates a new one if the pool
// has capacity. The returned connection is exclusively owned by the caller until
// returned via put().
func (p *ftpConnPool) get(ctx context.Context) (*ftpConnEntry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	p.mu.Lock()
	// Try to reuse an idle connection.
	if len(p.conns) > 0 {
		entry := p.conns[len(p.conns)-1]
		p.conns = p.conns[:len(p.conns)-1]
		p.active++
		p.mu.Unlock()
		entry.lastUsed = time.Now()
		return entry, nil
	}
	p.active++
	p.mu.Unlock()

	// Create a new connection.
	entry := &ftpConnEntry{
		dialAddr:      p.dialAddr,
		dialOpts:      p.dialOpts,
		user:          p.user,
		pass:          p.pass,
		isImplicitTLS: p.isImplicitTLS,
	}

	if err := p.dial(ctx, entry); err != nil {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
		return nil, err
	}

	return entry, nil
}

// put returns a connection to the pool for reuse.
func (p *ftpConnPool) put(entry *ftpConnEntry) {
	if entry == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	if entry.conn != nil {
		p.conns = append(p.conns, entry)
	}
}

// discard drops a broken connection without returning it to the pool.
func (p *ftpConnPool) discard(entry *ftpConnEntry) {
	if entry == nil {
		return
	}
	if entry.conn != nil {
		_ = entry.conn.Quit()
		entry.conn = nil
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
}

func (p *ftpConnPool) dial(ctx context.Context, entry *ftpConnEntry) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dialOpts := append([]ftp.DialOption{ftp.DialWithContext(ctx)}, entry.dialOpts...)
	conn, err := ftp.Dial(entry.dialAddr, dialOpts...)
	if err != nil {
		return err
	}
	if err := conn.Login(entry.user, entry.pass); err != nil {
		_ = conn.Quit()
		return err
	}
	entry.conn = conn
	entry.lastUsed = time.Now()
	logrus.WithFields(entry.logFields()).Info("FTP connection established")
	return nil
}

// closeAll closes all idle connections in the pool.
func (p *ftpConnPool) closeAll() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()

	for _, entry := range conns {
		if entry.conn != nil {
			logrus.WithFields(entry.logFields()).Info("FTP connection closed")
			if err := entry.conn.Quit(); err != nil {
				logrus.WithError(err).WithFields(entry.logFields()).Debug("failed to close FTP connection")
			}
			entry.conn = nil
		}
	}
}

func (r *RAria2) ftpConnKey(u *url.URL) (key string, addr string, user string, pass string, opts []ftp.DialOption, implicitTLS bool) {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if strings.ToLower(u.Scheme) == "ftps" {
			port = "990"
		} else {
			port = "21"
		}
	}
	addr = net.JoinHostPort(host, port)

	user = "anonymous"
	pass = "anonymous"
	if u.User != nil {
		user = u.User.Username()
		if p, ok := u.User.Password(); ok {
			pass = p
		} else {
			pass = ""
		}
	}

	implicitTLS = strings.ToLower(u.Scheme) == "ftps"

	opts = []ftp.DialOption{ftp.DialWithTimeout(r.HTTPTimeout)}
	if implicitTLS {
		opts = append(opts, ftp.DialWithTLS(&tls.Config{ServerName: host}))
	}

	// Include credentials in key: many servers apply different permissions per login.
	key = strings.ToLower(u.Scheme) + "://" + addr + "|" + user + ":" + pass
	return key, addr, user, pass, opts, implicitTLS
}

func (r *RAria2) ftpPoolSize() int {
	if r.Threads > 0 {
		return r.Threads
	}
	return 5
}

func (r *RAria2) ftpConnPoolGet(ctx context.Context, u *url.URL) (*ftpConnPool, *ftpConnEntry, error) {
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}

	key, addr, user, pass, opts, implicitTLS := r.ftpConnKey(u)

	r.ftpConnMu.Lock()
	if r.ftpConnPools == nil {
		r.ftpConnPools = make(map[string]*ftpConnPool)
	}
	pool := r.ftpConnPools[key]
	if pool == nil {
		pool = &ftpConnPool{
			poolSize:      r.ftpPoolSize(),
			dialAddr:      addr,
			dialOpts:      opts,
			user:          user,
			pass:          pass,
			isImplicitTLS: implicitTLS,
		}
		r.ftpConnPools[key] = pool
	}
	r.ftpConnMu.Unlock()

	entry, err := pool.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	return pool, entry, nil
}

func (r *RAria2) ftpConnCacheCloseAll() {
	r.ftpConnMu.Lock()
	pools := r.ftpConnPools
	r.ftpConnPools = nil
	r.ftpConnMu.Unlock()

	for _, pool := range pools {
		if pool != nil {
			pool.closeAll()
		}
	}
}
