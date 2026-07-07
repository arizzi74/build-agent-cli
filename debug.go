package main

import (
	"encoding/json"
	"strings"
)

const redactedDebugValue = "[REDACTED]"

func redactDebugJSON(v interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("<unmarshalable debug payload>")
	}
	return redactDebugJSONBytes(raw)
}

func redactDebugJSONBytes(raw []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	redactDebugValue(v, "")
	redacted, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return redacted
}

func redactDebugValue(v interface{}, key string) {
	switch typed := v.(type) {
	case map[string]interface{}:
		for k, child := range typed {
			if isSensitiveDebugKey(k) {
				typed[k] = redactedDebugValue
				continue
			}
			redactDebugValue(child, k)
		}
	case []interface{}:
		for _, child := range typed {
			redactDebugValue(child, key)
		}
	}
}

func isSensitiveDebugKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.ReplaceAll(k, "-", "_")
	k = strings.ReplaceAll(k, " ", "_")
	if isNonSecretTokenMetric(k) {
		return false
	}
	if strings.Contains(k, "token") {
		return true
	}
	switch k {
	case "authorization", "cookie", "cookies", "cookie_header", "cookieheader", "password", "secret", "g_ck", "glide_ck":
		return true
	default:
		return false
	}
}

func isNonSecretTokenMetric(normalizedKey string) bool {
	switch normalizedKey {
	case "maxoutputtokens", "max_output_tokens", "outputtokens", "output_tokens", "requesttokens", "request_tokens", "thinkingtokens", "thinking_tokens":
		return true
	default:
		return false
	}
}
