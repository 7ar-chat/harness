package githoststore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/githoststore"
	"github.com/7ar-chat/harness/core/githost"
)

func exercise(t *testing.T, r githost.Registry) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Get(ctx, "a.example.com"); !errors.Is(err, githost.ErrUnknownHost) {
		t.Fatalf("get missing: %v", err)
	}
	h := githost.Host{Name: "b.example.com", Kind: githost.KindGitLab, BaseURL: "https://b.example.com"}
	if err := r.Put(ctx, githost.Entry{Host: h, SealedToken: []byte("sealed")}); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(ctx, githost.Entry{Host: githost.Host{Name: "a.example.com", Kind: githost.KindGitea}}); err != nil {
		t.Fatal(err)
	}
	list, err := r.List(ctx)
	if err != nil || len(list) != 2 || list[0].Host.Name != "a.example.com" || string(list[1].SealedToken) != "sealed" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := r.Delete(ctx, "a.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "a.example.com"); err != nil {
		t.Fatal("deleting an absent host is not an error")
	}
	if list, _ := r.List(ctx); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
}

func TestMem(t *testing.T) { exercise(t, githoststore.NewMem(nil)) }

func TestFilePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "githosts.json")
	f, err := githoststore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, f)
	again, err := githoststore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := again.Get(context.Background(), "b.example.com")
	if err != nil || string(e.SealedToken) != "sealed" {
		t.Fatalf("reloaded = %+v, %v", e, err)
	}
}
