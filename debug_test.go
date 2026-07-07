package main

import (
	"bytes"
	"testing"
)

func TestRedactDebugJSONRedactsSensitiveFields(t *testing.T) {
	raw := []byte(`{
		"authorization":"Basic secret-value",
		"usertoken":"token-secret-one",
		"X-UserToken":"token-secret-two",
		"cookieHeader":"JSESSIONID=secret-cookie",
		"body":{"password":"password-secret","content":"keep me","author":"user"},
		"items":[{"refresh_token":"refresh-secret","message":"hello"}]
	}`)
	redacted := redactDebugJSONBytes(raw)
	for _, secret := range [][]byte{[]byte("Basic secret-value"), []byte("token-secret-one"), []byte("token-secret-two"), []byte("JSESSIONID=secret-cookie"), []byte("refresh-secret"), []byte("password-secret")} {
		if bytes.Contains(redacted, secret) {
			t.Fatalf("redacted payload still contains %q: %s", secret, redacted)
		}
	}
	for _, expected := range [][]byte{[]byte("keep me"), []byte("hello"), []byte("author")} {
		if !bytes.Contains(redacted, expected) {
			t.Fatalf("redacted payload lost %q: %s", expected, redacted)
		}
	}
}

func TestRedactDebugJSONKeepsNonSecretTokenMetrics(t *testing.T) {
	raw := []byte(`{"maxOutputTokens":64000,"thinkingTokens":2000,"request_tokens":123,"access_token":"secret"}`)
	redacted := redactDebugJSONBytes(raw)
	for _, expected := range [][]byte{[]byte(`"maxOutputTokens":64000`), []byte(`"thinkingTokens":2000`), []byte(`"request_tokens":123`)} {
		if !bytes.Contains(redacted, expected) {
			t.Fatalf("redacted payload lost non-secret token metric %q: %s", expected, redacted)
		}
	}
	if bytes.Contains(redacted, []byte("secret")) || !bytes.Contains(redacted, []byte(redactedDebugValue)) {
		t.Fatalf("sensitive token was not redacted: %s", redacted)
	}
}

func TestRedactDebugJSONLeavesNonJSONBytesAlone(t *testing.T) {
	raw := []byte("not json")
	if got := redactDebugJSONBytes(raw); !bytes.Equal(got, raw) {
		t.Fatalf("redactDebugJSONBytes(non-json) = %q, want %q", got, raw)
	}
}
