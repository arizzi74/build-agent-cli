package main

import (
	"crypto/rand"
	"fmt"
	"strings"
)

func uuidV4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func uuidV4Compact() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x%04x%04x%04x%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func normalizeGatewayConversationID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.ReplaceAll(id, "-", "")
	if len(id) != 32 {
		return ""
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return ""
	}
	return id
}

func normalizeCodeAssistConversationID(id string) string {
	compact := normalizeGatewayConversationID(id)
	if compact == "" {
		return ""
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", compact[0:8], compact[8:12], compact[12:16], compact[16:20], compact[20:32])
}
