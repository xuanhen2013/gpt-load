package gateway

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

func codexTestSnapshot() *state.ConfigSnapshot {
	return &state.ConfigSnapshot{ExecutionCandidates: state.ExecutionCandidateIndex{
		protocol.OpenAIResponses: {execution.OperationResponsesCreate: {
			"gpt-6-astra": {{GroupID: 1, UpstreamModelID: "gpt-6-astra"}},
			"friendly":    {{GroupID: 1, UpstreamModelID: "gpt-5.5"}},
			"unknown":     {{GroupID: 1, UpstreamModelID: "unknown"}},
			"conflict":    {{GroupID: 1, UpstreamModelID: "gpt-5.5"}, {GroupID: 2, UpstreamModelID: "gpt-5.4"}},
			"mixed":       {{GroupID: 1, UpstreamModelID: "gpt-5.5"}, {GroupID: 2, UpstreamModelID: "unknown"}},
		}},
		protocol.OpenAIImages: {execution.OperationImagesGenerate: {"gpt-image-2": {{GroupID: 1, UpstreamModelID: "gpt-image-2"}}}},
	}}
}

func TestCodexModelEndpointAuthenticationAndCompiledRoutes(t *testing.T) {
	handler, manager, _ := newHandlerForTest(t, panicForwarder{})
	_, err := manager.Publish(state.CompileInput{
		ChannelRegistry: channel.NewRegistry(),
		Groups: []state.GroupConfig{{ConnectionType: "api_key", ID: 1, Name: "codex-catalog-test", ChannelID: channel.OpenAI,
			Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "gpt-6-astra"}, {ID: "gpt-image-2"}}, Enabled: true}},
		AccessKeys: []state.AccessKeyConfig{
			{ID: 1, Name: "allowed", KeyHash: handler.encryption.Hash("gl-allowed"), Status: state.AccessKeyStatusActive},
			{ID: 2, Name: "disabled", KeyHash: handler.encryption.Hash("gl-disabled"), Status: state.AccessKeyStatusDisabled},
			{ID: 3, Name: "restricted", KeyHash: handler.encryption.Hash("gl-restricted"), Status: state.AccessKeyStatusActive, Filters: state.FilterSet{Groups: map[uint]struct{}{99: {}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler.registry = panicRuntimeRegistry{}
	engine := gin.New()
	bindGatewayRoutesForTest(t, engine, handler)
	for _, test := range []struct {
		key    string
		status int
		count  int
	}{
		{"", http.StatusUnauthorized, 0}, {"wrong", http.StatusUnauthorized, 0},
		{"gl-disabled", http.StatusUnauthorized, 0}, {"gl-restricted", http.StatusOK, 0}, {"gl-allowed", http.StatusOK, 1},
	} {
		t.Run(test.key, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.153.1", nil)
			if test.key != "" {
				request.Header.Set("Authorization", "Bearer "+test.key)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Code == http.StatusOK && len(codexSlugs(t, response.Body.Bytes())) != test.count {
				t.Fatal("unexpected catalog")
			}
		})
	}
}

func TestCodexCatalogIntegrityAndNoCrossRequestMutation(t *testing.T) {
	var payload struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(codexCatalogJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) == 0 || len(payload.Models) != len(codexCatalog()) {
		t.Fatal("empty or duplicate catalog")
	}
	for _, entry := range payload.Models {
		var base string
		_ = json.Unmarshal(entry["base_instructions"], &base)
		var messages struct {
			InstructionsTemplate string `json:"instructions_template"`
		}
		_ = json.Unmarshal(entry["model_messages"], &messages)
		if base == "" && messages.InstructionsTemplate == "" {
			t.Fatalf("missing instructions: %s", entry["slug"])
		}
	}
	before, err := buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, "0.153.1", math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, "0.140.0", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	after, err := buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, "0.153.1", math.MaxInt64)
	if err != nil || string(before) != string(after) {
		t.Fatal("shared catalog mutated")
	}
}

func TestCodexOldReasoningLevels(t *testing.T) {
	entry := map[string]json.RawMessage{
		"supported_reasoning_levels": json.RawMessage(`[{"effort":"medium","description":"medium"},{"effort":"max","description":"max"},{"effort":"ultra","description":"ultra"}]`),
		"default_reasoning_level":    json.RawMessage(`"ultra"`),
	}
	sanitizeCodexReasoning(entry, "0.143.0")
	if string(entry["default_reasoning_level"]) != `"medium"` {
		t.Fatal("invalid default")
	}
	assertJSONEqual(t, string(entry["supported_reasoning_levels"]), `[{"effort":"medium","description":"medium"}]`)
}

func codexSlugs(t *testing.T, body []byte) []string {
	t.Helper()
	var result struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Models == nil {
		t.Fatal("models must not be null")
	}
	ids := make([]string, 0, len(result.Models))
	for _, model := range result.Models {
		ids = append(ids, model.Slug)
	}
	return ids
}

func TestCodexModelListPermissionsAndAliases(t *testing.T) {
	for _, test := range []struct {
		name string
		key  state.AccessKeyView
		want []string
	}{
		{"unrestricted", state.AccessKeyView{}, []string{"friendly", "gpt-6-astra"}},
		{"groups", state.AccessKeyView{Filters: state.FilterSet{Groups: map[uint]struct{}{1: {}}}}, []string{"conflict", "friendly", "gpt-6-astra", "mixed"}},
		{"group two unknown and conflict resolved", state.AccessKeyView{Filters: state.FilterSet{Groups: map[uint]struct{}{2: {}}}}, []string{"conflict"}},
		{"missing group", state.AccessKeyView{Filters: state.FilterSet{Groups: map[uint]struct{}{99: {}}}}, []string{}},
		{"models use public alias", state.AccessKeyView{Filters: state.FilterSet{Models: map[string]struct{}{"friendly": {}, "gpt-5.5": {}}}}, []string{"friendly"}},
		{"responses", state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.OpenAIResponses: {}}}}, []string{"friendly", "gpt-6-astra"}},
		{"chat only", state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.OpenAICompletions: {}}}}, []string{}},
		{"images only", state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.OpenAIImages: {}}}}, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := buildCodexModelList(codexTestSnapshot(), test.key, "0.153.1", math.MaxInt64)
			if err != nil {
				t.Fatal(err)
			}
			if got := codexSlugs(t, body); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %v want %v", got, test.want)
			}
		})
	}
}

func TestCodexModelListPreservesInstructionsAndCapabilities(t *testing.T) {
	body, err := buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, "0.153.1", math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, entry := range payload.Models {
		var slug string
		_ = json.Unmarshal(entry["slug"], &slug)
		originalID := "gpt-6-astra"
		if slug == "friendly" {
			originalID = "gpt-5.5"
		}
		var original map[string]json.RawMessage
		_ = json.Unmarshal(codexCatalog()[originalID], &original)
		for _, field := range []string{"model_messages", "base_instructions", "shell_type", "context_window", "supported_reasoning_levels"} {
			if string(compactCodexJSON(t, entry[field])) != string(compactCodexJSON(t, original[field])) {
				t.Fatalf("%s changed %s", slug, field)
			}
		}
		if slug == "gpt-6-astra" && string(entry["visibility"]) != `"list"` {
			t.Fatal("Astra is not visible")
		}
	}
}

func compactCodexJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	if raw == nil {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCodexModelListSizeAndEmpty(t *testing.T) {
	empty, err := buildCodexModelList(nil, state.AccessKeyView{}, "", 13)
	if err != nil || string(empty) != `{"models":[]}` {
		t.Fatalf("empty=%s err=%v", empty, err)
	}
	for _, limit := range []int64{-1, 0, 12} {
		if _, err := buildCodexModelList(nil, state.AccessKeyView{}, "", limit); !errors.Is(err, errModelListTooLarge) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	body, err := buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, "0.153.1", math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{-1, 0, 1} {
		_, err = buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, "0.153.1", int64(len(body))+delta)
		if (delta < 0) != errors.Is(err, errModelListTooLarge) {
			t.Fatalf("boundary %d: %v", delta, err)
		}
	}
}

func TestCodexModelListFormatNegotiation(t *testing.T) {
	for _, query := range []string{"", "?client_version", "?client_version=", "?client_version=0.153.1", "?client_version=bad", "?client_version=0.153.1&client_version=bad"} {
		t.Run(query, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/v1/models"+query, nil)
			h := &Handler{modelListLimit: math.MaxInt64, writeTimeout: time.Second}
			h.writeVisibleModelList(ctx, codexTestSnapshot(), state.AccessKeyView{}, protocol.OpenAICompletions)
			if recorder.Code != 200 {
				t.Fatal(recorder.Code)
			}
			if query == "" {
				if !strings.Contains(recorder.Body.String(), `"object":"list"`) {
					t.Fatal("standard format changed")
				}
				if recorder.Header().Get("Cache-Control") != "" {
					t.Fatal("standard headers changed")
				}
			} else {
				if len(codexSlugs(t, recorder.Body.Bytes())) != 2 {
					t.Fatal("unexpected models")
				}
				if recorder.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("private cache header missing")
				}
			}
		})
	}
}

func TestCodexModelListVersionFiltering(t *testing.T) {
	for _, version := range []string{"", "bad", "0.153.1", "v0.153.1-alpha+build", "0.137.0"} {
		body, err := buildCodexModelList(codexTestSnapshot(), state.AccessKeyView{}, version, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		if version == "0.137.0" {
			if strings.Contains(string(body), `"gpt-6-astra"`) {
				t.Fatal("minimum client version ignored")
			}
		} else if len(codexSlugs(t, body)) != 2 {
			t.Fatal(version)
		}
	}
	for _, version := range []string{"", "x", "1.2", "1.2.3.4", "-1.2.3", "0.+2.3", "999999999999999999999.2.3"} {
		if _, ok := codexVersion(version); ok {
			t.Fatalf("accepted invalid version %q", version)
		}
	}
}

func TestCodexFormatDoesNotOverrideOtherDialects(t *testing.T) {
	for _, value := range []protocol.Protocol{protocol.Anthropic, protocol.Gemini} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.153.1", nil)
		handler := &Handler{modelListLimit: math.MaxInt64, writeTimeout: time.Second}
		handler.writeVisibleModelList(ctx, codexTestSnapshot(), state.AccessKeyView{}, value)
		want, err := buildVisibleModelList(codexTestSnapshot(), state.AccessKeyView{}, value, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		if recorder.Body.String() != string(want) {
			t.Fatalf("%s format changed", value)
		}
	}
}

func TestCodexModelListRequiresCreateOperation(t *testing.T) {
	snapshot := &state.ConfigSnapshot{ExecutionCandidates: state.ExecutionCandidateIndex{
		protocol.OpenAIResponses: {execution.OperationResponsesRetrieve: {"gpt-6-astra": {{GroupID: 1, UpstreamModelID: "gpt-6-astra"}}}},
	}}
	body, err := buildCodexModelList(snapshot, state.AccessKeyView{}, "0.153.1", math.MaxInt64)
	if err != nil || string(body) != `{"models":[]}` {
		t.Fatalf("retrieve-only route exposed: %s %v", body, err)
	}
}
