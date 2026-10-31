package memstore_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/conformance"
	"github.com/7ar-chat/harness/adapters/store/memstore"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Backend {
		s := memstore.New()
		var skew time.Duration
		return conformance.Backend{
			Store: s,
			Advance: func(t *testing.T, d time.Duration) {
				skew += d
				s.Now = func() time.Time { return time.Now().Add(skew) }
			},
		}
	})
}
