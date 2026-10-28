package sandboxtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// The in-memory Sandbox emulates the sandbox-side scripts of the sandbox tools
// (internal/tools/sandboxtools): a command whose first line is one of these
// markers is answered by the fake instead of the scripted ExecFunc, with the
// same environment variables and output format as the real script.
const (
	markJobStart  = "# work:job-start"
	markJobWait   = "# work:job-wait"
	markJobStatus = "# work:job-status"
	markJobKill   = "# work:job-kill"
	markExtract   = "# work:extract"
	markRender    = "# work:render"
	markSearch    = "# work:search"
	markSweep     = "# work:sweep"
)

// JobFunc runs the command of an execute_command job in the fake sandbox: out
// streams its output ("stdout" or "stderr"); it returns the exit code. The
// context is cancelled by job_kill.
type JobFunc func(ctx context.Context, command string, out func(stream, text string)) int

// Doc is the content the fake markitdown/LibreOffice see in a document: its
// pages (PDF), slides (PPTX) or sheets (XLSX, with Names), or one part (DOCX).
type Doc struct {
	Parts []string
	Names []string
}

// SetJobs scripts the commands of execute_command (default: the ExecFunc set
// with SetExec runs them, as plain commands).
func (s *Sandbox) SetJobs(f JobFunc) {
	s.mu.Lock()
	s.jobFn = f
	s.mu.Unlock()
}

// PutDoc stores a binary document file at p whose extracted text is d.
func (s *Sandbox) PutDoc(p string, d Doc) {
	s.Put(p, "%DOC\x00"+strings.Join(d.Parts, "\f"))
	s.mu.Lock()
	if s.docs == nil {
		s.docs = map[string]Doc{}
	}
	s.docs[p] = d
	s.mu.Unlock()
}

// Renders lists the paths of the page images written by the fake renderer.
func (s *Sandbox) Renders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.files {
		if strings.HasPrefix(p, "/workspace/.work/renders/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

type fakeJob struct {
	mu       sync.Mutex
	dir      string
	stdout   strings.Builder
	stderr   strings.Builder
	done     chan struct{}
	exit     int
	killed   bool
	removed  bool // its directory was deleted (RemoveTree)
	cancel   context.CancelFunc
	onOutput func(stream, text string)
}

func (j *fakeJob) write(stream, text string) {
	j.mu.Lock()
	if stream == "stderr" {
		j.stderr.WriteString(text)
	} else {
		j.stdout.WriteString(text)
	}
	f := j.onOutput
	j.mu.Unlock()
	if f != nil {
		f(stream, text)
	}
}

// intercept returns the fake handling a marked command (nil: a plain command).
func (s *Sandbox) intercept(req sandbox.ExecRequest) func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
	first, _, _ := strings.Cut(req.Command, "\n")
	switch strings.TrimSpace(first) {
	case markJobStart:
		return s.jobStart
	case markJobWait:
		return s.jobWait
	case markJobStatus:
		return s.jobStatus
	case markJobKill:
		return s.jobKill
	case markExtract:
		return s.extract
	case markRender:
		return s.render
	case markSearch:
		return s.search
	case sandbox.RealPathMarker:
		return s.realpath
	case sandbox.ReadFilesMarker:
		return s.readFiles
	case sandbox.PlaceMarker:
		return s.place
	case sandbox.UnstageMarker:
		return s.unstage
	case markSweep:
		return s.sweep
	}
	return nil
}

// RealPathCalls counts the realpath scripts the sandbox ran.
func (s *Sandbox) RealPathCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.realpaths
}

// place emulates the place script: WORK_STAGED moves onto WORK_TARGET unless
// the target is a symlink or its directory resolves elsewhere (exit 4, the
// real directory on stdout).
func (s *Sandbox) place(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	target, staged := req.Env["WORK_TARGET"], req.Env["WORK_STAGED"]
	dir := path.Dir(target)
	s.mu.Lock()
	data, ok := s.files[staged]
	delete(s.files, staged)
	real := s.realLocked(dir)
	_, link := s.links[path.Clean(target)]
	s.mu.Unlock()
	if !ok {
		return sandbox.ExecResult{ExitCode: 3}, nil
	}
	if real != dir || link {
		if req.OnStdout != nil {
			req.OnStdout(real)
		}
		return sandbox.ExecResult{ExitCode: 4}, nil
	}
	s.Put(target, string(data))
	return sandbox.ExecResult{}, nil
}

// unstage emulates the unstage script: WORK_STAGED is removed.
func (s *Sandbox) unstage(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	s.mu.Lock()
	delete(s.files, req.Env["WORK_STAGED"])
	s.mu.Unlock()
	return sandbox.ExecResult{}, nil
}

// sweep emulates the sweep script's staging part: the files directly under
// WORK_STAGING_DIR older than WORK_STAGING_MINUTES are removed (the fake's
// jobs keep no shadow records to prune).
func (s *Sandbox) sweep(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir := strings.TrimSuffix(req.Env["WORK_STAGING_DIR"], "/") + "/"
	cutoff := time.Now().Add(-time.Duration(atoiEnv(req.Env, "WORK_STAGING_MINUTES")) * time.Minute)
	s.mu.Lock()
	for p := range s.files {
		if dir != "/" && strings.HasPrefix(p, dir) && !strings.Contains(p[len(dir):], "/") && s.mtimes[p].Before(cutoff) {
			delete(s.files, p)
		}
	}
	s.mu.Unlock()
	return sandbox.ExecResult{}, nil
}

// readFiles emulates sandbox.ReadRegularFiles' script over the in-memory
// files: symlinked paths (or under a symlink) and missing ones are skipped.
func (s *Sandbox) readFiles(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	n := atoiEnv(req.Env, "WORK_N")
	budget, _ := strconv.ParseInt(req.Env["WORK_BUDGET"], 10, 64)
	var out strings.Builder
	s.mu.Lock()
	for i := 0; i < n; i++ {
		p := req.Env["WORK_PATH_"+strconv.Itoa(i)]
		data, ok := s.files[p]
		if !ok || s.realLocked(p) != p {
			continue
		}
		size := int64(len(data))
		st := fmt.Sprintf("%d %d", size, s.mtimes[p].Unix())
		k := req.Env["WORK_KNOWN_"+strconv.Itoa(i)]
		if k != "" && (k == st || k == fmt.Sprintf("%d -", size)) {
			fmt.Fprintf(&out, "%d %s U -\n", i, st)
			continue
		}
		if budget <= 0 {
			continue
		}
		s.fileReads++
		fmt.Fprintf(&out, "%d %s F %s\n", i, st, base64.StdEncoding.EncodeToString(data[:min(size, budget)]))
		budget = max(0, budget-size)
	}
	s.mu.Unlock()
	if req.OnStdout != nil {
		req.OnStdout(out.String())
	}
	return sandbox.ExecResult{}, nil
}

// FileReads counts the file contents the read-files script returned.
func (s *Sandbox) FileReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fileReads
}

func (s *Sandbox) job(dir string) *fakeJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[dir]
}

func (s *Sandbox) jobStart(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir := req.Env["WORK_JOB_DIR"]
	raw, err := base64.StdEncoding.DecodeString(req.Env["WORK_JOB_CMD_B64"])
	if err != nil || dir == "" {
		return sandbox.ExecResult{ExitCode: 2}, nil
	}
	command := string(raw)
	jctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j := &fakeJob{dir: dir, done: make(chan struct{}), cancel: cancel}
	s.mu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*fakeJob{}
	}
	s.jobs[dir] = j
	run := s.jobFn
	plain := s.execFn
	// The job's command is what the agent ran: record it like a plain command.
	s.execs = append(s.execs, sandbox.ExecRequest{Command: command, Cwd: sandbox.Workspace, Env: req.Env})
	s.mu.Unlock()
	if run == nil {
		run = func(ctx context.Context, command string, out func(stream, text string)) int {
			if plain == nil {
				return 0
			}
			res, err := plain(ctx, sandbox.ExecRequest{Command: command, Cwd: sandbox.Workspace,
				OnStdout: func(t string) { out("stdout", t) }, OnStderr: func(t string) { out("stderr", t) }})
			if err != nil {
				out("stderr", err.Error()+"\n")
				return 1
			}
			return res.ExitCode
		}
	}
	wait := atoiEnv(req.Env, "WORK_JOB_WAIT")
	if wait > 0 {
		j.onOutput = forward(req)
	}
	go func() {
		code := run(jctx, command, j.write)
		j.mu.Lock()
		j.exit = code
		j.mu.Unlock()
		close(j.done)
	}()
	if wait <= 0 {
		return sandbox.ExecResult{}, nil
	}
	return s.follow(ctx, j, req, wait)
}

func forward(req sandbox.ExecRequest) func(stream, text string) {
	return func(stream, text string) {
		if stream == "stderr" {
			if req.OnStderr != nil {
				req.OnStderr(text)
			}
		} else if req.OnStdout != nil {
			req.OnStdout(text)
		}
	}
}

// follow streams the job's output until it ends or wait seconds pass.
func (s *Sandbox) follow(ctx context.Context, j *fakeJob, req sandbox.ExecRequest, wait int) (sandbox.ExecResult, error) {
	defer func() {
		j.mu.Lock()
		j.onOutput = nil
		j.mu.Unlock()
	}()
	select {
	case <-j.done:
	case <-time.After(time.Duration(wait) * time.Second):
	case <-ctx.Done():
		return sandbox.ExecResult{}, ctx.Err()
	}
	return sandbox.ExecResult{}, nil
}

func (s *Sandbox) jobWait(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	j := s.job(req.Env["WORK_JOB_DIR"])
	if j == nil {
		return sandbox.ExecResult{}, nil
	}
	select {
	case <-j.done:
		return sandbox.ExecResult{}, nil
	default:
	}
	j.mu.Lock()
	j.onOutput = forward(req)
	j.mu.Unlock()
	return s.follow(ctx, j, req, atoiEnv(req.Env, "WORK_JOB_WAIT"))
}

func (s *Sandbox) jobKill(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	j := s.job(req.Env["WORK_JOB_DIR"])
	if j == nil {
		return sandbox.ExecResult{ExitCode: 1}, nil
	}
	j.mu.Lock()
	select {
	case <-j.done:
	default:
		j.killed = true
	}
	j.mu.Unlock()
	j.cancel()
	return sandbox.ExecResult{}, nil
}

// jobStatus prints the job-status report: status, exit code, then per stream
// its size and the base64 head and tail (WORK_HEAD / WORK_TAIL bytes).
func (s *Sandbox) jobStatus(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	j := s.job(req.Env["WORK_JOB_DIR"])
	var b strings.Builder
	removed := false
	if j != nil {
		j.mu.Lock()
		removed = j.removed
		j.mu.Unlock()
	}
	switch {
	case j == nil:
		b.WriteString("status missing\n")
	case removed: // the shadow record (/tmp/work-jobs) of a job whose directory was deleted
		select {
		case <-j.done:
			fmt.Fprintf(&b, "status removed\ncode %d\n", j.exit)
		default:
			b.WriteString("status running\ncode \n")
		}
		b.WriteString("stdout 0 - -\nstderr 0 - -\n")
	default:
		j.mu.Lock()
		status, code := "running", ""
		select {
		case <-j.done:
			status, code = "finished", strconv.Itoa(j.exit)
		default:
		}
		if j.killed {
			status, code = "killed", ""
		}
		fmt.Fprintf(&b, "status %s\ncode %s\n", status, code)
		head, tail := atoiEnv(req.Env, "WORK_HEAD"), atoiEnv(req.Env, "WORK_TAIL")
		for _, st := range []struct{ name, data string }{{"stdout", j.stdout.String()}, {"stderr", j.stderr.String()}} {
			h, t := st.data, ""
			if len(st.data) > head+tail {
				h, t = st.data[:head], st.data[len(st.data)-tail:]
			}
			fmt.Fprintf(&b, "%s %d %s %s\n", st.name, len(st.data), b64(h), b64(t))
		}
		j.mu.Unlock()
		if status != "running" {
			// The files the real job writes (outputPath).
			s.Put(j.dir+"/stdout", j.stdoutString())
			s.Put(j.dir+"/stderr", j.stderrString())
		}
	}
	if req.OnStdout != nil {
		req.OnStdout(b.String())
	}
	return sandbox.ExecResult{}, nil
}

func (j *fakeJob) stdoutString() string { j.mu.Lock(); defer j.mu.Unlock(); return j.stdout.String() }
func (j *fakeJob) stderrString() string { j.mu.Lock(); defer j.mu.Unlock(); return j.stderr.String() }

func b64(s string) string {
	if s == "" {
		return "-"
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func atoiEnv(env map[string]string, k string) int {
	n, _ := strconv.Atoi(env[k])
	return n
}

// selectParts resolves a "1-3,7" range over n parts (empty: all), 1-based.
func selectParts(spec string, n int) ([]int, bool) {
	if strings.TrimSpace(spec) == "" {
		out := make([]int, n)
		for i := range out {
			out[i] = i + 1
		}
		return out, true
	}
	var out []int
	for _, part := range strings.Split(spec, ",") {
		a, b, isRange := strings.Cut(strings.TrimSpace(part), "-")
		lo, err1 := strconv.Atoi(strings.TrimSpace(a))
		hi := lo
		var err2 error
		if isRange {
			hi, err2 = strconv.Atoi(strings.TrimSpace(b))
		}
		if err1 != nil || err2 != nil || lo < 1 || hi < lo {
			return nil, false
		}
		for p := lo; p <= hi && p <= n; p++ {
			out = append(out, p)
		}
	}
	return out, true
}

func (s *Sandbox) doc(p string) (Doc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[p]
	return d, ok
}

func reply(req sandbox.ExecRequest, v any) (sandbox.ExecResult, error) {
	data, _ := json.Marshal(v)
	if req.OnStdout != nil {
		req.OnStdout(string(data) + "\n")
	}
	return sandbox.ExecResult{}, nil
}

// extract emulates the markitdown extraction script.
func (s *Sandbox) extract(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	p := req.Env["WORK_PATH"]
	d, ok := s.doc(p)
	if !ok {
		return reply(req, map[string]any{"error": "cannot extract text from " + p})
	}
	ext := strings.TrimPrefix(path.Ext(p), ".")
	unit := map[string]string{"pdf": "pages", "pptx": "slides", "xlsx": "sheets"}[ext]
	out := map[string]any{"kind": ext, "unit": unit, "total": len(d.Parts)}
	if sheet := req.Env["WORK_SHEET"]; sheet != "" {
		for i, n := range d.Names {
			if n == sheet {
				out["shown"], out["text"] = "sheet "+n, "## "+n+"\n"+d.Parts[i]
				return reply(req, out)
			}
		}
		return reply(req, map[string]any{"error": fmt.Sprintf("no sheet %q (sheets: %s)", sheet, strings.Join(d.Names, ", "))})
	}
	pages, ok := selectParts(req.Env["WORK_PAGES"], len(d.Parts))
	if !ok || (unit == "" && req.Env["WORK_PAGES"] != "") {
		return reply(req, map[string]any{"error": "invalid pages " + req.Env["WORK_PAGES"]})
	}
	var parts []string
	var shown []string
	for _, pg := range pages {
		head := ""
		switch unit {
		case "pages":
			head = fmt.Sprintf("## Page %d\n", pg)
		case "slides":
			head = fmt.Sprintf("## Slide %d\n", pg)
		case "sheets":
			head = "## " + d.Names[pg-1] + "\n"
		}
		parts = append(parts, head+d.Parts[pg-1])
		shown = append(shown, strconv.Itoa(pg))
	}
	out["shown"], out["text"] = strings.Join(shown, ","), strings.Join(parts, "\n\n")
	return reply(req, out)
}

// render emulates the page renderer: one fake PNG per selected page.
func (s *Sandbox) render(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	p := req.Env["WORK_PATH"]
	d, ok := s.doc(p)
	if !ok {
		return reply(req, map[string]any{"error": "cannot render " + p})
	}
	pages, ok := selectParts(req.Env["WORK_PAGES"], len(d.Parts))
	if !ok {
		return reply(req, map[string]any{"error": "invalid pages " + req.Env["WORK_PAGES"]})
	}
	max := atoiEnv(req.Env, "WORK_MAX")
	if max > 0 && len(pages) > max {
		pages = pages[:max]
	}
	out := req.Env["WORK_OUT"]
	var list []map[string]any
	for _, pg := range pages {
		img := fmt.Sprintf("%s/p%d.png", out, pg)
		s.Put(img, "\x89PNG\r\n\x1a\nfake page "+strconv.Itoa(pg))
		list = append(list, map[string]any{"page": pg, "imagePath": img})
	}
	return reply(req, map[string]any{"totalPages": len(d.Parts), "pages": list})
}

// search emulates `rg --json` over the in-memory files (match lines only).
func (s *Sandbox) search(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	pattern := req.Env["WORK_PATTERN"]
	if req.Env["WORK_ICASE"] == "1" {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		if req.OnStderr != nil {
			req.OnStderr("regex parse error: " + err.Error() + "\n")
		}
		return sandbox.ExecResult{ExitCode: 2}, nil
	}
	root := path.Clean(req.Env["WORK_PATH"])
	glob := req.Env["WORK_GLOB"]
	s.mu.Lock()
	var paths []string
	for p := range s.files {
		if p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") {
			paths = append(paths, p)
		}
	}
	files := map[string][]byte{}
	for _, p := range paths {
		files[p] = s.files[p]
	}
	s.mu.Unlock()
	sort.Strings(paths)
	found := false
	for _, p := range paths {
		if strings.Contains(p, "/.git/") {
			continue
		}
		if glob != "" {
			if ok, _ := path.Match(glob, path.Base(p)); !ok {
				if ok2, _ := path.Match(glob, strings.TrimPrefix(p, strings.TrimSuffix(root, "/")+"/")); !ok2 {
					continue
				}
			}
		}
		for i, line := range strings.Split(string(files[p]), "\n") {
			if strings.ContainsRune(line, 0) {
				break
			}
			if re.MatchString(line) {
				found = true
				ev := map[string]any{"type": "match", "data": map[string]any{
					"path": map[string]any{"text": p}, "line_number": i + 1, "lines": map[string]any{"text": line + "\n"}}}
				data, _ := json.Marshal(ev)
				if req.OnStdout != nil {
					req.OnStdout(string(data) + "\n")
				}
			}
		}
	}
	if !found {
		return sandbox.ExecResult{ExitCode: 1}, nil
	}
	return sandbox.ExecResult{}, nil
}
