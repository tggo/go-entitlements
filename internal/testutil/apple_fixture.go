package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// AppleFixture is the shape of testdata/apple/*.json: the decoded notification
// with the nested transaction and renewal info kept as plain JSON objects, so
// they can be signed at test time.
type AppleFixture struct {
	Notification map[string]any `json:"notification"`
	Transaction  map[string]any `json:"transaction,omitempty"`
	Renewal      map[string]any `json:"renewal,omitempty"`
}

// LoadAppleFixture reads testdata/apple/<name>.json from the repo root.
func LoadAppleFixture(tb testing.TB, name string) AppleFixture {
	tb.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "apple", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	var f AppleFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		tb.Fatalf("fixture %s: %v", name, err)
	}
	return f
}

// SignBody signs the fixture's nested transaction/renewal info and the outer
// payload with ca and returns the ResponseBodyV2 JSON an App Store would POST.
func (f AppleFixture) SignBody(tb testing.TB, ca *AppleCA) []byte {
	tb.Helper()
	n := map[string]any{}
	for k, v := range f.Notification {
		n[k] = v
	}
	data, _ := n["data"].(map[string]any)
	if f.Transaction != nil || f.Renewal != nil {
		d := map[string]any{}
		for k, v := range data {
			d[k] = v
		}
		if f.Transaction != nil {
			d["signedTransactionInfo"] = ca.Sign(tb, f.Transaction)
		}
		if f.Renewal != nil {
			d["signedRenewalInfo"] = ca.Sign(tb, f.Renewal)
		}
		n["data"] = d
	}
	body, err := json.Marshal(map[string]string{"signedPayload": ca.Sign(tb, n)})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

// Data returns the notification's "data" object, for tests that tweak it.
func (f AppleFixture) Data() map[string]any {
	d, ok := f.Notification["data"].(map[string]any)
	if !ok {
		panic("fixture has no data object")
	}
	return d
}

// ShiftTimes moves every millisecond date field (keys ending in "Date") of the
// fixture's notification, transaction and renewal info by d. Demos use it to
// make a fixture's dates relative to now.
func (f AppleFixture) ShiftTimes(d time.Duration) {
	for _, m := range []map[string]any{f.Notification, f.Transaction, f.Renewal, f.Data()} {
		for k, v := range m {
			if n, ok := v.(float64); ok && strings.HasSuffix(k, "Date") && n != 0 {
				m[k] = n + float64(d.Milliseconds())
			}
		}
	}
}
