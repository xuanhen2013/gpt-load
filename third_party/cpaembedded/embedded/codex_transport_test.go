package embedded

// Regression tests exercise the production SOCKS5 + uTLS transport on loopback.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type fakeSOCKS struct {
	listener     net.Listener
	connects     atomic.Int64
	upstreamPort atomic.Int64
	active       atomic.Int64
	mu           sync.Mutex
	connections  map[net.Conn]bool
	hosts        []string
}

func startSOCKS(t *testing.T) *fakeSOCKS {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeSOCKS{listener: l, connections: make(map[net.Conn]bool)}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.connections[c] = true
			p.mu.Unlock()
			p.active.Add(1)
			go p.serve(c)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		p.mu.Lock()
		for c := range p.connections {
			c.Close()
		}
		p.mu.Unlock()
	})
	return p
}

func (p *fakeSOCKS) serve(c net.Conn) {
	defer func() { c.Close(); p.active.Add(-1); p.mu.Lock(); delete(p.connections, c); p.mu.Unlock() }()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(c)
	ver, err := r.ReadByte()
	if err != nil || ver != 5 {
		return
	}
	n, err := r.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, int(n))
	if _, err = io.ReadFull(r, methods); err != nil {
		return
	}
	if _, err = c.Write([]byte{5, 2}); err != nil {
		return
	}
	av, err := r.ReadByte()
	if err != nil || av != 1 {
		return
	}
	un, err := r.ReadByte()
	if err != nil {
		return
	}
	user := make([]byte, int(un))
	if _, err = io.ReadFull(r, user); err != nil {
		return
	}
	pn, err := r.ReadByte()
	if err != nil {
		return
	}
	pass := make([]byte, int(pn))
	if _, err = io.ReadFull(r, pass); err != nil {
		return
	}
	if string(user) != "test-user" || string(pass) != "test-pass" {
		c.Write([]byte{1, 1})
		return
	}
	if _, err = c.Write([]byte{1, 0}); err != nil {
		return
	}
	header := make([]byte, 4)
	if _, err = io.ReadFull(r, header); err != nil || header[0] != 5 || header[1] != 1 {
		return
	}
	var host string
	switch header[3] {
	case 3:
		sz, e := r.ReadByte()
		if e != nil {
			return
		}
		b := make([]byte, int(sz))
		if _, e = io.ReadFull(r, b); e != nil {
			return
		}
		host = string(b)
	case 1:
		b := make([]byte, 4)
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err = io.ReadFull(r, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	if override := p.upstreamPort.Load(); override != 0 {
		port = uint16(override)
	}
	// Simulate remote DNS; the test client must send the hostname to the proxy.
	if host != "example.com" && host != "chatgpt.com" {
		return
	}
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	p.connects.Add(1)
	p.mu.Lock()
	p.hosts = append(p.hosts, host)
	p.mu.Unlock()
	if _, err = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() { io.Copy(c, upstream); c.Close(); close(done) }()
	io.Copy(upstream, r)
	upstream.Close()
	<-done
}

func startH2(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{Certificates: []tls.Certificate{poolTestCertificate(t)}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func target(s *httptest.Server, path string) string {
	u, _ := url.Parse(s.URL)
	return "https://example.com:" + u.Port() + path
}

func newTransport(t *testing.T, p *fakeSOCKS, s *httptest.Server, idle time.Duration) *http2.Transport {
	return newTransportBudget(t, p, s, idle, 2*time.Second)
}

func newTransportBudget(t *testing.T, p *fakeSOCKS, s *httptest.Server, idle, connectBudget time.Duration) *http2.Transport {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	transport, err := newCodexTransport("socks5://test-user:test-pass@"+p.listener.Addr().String(), idle, connectBudget, roots)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.Close)
	return transport.h2
}

func post(t *testing.T, c *http.Client, endpoint string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"test":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol=%s", resp.Proto)
	}
	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestSequentialReuseThroughAuthenticatedSOCKS(t *testing.T) {
	var requests atomic.Int64
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		requests.Add(1)
		io.WriteString(w, "ok")
	}))
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s, time.Minute)}
	for i := 0; i < 20; i++ {
		post(t, c, target(s, "/responses"))
	}
	if n := p.connects.Load(); n != 1 {
		t.Fatalf("20 requests used %d proxy TCP connections", n)
	}
	if requests.Load() != 20 {
		t.Fatal("missing requests")
	}
	t.Logf("requests=%d proxy_tcp_connections=%d; hostname passed to proxy; verified TLS and h2", requests.Load(), p.connects.Load())
}

func TestCancellationOnlyClosesOneStream(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(finish) }) })
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/warm" {
			io.WriteString(w, "ok")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		if r.URL.Path == "/cancel" {
			<-r.Context().Done()
			return
		}
		select {
		case <-finish:
			io.WriteString(w, "completed\n")
		case <-r.Context().Done():
		}
	}))
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s, time.Minute)}
	post(t, c, target(s, "/warm"))
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	r1, _ := http.NewRequestWithContext(ctx1, "POST", target(s, "/cancel"), strings.NewReader("{}"))
	resp1, err := c.Do(r1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	r2, _ := http.NewRequestWithContext(ctx2, "POST", target(s, "/complete"), strings.NewReader("{}"))
	resp2, err := c.Do(r2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	cancel1()
	resp1.Body.Close()
	once.Do(func() { close(finish) })
	b, err := io.ReadAll(resp2.Body)
	if err != nil || !strings.Contains(string(b), "completed") {
		t.Fatalf("other stream interrupted: %q %v", b, err)
	}
	resp2.Body.Close()
	post(t, c, target(s, "/warm"))
	if n := p.connects.Load(); n != 1 {
		t.Fatalf("cancellation destroyed shared connection: connections=%d", n)
	}
	t.Log("two active streams, one canceled, other completed; subsequent request reused the same TCP")
}

func TestDifferentPortsUseSeparateConnections(t *testing.T) {
	var a, b atomic.Int64
	s1 := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.Add(1); io.WriteString(w, "a") }))
	s2 := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b.Add(1); io.WriteString(w, "b") }))
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s1, time.Minute)}
	post(t, c, target(s1, "/"))
	post(t, c, target(s2, "/"))
	post(t, c, target(s1, "/"))
	post(t, c, target(s2, "/"))
	if a.Load() != 2 || b.Load() != 2 || p.connects.Load() != 2 {
		t.Fatalf("wrong partition: a=%d b=%d tcp=%d", a.Load(), b.Load(), p.connects.Load())
	}
	t.Log("one Transport correctly partitions identical hostname with two destination ports")
}

func TestIdleTimeoutReleasesConnection(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s, 150*time.Millisecond)}
	post(t, c, target(s, "/"))
	eventually(t, func() bool { return p.active.Load() == 0 })
	post(t, c, target(s, "/"))
	if p.connects.Load() != 2 {
		t.Fatal("idle connection was not replaced")
	}
	t.Log("idle TCP released and recreated for the next request")
}

func TestActiveStreamSurvivesIdleTimeout(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		time.Sleep(350 * time.Millisecond)
		io.WriteString(w, "completed\n")
	}))
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s, 80*time.Millisecond)}
	post(t, c, target(s, "/"))
	if p.connects.Load() != 1 {
		t.Fatal("unexpected reconnect")
	}
	t.Log("silent active stream completed beyond the configured idle-connection timeout")
}

type connKey struct{}

func TestAmbiguousPOSTIsNotAutomaticallyReplayed(t *testing.T) {
	var attempts atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/warm" {
			io.WriteString(w, "ok")
			return
		}
		attempts.Add(1)
		r.Context().Value(connKey{}).(net.Conn).Close()
	}))
	s.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) }
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{Certificates: []tls.Certificate{poolTestCertificate(t)}}
	s.StartTLS()
	t.Cleanup(s.Close)
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s, time.Minute)}
	post(t, c, target(s, "/warm"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", target(s, "/fail-after-reading-body"), strings.NewReader("{\"generate\":true}"))
	resp, err := c.Do(r)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected interrupted response")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("ambiguous POST replayed: %d attempts", n)
	}
	t.Logf("server read POST body then broke connection: attempts=%d", attempts.Load())
}

func TestProxySwitchUsesNewTransport(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	p1, p2 := startSOCKS(t), startSOCKS(t)
	t1, t2 := newTransport(t, p1, s, time.Minute), newTransport(t, p2, s, time.Minute)
	c1, c2 := &http.Client{Transport: t1}, &http.Client{Transport: t2}
	post(t, c1, target(s, "/"))
	post(t, c2, target(s, "/"))
	post(t, c2, target(s, "/"))
	t1.CloseIdleConnections()
	eventually(t, func() bool { return p1.active.Load() == 0 })
	if p1.connects.Load() != 1 || p2.connects.Load() != 1 {
		t.Fatal("proxy separation failed")
	}
	t.Log("new proxy used its own TCP; old idle transport drained")
}

func TestAuthorizationStaysPerRequestOnReusedConnection(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	p := startSOCKS(t)
	c := &http.Client{Transport: newTransport(t, p, s, time.Minute)}
	for _, token := range []string{"Bearer synthetic-A", "Bearer synthetic-B"} {
		r, _ := http.NewRequest("POST", target(s, "/"), strings.NewReader("{}"))
		r.Header.Set("Authorization", token)
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] == seen[1] || seen[1] != "Bearer synthetic-B" || p.connects.Load() != 1 {
		t.Fatalf("request auth leaked across reuse: %v", seen)
	}
	t.Log("one reused TCP carried two distinct request Authorization headers correctly")
}

func TestCanceledRequestDoesNotLeaveUnboundedTLSHandshake(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	bl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()
	blackholeDone := make(chan struct{})
	go func() {
		defer close(blackholeDone)
		c, err := bl.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	p := startSOCKS(t)
	tr := newTransportBudget(t, p, s, time.Minute, 180*time.Millisecond)
	c := &http.Client{Transport: tr}
	_, port, _ := net.SplitHostPort(bl.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com:"+port+"/", strings.NewReader("{}"))
	start := time.Now()
	resp, err := c.Do(r)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("request did not cancel promptly")
	}
	eventually(t, func() bool { return p.active.Load() == 0 })
	select {
	case <-blackholeDone:
	case <-time.After(time.Second):
		t.Fatal("orphan handshake connection survived dial budget")
	}
	t.Log("caller cancellation returned promptly; detached TLS handshake closed within independent dial budget")
}

var poolTestCertOnce sync.Once
var poolTestCert tls.Certificate
var poolTestCertErr error

func poolTestCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	poolTestCertOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			poolTestCertErr = err
			return
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.com", "chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			poolTestCertErr = err
			return
		}
		poolTestCert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	})
	if poolTestCertErr != nil {
		t.Fatal(poolTestCertErr)
	}
	return poolTestCert
}
