package filestore

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Backend {
		dir := t.TempDir()
		s := open(t, dir)
		t.Cleanup(func() { _ = s.Close() })
		var skew time.Duration
		advance := func(t *testing.T, d time.Duration) {
			skew += d
			s.Now = func() time.Time { return time.Now().Add(skew) }
		}
		return conformance.Backend{
			Store:   s,
			Advance: advance,
			Reopen: func(t *testing.T) conformance.Store {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = open(t, dir)
				return s
			},
		}
	})
}
