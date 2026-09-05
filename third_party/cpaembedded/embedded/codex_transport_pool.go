package embedded

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	codexPoolCapacity       = 64
	codexPoolIdleTimeout    = 60 * time.Second
	codexPoolConnectTimeout = 30 * time.Second
)

type codexPoolKey struct {
	credentialID string
	generation   uint64
	account      [32]byte
	proxy        [32]byte
}

type codexPoolEntry struct {
	key       codexPoolKey
	transport *codexTransport
	leases    int
	retired   bool
	lastUsed  time.Time
	timer     *time.Timer
}

type codexTransportPool struct {
	mu      sync.Mutex
	entries map[codexPoolKey]*codexPoolEntry
	// Includes retired entries whose response bodies are still active.
	all            map[*codexPoolEntry]struct{}
	epoch          uint64
	closed         bool
	done           chan struct{}
	capacity       int
	idle           time.Duration
	connectTimeout time.Duration
	roots          *x509.CertPool // nil in production; test servers use a private CA.
}

type codexPoolEpochKey struct{}
type codexPoolEpoch struct {
	pool  *codexTransportPool
	value uint64
}

func newCodexTransportPool() *codexTransportPool {
	return &codexTransportPool{entries: make(map[codexPoolKey]*codexPoolEntry), all: make(map[*codexPoolEntry]struct{}), done: make(chan struct{}), capacity: codexPoolCapacity, idle: codexPoolIdleTimeout, connectTimeout: codexPoolConnectTimeout}
}

// RequestContext freezes the pool epoch before credential preparation. A
// retirement during preparation cannot make that attempt recreate a cached pool.
func (e *CodexHTTPExecutor) RequestContext(ctx context.Context) context.Context {
	if e.pool == nil {
		return ctx
	}
	e.pool.mu.Lock()
	epoch := e.pool.epoch
	e.pool.mu.Unlock()
	return context.WithValue(ctx, codexPoolEpochKey{}, codexPoolEpoch{pool: e.pool, value: epoch})
}

func (e *CodexHTTPExecutor) RetireCredential(id string) {
	if e.pool == nil {
		return
	}
	p := e.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.epoch++
	for entry := range p.all {
		if entry.key.credentialID == id {
			p.retireLocked(entry)
		}
	}
}

func (e *CodexHTTPExecutor) BeginShutdown() <-chan struct{} {
	if e.pool == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	p := e.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		for entry := range p.all {
			p.retireLocked(entry)
		}
		p.finishLocked()
	}
	return p.done
}

func (p *codexTransportPool) finishLocked() {
	if p.closed && len(p.all) == 0 {
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	}
}

func (p *codexTransportPool) retireLocked(entry *codexPoolEntry) {
	entry.retired = true
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if p.entries[entry.key] == entry {
		delete(p.entries, entry.key)
	}
	if entry.leases == 0 {
		entry.transport.Close()
		delete(p.all, entry)
	}
	p.finishLocked()
}

func (p *codexTransportPool) acquire(key codexPoolKey, proxyURL string, epoch uint64) (*codexPoolEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("Codex connection pool is shutting down")
	}
	// Old frozen attempts may finish, but do not register new persistent state.
	if epoch != p.epoch {
		return nil, nil
	}
	if entry := p.entries[key]; entry != nil {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entry.leases++
		return entry, nil
	}
	if len(p.all) >= p.capacity {
		var oldest *codexPoolEntry
		for entry := range p.all {
			if entry.leases == 0 && (oldest == nil || entry.lastUsed.Before(oldest.lastUsed)) {
				oldest = entry
			}
		}
		if oldest == nil {
			log.WithField("event", "codex_pool.capacity_fallback").Debug("Codex pool full; using request-owned connection")
			return nil, nil
		}
		p.retireLocked(oldest)
	}
	transport, err := newCodexTransport(proxyURL, p.idle, p.connectTimeout, p.roots)
	if err != nil {
		return nil, err
	}
	entry := &codexPoolEntry{key: key, transport: transport, leases: 1}
	p.entries[key] = entry
	p.all[entry] = struct{}{}
	return entry, nil
}

func (p *codexTransportPool) release(entry *codexPoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry.leases--
	if entry.leases != 0 {
		return
	}
	if entry.retired {
		p.retireLocked(entry)
		return
	}
	entry.lastUsed = time.Now()
	entry.timer = time.AfterFunc(p.idle, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if entry.leases == 0 && !entry.retired && time.Since(entry.lastUsed) >= p.idle {
			p.retireLocked(entry)
		}
	})
}

type codexPoolRoundTripper struct {
	pool         *codexTransportPool
	key          codexPoolKey
	epoch        uint64
	proxyURL     string
	environment  bool
	fallback     http.RoundTripper
	resolveProxy func(*http.Request) (string, error)
}

func (p *codexTransportPool) roundTripper(ctx context.Context, auth *cliproxyauth.Auth, generation uint64, environment bool, fallback http.RoundTripper) http.RoundTripper {
	p.mu.Lock()
	epoch := p.epoch
	p.mu.Unlock()
	if frozen, ok := ctx.Value(codexPoolEpochKey{}).(codexPoolEpoch); ok && frozen.pool == p {
		epoch = frozen.value
	}
	key := codexPoolKey{generation: generation}
	proxyURL := "direct"
	if auth != nil {
		key.credentialID = auth.ID
		if account, ok := auth.Metadata["account_id"].(string); ok {
			key.account = sha256.Sum256([]byte(account))
		}
		if strings.TrimSpace(auth.ProxyURL) != "" {
			proxyURL = auth.ProxyURL
		}
	}
	return &codexPoolRoundTripper{pool: p, key: key, epoch: epoch, proxyURL: proxyURL, environment: environment, fallback: fallback, resolveProxy: resolveCodexEnvironmentProxy}
}

func resolveCodexEnvironmentProxy(req *http.Request) (string, error) {
	u, err := http.ProxyFromEnvironment(req)
	if err != nil {
		return "", errors.New("resolve Codex environment proxy")
	}
	if u == nil {
		return "direct", nil
	}
	return u.String(), nil
}

func (rt *codexPoolRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Preserve CPA's existing transport for non-Codex hosts and protocols.
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Hostname(), "chatgpt.com") {
		return rt.fallback.RoundTrip(req)
	}
	proxyURL := rt.proxyURL
	if rt.environment {
		var err error
		proxyURL, err = rt.resolveProxy(req)
		if err != nil {
			return failCodexPoolRequest(req, err)
		}
	}
	setting, err := proxyutil.Parse(proxyURL)
	if err != nil {
		return failCodexPoolRequest(req, errors.New("invalid Codex proxy configuration"))
	}
	if setting.Mode == proxyutil.ModeInherit || setting.Mode == proxyutil.ModeDirect {
		proxyURL = "direct"
	} else {
		proxyURL = setting.URL.String()
	}
	key := rt.key
	key.proxy = sha256.Sum256([]byte(proxyURL))
	entry, err := rt.pool.acquire(key, proxyURL, rt.epoch)
	if err != nil {
		return failCodexPoolRequest(req, err)
	}
	var transport *codexTransport
	var release func()
	if entry == nil {
		// Select a request-owned transport BEFORE sending. It uses exactly the same
		// resolved proxy; this is not a retry or a direct-connection fallback.
		transport, err = newCodexTransport(proxyURL, rt.pool.idle, rt.pool.connectTimeout, rt.pool.roots)
		if err != nil {
			return failCodexPoolRequest(req, err)
		}
		release = transport.Close
	} else {
		transport = entry.transport
		release = func() { rt.pool.release(entry) }
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &codexPoolBody{ReadCloser: response.Body, release: release}
	return response, nil
}

// RoundTripper owns the request body even when it fails before dialing.
func failCodexPoolRequest(req *http.Request, err error) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, err
}

type codexPoolBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
	err     error
}

func (b *codexPoolBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		// Close the stream before releasing the lease, including early read errors.
		_ = b.Close()
	}
	return n, err
}

func (b *codexPoolBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close(); b.release() })
	return b.err
}
