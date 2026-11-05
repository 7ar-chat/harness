// Package documents is the user's document library: the files a person wants every agent to be able to use (a CV, a
// cover-letter template, an ID scan, a portfolio). A document belongs to a user, not to a conversation, so it is still
// there in the next one.
//
// Files live on disk, one directory per user (the user's login hashed), so the same directory can be mounted read-only in
// a service the agent drives (the browser, to attach a file to a web form). The agent reaches them three ways:
//
//   - documents_list      what the user has, with the path of each file in the sandbox and in the browser
//   - documents_to_workspace copies them to /workspace/documents/ in the conversation's sandbox
//   - documents_write, documents_save_from_sandbox, documents_save_from_url, documents_rename and documents_delete let
//     the agent keep the library up to date: save a résumé found on the user's website, store an edited copy of a CV
//   - a prompt section    names them, so the agent knows they exist without calling a tool
package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
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
	// HTTP fetches documents_save_from_url. It must refuse private and loopback addresses (the host decides: the
	// URL comes from a model reading web pages). Nil: the tool is not offered.
	HTTP *http.Client

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

// Rename moves the user's document to a new name; it refuses to replace another document.
func (s *Service) Rename(login, from, to string) (Doc, error) {
	from, err := CleanName(from)
	if err != nil {
		return Doc{}, ErrNotFound
	}
	to, err = CleanName(to)
	if err != nil {
		return Doc{}, err
	}
	dir, err := s.userDir(login)
	if err != nil {
		return Doc{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := filepath.Join(dir, from)
	if info, err := os.Lstat(src); err != nil || !info.Mode().IsRegular() {
		return Doc{}, ErrNotFound
	}
	if from != to {
		if _, err := os.Lstat(filepath.Join(dir, to)); err == nil {
			return Doc{}, fmt.Errorf("%w: a document named %q already exists", ErrInvalid, to)
		}
		if err := os.Rename(src, filepath.Join(dir, to)); err != nil {
			return Doc{}, err
		}
	}
	info, err := os.Stat(filepath.Join(dir, to))
	if err != nil {
		return Doc{}, err
	}
	return Doc{Name: to, Size: info.Size(), MimeType: MimeOf(to), Modified: info.ModTime().UTC()}, nil
}

// Fetch downloads rawURL (http or https) with the service's client, at most MaxFileBytes. name is the stored name;
// empty derives one from the response or the URL.
func (s *Service) Fetch(ctx context.Context, rawURL, name string) ([]byte, string, error) {
	if s.HTTP == nil {
		return nil, "", errors.New("downloading is not available")
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", fmt.Errorf("%w: give an http or https URL", ErrInvalid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req.Header.Set("User-Agent", "harness-documents/1")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("the download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("the download failed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("the download failed: %v", err)
	}
	if len(data) > MaxFileBytes {
		return nil, "", fmt.Errorf("%w: at most %d MiB per file", ErrLimit, MaxFileBytes>>20)
	}
	if name == "" {
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
			name = params["filename"]
		}
	}
	if name == "" {
		name = path.Base(u.Path)
	}
	if path.Ext(name) == "" {
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		for ext, m := range allowed {
			if m == ct {
				name += ext
				break
			}
		}
	}
	return data, name, nil
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
	return s.CopyToDir(ctx, login, sb, WorkspaceDir, only)
}

// CopyToDir is CopyTo into another directory of the sandbox.
func (s *Service) CopyToDir(ctx context.Context, login string, sb sandbox.Sandbox, dir, only string) ([]string, error) {
	docs, err := s.List(login)
	if err != nil {
		return nil, err
	}
	if err := sb.MkdirAll(ctx, dir); err != nil {
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
		if err := sb.WriteFile(ctx, path.Join(dir, d.Name), data); err != nil {
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
	if err != nil {
		return ""
	}
	if len(docs) == 0 {
		return "<user_documents>\nThe user's document library is empty. When the user gives you a document or asks you to find one (their CV on their website, say), keep it with documents_save_from_url, documents_save_from_sandbox or documents_write so every later conversation has it.\n</user_documents>"
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
	b.WriteString("Call documents_list for the exact paths. To read or edit a document, documents_to_workspace puts it in " + WorkspaceDir + ", and documents_save_from_sandbox stores the edited file back. To attach one to a web form, use the browser path from documents_list in the file chooser.\n</user_documents>")
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
	write, err := ai.InferTool("documents_write", "Save a text document (.txt, .md, .csv, .json) in the user's library, replacing one of the same name. Use it for a cover-letter template, a profile summary or notes the user wants kept.",
		func(_ context.Context, in writeIn) (savedDoc, error) {
			return saved(svc, login, in.Name, []byte(in.Content), true)
		})
	if err != nil {
		return nil, nil, err
	}
	rename, err := ai.InferTool("documents_rename", "Rename one of the user's documents (it never replaces another one).",
		func(_ context.Context, in renameIn) (savedDoc, error) {
			d, err := svc.Rename(login, in.From, in.To)
			if errors.Is(err, ErrNotFound) {
				return savedDoc{}, tools.Errorf("the user has no document named %q (documents_list shows them)", in.From)
			}
			if err != nil {
				return savedDoc{}, tools.Errorf("%v", err)
			}
			return savedDoc{Name: d.Name, Size: d.Size, SandboxPath: path.Join(WorkspaceDir, d.Name), BrowserPath: svc.BrowserPath(login, d.Name)}, nil
		})
	if err != nil {
		return nil, nil, err
	}
	del, err := ai.InferTool("documents_delete", "Delete one of the user's documents. Do it only when the user asked for that document to go.",
		func(_ context.Context, in deleteIn) (map[string]any, error) {
			if err := svc.Delete(login, in.Name); err != nil {
				return nil, tools.Errorf("could not delete: %v", err)
			}
			return map[string]any{"deleted": in.Name}, nil
		})
	if err != nil {
		return nil, nil, err
	}
	out = append(out, write, rename, del)
	if svc.HTTP != nil {
		fetch, err := ai.InferTool("documents_save_from_url", "Download a file from a public web address (a résumé PDF linked on the user's website, say) and save it in the user's library. Find the link first (browser_snapshot shows link targets). Replaces a document of the same name.",
			func(ctx context.Context, in fetchIn) (savedDoc, error) {
				data, name, err := svc.Fetch(ctx, in.URL, in.Name)
				if err != nil {
					return savedDoc{}, tools.Errorf("%v", err)
				}
				return saved(svc, login, name, data, false)
			})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fetch)
	}
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
		fromSb, err := ai.InferTool("documents_save_from_sandbox", "Save a file from the sandbox (an edited CV, a generated PDF) in the user's library, replacing a document of the same name. name defaults to the file's own name.",
			func(ctx context.Context, in fromSandboxIn) (savedDoc, error) {
				sb, err := svc.Sandboxes.Acquire(ctx, rc.ThreadID, login)
				if err != nil {
					return savedDoc{}, tools.Errorf("no sandbox available: %v", err)
				}
				p := strings.TrimSpace(in.Path)
				if !strings.HasPrefix(p, "/") {
					p = path.Join(sandbox.Workspace, p)
				}
				data, size, err := sb.ReadFile(ctx, p, MaxFileBytes+1)
				if err != nil {
					return savedDoc{}, tools.Errorf("could not read %s: %v", p, err)
				}
				if size > MaxFileBytes {
					return savedDoc{}, tools.Errorf("%s is larger than %d MiB", p, MaxFileBytes>>20)
				}
				name := in.Name
				if name == "" {
					name = path.Base(p)
				}
				return saved(svc, login, name, data, false)
			})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fromSb)
	}
	return out, func() {}, nil
}

type writeIn struct {
	Name    string `json:"name" jsonschema_description:"File name with a text extension, e.g. cover-letter.md"`
	Content string `json:"content" jsonschema_description:"The whole text of the document"`
}

type renameIn struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type deleteIn struct {
	Name string `json:"name"`
}

type fetchIn struct {
	URL  string `json:"url" jsonschema_description:"Address of the file (https)"`
	Name string `json:"name,omitempty" jsonschema_description:"Name to store it under, e.g. resume.pdf; defaults to the file's own name"`
}

type fromSandboxIn struct {
	Path string `json:"path" jsonschema_description:"Path of the file in the sandbox"`
	Name string `json:"name,omitempty" jsonschema_description:"Name to store it under; defaults to the file's name"`
}

type savedDoc struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SandboxPath string `json:"sandbox_path"`
	BrowserPath string `json:"browser_path,omitempty"`
}

// saved stores data and reports it the way documents_list does. textOnly refuses non-text extensions.
func saved(svc *Service, login, name string, data []byte, textOnly bool) (savedDoc, error) {
	if textOnly {
		switch strings.ToLower(path.Ext(name)) {
		case ".txt", ".md", ".csv", ".json":
		default:
			return savedDoc{}, tools.Errorf("documents_write saves text files (.txt, .md, .csv, .json); got %q", name)
		}
	}
	d, err := svc.Put(login, name, data)
	if err != nil {
		return savedDoc{}, tools.Errorf("%v", err)
	}
	return savedDoc{Name: d.Name, Size: d.Size, SandboxPath: path.Join(WorkspaceDir, d.Name), BrowserPath: svc.BrowserPath(login, d.Name)}, nil
}

// PublicClient is an HTTP client that only connects to public internet addresses: the check runs on the address
// actually dialled, so a name that resolves to a private address, a redirect to one and a DNS rebind are all refused.
// It ignores the proxy environment (a proxy would connect for it and hide the destination).
func PublicClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("refusing to connect to %q", address)
		}
		if !publicAddr(ap.Addr()) {
			return fmt.Errorf("refusing to connect to a non-public address (%s)", ap.Addr())
		}
		return nil
	}}
	return &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: true,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxIdleConns: 4},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return errors.New("redirect to an unsupported scheme refused")
			}
			return nil
		},
	}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && !(a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || cgnat.Contains(a))
}
