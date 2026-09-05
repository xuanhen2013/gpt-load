package gateway

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

// 元数据来自 openai/codex rust-v0.153.1，保持完整指令字段；不在请求中访问上游。
//
//go:embed codex_catalog.json
var codexCatalogJSON []byte

var codexCatalog = sync.OnceValue(func() map[string]json.RawMessage {
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if json.Unmarshal(codexCatalogJSON, &payload) != nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(payload.Models))
	for _, raw := range payload.Models {
		var model struct {
			Slug string `json:"slug"`
		}
		if json.Unmarshal(raw, &model) == nil && model.Slug != "" {
			result[model.Slug] = raw
		}
	}
	return result
})

func buildCodexModelList(snapshot *state.ConfigSnapshot, key state.AccessKeyView, clientVersion string, limit int64) ([]byte, error) {
	models := make([]json.RawMessage, 0)
	encodedSize := int64(len(`{"models":[]}`))
	if encodedSize > limit {
		return nil, errModelListTooLarge
	}
	if snapshot != nil && (len(key.Filters.Protocols) == 0 || containsResponses(key.Filters.Protocols)) {
		// 仅检查可创建 Responses 的路由，不能将其它操作或图像/嵌入模型混入。
		candidates := snapshot.ExecutionCandidates[protocol.OpenAIResponses][execution.OperationResponsesCreate]
		ids := make([]string, 0, len(candidates))
		for id := range candidates {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if id == state.NoModelRouteKey {
				continue
			}
			if len(key.Filters.Models) > 0 {
				if _, ok := key.Filters.Models[id]; !ok {
					continue
				}
			}
			var template json.RawMessage
			conflict := false
			for _, target := range candidates[id] {
				if len(key.Filters.Groups) > 0 {
					if _, ok := key.Filters.Groups[target.GroupID]; !ok {
						continue
					}
				}
				candidate, ok := codexCatalog()[target.UpstreamModelID]
				if !ok || (template != nil && string(template) != string(candidate)) {
					conflict = true
					break
				}
				template = candidate
			}
			if conflict || template == nil {
				continue
			}
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(template, &entry); err != nil {
				return nil, err
			}
			var minimum string
			_ = json.Unmarshal(entry["minimal_client_version"], &minimum)
			if olderCodexClient(clientVersion, minimum) {
				continue
			}
			entry["slug"], _ = json.Marshal(id)
			var original struct {
				Slug string `json:"slug"`
			}
			_ = json.Unmarshal(template, &original)
			upstreamID := original.Slug
			if id != upstreamID {
				entry["display_name"], _ = json.Marshal(id)
			}
			// Astra 的协议元数据已经存在；只有经过权限与实际路由筛选后才展示。
			if upstreamID == "gpt-6-astra" {
				entry["visibility"] = json.RawMessage(`"list"`)
			}
			sanitizeCodexReasoning(entry, clientVersion)
			raw, err := json.Marshal(entry)
			if err != nil {
				return nil, err
			}
			required := int64(len(raw))
			if len(models) > 0 {
				required++
			}
			if required > limit-encodedSize {
				return nil, errModelListTooLarge
			}
			encodedSize += required
			models = append(models, raw)
		}
	}
	// 始终返回 [] 而不是 null，并在完整元数据序列化后检查实际字节数。
	body, err := json.Marshal(struct {
		Models []json.RawMessage `json:"models"`
	}{models})
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errModelListTooLarge
	}
	return body, nil
}

func containsResponses(protocols map[protocol.Protocol]struct{}) bool {
	_, ok := protocols[protocol.OpenAIResponses]
	return ok
}

// 空值和未知版本仍选择 Codex 格式，但不猜测其能力上限。
func codexVersion(raw string) ([3]int, bool) {
	var result [3]int
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if index := strings.IndexAny(raw, "-+"); index >= 0 {
		raw = raw[:index]
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return result, false
	}
	for index, part := range parts {
		if part == "" {
			return result, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return result, false
			}
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return result, false
		}
		result[index] = value
	}
	return result, true
}

func olderCodexClient(client, minimum string) bool {
	left, validLeft := codexVersion(client)
	right, validRight := codexVersion(minimum)
	if !validLeft || !validRight {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

func sanitizeCodexReasoning(entry map[string]json.RawMessage, version string) {
	if !olderCodexClient(version, "0.144.0") {
		return
	}
	var levels []struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	}
	if json.Unmarshal(entry["supported_reasoning_levels"], &levels) != nil {
		return
	}
	filtered := levels[:0]
	for _, level := range levels {
		if level.Effort != "max" && level.Effort != "ultra" {
			filtered = append(filtered, level)
		}
	}
	entry["supported_reasoning_levels"], _ = json.Marshal(filtered)
	var current string
	_ = json.Unmarshal(entry["default_reasoning_level"], &current)
	for _, level := range filtered {
		if level.Effort == current {
			return
		}
	}
	delete(entry, "default_reasoning_level")
	if len(filtered) > 0 {
		entry["default_reasoning_level"], _ = json.Marshal(filtered[0].Effort)
	}
}
