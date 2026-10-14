package testutil

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// GoogleFixture returns the raw bytes of testdata/google/<name>.
func GoogleFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	b, err := os.ReadFile(filepath.Join(root, "testdata", "google", name))
	if err != nil {
		tb.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// GooglePushBody wraps a payload in a Pub/Sub push envelope with the given messageId.
func GooglePushBody(tb testing.TB, messageID string, payload []byte) []byte {
	tb.Helper()
	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":        base64.StdEncoding.EncodeToString(payload),
			"messageId":   messageID,
			"publishTime": "2026-10-14T10:00:00Z",
		},
		"subscription": "projects/p/subscriptions/s",
	})
	if err != nil {
		tb.Fatalf("marshal envelope: %v", err)
	}
	return body
}
