package artifacts_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
)

func TestEachOfficePreviewConvertsWithItsOwnLibreOfficeProfile(t *testing.T) {
	// Office previews run LibreOffice with a fresh user profile per conversion (--norestore), never one shared profile: parallel previews cannot collide.
	// Arrange
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	backend := sandboxtest.NewBackend()
	mgr := sandbox.NewManager(backend, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	svc := artifacts.New(st, artifacts.PGBlobs{S: st}, mgr, artifacts.Options{})
	th, _ := st.CreateThread(ctx, "alice", "t")
	if _, err := mgr.Acquire(ctx, th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	sb := backend.Created()[0]
	pdf, _ := os.ReadFile(filepath.Join("testdata", "memo.pdf"))
	var mu sync.Mutex
	var conversions []string
	sb.SetExec(sandboxtest.FindExec(sb, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if !strings.HasPrefix(req.Command, "soffice ") {
			return sandbox.ExecResult{}, nil
		}
		mu.Lock()
		conversions = append(conversions, req.Command)
		mu.Unlock()
		fields := strings.Fields(req.Command)
		src := strings.Trim(fields[len(fields)-1], "'")
		sb.Put(strings.TrimSuffix(src, filepath.Ext(src))+".pdf", string(pdf))
		return sandbox.ExecResult{}, nil
	}))
	for _, name := range []string{"report.docx", "deck.pptx"} {
		data, _ := os.ReadFile(filepath.Join("testdata", name))
		sb.Put("/workspace/"+name, string(data))
	}
	changes, err := svc.Snapshot(ctx, sb, artifacts.RunRef{ThreadID: th.ID, Login: "alice"})
	if err != nil || len(changes) != 2 {
		t.Fatalf("snapshot %d %v", len(changes), err)
	}

	// Act
	for _, c := range changes {
		if _, err := svc.Preview(ctx, c.Artifact, c.Artifact.Version); err != nil {
			t.Fatalf("preview of %s: %v", c.Artifact.Path, err)
		}
	}

	// Assert
	seen := map[string]bool{}
	for _, cmd := range conversions {
		profile := ""
		for _, f := range strings.Fields(cmd) {
			if v, ok := strings.CutPrefix(f, "-env:UserInstallation="); ok {
				profile = v
			}
		}
		if profile == "" || seen[profile] || !strings.Contains(cmd, "--norestore") {
			t.Fatalf("conversions %q", conversions)
		}
		seen[profile] = true
	}
	if len(conversions) != 2 {
		t.Fatalf("conversions %q", conversions)
	}
}
