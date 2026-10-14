// Command sign-fixture is a LOCAL DEMO tool. It generates a throwaway Apple-shaped
// CA, signs fixtures from testdata/apple with it and writes
//
//	<out>/root.pem          the root to give the demo server as APPLE_ROOT_CA_PEM
//	<out>/<fixture>.json    a signed request body to POST to /webhooks/apple
//
// The leaf certificate is valid for one hour. Never use this CA in production.
//
//	go run ./examples/sign-fixture -out /tmp/demo subscribed did_renew refund
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/tggo/go-entitlements/internal/testutil"
)

// tb lets the test helpers run outside `go test`.
type tb struct{ testing.TB }

func (tb) Helper()                   {}
func (tb) Fatal(args ...any)         { log.Fatal(args...) }
func (tb) Fatalf(f string, a ...any) { log.Fatalf(f, a...) }

func main() {
	out := flag.String("out", "demo", "output directory")
	flag.Parse()
	names := flag.Args()
	if len(names) == 0 {
		names = []string{"subscribed"}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	t := tb{}
	ca := testutil.NewAppleCA(t)
	rootPath := filepath.Join(*out, "root.pem")
	if err := os.WriteFile(rootPath, ca.RootPEM(), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", rootPath)
	for _, n := range names {
		body := testutil.LoadAppleFixture(t, n).SignBody(t, ca)
		p := filepath.Join(*out, n+".json")
		if err := os.WriteFile(p, body, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", p)
	}
}
