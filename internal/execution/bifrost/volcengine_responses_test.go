package bifrost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

func TestVolcengineResponsesProbeUsesNativePrefixAndValidatesResponse(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "response", false: "chat-response"}[valid], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v3/responses" || r.Header.Get("Authorization") != "Bearer native-key" {
					t.Errorf("probe target = %s %s, auth = %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != "upstream-model" || body["store"] != false || body["input"] != "ping" || body["messages"] != nil {
					t.Errorf("probe payload = %#v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				if valid {
					_, _ = io.WriteString(w, `{"id":"resp_probe","object":"response","status":"completed","output":[]}`)
				} else {
					writeSuccess(w, "must not accept a Chat Completions probe")
				}
			}))
			defer server.Close()
			runtime := newRuntimeForTest(t, testRuntimeOptions{allowPrivateNetwork: true})
			spec := openAIResponsesAttempt(t, channel.Volcengine, server.URL+"/api/v3")
			spec.Operation, spec.Method, spec.Path, spec.Body = execution.OperationProbe, "", "", nil
			result := runtime.Execute(t.Context(), freezeTestAttempt(spec))
			if err := result.Validate(); err != nil || (result.Error == nil) != valid || result.UpstreamProtocol != protocol.OpenAIResponses {
				t.Fatalf("probe = %+v, validation = %v", result, err)
			}
		})
	}
}

func TestVolcengineStoredResponseResourcesUseNativePrefix(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer native-key" {
			t.Error("resource request did not use the selected credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /api/v3/responses/resp_saved":
			_, _ = io.WriteString(w, `{"id":"resp_saved","object":"response","status":"completed","store":true,"output":[]}`)
		case "GET /api/v3/responses/resp_saved/input_items?limit=3":
			_, _ = io.WriteString(w, `{"object":"list","data":[],"has_more":false}`)
		case "DELETE /api/v3/responses/resp_saved":
			_, _ = io.WriteString(w, `{"id":"resp_saved","object":"response.deleted","deleted":true}`)
		default:
			t.Errorf("unexpected resource target = %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	runtime := newRuntimeForTest(t, testRuntimeOptions{allowPrivateNetwork: true})
	for _, tc := range []struct {
		operation execution.Operation
		method    string
		path      string
		query     string
	}{
		{execution.OperationResponsesRetrieve, http.MethodGet, "/v1/responses/resp_saved", ""},
		{execution.OperationResponsesInputItems, http.MethodGet, "/v1/responses/resp_saved/input_items", "limit=3"},
		{execution.OperationResponsesDelete, http.MethodDelete, "/v1/responses/resp_saved", ""},
	} {
		spec := openAIResponsesAttempt(t, channel.Volcengine, server.URL+"/api/v3")
		spec.Operation, spec.Method, spec.Path, spec.RawQuery = tc.operation, tc.method, tc.path, tc.query
		spec.Body, spec.ClientModel, spec.UpstreamModel = nil, "", ""
		result := runtime.Execute(t.Context(), freezeTestAttempt(spec))
		if err := result.Validate(); err != nil || result.Error != nil || result.StatusCode != http.StatusOK {
			t.Fatalf("%s = %+v, validation = %v", tc.operation, result, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("resource calls = %d, want 3", calls.Load())
	}
}
