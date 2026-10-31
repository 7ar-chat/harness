package inproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/hosts/client"
)

func TestMapErrTranslatesEngineErrorsToTheAPIErrorsFrontEndsTest(t *testing.T) {
	for name, tc := range map[string]struct {
		in   error
		want *client.Error
	}{
		"store not found":            {store.ErrNotFound, client.ErrNotFound},
		"wrapped store not found":    {fmt.Errorf("loading: %w", store.ErrNotFound), client.ErrNotFound},
		"store conflict":             {store.ErrConflict, client.ErrConflict},
		"a run is active":            {store.ErrRunActive, client.ErrConflict},
		"invalid request":            {fmt.Errorf("%w: model nope", harness.ErrInvalidRequest), client.ErrInvalidRequest},
		"steers closed":              {queue.ErrSteersClosed, client.ErrRunNotActive},
		"too many steers":            {queue.ErrTooManySteers, client.ErrTooManySteers},
		"a file that does not exist": {sandbox.ErrNotFound, client.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			got := mapErr(tc.in)

			if !errors.Is(got, tc.want) || client.AsError(got).Status != tc.want.Status {
				t.Fatalf("mapErr(%v) = %v, want %s (%d)", tc.in, got, tc.want.Code, tc.want.Status)
			}
		})
	}
}

func TestMapErrKeepsTheMessageOfAnInvalidRequest(t *testing.T) {
	cause := fmt.Errorf("%w: attachment %q is empty", harness.ErrInvalidRequest, "a.png")

	got := mapErr(cause)

	e := client.AsError(got)
	if e == nil || e.Message != cause.Error() || !errors.Is(got, harness.ErrInvalidRequest) {
		t.Fatalf("the user must see why the request was refused, and the cause stay reachable: %v", got)
	}
}

func TestMapErrHidesUnexpectedErrorsBehindAnInternalError(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, cause := range map[string]error{
		"plain":     errors.New("pq: password authentication failed for user admin"),
		"cancelled": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			got := mapErr(cause)

			e := client.AsError(got)
			if e == nil || e.Status != 500 || e.Code != "internal" || e.Message != "An internal error occurred." {
				t.Fatalf("mapErr = %#v", got)
			}
			if !errors.Is(got, cause) {
				t.Fatal("the original error must stay reachable for logs")
			}
			if errors.Is(got, client.ErrNotFound) || errors.Is(got, client.ErrConflict) {
				t.Fatal("an unexpected error must not look like a client error")
			}
		})
	}
}

func TestMapErrPassesAPIErrorsAndNilThrough(t *testing.T) {
	specific := client.Errorf(client.ErrInvalidPath, "bad")

	if mapErr(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	if got := mapErr(fmt.Errorf("wrapped: %w", specific)); client.AsError(got) != specific {
		t.Fatalf("an API error must not be rewrapped: %v", got)
	}
}

func TestHistoryErrIsSpecificForTheHistoryOperations(t *testing.T) {
	for name, tc := range map[string]struct {
		in      error
		want    *client.Error
		message string
	}{
		"run in progress":  {store.ErrRunActive, client.ErrConflict, "The thread has a run in progress."},
		"nothing to undo":  {harness.ErrNothingToUndo, client.ErrConflict, "Nothing to undo."},
		"nothing to redo":  {harness.ErrNothingToRedo, client.ErrConflict, "Nothing to redo."},
		"store without it": {harness.ErrHistoryUnsupported, client.ErrNotSupported, client.ErrNotSupported.Message},
		"missing thread":   {store.ErrNotFound, client.ErrNotFound, client.ErrNotFound.Message},
	} {
		t.Run(name, func(t *testing.T) {
			got := historyErr(fmt.Errorf("op: %w", tc.in))

			e := client.AsError(got)
			if !errors.Is(got, tc.want) || e.Message != tc.message {
				t.Fatalf("historyErr = %v, want %s %q", got, tc.want.Code, tc.message)
			}
		})
	}
}

func TestLimitOfDefaultsAndCaps(t *testing.T) {
	for _, tc := range []struct{ in, def, want int }{
		{0, 50, 50}, {-3, 50, 50}, {1, 50, 1}, {200, 50, 200}, {201, 50, 200}, {1 << 30, 50, 200},
	} {
		if got := limitOf(tc.in, tc.def); got != tc.want {
			t.Errorf("limitOf(%d, %d) = %d, want %d", tc.in, tc.def, got, tc.want)
		}
	}
}

func TestToAuthorIsNilForMessagesWithoutARecordedAuthor(t *testing.T) {
	if toAuthor(&store.Message{}) != nil {
		t.Fatal("a model message has no author")
	}
	got := toAuthor(&store.Message{AuthorLogin: "bot", AuthorKind: "supervisor", OnBehalfOf: "alice"})
	if got == nil || got.Login != "bot" || got.Kind != "supervisor" || got.OnBehalfOf != "alice" {
		t.Fatalf("author %+v", got)
	}
	if toAuthor(&store.Message{AuthorKind: "user"}) == nil {
		t.Fatal("a kind alone still identifies an author")
	}
}
