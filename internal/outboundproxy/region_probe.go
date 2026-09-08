package outboundproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RegionProbeResult is the persisted result of a proxy-save-time probe.
type RegionProbeResult struct {
	DetectedRegion  string    `json:"detectedRegion"`
	DetectedIP      string    `json:"detectedIP"`
	DetectedAt      time.Time `json:"detectedAt"`
	DetectionStatus string    `json:"detectionStatus"`
}

// ProbeRegion performs one low-frequency probe through the supplied proxy.
// Callers should invoke it from the proxy save path and persist the result;
// request execution must use the persisted value instead of probing again.
func ProbeRegion(ctx context.Context, config Config, endpoint string) (RegionProbeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	proxyURL, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return RegionProbeResult{}, fmt.Errorf("invalid proxy URL: %w", ErrInvalidConfig)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RegionProbeResult{}, err
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return RegionProbeResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return RegionProbeResult{}, fmt.Errorf("region probe returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		IP      string `json:"query"`
		Country string `json:"countryCode"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return RegionProbeResult{}, err
	}
	if payload.IP == "" || payload.Country == "" {
		return RegionProbeResult{}, fmt.Errorf("region probe returned incomplete result")
	}
	return RegionProbeResult{DetectedIP: payload.IP, DetectedRegion: strings.ToUpper(payload.Country), DetectedAt: time.Now().UTC(), DetectionStatus: "success"}, nil
}

// ProbeAndAttachRegion probes a newly saved custom proxy. On failure it keeps
// the previous persisted result, if any; callers can then persist the returned
// config atomically with the proxy save.
func ProbeAndAttachRegion(ctx context.Context, config Config, previous *RegionProbeResult, endpoint string) Config {
	result, err := ProbeRegion(ctx, config, endpoint)
	if err != nil {
		config.Region = previous
		return config
	}
	config.Region = &result
	return config
}
