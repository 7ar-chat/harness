// Package documents is the user's document library: the files a person wants every agent to be able to use (a CV, a
// cover-letter template, an ID scan, a portfolio). A document belongs to a user, not to a conversation, so it is still
// there in the next one.
//
// Files live on disk, one directory per user (the user's login hashed), so the same directory can be mounted read-only in
// a service the agent drives (the browser, to attach a file to a web form). The agent reaches them three ways:
//
//   - documents_list      what the user has, with the path of each file in the sandbox and in the browser
//   - documents_to_workspace copies them to /workspace/documents/ in the conversation's sandbox
//   - a prompt section    names them, so the agent knows they exist without calling a tool
package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
)

// Limits.
const (
	MaxFileBytes = 20 << 20 // one document
	MaxFiles     = 50       // documents per user
	MaxTotal     = 200 << 20
)

// WorkspaceDir is where documents_to_workspace puts the files in a sandbox.
const WorkspaceDir = "/workspace/documents"

var (
	// ErrInvalid is a name or content that is refused (the message says why).
	ErrInvalid = errors.New("invalid document")
	// ErrNotFound means the user has no such document.
	ErrNotFound = errors.New("document not found")
	// ErrLimit means the user is over the number or the total size of documents.
	ErrLimit = errors.New("document limit reached")
)

// allowed are the file types a document may have. Scripts, archives and executables are refused: the files end up in a
// sandbox and in a browser form, and none of those needs code.
var allowed = map[string]string{
	".pdf": "application/pdf", ".doc": "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".odt":  "application/vnd.oasis.opendocument.text", ".rtf": "application/rtf",
	".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv", ".json": "application/json",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".png":  "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
}

// Doc describes one stored document.
type Doc struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	MimeType string    `json:"mimeType"`
	Modified time.Time `json:"modified"`
}

// Service stores the documents under Dir.
type Service struct {
	// Dir is the root directory; each user has a subdirectory (see Tag).
	Dir string
	// BrowserRoot is where the browser service sees Dir ("" = no browser): documents_list reports
	// BrowserRoot/<tag>/<name> for a file chooser.
	BrowserRoot string
	// Sandboxes receives documents_to_workspace (nil: the tool is not offered).
	Sandboxes sandbox.Manager

	mu sync.Mutex
}

// New returns a service storing under dir.
func New(dir string) *Service { return &Service{Dir: dir} }

// Tag is the name of the user's directory: a hash, never the login.
func Tag(login string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(login))))
	return hex.EncodeToString(sum[:8])
}

func (s *Service) userDir(login string) (string, error) {
	if strings.TrimSpace(login) == "" {
		return "", fmt.Errorf("%w: no user", ErrInvalid)
	}
	return filepath.Join(s.Dir, Tag(login)), nil
}

var unsafeName = regexp.MustCompile(`[\x00-\x1f\x7f/\\:*?"<>|]+`)

// CleanName returns the file name to store, or an error: one path component, a known type, no control characters.
func CleanName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base("/" + name)
	name = strings.TrimSpace(unsafeName.ReplaceAllString(name, "_"))
	name = strings.Trim(name, ". ")
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "_")
	}
	if name == "" || name == "/" {
		return "", fmt.Errorf("%w: the file has no name", ErrInvalid)
	}
	if len(name) > 150 {
		ext := path.Ext(name)
		name = name[:150-len(ext)] + ext
	}
	if _, ok := allowed[strings.ToLower(path.Ext(name))]; !ok {
		return "", fmt.Errorf("%w: %q is not a supported type (PDF, Word, text, spreadsheet, slides or image)", ErrInvalid, path.Ext(name))
	}
	return name, nil
}

// Put stores (or replaces) the user's document name.
func (s *Service) Put(login, name string, data []byte) (Doc, error) {
	name, err := CleanName(name)
	if err != nil {
		return Doc{}, err
	}
	if len(data) == 0 {
		return Doc{}, fmt.Errorf("%w: the file is empty", ErrInvalid)
	}
	if len(data) > MaxFileBytes {
		return Doc{}, fmt.Errorf("%w: at most %d MiB per file", ErrLimit, MaxFileBytes>>20)
	}
	dir, err := s.userDir(login)
	if err != nil {
		return Doc{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, _ := s.list(dir)
	var total int64
	replacing := false
	for _, d := range existing {
		if d.Name == name {
			replacing = true
			continue
		}
		total += d.Size
	}
	if !replacing && len(existing) >= MaxFiles {
		return Doc{}, fmt.Errorf("%w: at most %d documents", ErrLimit, MaxFiles)
	}
	if total+int64(len(data)) > MaxTotal {
		return Doc{}, fmt.Errorf("%w: at most %d MiB in total", ErrLimit, MaxTotal>>20)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Doc{}, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return Doc{}, err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return Doc{}, err
	}
	// The browser container runs as another user and only reads these files.
	_ = os.Chmod(tmp.Name(), 0o644)
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		os.Remove(tmp.Name())
		return Doc{}, err
	}
	return Doc{Name: name, Size: int64(len(data)), MimeType: MimeOf(name), Modified: time.Now().UTC()}, nil
}

// MimeOf is the media type of a stored name.
func MimeOf(name string) string {
	if m, ok := allowed[strings.ToLower(path.Ext(name))]; ok {
		return m
	}
	return "application/octet-stream"
}

// List returns the user's documents by name.
func (s *Service) List(login string) ([]Doc, error) {
	dir, err := s.userDir(login)
	if err != nil {
		return nil, err
	}
	return s.list(dir)
}

func (s *Service) list(dir string) ([]Doc, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Doc
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Doc{Name: e.Name(), Size: info.Size(), MimeType: MimeOf(e.Name()), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns the content of the user's document.
func (s *Service) Get(login, name string) ([]byte, Doc, error) {
	name, err := CleanName(name)
	if err != nil {
		return nil, Doc{}, ErrNotFound
	}
	dir, err := s.userDir(login)
	if err != nil {
		return nil, Doc{}, err
	}
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return nil, Doc{}, ErrNotFound
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, Doc{}, ErrNotFound
	}
	return data, Doc{Name: name, Size: info.Size(), MimeType: MimeOf(name), Modified: info.ModTime().UTC()}, nil
}

// Delete removes the user's document (no error when it is already gone).
func (s *Service) Delete(login, name string) error {
	name, err := CleanName(name)
	if err != nil {
		return nil
	}
	dir, err := s.userDir(login)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// BrowserPath is where the browser service sees the document ("" without a BrowserRoot).
func (s *Service) BrowserPath(login, name string) string {
	if s.BrowserRoot == "" {
		return ""
	}
	return path.Join(s.BrowserRoot, Tag(login), name)
}

// CopyTo writes the user's documents to /workspace/documents of sb and returns the names written.
func (s *Service) CopyTo(ctx context.Context, login string, sb sandbox.Sandbox, only string) ([]string, error) {
	docs, err := s.List(login)
	if err != nil {
		return nil, err
	}
	if err := sb.MkdirAll(ctx, WorkspaceDir); err != nil {
		return nil, err
	}
	var done []string
	for _, d := range docs {
		if only != "" && d.Name != only {
			continue
		}
		data, _, err := s.Get(login, d.Name)
		if err != nil {
			continue
		}
		if err := sb.WriteFile(ctx, path.Join(WorkspaceDir, d.Name), data); err != nil {
			return done, err
		}
		done = append(done, d.Name)
	}
	if only != "" && len(done) == 0 {
		return nil, ErrNotFound
	}
	return done, nil
}

// PromptSection names the run's user's documents so the agent knows what it can use (an agent.PromptSection). It says
// nothing for a user with no documents.
func (s *Service) PromptSection(_ context.Context, in agent.Input) string {
	if in.Run == nil {
		return ""
	}
	docs, err := s.List(in.Run.UserLogin)
	if err != nil || len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<user_documents>\nThe user keeps these documents for you to use (their CV, templates, ...). They are the user's own files: use them as the source of facts about the user, and never invent what they do not say.\n")
	for i, d := range docs {
		if i == 20 {
			fmt.Fprintf(&b, "... and %d more (documents_list shows all)\n", len(docs)-i)
			break
		}
		fmt.Fprintf(&b, "- %s (%s)\n", d.Name, humanSize(d.Size))
	}
	b.WriteString("Call documents_list for the exact paths. To read or edit a document, documents_to_workspace puts it in " + WorkspaceDir + ". To attach one to a web form, use the browser path from documents_list in the file chooser.\n</user_documents>")
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// Provider offers the document tools (a tools.Provider).
type Provider struct{ Svc *Service }

// Name implements tools.Provider.
func (p Provider) Name() string { return "documents" }

type listIn struct{}

type listedDoc struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Type        string `json:"type"`
	SandboxPath string `json:"sandbox_path"`
	BrowserPath string `json:"browser_path,omitempty"`
}

type toWorkspaceIn struct {
	Name string `json:"name,omitempty" jsonschema_description:"One document to copy; leave empty to copy all of them"`
}

// Tools implements tools.Provider.
func (p Provider) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.Svc == nil || rc.Login == "" {
		return nil, func() {}, nil
	}
	svc, login := p.Svc, rc.Login
	list, err := ai.InferTool("documents_list", "List the user's saved documents (CV, templates...) with the path of each in the sandbox and, for uploading to a web form, in the browser.",
		func(_ context.Context, _ listIn) ([]listedDoc, error) {
			docs, err := svc.List(login)
			if err != nil {
				return nil, tools.Errorf("could not list the documents: %v", err)
			}
			out := make([]listedDoc, 0, len(docs))
			for _, d := range docs {
				out = append(out, listedDoc{Name: d.Name, Size: d.Size, Type: d.MimeType,
					SandboxPath: path.Join(WorkspaceDir, d.Name), BrowserPath: svc.BrowserPath(login, d.Name)})
			}
			return out, nil
		})
	if err != nil {
		return nil, nil, err
	}
	out := []ai.BaseTool{list}
	if svc.Sandboxes != nil && svc.Sandboxes.Enabled() && rc.Sandbox != "off" {
		cp, err := ai.InferTool("documents_to_workspace", "Copy the user's saved documents into "+WorkspaceDir+" in the sandbox so they can be read, converted or edited.",
			func(ctx context.Context, in toWorkspaceIn) (map[string]any, error) {
				sb, err := svc.Sandboxes.Acquire(ctx, rc.ThreadID, login)
				if err != nil {
					return nil, tools.Errorf("no sandbox available: %v", err)
				}
				names, err := svc.CopyTo(ctx, login, sb, strings.TrimSpace(in.Name))
				if errors.Is(err, ErrNotFound) {
					return nil, tools.Errorf("the user has no document named %q (documents_list shows them)", in.Name)
				}
				if err != nil {
					return nil, tools.Errorf("copy failed: %v", err)
				}
				return map[string]any{"directory": WorkspaceDir, "copied": names}, nil
			})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, cp)
	}
	return out, func() {}, nil
}
