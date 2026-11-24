package notify_test

import (
	"testing"

	"github.com/7ar-chat/harness/core/notify"
	"github.com/7ar-chat/harness/core/notify/notifytest"
)

func TestMemStoreConformance(t *testing.T) {
	notifytest.Run(t, func(*testing.T) notify.Store { return notify.NewMemStore(nil) })
}
