// Package memory is the harness's memory service: per-user entries with a
// path, title, content, tags, importance and metadata, searched with Work's
// hybrid ranking, written under Work's policy (triggers, warn/strict write
// mode, secret redaction, soft/hard delete), recalled in the system prompt and
// consolidated periodically (deduplication, cleanup of deleted entries).
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/store"
)

// Limits. Work bounds top_k to 1..50 and a memory file to 200 000 bytes
// (DEVFACTORY_MAX_ARTIFACT_CONTENT_BYTES); it has no entry count, tag or batch
// limits, which a shared Postgres store needs (see README "Memory").
const (
	MaxTopK                = 50
	DefaultTopK            = 5
	DefaultMaxContentBytes = 200_000
	DefaultMaxEntries      = 1000
	// DefaultMaxPending caps the entries waiting for confirmation apart from
	// MaxEntries, so the agent's proposals cannot fill the memory.
	DefaultMaxPending = 100
	MaxBatch          = 100
	MaxTags           = 50
	MaxTagChars       = 100
	MaxTitleChars     = 200
	MaxPathChars      = 512
	MaxRecallItems    = 4
)

// Scopes of the memory; it is per user ("personal" by default).
var allowedScopes = map[string]bool{"repository": true, "workspace": true, "personal": true}

// Options configure the service (HARNESS_MEMORY_*, see config.Memory).
type Options struct {
	WriteMode        string
	DeleteMode       string
	DefaultScope     string
	EmbeddingBackend string
	MaxEntries       int
	// MaxPending caps the pending entries (0: DefaultMaxPending, < 0: no limit).
	MaxPending      int
	MaxContentBytes int
	// RecallInjection adds the "[Repository Memory Recall]" block to the system prompt.
	RecallInjection bool
	Now             func() time.Time
	// Embedder, when set, gives entries and queries vectors: search and recall then rank by meaning
	// as well as by words (HARNESS_MEMORY_EMBEDDING_BACKEND=openai).
	Embedder Embedder
	// Extract turns on the end-of-run extraction of memories from the conversation; ExtractModel
	// is the model it uses (default: the run's own).
	Extract      bool
	ExtractModel string
}

func (o Options) normalized() Options {
	o.WriteMode = strings.ToLower(strings.TrimSpace(o.WriteMode))
	if o.WriteMode != WriteStrict {
		o.WriteMode = WriteWarn
	}
	o.DeleteMode = strings.ToLower(strings.TrimSpace(o.DeleteMode))
	if o.DeleteMode != DeleteHard {
		o.DeleteMode = DeleteSoft
	}
	o.DefaultScope = strings.ToLower(strings.TrimSpace(o.DefaultScope))
	if !allowedScopes[o.DefaultScope] {
		o.DefaultScope = "personal"
	}
	o.EmbeddingBackend = strings.ToLower(strings.TrimSpace(o.EmbeddingBackend))
	if o.EmbeddingBackend == "" {
		o.EmbeddingBackend = "simple"
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.MaxPending == 0 {
		o.MaxPending = DefaultMaxPending
	}
	if o.MaxContentBytes <= 0 {
		o.MaxContentBytes = DefaultMaxContentBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// SemanticAvailable is false for the keyword-only backends (Work: disabled, none, keyword).
func (o Options) SemanticAvailable() bool {
	switch o.EmbeddingBackend {
	case "disabled", "none", "keyword":
		return false
	}
	return true
}

// Error is a structured memory error ({"error": {code, message, details}}).
type Error struct {
	Code    string
	Message string
	Details map[string]any
}

func newError(code, message string, details map[string]any) *Error {
	if details == nil {
		details = map[string]any{}
	}
	return &Error{Code: code, Message: message, Details: details}
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Payload is the error object of the tool results.
func (e *Error) Payload() map[string]any {
	return map[string]any{"error": e.fields()}
}

func (e *Error) fields() map[string]any {
	return map[string]any{"code": e.Code, "message": e.Message, "details": e.Details}
}

// conflictError: the entry changed since it was read (store.ErrMemoryConflict).
func conflictError(id string) *Error {
	return newError("memory_conflict", "The memory changed meanwhile; read it again before changing it", map[string]any{"id": id})
}

func internalError(op string, err error) *Error {
	slog.Error("memory: storage failed", "op", op, "error", err)
	return newError("memory_store_failed", "The memory storage failed; try again later.", nil)
}

// Writer is who writes: the agent of a run (Source proxy) or the user in
// Settings > Memory (Source user). Untrusted makes every write a proposal.
type Writer struct {
	Login     string
	ThreadID  string
	Source    string
	Untrusted bool
}

// Change is one mutation, announced with work.memory.
type Change struct {
	// Action is created, updated, proposed (written pending) or deleted.
	Action string
	Entry  *store.MemoryEntry
	Mode   string
	// Proposed marks a delete made after untrusted content: soft, the user can undo it (restore).
	Proposed bool
}

// Result is the outcome of a tool operation.
type Result struct {
	Data    map[string]any
	IsError bool
	Changes []Change
}

// Service implements the memory operations on a store.
type Service struct {
	st store.MemoryStore
	o  Options
	// KeyFor, when set, names the memory a run reads and writes: the login by
	// default, or another key (a team host maps a project thread to the
	// project's memory, so what is learned there is shared by its members and
	// no member's personal memory leaks into a shared thread).
	KeyFor func(ctx context.Context, login, threadID string) string
	// UserKeyFor, when set, names a second, private layer of the same run: what the acting user keeps
	// for themselves inside a shared (project) memory ("" for none).
	UserKeyFor func(ctx context.Context, login, threadID string) string
	// Shared reports whether a memory key is shared between people (a project's), which selects the
	// project memory policy in the prompt.
	Shared func(key string) bool
	// Messages reads a thread's messages (recall uses the last user message as its query; extraction
	// reads the run).
	Messages func(ctx context.Context, threadID string) ([]*store.Message, error)

	stats serviceStats
}

// Layer is one memory a run sees: the shared or personal one (Key), or the user's own inside a project.
type Layer struct {
	Key  string
	Name string // "project", "user" (personal thread) or "me" (the user's layer inside a project)
}

// Layers lists the memories of a run, the primary one first.
func (s *Service) Layers(ctx context.Context, login, threadID string) []Layer {
	primary := s.Key(ctx, login, threadID)
	name := "user"
	if s.Shared != nil && s.Shared(primary) {
		name = "project"
	}
	out := []Layer{{primary, name}}
	if s.UserKeyFor != nil {
		if k := s.UserKeyFor(ctx, login, threadID); k != "" && k != primary {
			out = append(out, Layer{k, "me"})
		}
	}
	return out
}

// SharedMemory reports whether the run's primary memory is shared between people.
func (s *Service) SharedMemory(ctx context.Context, login, threadID string) bool {
	return s.Shared != nil && s.Shared(s.Key(ctx, login, threadID))
}

// Key is the memory key of a run of login on threadID.
func (s *Service) Key(ctx context.Context, login, threadID string) string {
	if s.KeyFor != nil {
		if k := s.KeyFor(ctx, login, threadID); k != "" {
			return k
		}
	}
	return login
}

// New returns the service.
func New(st store.MemoryStore, o Options) *Service {
	return &Service{st: st, o: o.normalized()}
}

// Options returns the normalized options.
func (s *Service) Options() Options { return s.o }

// Enabled is the user's memory setting.
func (s *Service) Enabled(ctx context.Context, login string) (bool, error) {
	return s.st.MemoryEnabled(ctx, login)
}

func (s *Service) now() time.Time { return s.o.Now().UTC() }

// limits are the per-status entry caps checked by the store under its per-user lock.
func (s *Service) limits() store.MemoryLimits {
	return store.MemoryLimits{Active: max(s.o.MaxEntries, 0), Pending: max(s.o.MaxPending, 0)}
}

// limitError is ErrMemoryLimit for an entry going to status.
func (s *Service) limitError(status string) *Error {
	if status == store.MemoryPending {
		return newError("memory_limit", fmt.Sprintf("%d memories already wait for the user's confirmation: ask them to keep or discard them first", s.o.MaxPending),
			map[string]any{"max_pending_entries": s.o.MaxPending})
	}
	return newError("memory_limit", fmt.Sprintf("The memory is full (%d entries): delete or update an outdated entry first", s.o.MaxEntries),
		map[string]any{"max_entries": s.o.MaxEntries})
}

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "mem_" + hex.EncodeToString(b)
}

func (s *Service) scope(raw string) (string, *Error) {
	c := strings.ToLower(strings.TrimSpace(raw))
	if c == "" {
		c = s.o.DefaultScope
	}
	if !allowedScopes[c] {
		return "", newError("invalid_scope", "scope must be one of [personal, repository, workspace]", map[string]any{"scope": raw})
	}
	return c, nil
}

func repository(raw string) string {
	if r := strings.TrimSpace(raw); r != "" {
		return r
	}
	return "default"
}

func cloneMeta(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func setDefault(m map[string]any, key string, v any) {
	if _, ok := m[key]; !ok {
		m[key] = v
	}
}

// metaImportance reads metadata.importance when it is a number in [0, 1].
func metaImportance(m map[string]any) *float64 {
	var f float64
	switch v := m["importance"].(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case interface{ Float64() (float64, error) }:
		x, err := v.Float64()
		if err != nil {
			return nil
		}
		f = x
	default:
		return nil
	}
	if f < 0 || f > 1 {
		return nil
	}
	return &f
}

// PublicEntry is the "memory" object of the tool results (Work's
// _entry_public_payload plus title, status and importance).
func PublicEntry(e *store.MemoryEntry) map[string]any {
	var importance any
	if e.Importance != nil {
		importance = *e.Importance
	}
	return map[string]any{
		"id": e.ID, "path": e.Path, "scope": e.Scope, "repository": e.Repository, "tags": nonNil(e.Tags),
		"metadata": cloneMeta(e.Metadata), "created_at": ts(e.CreatedAt), "updated_at": ts(e.UpdatedAt), "source": e.Source,
		"embedding_updated_at": ts(e.ContentUpdatedAt), "title": e.Title, "status": e.Status, "importance": importance,
	}
}

// reversible is added to the result of a delete made after untrusted content.
func reversible() map[string]any {
	return map[string]any{"undoable": true, "message": "This conversation contains content from files, tools or attachments, " +
		"so this memory was only soft-deleted: the user can restore it (in the chat or in Settings > Memory). Tell them."}
}

// confirmation is added to the result of a write that is only proposed.
func confirmation() map[string]any {
	return map[string]any{"required": true, "message": "This conversation contains content from files, tools or attachments, " +
		"so this memory is only proposed: the user must confirm it (in the chat or in Settings > Memory) before it is used. Tell them."}
}

// resolve finds an entry by id or path (Work's _resolve_entry).
func (s *Service) resolve(ctx context.Context, login, id, path string) (*store.MemoryEntry, *Error) {
	if id = strings.TrimSpace(id); id != "" {
		e, err := s.st.GetMemoryEntry(ctx, login, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, newError("memory_not_found", "Memory id not found", map[string]any{"id": id})
		}
		if err != nil {
			return nil, internalError("get", err)
		}
		if e.Status == store.MemoryDeleted {
			return nil, newError("memory_deleted", "Requested memory is deleted", map[string]any{"id": id, "path": e.Path})
		}
		return e, nil
	}
	if strings.TrimSpace(path) == "" {
		return nil, newError("invalid_target", "Either id or path must be provided", nil)
	}
	rel, perr := NormalizePath(path)
	if perr != nil {
		return nil, perr
	}
	e, err := s.st.GetMemoryEntryByPath(ctx, login, rel)
	if errors.Is(err, store.ErrNotFound) {
		return nil, newError("memory_not_found", "Memory path not found", map[string]any{"path": rel})
	}
	if err != nil {
		return nil, internalError("get", err)
	}
	if e.Status == store.MemoryDeleted {
		return nil, newError("memory_deleted", "Requested memory is deleted", map[string]any{"path": rel})
	}
	return e, nil
}

// SearchInput are the memory_search arguments.
type SearchInput struct {
	Query      string
	Scope      string
	Repository string
	TopK       int
	Filters    map[string]any
}

func stringList(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if s := strings.TrimSpace(stringify(item)); s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, item := range x {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// Search ranks the user's active entries (Work's MemoryStore.search).
func (s *Service) Search(ctx context.Context, login string, in SearchInput) Result {
	redactedQuery, _ := RedactText(in.Query)
	scope, serr := s.scope(in.Scope)
	if serr != nil {
		return Result{Data: serr.Payload(), IsError: true}
	}
	repo := repository(in.Repository)
	topK := in.TopK
	if topK == 0 {
		topK = DefaultTopK
	}
	prefix := ""
	if raw := strings.TrimSpace(stringify(in.Filters["path_prefix"])); raw != "" {
		p, perr := normalizePrefix(raw)
		if perr != nil {
			return Result{Data: perr.Payload(), IsError: true}
		}
		prefix = p
	}
	wanted := map[string]bool{}
	for _, t := range stringList(in.Filters["tags"]) {
		wanted[strings.ToLower(t)] = true
	}
	all, err := s.st.ListMemoryEntries(ctx, login, store.MemoryActive)
	if err != nil {
		return Result{Data: internalError("search", err).Payload(), IsError: true}
	}
	var candidates []*store.MemoryEntry
	for _, e := range all {
		if e.Scope != scope || e.Repository != repo || (prefix != "" && !strings.HasPrefix(e.Path, prefix)) {
			continue
		}
		have := map[string]bool{}
		for _, t := range e.Tags {
			have[strings.ToLower(strings.TrimSpace(t))] = true
		}
		ok := true
		for t := range wanted {
			ok = ok && have[t]
		}
		if ok {
			candidates = append(candidates, e)
		}
	}
	opts := RankOptions{}
	if len(candidates) > 0 {
		if qv := s.embedQuery(ctx, in.Query); qv != nil {
			opts = RankOptions{QueryVec: qv, Vecs: s.vectors(ctx, login, candidates)}
		}
	}
	hits := RankWith(candidates, in.Query, topK, s.now(), opts)
	s.stats.searches.Add(1)
	if len(hits) > 0 {
		s.stats.searchHits.Add(1)
	}
	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		results = append(results, map[string]any{
			"id": h.Entry.ID, "entry_id": h.Entry.ID, "chunk_id": h.ChunkID, "path": h.Entry.Path, "title": h.Entry.Title,
			"scope": h.Entry.Scope, "repository": h.Entry.Repository, "score": h.Score, "rank_components": h.Components,
			"timestamp": ts(entryTime(h.Entry)), "source": h.Entry.Source, "snippet": Snippet(h.Chunk), "tags": nonNil(h.Entry.Tags),
			"importance": h.Components["importance"], "recency": h.Components["recency"], "content_hash": sha(h.Chunk),
			"index_version": SearchIndexVersion,
		})
	}
	reason := "no_match"
	if len(results) > 0 {
		reason = "matches_found"
	}
	return Result{Data: map[string]any{
		"results": results,
		"trace": map[string]any{
			"used_memory": len(results) > 0, "reason": reason, "query": redactedQuery, "scope": scope, "repository": repo,
			"semantic_backend": s.semanticBackend(), "semantic_fallback": !s.o.SemanticAvailable() || len(opts.Vecs) == 0, "index_version": SearchIndexVersion,
			"candidate_count": len(candidates), "chunk_count": len(hits), "precedence": append([]string{}, Precedence...),
		},
		"provenance": map[string]any{"source": "memory_proxy", "timestamp": ts(s.now())},
	}}
}

// Get returns one entry with its content (Work's MemoryStore.get).
func (s *Service) Get(ctx context.Context, login, id, path string, includeMetadata bool) Result {
	e, rerr := s.resolve(ctx, login, id, path)
	if rerr != nil {
		return Result{Data: rerr.Payload(), IsError: true}
	}
	if e.Status == store.MemoryPending {
		// A proposal is not used (nor read back) until the user confirms it.
		return Result{Data: newError("memory_pending", "Requested memory waits for the user's confirmation",
			map[string]any{"id": e.ID, "path": e.Path}).Payload(), IsError: true}
	}
	data := map[string]any{"id": e.ID, "path": e.Path, "content": e.Content, "source": e.Source, "timestamp": ts(entryTime(e)),
		"title": e.Title, "status": e.Status}
	if includeMetadata {
		data["metadata"] = cloneMeta(e.Metadata)
		data["tags"] = nonNil(e.Tags)
		data["scope"] = e.Scope
		data["repository"] = e.Repository
		if e.Importance != nil {
			data["importance"] = *e.Importance
		} else {
			data["importance"] = nil
		}
	}
	reason := "resolved_by_path"
	if strings.TrimSpace(id) != "" {
		reason = "resolved_by_id"
	}
	return Result{Data: map[string]any{"memory": data, "trace": map[string]any{"used_memory": true, "reason": reason}}}
}

// CreateInput are the memory_create arguments (Title and Importance: Settings > Memory).
type CreateInput struct {
	Content    string
	Metadata   map[string]any
	Tags       []string
	Scope      string
	Repository string
	Path       string
	Title      *string
	Importance *float64
}

func (s *Service) checkContent(content string) *Error {
	if len(content) > s.o.MaxContentBytes {
		return newError("content_too_large", fmt.Sprintf("content must be at most %d bytes", s.o.MaxContentBytes),
			map[string]any{"bytes": len(content), "max_bytes": s.o.MaxContentBytes})
	}
	return nil
}

func checkTags(tags []string) *Error {
	if len(tags) > MaxTags {
		return newError("invalid_tags", fmt.Sprintf("at most %d tags", MaxTags), nil)
	}
	for _, t := range tags {
		if utf8.RuneCountInString(t) > MaxTagChars {
			return newError("invalid_tags", fmt.Sprintf("a tag has at most %d characters", MaxTagChars), map[string]any{"tag": t})
		}
	}
	return nil
}

func checkImportance(v *float64) *Error {
	if v != nil && (*v < 0 || *v > 1) {
		return newError("invalid_importance", "importance must be between 0 and 1", nil)
	}
	return nil
}

func policyError(d Decision) *Error {
	var trigger any
	if d.Trigger != "" {
		trigger = d.Trigger
	}
	return newError("policy_violation", "Write blocked by memory policy",
		map[string]any{"write_mode": d.WriteMode, "trigger": trigger, "violations": nonNil(d.Violations)})
}

func title(explicit *string, meta map[string]any, content string) string {
	t := ""
	switch {
	case explicit != nil:
		t = *explicit
	case metaString(meta, "title") != "":
		t = metaString(meta, "title")
	default:
		return DeriveTitle(content)
	}
	t, _ = RedactText(strings.Join(strings.Fields(t), " "))
	return truncateRunes(t, MaxTitleChars)
}

// Create writes a new entry (Work's MemoryStore.create); after untrusted content it is pending.
func (s *Service) Create(ctx context.Context, w Writer, in CreateInput) Result {
	e, data, cerr := s.create(ctx, w, in)
	if cerr != nil {
		return Result{Data: cerr.Payload(), IsError: true}
	}
	return Result{Data: data, Changes: []Change{createdChange(e)}}
}

func createdChange(e *store.MemoryEntry) Change {
	if e.Status == store.MemoryPending {
		return Change{Action: "proposed", Entry: e}
	}
	return Change{Action: "created", Entry: e}
}

func (s *Service) create(ctx context.Context, w Writer, in CreateInput) (*store.MemoryEntry, map[string]any, *Error) {
	scope, serr := s.scope(in.Scope)
	if serr != nil {
		return nil, nil, serr
	}
	repo := repository(in.Repository)
	if strings.TrimSpace(in.Content) == "" {
		return nil, nil, newError("invalid_content", "content is required", nil)
	}
	if err := s.checkContent(in.Content); err != nil {
		return nil, nil, err
	}
	if err := checkImportance(in.Importance); err != nil {
		return nil, nil, err
	}
	meta := cloneMeta(in.Metadata)
	setDefault(meta, "scope", scope)
	setDefault(meta, "repository", repo)
	d := Evaluate(s.o.WriteMode, "create", in.Content, meta)
	if !d.Allowed {
		return nil, nil, policyError(d)
	}
	id := newID()
	tags := NormalizeTags(in.Tags)
	if err := checkTags(tags); err != nil {
		return nil, nil, err
	}
	now := s.now()
	path := ""
	if strings.TrimSpace(in.Path) != "" {
		p, perr := NormalizePath(in.Path)
		if perr != nil {
			return nil, nil, perr
		}
		path = p
	} else {
		path = DefaultPath(id, DefaultKind(d.RedactedContent, d.RedactedMetadata, tags, d.Trigger), now)
	}
	if err := assertDurable(path); err != nil {
		return nil, nil, err
	}
	importance := in.Importance
	if importance == nil {
		importance = metaImportance(d.RedactedMetadata)
	}
	status := store.MemoryActive
	if w.Untrusted {
		status = store.MemoryPending
	}
	source := w.Source
	if source == "" {
		source = store.MemorySourceProxy
	}
	entry := &store.MemoryEntry{
		ID: id, UserLogin: w.Login, Path: path, Title: title(in.Title, d.RedactedMetadata, d.RedactedContent), Content: d.RedactedContent,
		Tags: tags, Importance: importance, Metadata: d.RedactedMetadata, Scope: scope, Repository: repo, Source: source, Status: status,
		SourceThreadID: w.ThreadID, ContentHash: sha(d.RedactedContent), CreatedAt: now, UpdatedAt: now, ContentUpdatedAt: now,
	}
	out, err := s.st.CreateMemoryEntry(ctx, entry, s.limits())
	switch {
	case errors.Is(err, store.ErrMemoryLimit):
		return nil, nil, s.limitError(entry.Status)
	case errors.Is(err, store.ErrMemoryPathExists):
		return nil, nil, newError("path_exists", "Target path already exists", map[string]any{"path": path})
	case err != nil:
		return nil, nil, internalError("create", err)
	}
	s.embedAsync(w.Login, out)
	data := map[string]any{"memory": PublicEntry(out), "policy": d.Payload()}
	if out.Status == store.MemoryPending {
		data["confirmation"] = confirmation()
	}
	return out, data, nil
}

// UpdateInput are the memory_update arguments (Title, Importance, NewPath: Settings > Memory).
type UpdateInput struct {
	ID       string
	Path     string
	Content  *string
	Metadata map[string]any
	// Tags replace the entry's tags when not nil.
	Tags            *[]string
	Title           *string
	Importance      *float64
	ClearImportance bool
	NewPath         *string
}

// assertReference rejects mutations of transient paths before resolving them.
func assertReference(path string) *Error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	rel, err := NormalizePath(path)
	if err != nil {
		return err
	}
	return assertDurable(rel)
}

// Update changes an entry (Work's MemoryStore.update); after untrusted content
// the changed entry becomes pending until the user confirms it.
func (s *Service) Update(ctx context.Context, w Writer, in UpdateInput) Result {
	if err := assertReference(in.Path); err != nil {
		return Result{Data: err.Payload(), IsError: true}
	}
	target, rerr := s.resolve(ctx, w.Login, in.ID, in.Path)
	if rerr != nil {
		return Result{Data: rerr.Payload(), IsError: true}
	}
	if err := assertDurable(target.Path); err != nil {
		return Result{Data: err.Payload(), IsError: true}
	}
	proposed := target.Content
	if in.Content != nil {
		proposed = *in.Content
		if strings.TrimSpace(proposed) == "" {
			return Result{Data: newError("invalid_content", "content must not be empty", nil).Payload(), IsError: true}
		}
		if err := s.checkContent(proposed); err != nil {
			return Result{Data: err.Payload(), IsError: true}
		}
	}
	if err := checkImportance(in.Importance); err != nil {
		return Result{Data: err.Payload(), IsError: true}
	}
	merged := cloneMeta(target.Metadata)
	for k, v := range in.Metadata {
		merged[k] = v
	}
	setDefault(merged, "scope", target.Scope)
	setDefault(merged, "repository", target.Repository)
	d := Evaluate(s.o.WriteMode, "update", proposed, merged)
	if !d.Allowed {
		return Result{Data: policyError(d).Payload(), IsError: true}
	}
	now := s.now()
	e := target.Clone()
	e.Metadata = d.RedactedMetadata
	contentChanged := in.Content != nil && d.RedactedContent != target.Content
	if contentChanged {
		e.Content = d.RedactedContent
		e.ContentHash = sha(e.Content)
		e.ContentUpdatedAt = now
		// A proposal records its revision when it is confirmed, not now.
		if !(w.Untrusted && target.Status == store.MemoryActive) {
			addRevision(e, target.Title, target.Content, now, w.ThreadID)
		}
	}
	tagsChanged := in.Tags != nil
	if tagsChanged {
		e.Tags = NormalizeTags(*in.Tags)
		if err := checkTags(e.Tags); err != nil {
			return Result{Data: err.Payload(), IsError: true}
		}
	}
	switch {
	case in.Title != nil:
		e.Title = title(in.Title, nil, e.Content)
	case metaString(in.Metadata, "title") != "":
		e.Title = title(nil, d.RedactedMetadata, e.Content)
	case contentChanged && target.Title == DeriveTitle(target.Content):
		e.Title = DeriveTitle(e.Content)
	}
	switch {
	case in.ClearImportance:
		e.Importance = nil
	case in.Importance != nil:
		e.Importance = in.Importance
	case metaImportance(in.Metadata) != nil:
		e.Importance = metaImportance(d.RedactedMetadata)
	}
	if in.NewPath != nil {
		p, perr := NormalizePath(*in.NewPath)
		if perr != nil {
			return Result{Data: perr.Payload(), IsError: true}
		}
		if err := assertDurable(p); err != nil {
			return Result{Data: err.Payload(), IsError: true}
		}
		e.Path = p
	}
	e.UpdatedAt = now
	changes := map[string]any{"content_changed": contentChanged, "metadata_changed": len(in.Metadata) > 0, "tags_changed": tagsChanged}
	if w.Untrusted && target.Status == store.MemoryActive {
		// The confirmed entry stays as it is (and in use): the change is only proposed.
		return s.propose(ctx, w, target, e, d, changes)
	}
	out, err := s.st.UpdateMemoryEntry(ctx, e, s.limits())
	switch {
	case errors.Is(err, store.ErrMemoryPathExists):
		return Result{Data: newError("path_exists", "Target path already exists", map[string]any{"path": e.Path}).Payload(), IsError: true}
	case errors.Is(err, store.ErrNotFound):
		return Result{Data: newError("memory_not_found", "Memory id not found", map[string]any{"id": e.ID}).Payload(), IsError: true}
	case errors.Is(err, store.ErrMemoryConflict):
		return Result{Data: conflictError(e.ID).Payload(), IsError: true}
	case err != nil:
		return Result{Data: internalError("update", err).Payload(), IsError: true}
	}
	s.embedAsync(w.Login, out)
	data := map[string]any{"memory": PublicEntry(out), "policy": d.Payload(), "changes": changes}
	action := "updated"
	if w.Untrusted && out.Status == store.MemoryPending {
		action = "proposed"
		data["confirmation"] = confirmation()
	}
	return Result{Data: data, Changes: []Change{{Action: action, Entry: out}}}
}

// propose stores the updated fields of next as the proposal of the active entry target.
func (s *Service) propose(ctx context.Context, w Writer, target, next *store.MemoryEntry, d Decision, changes map[string]any) Result {
	saved := target.Clone()
	saved.Proposal = &store.MemoryProposal{Path: next.Path, Title: next.Title, Content: next.Content, Tags: next.Tags,
		Importance: next.Importance, Metadata: next.Metadata, ContentHash: next.ContentHash, ThreadID: w.ThreadID, ProposedAt: s.now()}
	out, err := s.st.UpdateMemoryEntry(ctx, saved, s.limits())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Result{Data: newError("memory_not_found", "Memory id not found", map[string]any{"id": target.ID}).Payload(), IsError: true}
	case errors.Is(err, store.ErrMemoryConflict):
		return Result{Data: conflictError(target.ID).Payload(), IsError: true}
	case err != nil:
		return Result{Data: internalError("update", err).Payload(), IsError: true}
	}
	data := map[string]any{"memory": PublicEntry(out), "proposal": PublicProposal(out.Proposal), "policy": d.Payload(),
		"changes": changes, "confirmation": confirmation()}
	return Result{Data: data, Changes: []Change{{Action: "proposed", Entry: out}}}
}

// PublicProposal is the "proposal" object of a proposed memory_update.
func PublicProposal(p *store.MemoryProposal) map[string]any {
	if p == nil {
		return nil
	}
	var importance any
	if p.Importance != nil {
		importance = *p.Importance
	}
	return map[string]any{"path": p.Path, "title": p.Title, "tags": nonNil(p.Tags), "importance": importance,
		"metadata": cloneMeta(p.Metadata), "content_hash": p.ContentHash, "proposed_at": ts(p.ProposedAt)}
}

// Delete removes an entry (Work's MemoryStore.delete) with the configured
// delete mode, or mode when set (Settings > Memory deletes permanently).
func (s *Service) Delete(ctx context.Context, w Writer, id, path string, metadata map[string]any, mode string) Result {
	if err := assertReference(path); err != nil {
		return Result{Data: err.Payload(), IsError: true}
	}
	target, rerr := s.resolve(ctx, w.Login, id, path)
	if rerr != nil {
		return Result{Data: rerr.Payload(), IsError: true}
	}
	return s.deleteEntry(ctx, w, target, metadata, mode)
}

// DeleteEntry is Delete of an entry already read: it fails with
// memory_conflict when the entry changed since (its status for a hard delete,
// its version for a soft one).
func (s *Service) DeleteEntry(ctx context.Context, w Writer, target *store.MemoryEntry, metadata map[string]any, mode string) Result {
	return s.deleteEntry(ctx, w, target, metadata, mode)
}

func (s *Service) deleteEntry(ctx context.Context, w Writer, target *store.MemoryEntry, metadata map[string]any, mode string) Result {
	if err := assertDurable(target.Path); err != nil {
		return Result{Data: err.Payload(), IsError: true}
	}
	meta := cloneMeta(metadata)
	setDefault(meta, "scope", target.Scope)
	setDefault(meta, "repository", target.Repository)
	d := Evaluate(s.o.WriteMode, "delete", "", meta)
	if !d.Allowed {
		return Result{Data: policyError(d).Payload(), IsError: true}
	}
	if mode != DeleteHard && mode != DeleteSoft {
		mode = s.o.DeleteMode
	}
	// After untrusted content a delete is never final (Work deletes softly by
	// default): an active entry is soft-deleted, announced as a proposed delete
	// the user can undo. A pending entry was never confirmed: it goes for good
	// (restoring it would make it active without confirmation).
	proposedDelete := false
	if w.Untrusted {
		mode, proposedDelete = DeleteSoft, true
		if target.Status == store.MemoryPending {
			mode, proposedDelete = DeleteHard, false
		}
	}
	out := target.Clone()
	if mode == DeleteHard {
		err := s.st.DeleteMemoryEntry(ctx, w.Login, target.ID, target.Status)
		if errors.Is(err, store.ErrMemoryConflict) {
			return Result{Data: conflictError(target.ID).Payload(), IsError: true}
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{Data: internalError("delete", err).Payload(), IsError: true}
		}
	} else {
		now := s.now()
		out.Status, out.DeletedAt, out.UpdatedAt = store.MemoryDeleted, &now, now
		saved, err := s.st.UpdateMemoryEntry(ctx, out, s.limits())
		if errors.Is(err, store.ErrMemoryConflict) {
			return Result{Data: conflictError(target.ID).Payload(), IsError: true}
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{Data: internalError("delete", err).Payload(), IsError: true}
		}
		if saved != nil {
			out = saved
		}
	}
	data := map[string]any{"deleted": true, "id": target.ID, "path": target.Path, "mode": mode, "policy": d.Payload()}
	if proposedDelete {
		data["undo"] = reversible()
	}
	return Result{Data: data, Changes: []Change{{Action: "deleted", Entry: out, Mode: mode, Proposed: proposedDelete}}}
}

func errorItem(index int, data map[string]any) map[string]any {
	item := map[string]any{"index": index}
	if e, ok := data["error"].(map[string]any); ok {
		for k, v := range e {
			item[k] = v
		}
	}
	return item
}

// CreateBatch creates several entries (Work's create_batch): per-entry errors with their index.
func (s *Service) CreateBatch(ctx context.Context, w Writer, entries []CreateInput) Result {
	created, errs := []map[string]any{}, []map[string]any{}
	var changes []Change
	for i, in := range entries {
		r := s.Create(ctx, w, in)
		if r.IsError {
			errs = append(errs, errorItem(i, r.Data))
			continue
		}
		created = append(created, r.Data["memory"].(map[string]any))
		changes = append(changes, r.Changes...)
	}
	data := map[string]any{"created": created, "errors": errs, "created_count": len(created), "error_count": len(errs)}
	if w.Untrusted && len(created) > 0 {
		data["confirmation"] = confirmation()
	}
	return Result{Data: data, IsError: len(errs) > 0 && len(created) == 0, Changes: changes}
}

// DeleteBatch deletes entries by ids, then by paths (Work's delete_batch, reason batch_delete).
func (s *Service) DeleteBatch(ctx context.Context, w Writer, ids, paths []string) Result {
	deleted, errs := []map[string]any{}, []map[string]any{}
	var changes []Change
	run := func(i int, id, path string) {
		r := s.Delete(ctx, w, id, path, map[string]any{"reason": "batch_delete"}, "")
		if r.IsError {
			errs = append(errs, errorItem(i, r.Data))
			return
		}
		deleted = append(deleted, r.Data)
		changes = append(changes, r.Changes...)
	}
	for i, id := range ids {
		run(i, id, "")
	}
	for i, p := range paths {
		run(i, "", p)
	}
	data := map[string]any{"deleted": deleted, "errors": errs, "deleted_count": len(deleted), "error_count": len(errs)}
	if w.Untrusted && len(deleted) > 0 {
		data["undo"] = reversible()
	}
	return Result{Data: data, IsError: len(errs) > 0 && len(deleted) == 0, Changes: changes}
}

// Capabilities is the memory_capabilities result.
func (s *Service) Capabilities() Result {
	return Result{Data: map[string]any{"capabilities": map[string]any{
		"storage": map[string]any{"local_only": false, "cloud_backed": true, "read_only": false},
		"search": map[string]any{"hybrid_enabled": s.o.SemanticAvailable(), "keyword_fallback": true,
			"embedding_backend": s.o.EmbeddingBackend},
		"write_mode": s.o.WriteMode, "delete_mode": s.o.DeleteMode,
		"limits": map[string]any{"max_entries": s.o.MaxEntries, "max_pending_entries": s.o.MaxPending, "max_content_bytes": s.o.MaxContentBytes,
			"max_batch": MaxBatch, "max_top_k": MaxTopK},
	}}}
}

// Expect is what the user saw when they keep or discard a proposal: the
// content hash shown (the proposal's, or the pending entry's) and whether a
// proposal must still be waiting (the chat note's Keep / Discard).
type Expect struct {
	ContentHash string
	Pending     bool
}

func notPendingError(id string) *Error {
	return newError("memory_not_pending", "This memory is no longer waiting for confirmation", map[string]any{"id": id})
}

// proposalHash is the content hash of what waits for confirmation ("" when nothing does).
func proposalHash(e *store.MemoryEntry) string {
	switch {
	case e.Proposal != nil:
		return e.Proposal.ContentHash
	case e.Status == store.MemoryPending:
		return e.ContentHash
	}
	return ""
}

// checkExpect compares what waits for confirmation with what the user saw.
func checkExpect(e *store.MemoryEntry, exp Expect) *Error {
	waiting := e.Proposal != nil || e.Status == store.MemoryPending
	if !waiting {
		if exp.Pending {
			return notPendingError(e.ID)
		}
		return nil
	}
	if exp.ContentHash != "" && exp.ContentHash != proposalHash(e) {
		return conflictError(e.ID)
	}
	return nil
}

// Confirm keeps what the agent proposed: a pending entry becomes active, the
// proposal of an active entry is applied. Without anything waiting it returns
// the entry unchanged, unless exp.Pending (memory_not_pending).
func (s *Service) Confirm(ctx context.Context, login, id string, exp Expect) (*store.MemoryEntry, *Error) {
	e, rerr := s.resolve(ctx, login, id, "")
	if rerr != nil {
		return nil, rerr
	}
	if cerr := checkExpect(e, exp); cerr != nil {
		return nil, cerr
	}
	now := s.now()
	switch {
	case e.Proposal != nil:
		p := e.Proposal
		prevTitle, prevContent := e.Title, e.Content
		if p.Content != e.Content {
			e.ContentUpdatedAt = now
		}
		e.Path, e.Title, e.Content, e.Tags, e.Importance, e.Metadata, e.ContentHash =
			p.Path, p.Title, p.Content, p.Tags, p.Importance, p.Metadata, p.ContentHash
		if prevContent != e.Content {
			if e.Metadata == nil {
				e.Metadata = map[string]any{}
			}
			addRevision(e, prevTitle, prevContent, now, p.ThreadID)
		}
		e.Proposal = nil
	case e.Status == store.MemoryPending:
		e.Status = store.MemoryActive
	default:
		return e, nil
	}
	e.UpdatedAt = now
	out, err := s.st.UpdateMemoryEntry(ctx, e, s.limits())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, newError("memory_not_found", "Memory id not found", map[string]any{"id": id})
	case errors.Is(err, store.ErrMemoryConflict):
		return nil, conflictError(e.ID)
	case errors.Is(err, store.ErrMemoryPathExists):
		return nil, newError("path_exists", "Target path already exists", map[string]any{"path": e.Path})
	case errors.Is(err, store.ErrMemoryLimit):
		return nil, s.limitError(store.MemoryActive)
	case err != nil:
		return nil, internalError("confirm", err)
	}
	s.embedAsync(login, out)
	return out, nil
}

// Reject discards what the agent proposed: the proposal of an active entry is
// dropped (the confirmed entry stays as it is), a pending entry is deleted
// (it was never confirmed). Nothing waiting: memory_not_pending. It returns
// the entry kept (nil when deleted).
func (s *Service) Reject(ctx context.Context, login, id string, exp Expect) (*store.MemoryEntry, *Error) {
	e, rerr := s.resolve(ctx, login, id, "")
	if rerr != nil {
		return nil, rerr
	}
	exp.Pending = true
	if cerr := checkExpect(e, exp); cerr != nil {
		return nil, cerr
	}
	if e.Proposal == nil {
		err := s.st.DeleteMemoryEntry(ctx, login, e.ID, store.MemoryPending)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil, newError("memory_not_found", "Memory id not found", map[string]any{"id": id})
		case errors.Is(err, store.ErrMemoryConflict):
			return nil, notPendingError(e.ID)
		case err != nil:
			return nil, internalError("reject", err)
		}
		return nil, nil
	}
	e.Proposal, e.UpdatedAt = nil, s.now()
	out, err := s.st.UpdateMemoryEntry(ctx, e, s.limits())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, newError("memory_not_found", "Memory id not found", map[string]any{"id": id})
	case errors.Is(err, store.ErrMemoryConflict):
		return nil, conflictError(e.ID)
	case err != nil:
		return nil, internalError("reject", err)
	}
	return out, nil
}

// Restore makes a soft-deleted entry active again.
func (s *Service) Restore(ctx context.Context, login, id string) (*store.MemoryEntry, *Error) {
	e, err := s.st.GetMemoryEntry(ctx, login, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, newError("memory_not_found", "Memory id not found", map[string]any{"id": id})
	}
	if err != nil {
		return nil, internalError("restore", err)
	}
	if e.Status != store.MemoryDeleted {
		return e, nil
	}
	// The limit is checked by the store under its per-user lock (a concurrent create cannot overshoot it).
	e.Status, e.DeletedAt, e.UpdatedAt = store.MemoryActive, nil, s.now()
	out, err := s.st.UpdateMemoryEntry(ctx, e, s.limits())
	if errors.Is(err, store.ErrMemoryPathExists) {
		return nil, newError("path_exists", "Target path already exists", map[string]any{"path": e.Path})
	}
	if errors.Is(err, store.ErrMemoryConflict) {
		return nil, conflictError(e.ID)
	}
	if errors.Is(err, store.ErrMemoryLimit) {
		return nil, s.limitError(store.MemoryActive)
	}
	if err != nil {
		return nil, internalError("restore", err)
	}
	return out, nil
}

func (s *Service) semanticBackend() string {
	if s.o.Embedder != nil {
		return "embeddings:" + s.o.Embedder.Model()
	}
	return s.o.EmbeddingBackend
}

// SearchAll searches several layers (the shared memory and the user's own) and merges the results by score.
func (s *Service) SearchAll(ctx context.Context, layers []Layer, in SearchInput) Result {
	if len(layers) == 1 {
		return s.Search(ctx, layers[0].Key, in)
	}
	topK := in.TopK
	if topK == 0 {
		topK = DefaultTopK
	}
	var merged []map[string]any
	var first Result
	for i, l := range layers {
		r := s.Search(ctx, l.Key, in)
		if i == 0 {
			first = r
		}
		if r.IsError {
			return r
		}
		list, _ := r.Data["results"].([]map[string]any)
		for _, h := range list {
			h["layer"] = l.Name
			merged = append(merged, h)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		a, _ := merged[i]["score"].(float64)
		b, _ := merged[j]["score"].(float64)
		return a > b
	})
	if len(merged) > topK {
		merged = merged[:topK]
	}
	first.Data["results"] = merged
	if tr, ok := first.Data["trace"].(map[string]any); ok {
		tr["used_memory"] = len(merged) > 0
		tr["chunk_count"] = len(merged)
		if len(merged) > 0 {
			tr["reason"] = "matches_found"
		}
	}
	return first
}
