package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// maxBody is the largest delivery accepted (GitHub caps its payloads at 25 MiB;
// the events the factory reads are far smaller).
const maxBody = 1 << 20

// Webhook is the http.Handler of GitHub webhook deliveries.
type Webhook struct {
	o  Options
	wg sync.WaitGroup
}

// NewWebhook returns the webhook handler. An empty Options.Secret makes every
// delivery fail with 503: an unsigned endpoint would let anyone create tasks.
func NewWebhook(o Options) *Webhook {
	if o.Store == nil || o.Controller == nil {
		panic("triggers: Options.Store and Options.Controller are required")
	}
	return &Webhook{o: o}
}

// ServeHTTP implements http.Handler.
func (w *Webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", http.MethodPost)
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(w.o.Secret) == 0 {
		http.Error(rw, "webhook secret is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(rw, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(rw, "reading the body failed", http.StatusBadRequest)
		return
	}
	if !validSignature(w.o.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(rw, "invalid signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		http.Error(rw, "missing X-GitHub-Event", http.StatusBadRequest)
		return
	}
	ev, err := Normalize(event, body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	delivery := r.Header.Get("X-GitHub-Delivery")
	seen, err := w.o.Store.SeenDelivery(ctx, delivery)
	if err != nil {
		slog.Error("triggers: recording a delivery failed", "delivery", delivery, "error", err)
		http.Error(rw, "storing the delivery failed", http.StatusInternalServerError)
		return
	}
	if seen {
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, "duplicate")
		return
	}
	ev.DeliveryID = delivery
	ev.At = w.o.now()
	w.dispatch(ev)

	created, failed := w.fire(r, ev)
	resp := struct {
		Created []string `json:"created"`
		Errors  []string `json:"errors,omitempty"`
	}{Created: created, Errors: failed}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(rw).Encode(resp)
}

// dispatch hands the event to OnEvent off the request: the listener may call GitHub (the pull request loop
// evaluates the PR the event is about), and GitHub gives up on a delivery after 10 seconds and redelivers it.
// The listener gets a context that outlives the request, bounded so a stuck call cannot leak the goroutine.
func (w *Webhook) dispatch(ev factory.Event) {
	if w.o.OnEvent == nil {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("triggers: the event listener panicked", "event", ev.Name, "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
		defer cancel()
		w.o.OnEvent(ctx, ev)
	}()
}

// Wait blocks until the listeners started by deliveries so far have returned (for tests and graceful shutdown).
func (w *Webhook) Wait() { w.wg.Wait() }

// eventTimeout bounds one OnEvent call.
const eventTimeout = 2 * time.Minute

// validSignature checks a "sha256=<hex>" header against the HMAC of body,
// in constant time.
func validSignature(secret, body []byte, header string) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// fire creates the task of every enabled webhook trigger that ev selects and
// returns the IDs of the tasks that are new. One trigger's failure does not
// stop the others.
func (w *Webhook) fire(r *http.Request, ev factory.Event) (created, failed []string) {
	created = []string{}
	ctx := r.Context()
	all, err := w.o.Store.ListTriggers(ctx, "")
	if err != nil {
		slog.Error("triggers: listing triggers failed", "event", ev.Name, "error", err)
		return created, []string{"listing triggers: " + err.Error()}
	}
	data := make(map[string]string, len(ev.Fields)+4)
	for k, v := range ev.Fields {
		data[k] = v
	}
	data["Repo"], data["Key"], data["URL"], data["Name"] = ev.Repo, ev.Key, ev.URL, ev.Name
	for _, t := range all {
		if !t.Enabled || t.Kind != factory.TriggerWebhook || !eventMatches(t.Event, ev.Name) || !matches(t.Match, ev.Fields) {
			continue
		}
		key := sourceKey(ev, t)
		task, err := renderTask(t, data, factory.Source{Kind: factory.SourceGitHub, Key: key, URL: ev.URL})
		if err != nil {
			slog.Warn("triggers: rendering the task template failed; trigger skipped", "trigger", t.Name, "event", ev.Name, "error", err)
			continue
		}
		got, isNew, err := w.o.Controller.Create(ctx, *task)
		if err != nil {
			slog.Warn("triggers: creating the task failed", "trigger", t.Name, "event", ev.Name, "error", err)
			failed = append(failed, fmt.Sprintf("trigger %q: %v", t.Name, err))
			continue
		}
		if isNew {
			created = append(created, got.ID)
		}
	}
	return created, failed
}

// sourceKey makes a trigger's task for one thing idempotent: a redelivery, or a
// second event about the same issue, finds the task the first one created. An
// event about no particular thing (a check on a commit without a pull
// request) falls back to its delivery, and has no key without one.
func sourceKey(ev factory.Event, t factory.Trigger) string {
	switch {
	case ev.Key != "":
		return ev.Key + ":" + t.ID
	case ev.DeliveryID != "":
		return "github:delivery:" + ev.DeliveryID + ":" + t.ID
	}
	return ""
}
