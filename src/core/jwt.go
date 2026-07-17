package core

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

func decodeJWTPayload(token string) map[string]interface{} {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]interface{}{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some issuers include padding. RawURLEncoding rejects it, URLEncoding accepts it.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return map[string]interface{}{}
		}
	}
	var out map[string]interface{}
	if err := json.Unmarshal(payload, &out); err != nil {
		return map[string]interface{}{}
	}
	return out
}
