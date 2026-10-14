package memory_test

import (
	"testing"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/store/memory"
	"github.com/tggo/go-entitlements/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) entitlements.Store { return memory.New() })
}
