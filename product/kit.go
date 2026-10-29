package product

import "github.com/GhaziBenDahmane/harness/hosts/httpapi"

// The vocabulary of the HTTP API this package builds on.
type (
	Caller        = httpapi.Caller
	Authenticator = httpapi.Authenticator
	handler       = httpapi.Handler
	resourceKind  = httpapi.ResourceKind
)

const (
	kindNone   = httpapi.KindNone
	kindThread = httpapi.KindThread
	kindRun    = httpapi.KindRun
)

var (
	writeJSON   = httpapi.WriteJSON
	writeError  = httpapi.WriteError
	fail        = httpapi.Fail
	decode      = httpapi.Decode
	decodeLimit = httpapi.DecodeLimit
	limitParam  = httpapi.LimitParam
)
