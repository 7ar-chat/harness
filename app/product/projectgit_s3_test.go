package product

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/blobs/s3blobs"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

// The project files against a real S3-compatible store (RustFS), where a key keeps its first value: run with
// HARNESS_TEST_S3_ENDPOINT, HARNESS_TEST_S3_ACCESS_KEY and HARNESS_TEST_S3_SECRET_KEY.
func TestProjectFilesRoundTripThroughAnS3Store(t *testing.T) {
	endpoint := os.Getenv("HARNESS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("HARNESS_TEST_S3_ENDPOINT is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s3, err := s3blobs.New(s3blobs.Options{Bucket: "harness-project-test", Endpoint: endpoint, AccessKey: os.Getenv("HARNESS_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("HARNESS_TEST_S3_SECRET_KEY"), PathStyle: true, Prefix: "t" + time.Now().Format("150405") + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s3.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	tm := teamstore.NewMem()
	cnUsers(t, tm, "alice")
	p, _ := tm.CreateProject(ctx, store.Project{Name: "site"}, "alice")
	rc := &RepoCheckout{Team: tm, Blobs: s3}
	if err := SeedProjectTemplate(ctx, s3, p.ID, "static-html"); err != nil {
		t.Fatal(err)
	}

	// Five saves, each from a fresh sandbox that restored the previous one: every save must be the one read back.
	var last string
	for i := 0; i < 5; i++ {
		sb := newHostSandbox(t)
		rc.setupLocal(ctx, "thread-"+string(rune('a'+i)), sb, p)
		last = strings.Repeat("v", i+1)
		if err := sb.WriteFile(ctx, RepoDir+"/version.txt", []byte(last)); err != nil {
			t.Fatal(err)
		}
		if _, err := rc.PublishLocal(ctx, "thread-"+string(rune('a'+i)), sb, p); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	check := newHostSandbox(t)
	rc.setupLocal(ctx, "thread-z", check, p)
	if got := readHost(t, check, "version.txt"); got != last {
		t.Errorf("restored %q, want the last save %q", got, last)
	}
	if !strings.Contains(readHost(t, check, "index.html"), "Hello") {
		t.Error("the template files were lost")
	}
	DeleteProjectFiles(ctx, s3, p.ID)
	for slot := 0; slot < 2; slot++ {
		if ok, _ := s3.Has(ctx, localBundleKey(p.ID, slot)); ok {
			t.Errorf("slot %d survived the deletion", slot)
		}
	}
}
