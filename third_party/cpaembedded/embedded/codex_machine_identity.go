package embedded

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// RegionProfile is the desktop signals image for one detected proxy region.
// Language, timezone, and screen geometry move together with the egress region
// so a CN-egress request never carries a JP locale image.
type RegionProfile struct {
	Locale      string
	Timezone    string
	Languages   []string
	ScreenSum   int
	ScreenScale float64
}

var codexRegionTable = map[string]RegionProfile{
	"CN": {"zh-CN", "Asia/Shanghai", []string{"zh-CN"}, 2080, 1.5},
	"JP": {"ja-JP", "Asia/Tokyo", []string{"ja-JP", "ja", "en-JP", "en"}, 2080, 1.5},
	"KR": {"ko-KR", "Asia/Seoul", []string{"ko-KR", "ko", "en-KR", "en"}, 3000, 1.25},
	"SG": {"en-SG", "Asia/Singapore", []string{"en-SG", "en"}, 3000, 1.25},
	"HK": {"zh-HK", "Asia/Hong_Kong", []string{"zh-HK", "zh-TW", "zh", "en"}, 2256, 1.5},
	"TW": {"zh-TW", "Asia/Taipei", []string{"zh-TW", "zh", "en"}, 3000, 1.25},
	"US": {"en-US", "America/Chicago", []string{"en-US", "en"}, 2256, 1.5},
	"DE": {"de-DE", "Europe/Berlin", []string{"de-DE", "de", "en-DE", "en"}, 2080, 1.5},
	"GB": {"en-GB", "Europe/London", []string{"en-GB", "en"}, 3000, 1.25},
	"AU": {"en-AU", "Australia/Sydney", []string{"en-AU", "en"}, 2080, 1.5},
}

const (
	codexDesktopOriginator = "Codex Desktop"
	codexDesktopCoreVer    = "0.153.4"
	codexDesktopAppVer     = "26.901.51231"
	codexDesktopOSCore     = "Windows 10.0.19045"
	codexDesktopOSMain     = "Windows NT 10.0"
	codexDesktopArch       = "x86_64"
	codexDesktopBundleID   = "com.openai.codex"
	codexDesktopSchemaVer  = 1
	codexDesktopBetaFeat   = "remote_compaction_v2"
	codexDesktopAgentName  = "/root"
)

const (
	codexAppSessionMinTTL = 8 * time.Hour
	codexAppSessionMaxTTL = 30 * time.Hour
)

// ProxyRegionResult is the persisted outcome of a proxy-save-time region probe.
type ProxyRegionResult struct {
	DetectedRegion  string    `json:"detectedRegion"`
	DetectedIP      string    `json:"detectedIP"`
	DetectedAt      time.Time `json:"detectedAt"`
	DetectionStatus string    `json:"detectionStatus"`
}

// CodexIdentityConfig isolates device identity per stable account ID and
// resolves the region image from the persisted proxy configuration result.
type CodexIdentityConfig struct {
	ProxyRegions  map[string]ProxyRegionResult
	DefaultRegion string

	mu       sync.Mutex
	profiles map[string]*codexDeviceProfile
}

type codexDeviceProfile struct {
	mu             sync.Mutex
	accountSeed    string
	InstallationID string
	Region         RegionProfile
	AppSessionID   string
	appSessionBorn time.Time
	sessionTTL     time.Duration
}

func NewCodexIdentityConfig(regions map[string]ProxyRegionResult, defaultRegion string) *CodexIdentityConfig {
	if strings.TrimSpace(defaultRegion) == "" {
		defaultRegion = "US"
	}
	return &CodexIdentityConfig{ProxyRegions: regions, DefaultRegion: defaultRegion}
}

// UpdateRegion records a freshly detected region for one proxy configuration.
func (c *CodexIdentityConfig) UpdateRegion(proxyConfigID string, result *ProxyRegionResult) {
	if c == nil || strings.TrimSpace(proxyConfigID) == "" || result == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ProxyRegions == nil {
		c.ProxyRegions = make(map[string]ProxyRegionResult)
	}
	c.ProxyRegions[proxyConfigID] = *result
}

// profileFor returns the account-scoped profile. accountID must be the stable
// chatgpt-account-id; proxyConfigID only selects the region image and never
// participates in the profile key.
func (c *CodexIdentityConfig) profileFor(accountID, proxyConfigID, proxyURL string) *codexDeviceProfile {
	if c == nil {
		return nil
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	code := ""
	if result, ok := c.ProxyRegions[strings.TrimSpace(proxyConfigID)]; ok {
		code = strings.TrimSpace(result.DetectedRegion)
	}
	if code == "" {
		code = strings.TrimSpace(c.DefaultRegion)
		if code == "" {
			code = "US"
		}
		log.WithField("event", "codex_identity.default_region_fallback").
			WithField("proxy", proxyURL).
			Warn("codex proxy has no detected region, using default region")
	}
	region, ok := codexRegionTable[code]
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profiles == nil {
		c.profiles = make(map[string]*codexDeviceProfile)
	}
	if profile := c.profiles[accountID]; profile != nil {
		profile.mu.Lock()
		profile.Region = region
		profile.mu.Unlock()
		_ = profile.currentAppSession(time.Now())
		return profile
	}
	profile := newCodexDeviceProfile(accountID, region)
	c.profiles[accountID] = profile
	return profile
}

func newCodexDeviceProfile(accountSeed string, region RegionProfile) *codexDeviceProfile {
	born := time.Now()
	return &codexDeviceProfile{
		InstallationID: installationIDFromSeed(accountSeed),
		accountSeed:    accountSeed,
		Region:         region,
		AppSessionID:   uuid.NewString(),
		appSessionBorn: born,
		sessionTTL:     appSessionTTLForSeed(accountSeed, born),
	}
}

func appSessionTTLForSeed(seed string, born time.Time) time.Duration {
	sum := sha256.Sum256([]byte("app-session-ttl:" + seed + ":" + born.UTC().Format(time.RFC3339Nano)))
	span := codexAppSessionMaxTTL - codexAppSessionMinTTL
	return codexAppSessionMinTTL + time.Duration(binary.BigEndian.Uint64(sum[:8])%uint64(span))
}

func (p *codexDeviceProfile) currentAppSession(now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.appSessionBorn) >= p.sessionTTL {
		p.AppSessionID = uuid.NewString()
		p.appSessionBorn = now
		p.sessionTTL = appSessionTTLForSeed(p.accountSeed, now)
	}
	return p.AppSessionID
}

func installationIDFromSeed(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	id := make([]byte, 16)
	copy(id, sum[:16])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return uuid.UUID(id).String()
}

func codexExecUserAgent() string {
	return codexDesktopOriginator + "/" + codexDesktopCoreVer +
		" (" + codexDesktopOSCore + "; " + codexDesktopArch + ") unknown" +
		" (" + codexDesktopOriginator + "; " + codexDesktopAppVer + ")"
}

func codexMainProcessUserAgent() string {
	return codexDesktopOriginator + "/" + codexDesktopAppVer +
		" (" + codexDesktopOSMain + "; x64)"
}

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major + byte(n)}
	case n <= 0xff:
		return []byte{major + 24, byte(n)}
	case n <= 0xffff:
		return []byte{major + 25, byte(n >> 8), byte(n)}
	default:
		b := make([]byte, 5)
		b[0] = major + 26
		binary.BigEndian.PutUint32(b[1:], uint32(n))
		return b
	}
}

func cborUint(v uint64) []byte  { return cborHead(0x00, v) }
func cborText(s string) []byte  { b := []byte(s); return append(cborHead(0x60, uint64(len(b))), b...) }
func cborBytes(b []byte) []byte { return append(cborHead(0x40, uint64(len(b))), b...) }
func cborFloat(f float64) []byte {
	b := make([]byte, 9)
	b[0] = 0xfb
	binary.BigEndian.PutUint64(b[1:], math.Float64bits(f))
	return b
}
func cborArray(items ...[]byte) []byte {
	out := cborHead(0x80, uint64(len(items)))
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}
func cborMap(pairs ...[2][]byte) []byte {
	out := cborHead(0xa0, uint64(len(pairs)))
	for _, pair := range pairs {
		out = append(out, pair[0]...)
		out = append(out, pair[1]...)
	}
	return out
}

func (p *codexDeviceProfile) signals(appSessionID string) []byte {
	scale := cborUint(uint64(p.Region.ScreenScale))
	if p.Region.ScreenScale != math.Trunc(p.Region.ScreenScale) || p.Region.ScreenScale < 0 {
		scale = cborFloat(p.Region.ScreenScale)
	}
	items := make([][]byte, 0, len(p.Region.Languages))
	for _, language := range p.Region.Languages {
		items = append(items, cborText(language))
	}
	inner := cborMap(
		[2][]byte{cborUint(0), cborUint(codexDesktopSchemaVer)},
		[2][]byte{cborUint(1), cborArray(items...)},
		[2][]byte{cborUint(2), cborText(p.Region.Locale)},
		[2][]byte{cborUint(3), cborText(p.Region.Timezone)},
		[2][]byte{cborUint(4), cborUint(uint64(p.Region.ScreenSum))},
		[2][]byte{cborUint(5), scale},
		[2][]byte{cborUint(6), cborText(appSessionID)},
	)
	return cborBytes(inner)
}

func (p *codexDeviceProfile) attestationToken(appSessionID string) string {
	top := cborMap(
		[2][]byte{cborText("error_code"), cborUint(1)},
		[2][]byte{cborText("bundle_id"), cborText(codexDesktopBundleID)},
		[2][]byte{cborText("f"), p.signals(appSessionID)},
	)
	return "v1." + base64.RawURLEncoding.EncodeToString(top)
}

func (p *codexDeviceProfile) attestationHeader(appSessionID string) string {
	raw, _ := json.Marshal(struct {
		V int    `json:"v"`
		S int    `json:"s"`
		T string `json:"t"`
	}{V: 1, S: 0, T: p.attestationToken(appSessionID)})
	return string(raw)
}

// sessionHeaders builds the per-turn desktop headers. Session-Id is omitted:
// CPA derives it from the injected prompt_cache_key so both stay one v7 value.
func (p *codexDeviceProfile) sessionHeaders(threadID, turnID, contextWindowID string) (http.Header, string, error) {
	windowID := fmt.Sprintf("%s:0", threadID)
	metadata := map[string]any{
		"installation_id":                p.InstallationID,
		"session_id":                     threadID,
		"thread_id":                      threadID,
		"agent_name":                     codexDesktopAgentName,
		"turn_id":                        turnID,
		"window_id":                      windowID,
		"window_number":                  0,
		"context_window_id":              contextWindowID,
		"request_kind":                   "turn",
		"thread_source":                  "user",
		"sandbox":                        "none",
		"sandbox_mode":                   "danger-full-access",
		"auto_review_enabled":            false,
		"node_repl_auto_review_required": false,
		"node_repl_disabled":             false,
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", err
	}
	headers := make(http.Header)
	headers.Set("Thread-Id", threadID)
	headers.Set("X-Client-Request-Id", threadID)
	headers.Set("X-Codex-Window-Id", windowID)
	headers.Set("X-Codex-Turn-Metadata", string(raw))
	headers.Set("X-Codex-Beta-Features", codexDesktopBetaFeat)
	headers.Set("Originator", codexDesktopOriginator)
	return headers, windowID, nil
}
