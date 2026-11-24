package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
)

// A worker uses the same S3 content store as the web replicas (ARTIFACTS_S3_*): found by the stack e2e, where the
// artifacts of a run executed by the worker went to the database while the replicas looked in S3 first.
func TestWorkerBuildsTheS3BlobStoreFromTheEnvironment(t *testing.T) {
	// Arrange: a fake S3 endpoint that says the bucket exists
	quietEnv(t)
	var heads atomic.Int32
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()
	t.Setenv("ARTIFACTS_S3_BUCKET", "bucket")
	t.Setenv("ARTIFACTS_S3_ENDPOINT", s3.URL)
	t.Setenv("ARTIFACTS_S3_CREATE_BUCKET", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	b, err := buildHarness(context.Background(), cfg, Options{Standalone: true, Store: memstore.New(), Models: llmtest.Factory{M: llmtest.New()}}, newMetrics())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	if heads.Load() == 0 {
		t.Fatal("the worker never contacted the configured S3 endpoint")
	}
}
