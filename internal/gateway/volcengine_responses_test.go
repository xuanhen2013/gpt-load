package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpt-load/internal/channel"
)

func TestVolcengineStreamingResponsesContinueOnStoredUpstream(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := calls.Add(1)
		if r.URL.Path != "/api/v3/responses" || r.Header.Get("Authorization") != "Bearer sk-one" {
			t.Errorf("continuation target = %s, credential = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Previous string `json:"previous_response_id"`
			Store    bool   `json:"store"`
			Stream   bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !body.Store || !body.Stream || (index == 1 && body.Previous != "") || (index == 2 && body.Previous != "resp_stream_1") {
			t.Errorf("request %d lost stored continuation: %+v", index, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_%d\",\"object\":\"response\",\"status\":\"in_progress\",\"store\":true,\"output\":[]}}\n\n", index)
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_%d\",\"object\":\"response\",\"status\":\"completed\",\"store\":true,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n", index)
	}))
	defer server.Close()
	handler, engine, sink := newContinuationFixture(t, newTestExecutionForwarder(t))
	setContinuationChannel(t, handler, channel.Volcengine, server.URL+"/api/v3")
	for _, body := range []string{
		`{"model":"gpt-4o","input":"initial","store":true,"stream":true}`,
		`{"model":"gpt-4o","input":"continue","previous_response_id":"resp_stream_1","store":true,"stream":true}`,
	} {
		response := serveContinuation(t, engine, "gl-client", body, http.StatusOK)
		if !bytes.Contains(response.Body.Bytes(), []byte("response.completed")) || !bytes.Contains(response.Body.Bytes(), []byte(`"store":true`)) {
			t.Fatalf("stored Responses stream was changed: %s", response.Body.String())
		}
	}
	if binding, found := handler.responseBindings.Lookup(1, "resp_stream_2"); !found || binding.CredentialID != 1 {
		t.Fatalf("continued stream binding = %+v, found = %t", binding, found)
	}
	assertAffinityHits(t, sink.snapshot(), []bool{false, true})
	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"continue","previous_response_id":"old_stateless_response"}`, http.StatusBadRequest)
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}
