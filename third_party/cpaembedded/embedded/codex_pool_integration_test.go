package embedded

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http/httpproxy"
)

func TestCodexPoolCanonicalUnaryAndStreamingUseInjectedTransport(t *testing.T) {
	var requests atomic.Int64
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "" || r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("unexpected path %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing request credential")
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", "12")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-5.2\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	p := startSOCKS(t)
	u, _ := url.Parse(s.URL)
	port, _ := strconv.Atoi(u.Port())
	p.upstreamPort.Store(int64(port))
	e := poolExecutor(t, s)
	credential := CodexCredential{Type: ProviderCodex, AccessToken: "synthetic-token", RefreshToken: "synthetic-refresh", AccountID: "synthetic-account"}
	request := ExecuteRequest{IdentityGeneration: 17, Model: "gpt-5.2", Payload: []byte(`{"model":"gpt-5.2","input":"hello"}`), Format: "openai-response", ProxyURL: poolAuth("1", p).ProxyURL}
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(e.RequestContext(context.Background()), 3*time.Second)
		if i%2 == 0 {
			response, err := e.ExecuteCanonical(ctx, "1", credential, request)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if response.QuotaSignals.Signals["X-Codex-Primary-Used-Percent"] != "12" {
				t.Error("lost quota observation")
			}
		} else {
			response, err := e.ExecuteStreamCanonical(ctx, "1", credential, request)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			for chunk := range response.Chunks {
				if chunk.Err != nil {
					t.Error(chunk.Err)
				}
			}
			if response.QuotaSignals.Signals["X-Codex-Primary-Used-Percent"] != "12" {
				t.Error("lost streaming observation")
			}
		}
		cancel()
	}
	if requests.Load() != 10 || p.connects.Load() != 1 {
		t.Fatalf("requests=%d TCP=%d", requests.Load(), p.connects.Load())
	}
}

func TestCodexPoolHTTPConnectAndEnvironmentResolution(t *testing.T) {
	s := startH2(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	var connects, active atomic.Int64
	var wg sync.WaitGroup
	connectProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("test-user:test-pass")) {
			http.Error(w, "proxy auth", 407)
			return
		}
		upstream, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "https://"))
		if err != nil {
			http.Error(w, "upstream", 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		connects.Add(1)
		active.Add(1)
		wg.Add(1)
		defer wg.Done()
		defer active.Add(-1)
		defer client.Close()
		defer upstream.Close()
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		done := make(chan struct{})
		go func() { io.Copy(client, upstream); client.Close(); close(done) }()
		io.Copy(upstream, buffer)
		upstream.Close()
		<-done
	}))
	t.Cleanup(func() { connectProxy.Close(); wg.Wait() })
	e := poolExecutor(t, s)
	auth := NewCodexAuth("1", CodexCredential{AccountID: "synthetic-account"}, "")
	proxyURL := strings.Replace(connectProxy.URL, "http://", "http://test-user:test-pass@", 1)
	resolver := (&httpproxy.Config{HTTPSProxy: proxyURL, NoProxy: "excluded.example"}).ProxyFunc()
	for i := 0; i < 5; i++ {
		rt := e.pool.roundTripper(t.Context(), auth, 1, true, http.DefaultTransport).(*codexPoolRoundTripper)
		rt.resolveProxy = func(r *http.Request) (string, error) {
			u, err := resolver(r.URL)
			if u == nil {
				return "direct", err
			}
			return u.String(), err
		}
		post(t, &http.Client{Transport: rt}, poolTarget(s, "/"))
		excluded, _ := http.NewRequest("GET", "https://excluded.example/", nil)
		if value, err := rt.resolveProxy(excluded); err != nil || value != "direct" {
			t.Fatal("NO_PROXY was lost")
		}
	}
	if connects.Load() != 1 {
		t.Fatalf("HTTP proxy TCP=%d", connects.Load())
	}
	<-e.BeginShutdown()
	eventually(t, func() bool { return active.Load() == 0 })
}

func TestCodexPoolRejectsInvalidCertificatesAndALPN(t *testing.T) {
	for _, test := range []string{"untrusted certificate", "wrong hostname", "HTTP1 only"} {
		t.Run(test, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
			server.EnableHTTP2 = test != "HTTP1 only"
			if test != "wrong hostname" {
				server.TLS = &tls.Config{Certificates: []tls.Certificate{poolTestCertificate(t)}}
			}
			server.StartTLS()
			defer server.Close()
			p := startSOCKS(t)
			roots := x509.NewCertPool()
			if test != "untrusted certificate" {
				roots.AddCert(server.Certificate())
			}
			transport, err := newCodexTransport(poolAuth("1", p).ProxyURL, time.Minute, time.Second, roots)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Close()
			request, _ := http.NewRequest("POST", poolTarget(server, "/"), strings.NewReader("{}"))
			response, err := transport.RoundTrip(request)
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("invalid TLS connection accepted")
			}
			eventually(t, func() bool { return p.active.Load() == 0 })
		})
	}
}

func TestCodexPoolReconnectsAfterGracefulServerShutdown(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	s := startH2(t, handler)
	p := startSOCKS(t)
	e := poolExecutor(t, s)
	client := poolClient(e, poolAuth("1", p), 1)
	endpoint := poolTarget(s, "/")
	post(t, client, endpoint)
	address := s.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Config.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return p.active.Load() == 0 })
	replacement := httptest.NewUnstartedServer(handler)
	replacement.Listener.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Listener = listener
	replacement.EnableHTTP2 = true
	replacement.TLS = &tls.Config{Certificates: []tls.Certificate{poolTestCertificate(t)}}
	replacement.StartTLS()
	defer replacement.Close()
	post(t, client, endpoint)
	if p.connects.Load() != 2 {
		t.Fatalf("replacement did not get a fresh connection: %d", p.connects.Load())
	}
}
