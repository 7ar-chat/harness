package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/hosts/client"
)

// sseServer serves one scripted response per connection; the last script repeats.
type sseServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []seen
}

type script func(w http.ResponseWriter)

func newSSEServer(t *testing.T, scripts ...script) *sseServer {
	t.Helper()
	s := &sseServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.got)
		s.got = append(s.got, seen{Path: r.URL.EscapedPath(), Query: r.URL.Query().Get("after"), Auth: r.Header.Get("Last-Event-ID"), ContentType: r.Header.Get("Accept")})
		s.mu.Unlock()
		scripts[min(n, len(scripts)-1)](w)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sseServer) connections() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.got...)
}

// frames writes raw text as an event stream.
func frames(raw string) script {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(raw))
		w.(http.Flusher).Flush()
	}
}

func status(code int, body string) script {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

var fastBackoff = client.WithBackoff(time.Millisecond, 2*time.Millisecond)

func drain(t *testing.T, ch <-chan client.Event) []client.Event {
	t.Helper()
	var out []client.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("the stream stayed open after %d events", len(out))
		}
	}
}

func TestHTTPEventsParseTheSSEFraming(t *testing.T) {
	// Arrange: comments, CRLF line ends, "data:" without a space, a multi-line payload, an unknown field.
	raw := ": connected\n\n" +
		"id: 1\r\ndata: {\"type\":\"RUN_STARTED\"}\r\n\r\n" +
		": ping\n\n" +
		"event: ignored\nid:2\ndata:{\"type\":\"TEXT_MESSAGE_CONTENT\",\ndata: \"delta\":\"x\"}\n\n" +
		"id: 3\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"
	s := newSSEServer(t, frames(raw))

	// Act
	ch, err := client.HTTP(s.srv.URL, "").Events(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	// Assert
	var types []string
	for _, e := range evs {
		types = append(types, fmt.Sprintf("%d:%s", e.Seq, e.Type))
	}
	if fmt.Sprint(types) != "[1:RUN_STARTED 2:TEXT_MESSAGE_CONTENT 3:RUN_FINISHED]" {
		t.Fatalf("events %v", types)
	}
	var multi struct{ Delta string }
	if err := json.Unmarshal(evs[1].Data, &multi); err != nil || multi.Delta != "x" {
		t.Fatalf("the data lines of one frame must be joined by a newline: %q (%v)", evs[1].Data, err)
	}
	if got := s.connections(); len(got) != 1 {
		t.Fatalf("a terminal event must end the stream without reconnecting: %d connections", len(got))
	}
}

func TestHTTPEventsRequestTheRunStreamWithTheTokenAndAcceptHeader(t *testing.T) {
	s := newSSEServer(t, frames("id: 1\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"))

	ch, err := client.HTTP(s.srv.URL, "").Events(context.Background(), "run/1", 0)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)

	got := s.connections()[0]
	if got.Path != "/api/runs/run%2F1/events" || got.ContentType != "text/event-stream" || got.Auth != "" {
		t.Fatalf("request %+v: no Last-Event-ID for a stream from the start", got)
	}
}

func TestHTTPEventsKeepFramesThatCarryNoValidSeqOrJSON(t *testing.T) {
	// Arrange: a frame without id, one with a garbage id, one that is not JSON, then the terminal one.
	raw := "data: {\"type\":\"CUSTOM\"}\n\n" +
		"id: abc\ndata: {\"type\":\"CUSTOM\"}\n\n" +
		"id: 5\ndata: not json\n\n" +
		"id: 6\ndata: {\"type\":\"RUN_ERROR\"}\n\n"
	s := newSSEServer(t, frames(raw))

	ch, err := client.HTTP(s.srv.URL, "").Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 4 {
		t.Fatalf("events %+v", evs)
	}
	if evs[0].Seq != 0 || evs[0].Type != "CUSTOM" || evs[1].Seq != 0 {
		t.Fatalf("frames without a valid id are delivered with seq 0: %+v %+v", evs[0], evs[1])
	}
	if evs[2].Seq != 5 || evs[2].Type != "" || string(evs[2].Data) != "not json" {
		t.Fatalf("a non-JSON frame keeps its raw data and has no type: %+v", evs[2])
	}
	if evs[3].Seq != 6 || evs[3].Type != "RUN_ERROR" {
		t.Fatalf("RUN_ERROR is terminal: %+v", evs[3])
	}
}

func TestHTTPEventsDropAFrameCutOffBeforeItsBlankLine(t *testing.T) {
	// Arrange: the connection dies in the middle of event 2; the reconnection redelivers it whole.
	s := newSSEServer(t,
		frames("id: 1\ndata: {\"type\":\"RUN_STARTED\"}\n\nid: 2\ndata: {\"type\":\"TEXT_MESS"),
		frames("id: 2\ndata: {\"type\":\"TEXT_MESSAGE_CONTENT\"}\n\nid: 3\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"),
	)

	ch, err := client.HTTP(s.srv.URL, "", fastBackoff).Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	var seqs []string
	for _, e := range evs {
		seqs = append(seqs, fmt.Sprintf("%d:%s", e.Seq, e.Type))
	}
	if fmt.Sprint(seqs) != "[1:RUN_STARTED 2:TEXT_MESSAGE_CONTENT 3:RUN_FINISHED]" {
		t.Fatalf("a half-received frame must never be delivered: %v", seqs)
	}
	if got := s.connections(); got[1].Auth != "1" {
		t.Fatalf("the reconnection must resume after the last complete event: Last-Event-ID %q", got[1].Auth)
	}
}

func TestHTTPEventsSkipEventsAlreadyDeliveredBeforeAReconnection(t *testing.T) {
	// Arrange: the server replays from the start on reconnection, ignoring Last-Event-ID.
	s := newSSEServer(t,
		frames("id: 1\ndata: {\"type\":\"A\"}\n\nid: 2\ndata: {\"type\":\"B\"}\n\n"),
		frames("id: 1\ndata: {\"type\":\"A\"}\n\nid: 2\ndata: {\"type\":\"B\"}\n\nid: 3\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"),
	)

	ch, err := client.HTTP(s.srv.URL, "", fastBackoff).Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 3 || evs[0].Seq != 1 || evs[1].Seq != 2 || evs[2].Seq != 3 {
		t.Fatalf("no duplicate may reach the consumer: %+v", evs)
	}
}

func TestHTTPEventsSkipEventsAtOrBelowTheStartingSeq(t *testing.T) {
	s := newSSEServer(t, frames("id: 4\ndata: {\"type\":\"OLD\"}\n\nid: 5\ndata: {\"type\":\"OLD\"}\n\nid: 6\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"))

	ch, err := client.HTTP(s.srv.URL, "").Events(context.Background(), "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 1 || evs[0].Seq != 6 {
		t.Fatalf("events %+v", evs)
	}
}

func TestHTTPEventsAnUnknownRunFailsBeforeTheChannelIsReturned(t *testing.T) {
	s := newSSEServer(t, status(404, `{"error":{"code":"not_found","message":"Not found."}}`))

	ch, err := client.HTTP(s.srv.URL, "").Events(context.Background(), "nope", 0)

	if ch != nil || !isCode(err, client.ErrNotFound) {
		t.Fatalf("channel %v, error %v", ch, err)
	}
}

func isCode(err error, want *client.Error) bool {
	e := client.AsError(err)
	return e != nil && e.Code == want.Code
}

func lostCode(t *testing.T, ev client.Event) (code, message string) {
	t.Helper()
	var e struct{ Type, Code, Message string }
	if err := json.Unmarshal(ev.Data, &e); err != nil || e.Type != "RUN_ERROR" || ev.Type != "RUN_ERROR" {
		t.Fatalf("not a RUN_ERROR event: %+v (%v)", ev, err)
	}
	return e.Code, e.Message
}

func TestHTTPEventsAClientErrorOnReconnectionEndsTheStreamAtOnce(t *testing.T) {
	// Arrange: the run is gone (404) when the client comes back; retrying could never succeed.
	s := newSSEServer(t,
		frames("id: 1\ndata: {\"type\":\"RUN_STARTED\"}\n\n"),
		status(404, `{"error":{"code":"not_found","message":"Not found."}}`),
	)
	c := client.HTTP(s.srv.URL, "", fastBackoff, client.WithMaxReconnects(50))

	ch, err := c.Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 2 || evs[0].Seq != 1 {
		t.Fatalf("events %+v", evs)
	}
	if code, msg := lostCode(t, evs[1]); code != "stream_lost" || !strings.Contains(msg, "not_found") {
		t.Fatalf("code %q message %q: the cause must be reported", code, msg)
	}
	if n := len(s.connections()); n != 2 {
		t.Fatalf("%d connections: a 4xx is not retried", n)
	}
}

func TestHTTPEventsRetryServerErrorsAndRateLimits(t *testing.T) {
	// Arrange: after the first event the server answers 429, then 503, then streams the rest.
	s := newSSEServer(t,
		frames("id: 1\ndata: {\"type\":\"RUN_STARTED\"}\n\n"),
		status(429, ""),
		status(503, "busy"),
		frames("id: 2\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"),
	)

	ch, err := client.HTTP(s.srv.URL, "", fastBackoff).Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 2 || evs[1].Type != "RUN_FINISHED" {
		t.Fatalf("events %+v", evs)
	}
	got := s.connections()
	if len(got) != 4 || got[1].Auth != "1" || got[2].Auth != "1" || got[3].Auth != "1" {
		t.Fatalf("every retry must resume from seq 1: %+v", got)
	}
}

func TestHTTPEventsADeliveredEventResetsTheReconnectBudget(t *testing.T) {
	// Arrange: with a budget of one retry, every connection delivers one new event then drops. Each
	// reconnection succeeds, so the stream outlives the budget.
	s := newSSEServer(t,
		frames("id: 1\ndata: {\"type\":\"A\"}\n\n"),
		frames("id: 2\ndata: {\"type\":\"A\"}\n\n"),
		frames("id: 3\ndata: {\"type\":\"A\"}\n\n"),
		frames("id: 4\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"),
	)

	ch, err := client.HTTP(s.srv.URL, "", fastBackoff, client.WithMaxReconnects(1)).Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 4 || evs[3].Type != "RUN_FINISHED" {
		t.Fatalf("events %+v", evs)
	}
}

func TestHTTPEventsAConnectionThatDeliversNothingNewDoesNotResetTheBudget(t *testing.T) {
	// Arrange: every reconnection streams only an already delivered event, then drops.
	s := newSSEServer(t,
		frames("id: 1\ndata: {\"type\":\"A\"}\n\n"),
		frames("id: 1\ndata: {\"type\":\"A\"}\n\n"),
	)

	ch, err := client.HTTP(s.srv.URL, "", fastBackoff, client.WithMaxReconnects(3)).Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	code, msg := lostCode(t, evs[1])
	if code != "stream_lost" || !strings.Contains(msg, "3 reconnection attempts") || !strings.Contains(msg, "run r") {
		t.Fatalf("code %q message %q", code, msg)
	}
	if n := len(s.connections()); n != 4 {
		t.Fatalf("%d connections, want the first plus 3 attempts", n)
	}
}

func TestHTTPEventsGiveUpWhenConnectionsAreRefused(t *testing.T) {
	// Arrange: the first connection works, then the server goes away entirely.
	var s *sseServer
	s = newSSEServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("id: 1\ndata: {\"type\":\"A\"}\n\n"))
		w.(http.Flusher).Flush()
		go s.srv.CloseClientConnections()
	})
	hc := &http.Client{Transport: &http.Transport{}}
	c := client.HTTP(s.srv.URL, "", fastBackoff, client.WithMaxReconnects(2), client.WithHTTPClient(hc))

	ch, err := c.Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	var evs []client.Event
	for ev := range ch {
		evs = append(evs, ev)
		if len(evs) == 1 {
			s.srv.Close() // refuse every later connection
		}
	}

	if len(evs) != 2 || evs[0].Seq != 1 {
		t.Fatalf("events %+v", evs)
	}
	if code, _ := lostCode(t, evs[1]); code != "stream_lost" {
		t.Fatalf("code %q", code)
	}
}

func TestHTTPEventsCancelDuringTheBackoffClosesTheChannelWithoutAnError(t *testing.T) {
	// Arrange: after the first event the connection drops and the client waits a long backoff.
	s := newSSEServer(t, frames("id: 1\ndata: {\"type\":\"A\"}\n\n"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := client.HTTP(s.srv.URL, "", client.WithBackoff(time.Hour, time.Hour))
	ch, err := c.Events(ctx, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Seq != 1 {
		t.Fatalf("first event %+v", ev)
	}

	// Act
	cancel()
	rest := drain(t, ch)

	// Assert
	if len(rest) != 0 {
		t.Fatalf("a cancelled stream must close silently, got %+v", rest)
	}
	if n := len(s.connections()); n != 1 {
		t.Fatalf("%d connections: nothing may reconnect after the cancel", n)
	}
}

func TestHTTPEventsCancelWhileTheConsumerIsNotReadingClosesTheChannel(t *testing.T) {
	// Arrange: the server sends two events; nobody reads the channel, so the pump blocks on the first.
	s := newSSEServer(t, frames("id: 1\ndata: {\"type\":\"A\"}\n\nid: 2\ndata: {\"type\":\"A\"}\n\n"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := client.HTTP(s.srv.URL, "").Events(ctx, "r", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	cancel()

	// Assert: the channel closes (with at most the one event that raced the cancel).
	var n int
	deadline := time.After(10 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
			if open {
				n++
			}
		case <-deadline:
			t.Fatal("the channel stayed open: the pump leaked")
		}
	}
	if n > 1 {
		t.Fatalf("%d events after a cancel", n)
	}
}

func TestHTTPEventsAMidStreamErrorFrameIsDeliveredAsData(t *testing.T) {
	// Arrange: the run fails; RUN_ERROR from the server is a regular terminal event, not stream_lost.
	s := newSSEServer(t, frames("id: 1\ndata: {\"type\":\"RUN_ERROR\",\"code\":\"model_failed\",\"message\":\"x\"}\n\n"))

	ch, err := client.HTTP(s.srv.URL, "").Events(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)

	if len(evs) != 1 || evs[0].Seq != 1 {
		t.Fatalf("events %+v", evs)
	}
	if code, _ := lostCode(t, evs[0]); code != "model_failed" {
		t.Fatalf("the server's own error code must pass through: %q", code)
	}
}
