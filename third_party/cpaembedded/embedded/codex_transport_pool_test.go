package embedded

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func poolExecutor(t *testing.T, s *httptest.Server) *CodexHTTPExecutor {
	t.Helper()
	e := NewCodexHTTPExecutorWithConnectionReuse(true)
	e.pool.roots = x509.NewCertPool()
	e.pool.roots.AddCert(s.Certificate())
	e.pool.connectTimeout = time.Second
	t.Cleanup(func() {
		select {
		case <-e.BeginShutdown():
		case <-time.After(3 * time.Second):
			t.Error("pool did not drain")
		}
	})
	return e
}

func poolTarget(s *httptest.Server, path string) string {
	return strings.Replace(target(s, path), "example.com", "chatgpt.com", 1)
}
func poolAuth(id string, p *fakeSOCKS) *cliproxyauth.Auth {
	auth := NewCodexAuth(id, CodexCredential{AccountID: "synthetic-account", AccessToken: "synthetic-token"}, "")
	auth.ProxyURL = "socks5://test-user:test-pass@" + p.listener.Addr().String()
	return auth
}
func poolClient(e *CodexHTTPExecutor, auth *cliproxyauth.Auth, generation uint64) *http.Client {
	ctx := e.executionContext(e.RequestContext(context.Background()), auth, nil, false, generation)
	return &http.Client{Transport: ctx.Value("cliproxy.roundtripper").(http.RoundTripper)}
}
func poolSize(p *codexTransportPool) int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.all) }

func TestCodexPoolReusesAcrossExecutionContextsAndTokenRefresh(t *testing.T) {
	var seenMu sync.Mutex
	var seen []string
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		seenMu.Unlock()
		io.WriteString(w, "ok")
	}))
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	for i := 0; i < 20; i++ {
		auth := poolAuth("1", p)
		token := "Bearer first"
		if i >= 10 {
			token = "Bearer refreshed"
			auth.Metadata["access_token"] = "refreshed"
		}
		req, _ := http.NewRequest("POST", poolTarget(s, "/responses"), strings.NewReader("{}"))
		req.Header.Set("Authorization", token)
		response, err := poolClient(e, auth, 123).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if p.connects.Load() != 1 || poolSize(e.pool) != 1 {
		t.Fatalf("connections=%d pools=%d", p.connects.Load(), poolSize(e.pool))
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 20 || seen[0] != "Bearer first" || seen[19] != "Bearer refreshed" {
		t.Fatalf("wrong request auth: %v", seen)
	}
}

func TestCodexPoolIsolatesCredentialsGenerationsAndProxies(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	p1, p2 := startSOCKS(t), startSOCKS(t)
	e := poolExecutor(t, s)
	for _, item := range []struct {
		id         string
		generation uint64
		p          *fakeSOCKS
	}{{"1", 1, p1}, {"2", 1, p1}, {"1", 2, p1}, {"1", 2, p2}, {"1", 2, p2}} {
		post(t, poolClient(e, poolAuth(item.id, item.p), item.generation), poolTarget(s, "/"))
	}
	if p1.connects.Load() != 3 || p2.connects.Load() != 1 || poolSize(e.pool) != 4 {
		t.Fatal("identity/proxy partitions were mixed")
	}
	e.RetireCredential("1")
	eventually(t, func() bool { return p1.active.Load() == 1 && p2.active.Load() == 0 })
	if poolSize(e.pool) != 1 {
		t.Fatal("credential retirement did not clean all generations and proxies")
	}
}

func TestCodexPoolConcurrentFirstUseAndBodyLeases(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	const n = 12
	var wg sync.WaitGroup
	responses := make(chan *http.Response, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Go(func() {
			response, err := poolClient(e, poolAuth("1", p), 1).Post(poolTarget(s, "/"), "application/json", strings.NewReader("{}"))
			if err != nil {
				errs <- err
				return
			}
			responses <- response
		})
	}
	wg.Wait()
	close(errs)
	close(responses)
	defer func() {
		for response := range responses {
			response.Body.Close()
		}
	}()
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if poolSize(e.pool) != 1 {
		t.Fatalf("concurrent construction created %d partitions", poolSize(e.pool))
	}
	if p.connects.Load() != 1 {
		t.Fatalf("concurrent construction made %d TCP connections", p.connects.Load())
	}
	e.RetireCredential("1")
	if poolSize(e.pool) != 1 {
		t.Fatal("retired active pool no longer counted against capacity")
	}
	once.Do(func() { close(finish) })
	for response := range responses {
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || !strings.Contains(string(body), "done") {
			t.Fatalf("retirement interrupted response: %q %v", body, err)
		}
	}
	eventually(t, func() bool { return poolSize(e.pool) == 0 && p.active.Load() == 0 })
}

func TestCodexPoolCapacityFallbackAndIdleEviction(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
		w.(http.Flusher).Flush()
		if r.URL.Path == "/hold" {
			select {
			case <-finish:
			case <-r.Context().Done():
			}
		}
	}))
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	e.pool.capacity = 1
	held, err := poolClient(e, poolAuth("1", p), 1).Get(poolTarget(s, "/hold"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Body.Close()
	post(t, poolClient(e, poolAuth("2", p), 1), poolTarget(s, "/"))
	if poolSize(e.pool) != 1 {
		t.Fatal("capacity fallback grew cache")
	}
	eventually(t, func() bool { return p.active.Load() == 1 })
	once.Do(func() { close(finish) })
	io.Copy(io.Discard, held.Body)
	held.Body.Close()
	post(t, poolClient(e, poolAuth("2", p), 1), poolTarget(s, "/"))
	if poolSize(e.pool) != 1 {
		t.Fatal("idle eviction grew cache")
	}
	eventually(t, func() bool { return p.active.Load() == 1 })
}

func TestCodexPoolExpiryAndFrozenEpochDoNotResurrect(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	e.pool.idle = 80 * time.Millisecond
	old := poolClient(e, poolAuth("1", p), 1)
	post(t, old, poolTarget(s, "/"))
	e.RetireCredential("1")
	post(t, old, poolTarget(s, "/"))
	if poolSize(e.pool) != 0 {
		t.Fatal("frozen pre-retirement request recreated persistent pool")
	}
	eventually(t, func() bool { return p.active.Load() == 0 })
	post(t, poolClient(e, poolAuth("1", p), 2), poolTarget(s, "/"))
	eventually(t, func() bool { return poolSize(e.pool) == 0 && p.active.Load() == 0 })
	<-e.BeginShutdown()
	req, _ := http.NewRequest("POST", poolTarget(s, "/"), strings.NewReader("{}"))
	if response, err := old.Do(req); err == nil {
		response.Body.Close()
		t.Fatal("shutdown allowed new work")
	}
}

func TestCodexPoolShutdownDrainsActiveResponse(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	response, err := poolClient(e, poolAuth("1", p), 1).Get(poolTarget(s, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	done := e.BeginShutdown()
	select {
	case <-done:
		t.Fatal("shutdown completed before body drained")
	default:
	}
	once.Do(func() { close(finish) })
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !strings.Contains(string(body), "done") {
		t.Fatalf("shutdown interrupted response: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	eventually(t, func() bool { return p.active.Load() == 0 })
}

func TestCodexPoolRetirementCancelsDetachedDial(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		close(accepted)
		io.Copy(io.Discard, c)
	}()
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	e.pool.connectTimeout = 5 * time.Second
	client := poolClient(e, poolAuth("1", p), 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com:"+port+"/", strings.NewReader("{}"))
	result := make(chan error, 1)
	go func() { _, err := client.Do(req); result <- err }()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	e.RetireCredential("1")
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("retirement left detached TLS dial running")
	}
	eventually(t, func() bool { return p.active.Load() == 0 })
}

func TestCodexPoolRejectsInvalidProxyAndPreservesNonCodexTransport(t *testing.T) {
	e := NewCodexHTTPExecutorWithConnectionReuse(true)
	defer e.BeginShutdown()
	var fallback atomic.Int64
	rt := e.pool.roundTripper(t.Context(), &cliproxyauth.Auth{ID: "1", ProxyURL: "ftp://secret@invalid"}, 1, false, environmentRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		fallback.Add(1)
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})).(*codexPoolRoundTripper)
	body := &poolTrackedRequestBody{Reader: strings.NewReader("synthetic-body")}
	req, _ := http.NewRequest("POST", "https://chatgpt.com/", body)
	if _, err := rt.RoundTrip(req); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid proxy accepted/leaked: %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("pre-dispatch failure leaked request body")
	}
	if fallback.Load() != 0 {
		t.Fatal("invalid proxy fell back")
	}
	req, _ = http.NewRequest("GET", "https://other.example/", nil)
	if _, err := rt.RoundTrip(req); err != nil || fallback.Load() != 1 {
		t.Fatal("non-Codex behavior changed")
	}
}

type poolTrackedRequestBody struct {
	*strings.Reader
	closed atomic.Bool
}

func (b *poolTrackedRequestBody) Close() error { b.closed.Store(true); return nil }
