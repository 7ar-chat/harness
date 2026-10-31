package client_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/modelinfo"
)

func TestErrorMatchesByCodeWhateverTheMessageOrStatus(t *testing.T) {
	specific := client.Errorf(client.ErrNotFound, "no thread %q", "t1")
	foreign := &client.Error{Status: 418, Code: "not_found", Message: "from a server"}

	if !errors.Is(specific, client.ErrNotFound) || !errors.Is(foreign, client.ErrNotFound) {
		t.Fatal("errors of the same code must match the sentinel")
	}
	if errors.Is(specific, client.ErrConflict) {
		t.Fatal("errors of another code must not match")
	}
	if errors.Is(fmt.Errorf("plain"), client.ErrNotFound) || errors.Is(client.ErrNotFound, errors.New("not_found: Not found.")) {
		t.Fatal("only *client.Error values match")
	}
}

func TestErrorfCopiesTheSentinel(t *testing.T) {
	e := client.Errorf(client.ErrInvalidPath, "bad %s", "path")

	if e.Message != "bad path" || e.Status != 400 || e.Code != "invalid_path" {
		t.Fatalf("%+v", e)
	}
	if client.ErrInvalidPath.Message != "Invalid path." {
		t.Fatalf("Errorf modified the shared sentinel: %q", client.ErrInvalidPath.Message)
	}
	if e.Error() != "invalid_path: bad path" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestWrapKeepsTheCauseReachableAndTheSentinelUntouched(t *testing.T) {
	cause := errors.New("disk on fire")

	e := client.Wrap(client.ErrConflict, "could not save", cause)

	if !errors.Is(e, cause) || !errors.Is(e, client.ErrConflict) || e.Status != 409 || e.Message != "could not save" {
		t.Fatalf("%+v", e)
	}
	if client.ErrConflict.Message != "Conflict." || errors.Unwrap(client.ErrConflict) != nil {
		t.Fatalf("Wrap modified the shared sentinel: %+v", client.ErrConflict)
	}
}

func TestAsErrorFindsTheAPIErrorInAWrappedChain(t *testing.T) {
	wrapped := fmt.Errorf("loading: %w", client.ErrTooLarge)

	if e := client.AsError(wrapped); e == nil || e.Code != "too_large" {
		t.Fatalf("AsError(wrapped) = %v", e)
	}
	if client.AsError(errors.New("x")) != nil || client.AsError(nil) != nil {
		t.Fatal("a non-API error and nil are not API errors")
	}
}

func TestFromModelInfoKeepsEveryFieldAndCostAppliesTheTier(t *testing.T) {
	in := modelinfo.Info{Provider: "p", ContextWindow: 1000, MaxOutput: 200, Priced: true, InputPerToken: 1e-6, OutputPerToken: 2e-6,
		CacheReadPerToken: 1e-7, CacheWritePerToken: 3e-6, Vision: true, Reasoning: true, Tools: true,
		Tier: &modelinfo.Tier{Above: 100, Input: 4e-6, Output: 8e-6, CacheRead: 4e-7, CacheWrite: 1e-5}}

	got := client.FromModelInfo(in)

	want := client.ModelInfo{Provider: "p", ContextWindow: 1000, MaxOutput: 200, Priced: true, InputPerToken: 1e-6, OutputPerToken: 2e-6,
		CacheReadPerToken: 1e-7, CacheWritePerToken: 3e-6, Vision: true, Reasoning: true, Tools: true,
		Tier: &client.PriceTier{Above: 100, Input: 4e-6, Output: 8e-6, CacheRead: 4e-7, CacheWrite: 1e-5}}
	if fmt.Sprintf("%+v", got.Tier) != fmt.Sprintf("%+v", want.Tier) {
		t.Fatalf("tier %+v, want %+v", got.Tier, want.Tier)
	}
	got.Tier, want.Tier = nil, nil
	if got != want {
		t.Fatalf("converted %+v, want %+v", got, want)
	}
	if client.FromModelInfo(modelinfo.Info{}).Tier != nil {
		t.Fatal("a model without a tier must convert to none")
	}

	base, ok1 := client.FromModelInfo(in).Cost(50, 10, 0, 50)
	high, ok2 := client.FromModelInfo(in).Cost(500, 10, 0, 500)

	if !ok1 || !ok2 || base >= high {
		t.Fatalf("base %v, above the tier %v: the tier price must apply to a prompt above Above", base, high)
	}
	if want := 500*4e-6 + 10*8e-6; high < want-1e-12 || high > want+1e-12 {
		t.Fatalf("cost above the tier %v, want %v", high, want)
	}
}
