package embedded

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type capturedCodexRequest struct {
	headers http.Header
	body    map[string]json.RawMessage
}

// TestCodexDesktopWireAudit sends one synthetic credential through the real
// CPA executor and transport into a local receiver. It asserts the desktop
// wire suite without touching chatgpt.com.
func TestCodexDesktopWireAudit(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedCodexRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		mu.Lock()
		captured = append(captured, capturedCodexRequest{headers: r.Header.Clone(), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_audit\",\"model\":\"gpt-5.5\",\"output\":[]}}\n\n")
	}))
	defer server.Close()

	executor := NewCodexHTTPExecutor()
	executor.SetIdentityConfig(NewCodexIdentityConfig(map[string]ProxyRegionResult{
		"audit": {DetectedRegion: "US"},
	}, "US"))
	credential := CodexCredential{Type: ProviderCodex, AccessToken: "synthetic-token", AccountID: "synthetic-audit-account"}

	for range 2 {
		auth := NewCodexAuth("audit", credential, server.URL)
		request := ExecuteRequest{
			AccountID:     credential.AccountID,
			ProxyConfigID: "audit",
			Model:         "gpt-5.5",
			Payload:       []byte(`{"model":"gpt-5.5","input":"OK"}`),
			Format:        "openai-response",
			Headers:       make(http.Header),
		}
		executor.prepareCodexIdentity(auth, &request)
		observation := newExecutionObservation(request)
		ctx := executor.executionContext(context.Background(), auth, observation, false)
		format := sdktranslator.FromString(request.Format)
		_, err := executor.inner.Execute(ctx, auth, cliproxyexecutor.Request{
			Model: request.Model, Payload: request.Payload, Format: format,
		}, cliproxyexecutor.Options{Headers: request.Headers, SourceFormat: format})
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("captured requests = %d, want 2", len(captured))
	}
	first, second := captured[0], captured[1]
	if got := first.headers.Get("User-Agent"); got != codexExecUserAgent() {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := first.headers.Get("Originator"); got != codexDesktopOriginator {
		t.Fatalf("Originator = %q", got)
	}
	threadID := first.headers.Get("Thread-Id")
	if threadID == "" || first.headers.Get("X-Client-Request-Id") != threadID ||
		first.headers.Get("Session-Id") != threadID {
		t.Fatalf("thread/session/request ids diverge: thread=%q client=%q session=%q",
			threadID, first.headers.Get("X-Client-Request-Id"), first.headers.Get("Session-Id"))
	}
	if first.headers.Get("X-Codex-Window-Id") != threadID+":0" {
		t.Fatalf("window id = %q", first.headers.Get("X-Codex-Window-Id"))
	}
	if got := first.headers.Get("X-Codex-Beta-Features"); got != codexDesktopBetaFeat {
		t.Fatalf("beta features = %q", got)
	}
	var turnMetadata map[string]any
	if err := json.Unmarshal([]byte(first.headers.Get("X-Codex-Turn-Metadata")), &turnMetadata); err != nil {
		t.Fatalf("turn metadata: %v", err)
	}
	if turnMetadata["installation_id"] == "" || turnMetadata["turn_id"] == "" ||
		turnMetadata["context_window_id"] == "" || turnMetadata["request_kind"] != "turn" {
		t.Fatalf("turn metadata = %#v", turnMetadata)
	}
	var attestation struct {
		V int    `json:"v"`
		S int    `json:"s"`
		T string `json:"t"`
	}
	if err := json.Unmarshal([]byte(first.headers.Get("X-Oai-Attestation")), &attestation); err != nil {
		t.Fatalf("attestation: %v", err)
	}
	if attestation.V != 1 || attestation.S != 0 || !strings.HasPrefix(attestation.T, "v1.") {
		t.Fatalf("attestation envelope = %#v", attestation)
	}
	if _, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(attestation.T, "v1.")); err != nil {
		t.Fatalf("attestation token: %v", err)
	}
	var cacheKey string
	if raw, ok := first.body["prompt_cache_key"]; ok {
		_ = json.Unmarshal(raw, &cacheKey)
	}
	if cacheKey != threadID {
		t.Fatalf("prompt_cache_key = %q, want %q", cacheKey, threadID)
	}
	if first.headers.Get("Conversation_id") != "" || first.headers.Get("session_id") != "" {
		t.Fatalf("CPA private cache headers leaked: %#v", first.headers)
	}
	if second.headers.Get("Thread-Id") == threadID || second.headers.Get("Thread-Id") == "" {
		t.Fatal("second request must carry a fresh thread id")
	}
	if first.headers.Get("X-Oai-Attestation") != second.headers.Get("X-Oai-Attestation") {
		t.Fatal("attestation must stay stable across requests in one app session")
	}
}

func TestNormalizeCodexWireHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("session_id", "session-value")
	request.Header.Set("Conversation_id", "conversation-value")
	request.Header.Set("Conversation-Id", "another-value")
	normalizeCodexWireHeaders(request)
	if got := request.Header.Get("Session-Id"); got != "session-value" {
		t.Fatalf("Session-Id = %q", got)
	}
	if request.Header.Get("session_id") != "" || request.Header.Get("Conversation_id") != "" ||
		request.Header.Get("Conversation-Id") != "" {
		t.Fatalf("private headers survived: %#v", request.Header)
	}
}
