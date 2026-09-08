package embedded

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const codexGoldenCNToken = "v1.o2plcnJvcl9jb2RlAWlidW5kbGVfaWRwY29tLm9wZW5haS5jb2RleGFmWFanAAEBgWV6aC1DTgJlemgtQ04DbUFzaWEvU2hhbmdoYWkEGQggBfs_-AAAAAAAAAZ4JDE1NjZlYWU0LWIyOGMtNDcyYS1hOTk3LTIzODc5NzRlMDRmMg"

func TestCodexAttestationTokenMatchesCapturedGolden(t *testing.T) {
	profile := &codexDeviceProfile{Region: codexRegionTable["CN"]}
	token := profile.attestationToken("1566eae4-b28c-472a-a997-2387974e04f2")
	if token != codexGoldenCNToken {
		t.Fatalf("attestation token mismatch\ngot  %s\nwant %s", token, codexGoldenCNToken)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "v1."))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 130 {
		t.Fatalf("decoded CBOR length = %d, want 130", len(raw))
	}
}

func TestCodexAttestationHeaderEnvelope(t *testing.T) {
	profile := &codexDeviceProfile{Region: codexRegionTable["US"]}
	header := profile.attestationHeader("1566eae4-b28c-472a-a997-2387974e04f2")
	var envelope struct {
		V int    `json:"v"`
		S int    `json:"s"`
		T string `json:"t"`
	}
	if err := json.Unmarshal([]byte(header), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.V != 1 || envelope.S != 0 || !strings.HasPrefix(envelope.T, "v1.") {
		t.Fatalf("invalid envelope: %s", header)
	}
	if _, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(envelope.T, "v1.")); err != nil {
		t.Fatalf("invalid CBOR token: %v", err)
	}
}

func TestCaptureCodexOutboundIdentityHeadersWhitelist(t *testing.T) {
	header := http.Header{}
	header.Set("User-Agent", "codex/0.153.4 desktop")
	header.Set("Originator", "Codex Desktop")
	header.Set("X-Oai-Attestation", `{"v":1,"s":0,"t":"v1.abc"}`)
	header.Set("X-Codex-Window-Id", "thread:0")
	header.Set("X-Codex-Turn-Metadata", `{"installation_id":"i"}`)
	header.Set("Authorization", "Bearer secret")
	header.Set("Chatgpt-Account-Id", "account-secret")

	captured := captureCodexOutboundIdentityHeaders(header)
	if captured == "" {
		t.Fatal("capture returned empty result for identity headers")
	}
	var snapshot map[string]string
	if err := json.Unmarshal([]byte(captured), &snapshot); err != nil {
		t.Fatalf("captured snapshot is not JSON: %v", err)
	}
	for key, want := range map[string]string{
		"user-agent":            "codex/0.153.4 desktop",
		"originator":            "Codex Desktop",
		"x-oai-attestation":     `{"v":1,"s":0,"t":"v1.abc"}`,
		"x-codex-window-id":     "thread:0",
		"x-codex-turn-metadata": `{"installation_id":"i"}`,
	} {
		if snapshot[key] != want {
			t.Fatalf("captured %q = %q, want %q", key, snapshot[key], want)
		}
	}
	for _, secret := range []string{"authorization", "chatgpt-account-id", "Bearer secret", "account-secret"} {
		if strings.Contains(strings.ToLower(captured), strings.ToLower(secret)) {
			t.Fatalf("captured snapshot leaks %q: %s", secret, captured)
		}
	}
}

func TestCaptureCodexOutboundIdentityHeadersEmptyAndOversized(t *testing.T) {
	if got := captureCodexOutboundIdentityHeaders(http.Header{}); got != "" {
		t.Fatalf("empty header capture = %q, want empty", got)
	}
	oversized := http.Header{}
	oversized.Set("User-Agent", strings.Repeat("x", maxOutboundIdentityHeaderBytes+1))
	if got := captureCodexOutboundIdentityHeaders(oversized); got != "" {
		t.Fatalf("oversized header capture = %q, want empty", got)
	}
}

func TestCodexRegionProfilesAreInternallyConsistent(t *testing.T) {
	for code, region := range codexRegionTable {
		if len(region.Languages) == 0 {
			t.Fatalf("%s has no languages", code)
		}
		first := strings.Split(region.Languages[0], "-")[0]
		if !strings.HasPrefix(region.Locale, first) {
			t.Fatalf("%s locale %q does not match languages[0] %q", code, region.Locale, region.Languages[0])
		}
		if region.Timezone == "" || region.ScreenSum <= 0 || region.ScreenScale <= 0 {
			t.Fatalf("%s has incomplete profile: %#v", code, region)
		}
	}
	if got := codexRegionTable["JP"].Timezone; got != "Asia/Tokyo" {
		t.Fatalf("JP timezone = %q", got)
	}
	if got := codexRegionTable["US"].ScreenSum; got != 2256 {
		t.Fatalf("US screen sum = %d, want 2256", got)
	}
	if got := codexRegionTable["US"].ScreenScale; got != 1.5 {
		t.Fatalf("US screen scale = %v, want 1.5", got)
	}
}

func TestCodexDesktopUserAgents(t *testing.T) {
	const execUA = "Codex Desktop/0.153.4 (Windows 10.0.19045; x86_64) unknown (Codex Desktop; 26.901.51231)"
	const mainUA = "Codex Desktop/26.901.51231 (Windows NT 10.0; x64)"
	if got := codexExecUserAgent(); got != execUA {
		t.Fatalf("execution UA = %q", got)
	}
	if got := codexMainProcessUserAgent(); got != mainUA {
		t.Fatalf("main process UA = %q", got)
	}
}

func TestCodexIdentityIsPerAccountAndRegionFollowsProxy(t *testing.T) {
	config := NewCodexIdentityConfig(map[string]ProxyRegionResult{
		"proxy-a": {DetectedRegion: "JP"},
		"proxy-b": {DetectedRegion: "US"},
	}, "US")
	accountA := config.profileFor("account-a", "proxy-a", "http://proxy-a")
	accountB := config.profileFor("account-b", "proxy-a", "http://proxy-a")
	if accountA == nil || accountB == nil {
		t.Fatal("missing profile")
	}
	if accountA.InstallationID == accountB.InstallationID || accountA.AppSessionID == accountB.AppSessionID {
		t.Fatal("accounts must have independent identities")
	}
	if accountA.Region.Locale != "ja-JP" || accountA.Region.Timezone != "Asia/Tokyo" {
		t.Fatalf("account-a region = %#v", accountA.Region)
	}
	switched := config.profileFor("account-a", "proxy-b", "http://proxy-b")
	if switched != accountA || switched.InstallationID != accountA.InstallationID {
		t.Fatal("proxy switch must preserve account identity")
	}
	if switched.Region.Locale != "en-US" || switched.Region.Timezone != "America/Chicago" {
		t.Fatalf("switched region = %#v", switched.Region)
	}
	if accountA.Region.Locale != "en-US" {
		t.Fatal("profile region was not updated in place")
	}
}

func TestCodexIdentityRequiresStableAccountID(t *testing.T) {
	config := NewCodexIdentityConfig(nil, "US")
	if config.profileFor("", "proxy", "http://proxy") != nil {
		t.Fatal("missing account id must disable identity")
	}
	if config.profileFor("account", "proxy-unknown", "http://proxy") == nil {
		t.Fatal("unknown proxy region must fall back to the default region")
	}
}

func TestCodexAppSessionRotationPreservesInstallation(t *testing.T) {
	config := NewCodexIdentityConfig(map[string]ProxyRegionResult{"proxy": {DetectedRegion: "JP"}}, "US")
	profile := config.profileFor("account", "proxy", "")
	installation, first := profile.InstallationID, profile.currentAppSession(time.Now())
	profile.mu.Lock()
	profile.appSessionBorn = time.Now().Add(-48 * time.Hour)
	profile.sessionTTL = time.Hour
	profile.mu.Unlock()
	second := profile.currentAppSession(time.Now())
	if first == second || installation != profile.InstallationID {
		t.Fatal("rotation must change only the app session")
	}
	parsed, err := uuid.Parse(second)
	if err != nil || parsed.Version() != 4 {
		t.Fatalf("app session id = %q, want v4", second)
	}
}

func TestCodexSessionHeadersCarryFullTurnMetadata(t *testing.T) {
	profile := &codexDeviceProfile{
		InstallationID: "11111111-2222-4333-8444-555555555555",
		Region:         codexRegionTable["US"],
	}
	threadID, _ := uuid.NewV7()
	turnID, _ := uuid.NewV7()
	contextWindowID, _ := uuid.NewV7()
	headers, windowID, err := profile.sessionHeaders(threadID.String(), turnID.String(), contextWindowID.String())
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("Thread-Id") != threadID.String() || headers.Get("X-Client-Request-Id") != threadID.String() {
		t.Fatal("thread and client request ids must match")
	}
	if windowID != threadID.String()+":0" || headers.Get("X-Codex-Window-Id") != windowID {
		t.Fatalf("window id = %q", windowID)
	}
	if headers.Get("Originator") != codexDesktopOriginator || headers.Get("X-Codex-Beta-Features") != codexDesktopBetaFeat {
		t.Fatal("originator or beta features missing")
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(headers.Get("X-Codex-Turn-Metadata")), &metadata); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"installation_id", "session_id", "thread_id", "agent_name", "turn_id",
		"window_id", "context_window_id", "request_kind", "thread_source",
		"sandbox", "sandbox_mode",
	} {
		if _, ok := metadata[key]; !ok {
			t.Fatalf("turn metadata missing %q", key)
		}
	}
	if metadata["installation_id"] != profile.InstallationID ||
		metadata["session_id"] != threadID.String() ||
		metadata["thread_id"] != threadID.String() ||
		metadata["turn_id"] != turnID.String() ||
		metadata["context_window_id"] != contextWindowID.String() ||
		metadata["agent_name"] != codexDesktopAgentName ||
		metadata["request_kind"] != "turn" ||
		metadata["thread_source"] != "user" ||
		metadata["window_number"] != float64(0) {
		t.Fatalf("turn metadata mismatch: %#v", metadata)
	}
	if _, exists := metadata["workspaces"]; exists {
		t.Fatal("turn metadata must not include workspaces")
	}
}
