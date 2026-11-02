package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/hosts/client"
)

func sseEvent(w http.ResponseWriter, seq int, typ string) {
	fmt.Fprintf(w, "id: %d\ndata: {\"type\":%q,\"n\":%d}\n\n", seq, typ, seq)
	w.(http.Flusher).Flush()
}

func TestHTTPEventsReconnectWithLastEventIDWithoutGapsOrDuplicates(t *testing.T) {
	// Arrange: the first connection dies after seq 2; the second one repeats seq 2 and finishes.
	var mu sync.Mutex
	var lastIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		n := len(lastIDs)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if n == 1 {
			sseEvent(w, 1, "RUN_STARTED")
			sseEvent(w, 2, "TEXT_MESSAGE_CONTENT")
			return // the connection drops
		}
		sseEvent(w, 2, "TEXT_MESSAGE_CONTENT")
		sseEvent(w, 3, "TEXT_MESSAGE_CONTENT")
		sseEvent(w, 4, "RUN_FINISHED")
	}))
	defer srv.Close()
	c := client.HTTP(srv.URL, "", client.WithBackoff(time.Millisecond, 5*time.Millisecond))

	// Act
	ch, err := c.Events(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for ev := range ch {
		seqs = append(seqs, ev.Seq)
	}

	// Assert
	if fmt.Sprint(seqs) != "[1 2 3 4]" {
		t.Fatalf("seqs %v", seqs)
	}
	if fmt.Sprint(lastIDs) != "[ 2]" {
		t.Fatalf("Last-Event-ID headers %q: the reconnection must resume from the last seq", lastIDs)
	}
}

func TestHTTPEventsAfterIsSentAsLastEventID(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, 8, "RUN_FINISHED")
	}))
	defer srv.Close()

	ch, err := client.HTTP(srv.URL, "").Events(context.Background(), "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	ev := <-ch

	if got != "7" || ev.Seq != 8 || ev.Type != "RUN_FINISHED" {
		t.Fatalf("Last-Event-ID %q, event %+v", got, ev)
	}
}

func TestHTTPEventsGiveUpWithARunErrorWhenTheServerStaysDown(t *testing.T) {
	// Arrange: one event, then the server drops every connection.
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if !first {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, 1, "RUN_STARTED")
	}))
	defer srv.Close()
	c := client.HTTP(srv.URL, "", client.WithBackoff(time.Millisecond, 2*time.Millisecond), client.WithMaxReconnects(3))

	// Act
	ch, err := c.Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	var evs []client.Event
	for ev := range ch {
		evs = append(evs, ev)
	}

	// Assert
	if len(evs) != 2 || evs[0].Type != "RUN_STARTED" || evs[1].Type != "RUN_ERROR" {
		t.Fatalf("events %+v", evs)
	}
	var e struct{ Code string }
	_ = json.Unmarshal(evs[1].Data, &e)
	if e.Code != "stream_lost" {
		t.Fatalf("error event %s", evs[1].Data)
	}
}

func TestHTTPEventsStopWhenTheContextIsCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, 1, "RUN_STARTED")
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := client.HTTP(srv.URL, "").Events(ctx, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-ch

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an event after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open")
	}
}

func TestHTTPSendsTheBearerTokenAndMapsAnAuthenticationFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"Authentication required."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":["m"],"default":"m"}`))
	}))
	defer srv.Close()

	_, bad := client.HTTP(srv.URL, "bad").Models(context.Background())
	ok, err := client.HTTP(srv.URL+"/", "good").Models(context.Background())

	if !errors.Is(bad, client.ErrUnauthenticated) || client.AsError(bad).Status != 401 {
		t.Fatalf("bad token: %v", bad)
	}
	if err != nil || ok.Default != "m" {
		t.Fatalf("good token: %+v, %v", ok, err)
	}
}

func TestHTTPRunRequestKeepsTheWireFormatAndForwardsTheSandboxField(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"id":"r1","threadId":"t1","status":"queued","model":"m","lastSeq":0,"createdAt":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	run, err := client.HTTP(srv.URL, "").Send(context.Background(), "t1", "hi",
		client.SendOptions{Model: "m", ReasoningEffort: "low", Skills: []string{"s"}, Interactive: true, Sandbox: "e2b"})

	if err != nil || run.ID != "r1" {
		t.Fatalf("%+v, %v", run, err)
	}
	for k, want := range map[string]any{"message": "hi", "model": "m", "reasoningEffort": "low", "interactive": true, "sandbox": "e2b"} {
		if body[k] != want {
			t.Errorf("body[%s] = %v, want %v", k, body[k], want)
		}
	}
	if s, _ := body["skills"].([]any); len(s) != 1 || s[0] != "s" {
		t.Errorf("skills = %v", body["skills"])
	}
}
