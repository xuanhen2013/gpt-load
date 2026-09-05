package embedded

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// codexTransport owns both the library's HTTP/2 pool and any in-flight dials.
// Closing only idle HTTP/2 connections is insufficient: a dial detached from a
// canceled request may finish after eviction and otherwise resurrect the pool.
type codexTransport struct {
	h2          *http2.Transport
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	starting    chan struct{}
	connections map[*codexPooledConn]struct{}
}

type codexPooledConn struct {
	net.Conn
	owner *codexTransport
	once  sync.Once
	err   error
}

func (c *codexPooledConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		c.owner.mu.Unlock()
		log.WithField("event", "codex_pool.connection_closed").Debug("Codex upstream connection closed")
	})
	return c.err
}

func newCodexTransport(proxyURL string, idle, connectTimeout time.Duration, roots *x509.CertPool) (*codexTransport, error) {
	dialer, mode, err := proxyutil.BuildDialer(proxyURL)
	if err != nil {
		return nil, errors.New("initialize Codex proxy dialer")
	}
	if mode == proxyutil.ModeInherit {
		dialer = proxy.Direct
	}
	cd, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("Codex proxy dialer does not support cancellation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport := &codexTransport{ctx: ctx, cancel: cancel, connections: make(map[*codexPooledConn]struct{})}
	transport.h2 = &http2.Transport{IdleConnTimeout: idle}
	transport.h2.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		// Go's shared Transport may detach dialing from the first request. Bound
		// TCP + proxy negotiation + TLS independently, and cancel on pool eviction.
		ctx, cancel := context.WithTimeout(ctx, connectTimeout)
		stop := context.AfterFunc(transport.ctx, cancel)
		defer func() { stop(); cancel() }()
		if transport.ctx.Err() != nil {
			return nil, context.Canceled
		}
		started := time.Now()
		raw, err := cd.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("Codex proxy connect: %w", err)
		}
		proxyElapsed := time.Since(started)
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			_ = raw.Close()
			return nil, errors.New("invalid Codex upstream address")
		}
		conn := utls.UClient(raw, &utls.Config{ServerName: host, RootCAs: roots}, utls.HelloChrome_Auto)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("Codex TLS handshake: %w", err)
		}
		if conn.ConnectionState().NegotiatedProtocol != "h2" {
			_ = conn.Close()
			return nil, errors.New("Codex upstream did not negotiate HTTP/2")
		}
		tracked := &codexPooledConn{Conn: conn, owner: transport}
		transport.mu.Lock()
		if transport.closed || ctx.Err() != nil {
			transport.mu.Unlock()
			_ = conn.Close()
			return nil, context.Canceled
		}
		transport.connections[tracked] = struct{}{}
		transport.mu.Unlock()
		log.WithFields(log.Fields{"event": "codex_pool.connection_opened", "proxy_connect_ms": proxyElapsed.Milliseconds(), "tls_ms": time.Since(started.Add(proxyElapsed)).Milliseconds()}).Debug("Codex upstream connection opened")
		return tracked, nil
	}
	return transport, nil
}

// awaitFirstConnection suppresses a cold-start TCP stampede in Go 1.27's
// Transport. Followers wait only for connection acquisition, never for the
// first response or stream to finish. HTTP/2 still owns all connection/stream
// selection and may open additional connections when existing ones are full.
func (t *codexTransport) awaitFirstConnection(ctx context.Context) (func(), error) {
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return nil, context.Canceled
		}
		if pending := t.starting; pending != nil {
			t.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if len(t.connections) > 0 {
			t.mu.Unlock()
			return func() {}, nil
		}
		pending := make(chan struct{})
		t.starting = pending
		t.mu.Unlock()
		var once sync.Once
		return func() { once.Do(func() { t.mu.Lock(); t.starting = nil; close(pending); t.mu.Unlock() }) }, nil
	}
}

func (t *codexTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	acquired, err := t.awaitFirstConnection(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	defer acquired()
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		acquired()
		log.WithFields(log.Fields{"event": "codex_pool.connection_acquired", "reused": info.Reused}).Debug("Codex upstream connection acquired")
	}}
	return t.h2.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}

// Close is called only after all body leases drain, or when evicting an idle
// entry. It also cancels pending dials and closes late, unclaimed connections.
func (t *codexTransport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.cancel()
	conns := make([]*codexPooledConn, 0, len(t.connections))
	for c := range t.connections {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	// These connections are being destroyed after their leases drained. Set a
	// deadline only here so a TLS close-notify cannot block retirement on a
	// stalled proxy. Established active connections never receive this deadline.
	for _, c := range conns {
		_ = c.SetDeadline(time.Now())
	}
	t.h2.CloseIdleConnections()
	for _, c := range conns {
		_ = c.Close()
	}
}
