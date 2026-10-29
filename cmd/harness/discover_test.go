package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestDiscoverModelsOpenAIAndAnthropic(t *testing.T) {
	var gotPath, gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotKey = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		if r.URL.Path == "/bad/models" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"b"},{"id":"a"},{"id":""}]}`))
	}))
	defer srv.Close()

	ids, err := discoverModels(context.Background(), srv.Client(), "openai", srv.URL+"/v1/", "sk")
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) || gotPath != "/v1/models" || gotAuth != "Bearer sk" {
		t.Fatalf("openai: %v %v %q %q", ids, err, gotPath, gotAuth)
	}
	ids, err = discoverModels(context.Background(), srv.Client(), "anthropic", srv.URL, "ak")
	if err != nil || len(ids) != 2 || gotPath != "/v1/models" || gotKey != "ak" {
		t.Fatalf("anthropic: %v %v %q %q", ids, err, gotPath, gotKey)
	}
	if _, err = discoverModels(context.Background(), srv.Client(), "openai", srv.URL+"/bad", "sk"); err == nil {
		t.Fatal("a 401 must be an error")
	}
}
