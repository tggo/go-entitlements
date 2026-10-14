package memory_test

import (
	"testing"

	"github.com/tggo/go-entitlements/inbox"
	"github.com/tggo/go-entitlements/inbox/memory"
	"github.com/tggo/go-entitlements/inboxtest"
)

func TestConformance(t *testing.T) {
	inboxtest.Run(t, func(*testing.T) inbox.Inbox { return memory.New() })
}
