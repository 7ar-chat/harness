package httpapi

import (
	"net/http"
	"strings"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// ResourceKind says what {id} names in a route path.
type ResourceKind int

const (
	KindNone ResourceKind = iota
	KindThread
	KindRun
)

// Handler serves an authenticated request.
type Handler func(w http.ResponseWriter, r *http.Request, c Caller)

// kindOf says what {id} names in a route path.
func kindOf(path string) ResourceKind {
	switch {
	case strings.HasPrefix(path, "/threads/{id}"):
		return KindThread
	case strings.HasPrefix(path, "/runs/{id}"):
		return KindRun
	}
	return KindNone
}

// Policy is how a product specialises the core routes (who may see a thread,
// who is recorded as the author of a message, what is logged). The methods that
// take over a route return true once they have answered; false leaves the
// route to the plain single-user behaviour.
type Policy interface {
	// Resolve turns the authenticated actor and the thread or run the request names into the Caller the handler works as.
	Resolve(r *http.Request, kind ResourceKind, actor string) (Caller, error)
	// FailAccess answers an error of Resolve.
	FailAccess(w http.ResponseWriter, err error)
	// Author is how the caller is recorded on what they send.
	Author(c Caller) harness.Author

	CreateThread(w http.ResponseWriter, r *http.Request, c Caller) bool
	ListThreads(w http.ResponseWriter, r *http.Request, c Caller) bool
	GetThread(w http.ResponseWriter, r *http.Request, c Caller) bool
	DeleteThread(w http.ResponseWriter, r *http.Request, c Caller) bool

	// BeforeRun may adjust the options of a run about to be sent on the thread.
	BeforeRun(r *http.Request, threadID string, o *client.SendOptions)
	// BeforeResume may adjust a resume request; it returns the name of the tool the approval is for, for the audit log.
	BeforeResume(r *http.Request, c Caller, threadID string, body *client.ResumeRequest) (tool string)
	// AfterRun runs once a run was queued by action "run.send" or "run.resume".
	AfterRun(c Caller, run *client.Run, action string, detail map[string]any)
	// AfterRunAction runs after a cancel or a steer of the run named by the route.
	AfterRunAction(r *http.Request, c Caller, action string)
	// AfterFork runs once a thread was forked; true means it failed and has answered.
	AfterFork(w http.ResponseWriter, r *http.Request, c Caller, th *client.Thread) bool
}

// Extension plugs a product into the server: a Policy and the routes that
// belong to it. Setup runs first (it may set the authenticator and mount login
// routes), Install after the core routes.
type Extension interface {
	Policy
	Setup(o *Options, mux *http.ServeMux) error
	Install(s *Server) error
}

// soloPolicy is the plain single-user behaviour: every caller sees their own threads.
type soloPolicy struct{}

func (soloPolicy) Resolve(_ *http.Request, _ ResourceKind, actor string) (Caller, error) {
	return Caller{Login: actor, Actor: actor}, nil
}
func (soloPolicy) FailAccess(w http.ResponseWriter, err error)                  { fail(w, err) }
func (soloPolicy) Author(Caller) harness.Author                                 { return harness.Author{} }
func (soloPolicy) CreateThread(http.ResponseWriter, *http.Request, Caller) bool { return false }
func (soloPolicy) ListThreads(http.ResponseWriter, *http.Request, Caller) bool  { return false }
func (soloPolicy) GetThread(http.ResponseWriter, *http.Request, Caller) bool    { return false }
func (soloPolicy) DeleteThread(http.ResponseWriter, *http.Request, Caller) bool { return false }
func (soloPolicy) BeforeRun(*http.Request, string, *client.SendOptions)         {}
func (soloPolicy) BeforeResume(*http.Request, Caller, string, *client.ResumeRequest) string {
	return ""
}
func (soloPolicy) AfterRun(Caller, *client.Run, string, map[string]any) {}
func (soloPolicy) AfterRunAction(*http.Request, Caller, string)         {}
func (soloPolicy) AfterFork(http.ResponseWriter, *http.Request, Caller, *client.Thread) bool {
	return false
}

// The helpers an Extension builds its routes with.

// WriteJSON writes v as JSON with the status.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// WriteError writes the API's error body.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	writeError(w, status, code, msg)
}

// Fail writes a client error with its own status and code, anything else as an internal error.
func Fail(w http.ResponseWriter, err error) { fail(w, err) }

// Decode reads a JSON request body (at most 2 MiB; an empty body is fine).
func Decode(w http.ResponseWriter, r *http.Request, v any) bool { return decode(w, r, v) }

// DecodeLimit is Decode with a size limit.
func DecodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	return decodeLimit(w, r, v, limit)
}

// LimitParam reads the ?limit= query parameter (at most 200).
func LimitParam(r *http.Request, def int) int { return limitParam(r, def) }
