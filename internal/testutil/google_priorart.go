package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// PriorArtFixture returns the bytes of testdata/prior-art/<rel>.
func PriorArtFixture(tb testing.TB, rel string) []byte {
	tb.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	b, err := os.ReadFile(filepath.Join(root, "testdata", "prior-art", rel))
	if err != nil {
		tb.Fatalf("read prior-art fixture %s: %v", rel, err)
	}
	return b
}
