package sandboxtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

const readDesc = `Read a file from the sandbox. path is absolute or relative to /workspace.
- Text files: numbered lines like "cat -n". offset is the 1-based first line (default 1), limit the number of lines (default 2000);
  a page also stops at about 18,000 characters. When there is more, the last line tells you the exact offset to continue with.
- Documents (PDF, DOCX, PPTX, XLSX, ODT/ODS/ODP, DOC/XLS/PPT): their text, extracted with markitdown, paged the same way, after a
  header "[document: N pages/slides/sheets; showing …]". pages selects PDF pages or PPTX slides ("1-3,7"); sheet selects one XLSX sheet.
- Images (PNG, JPEG, WEBP, GIF up to 5 MiB): returns {path, image: true} and shows you the image (models that can see images).
To check how a document looks (layout, overflow, charts), use view_file. Other binary files are refused: inspect them with execute_command.`

const (
	maxImageBytes = 5 << 20
	extractWait   = 90 * time.Second
)

var imageMIME = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}

// documentExt are the files read_file extracts text from (markitdown, LibreOffice for the legacy and ODF formats).
var documentExt = map[string]bool{".pdf": true, ".docx": true, ".pptx": true, ".xlsx": true, ".xlsm": true,
	".odt": true, ".ods": true, ".odp": true, ".doc": true, ".xls": true, ".ppt": true, ".rtf": true}

type readInput struct {
	Path   string `json:"path" jsonschema_description:"File path, absolute or relative to /workspace"`
	Offset int    `json:"offset,omitempty" jsonschema_description:"1-based line to start from (default 1); use the offset given at the end of the previous page"`
	Limit  int    `json:"limit,omitempty" jsonschema_description:"Maximum number of lines to return (default 2000; a page also stops at about 18,000 characters)"`
	Pages  string `json:"pages,omitempty" jsonschema_description:"PDF pages or PPTX slides to extract, e.g. \"1-3,7\" (documents only; default all)"`
	Sheet  string `json:"sheet,omitempty" jsonschema_description:"Name of the XLSX sheet to extract (default all sheets)"`
}

func (b *binding) read(ctx context.Context, in readInput) (string, error) {
	p, err := resolve(in.Path)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(path.Ext(p))
	if mime, ok := imageMIME[ext]; ok {
		return b.readImage(ctx, p, mime)
	}
	if documentExt[ext] {
		return b.readDocument(ctx, p, in)
	}
	if in.Pages != "" || in.Sheet != "" {
		return "", tools.Errorf("pages and sheet only apply to documents (PDF, PPTX, XLSX)")
	}
	var data []byte
	var size int64
	if err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		var err error
		data, size, err = sb.ReadFile(ctx, p, maxReadBytes)
		return err
	}); err != nil {
		return "", mapFileErr(err, p)
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", tools.Errorf("%s looks like a binary file (%d bytes) that read_file cannot show; inspect it with execute_command (e.g. file, xxd | head), or convert it first", p, size)
	}
	byteTruncated := size > int64(len(data))
	if !byteTruncated {
		b.p.seen.record(b.rc.ThreadID, p, data)
	}
	text := string(data)
	if text == "" {
		return fmt.Sprintf("[%s is empty]", p), nil
	}
	out, err := page(p, "", text, in.Offset, in.Limit)
	if err != nil {
		return "", err
	}
	if byteTruncated {
		out += fmt.Sprintf("[The file is %d bytes; only the first %d bytes were read. Use execute_command (e.g. tail -c, sed -n) or search_files for the rest.]\n", size, maxReadBytes)
	}
	if note := b.pathNote(ctx, p); note != "" {
		out += note + "\n"
	}
	return out, nil
}

// page renders lines offset.. of text as numbered lines, at most limit lines
// and about maxReadChars characters, with an exact continuation footer.
func page(p, header, text string, offset, limit int) (string, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if offset > len(lines) {
		return "", tools.Errorf("offset %d is past the end of %s (%d lines)", offset, p, len(lines))
	}
	var sbuf strings.Builder
	sbuf.WriteString(header)
	end := offset - 1
	for i := offset - 1; i < len(lines) && i < offset-1+limit; i++ {
		line := lines[i]
		if len(line) > maxLineChars {
			line = truncateUTF8(line, maxLineChars) + " [line truncated]"
		}
		row := fmt.Sprintf("%6d\t%s\n", i+1, line)
		if i > offset-1 && sbuf.Len()+len(row) > maxReadChars {
			break
		}
		sbuf.WriteString(row)
		end = i + 1
	}
	if end < len(lines) {
		fmt.Fprintf(&sbuf, "[Showing lines %d-%d of %d. Use offset=%d to read more.]\n", offset, end, len(lines), end+1)
	}
	return sbuf.String(), nil
}

type imageResult struct {
	Path  string `json:"path"`
	Image bool   `json:"image"`
	Bytes int    `json:"bytes"`
	Note  string `json:"note,omitempty"`
}

func (b *binding) readImage(ctx context.Context, p, mime string) (string, error) {
	var data []byte
	var size int64
	if err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		var err error
		data, size, err = sb.ReadFile(ctx, p, maxImageBytes+1)
		return err
	}); err != nil {
		return "", mapFileErr(err, p)
	}
	if size > maxImageBytes {
		return "", tools.Errorf("%s is %d bytes; images over 5 MiB cannot be shown (downscale it first, e.g. with Pillow)", p, size)
	}
	res := imageResult{Path: p, Image: true, Bytes: len(data)}
	if !tools.AttachImage(ctx, tools.Image{Path: p, MIMEType: mime, Data: data}) {
		res.Note = "this model cannot view images; describe the file from its metadata or the script that produced it"
	}
	out, _ := json.Marshal(res)
	return string(out), nil
}

// extracted is the output of the extraction script.
type extracted struct {
	Kind  string `json:"kind"`
	Unit  string `json:"unit"`
	Total int    `json:"total"`
	Shown string `json:"shown"`
	Text  string `json:"text"`
	Error string `json:"error"`
}

// extractScript prints {kind, unit, total, shown, text} or {error} for HARNESS_PATH:
// markitdown for Office files (LibreOffice converts legacy/ODF formats first),
// pdftotext per page for PDFs; HARNESS_PAGES selects pages/slides, HARNESS_SHEET a sheet.
const extractScript = `# work:extract
python3 - <<'HARNESS_EOF'
import json, os, re, shutil, subprocess, sys, tempfile
p, pages, sheet = os.environ["HARNESS_PATH"], os.environ.get("HARNESS_PAGES", ""), os.environ.get("HARNESS_SHEET", "")
def out(**kw):
    print(json.dumps(kw)); sys.exit(0)
def sel(spec, n, unit="pages"):
    if not spec.strip():
        return list(range(1, n + 1))
    res = []
    for part in spec.split(","):
        a, _, b = part.strip().partition("-")
        lo = int(a); hi = int(b) if b else lo
        if lo < 1 or hi < lo:
            raise ValueError(part)
        res += [i for i in range(lo, hi + 1) if i <= n]
    if not res:  # review round 2 (A4): pages past the end name the valid ones
        out(error="%s has %d %s: pages %s are past the end (valid %s: %s)" % (p, n, unit, spec.strip(), unit,
            "1" if n == 1 else "1-%d" % n) if n else "%s has no %s" % (p, unit))
    return res
if not os.path.isfile(p):
    out(error="%s: no such file" % p)
ext = os.path.splitext(p)[1].lower()
conv = {".doc": "docx", ".odt": "docx", ".rtf": "docx", ".xls": "xlsx", ".ods": "xlsx", ".ppt": "pptx", ".odp": "pptx"}
src = p
if ext in conv:
    tmp = tempfile.mkdtemp(prefix="work-extract-")
    prof = tempfile.mkdtemp(prefix="work-lo-")  # one LibreOffice profile per conversion: parallel ones never collide
    try:
        r = subprocess.run(["soffice", "--headless", "--norestore", "--nolockcheck", "-env:UserInstallation=file://" + prof,
                            "--convert-to", conv[ext], "--outdir", tmp, p], capture_output=True, timeout=80)
    except subprocess.TimeoutExpired:
        out(error="LibreOffice could not convert %s within 80 s" % p)
    finally:
        shutil.rmtree(prof, ignore_errors=True)
    names = [f for f in os.listdir(tmp)]
    if not names:
        msg = " ".join(l for l in (r.stdout + b"\n" + r.stderr).decode(errors="replace").splitlines() if l.strip() and "javaldx" not in l)
        out(error="LibreOffice could not convert %s (soffice exit %d%s)" % (p, r.returncode, (": " + msg)[-300:] if msg else ""))
    src, ext = os.path.join(tmp, names[0]), "." + conv[ext]
try:
    if ext == ".pdf":
        from pypdf import PdfReader
        n = len(PdfReader(src).pages)
        chosen = sel(pages, n)
        parts = []
        for i in chosen:
            t = subprocess.run(["pdftotext", "-layout", "-f", str(i), "-l", str(i), src, "-"], capture_output=True, timeout=60).stdout.decode(errors="replace")
            parts.append("## Page %d\n%s" % (i, t.strip()))
        out(kind="pdf", unit="pages", total=n, shown=",".join(map(str, chosen)), text="\n\n".join(parts))
    from markitdown import MarkItDown
    md = MarkItDown().convert(src).text_content
    if ext == ".pptx":
        chunks = re.split(r"<!-- Slide number: (\d+) -->", md)
        slides = {int(chunks[i]): chunks[i + 1].strip() for i in range(1, len(chunks) - 1, 2)}
        chosen = sel(pages, len(slides), "slides")
        out(kind="pptx", unit="slides", total=len(slides), shown=",".join(map(str, chosen)),
            text="\n\n".join("## Slide %d\n%s" % (i, slides.get(i, "")) for i in chosen))
    if ext in (".xlsx", ".xlsm"):
        import openpyxl
        names = openpyxl.load_workbook(src, read_only=True).sheetnames
        if pages:
            out(error="pages does not apply to workbooks; use sheet")
        if sheet:
            if sheet not in names:
                out(error="no sheet %r (sheets: %s)" % (sheet, ", ".join(names)))
            m = re.search(r"(?ms)^## " + re.escape(sheet) + r"\s*$(.*?)(?=^## |\Z)", md)
            out(kind="xlsx", unit="sheets", total=len(names), shown="sheet " + sheet, text="## " + sheet + "\n" + (m.group(1).strip() if m else ""))
        out(kind="xlsx", unit="sheets", total=len(names), shown="all", text=md)
    if pages:
        out(error="pages only applies to PDF and PPTX files")
    out(kind=ext.lstrip("."), unit="", total=0, shown="all", text=md)
except ValueError as e:
    out(error="invalid pages %r" % pages)
except Exception as e:
    out(error="cannot extract text from %s: %s" % (p, str(e)[:300]))
HARNESS_EOF
`

func (b *binding) readDocument(ctx context.Context, p string, in readInput) (string, error) {
	var res extracted
	err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		if _, err := sb.Stat(ctx, p); err != nil {
			return mapFileErr(err, p)
		}
		var err error
		res, err = runJSON[extracted](ctx, sb, extractScript, map[string]string{"HARNESS_PATH": p, "HARNESS_PAGES": in.Pages, "HARNESS_SHEET": in.Sheet}, extractWait)
		return err
	})
	if err != nil {
		return "", err
	}
	if res.Error != "" {
		return "", tools.Errorf("%s", res.Error)
	}
	header := fmt.Sprintf("[document: %s; showing %s]\n", docSize(res), shownOf(res))
	if strings.TrimSpace(res.Text) == "" {
		return header + "[no text could be extracted (scanned pages or images only?); look at it with view_file]\n", nil
	}
	return page(p, header, res.Text, in.Offset, in.Limit)
}

func docSize(r extracted) string {
	if r.Unit == "" {
		return strings.ToUpper(r.Kind)
	}
	unit := r.Unit
	if r.Total == 1 {
		unit = strings.TrimSuffix(unit, "s")
	}
	return fmt.Sprintf("%d %s", r.Total, unit)
}

func shownOf(r extracted) string {
	switch {
	case r.Shown == "" || r.Shown == "all":
		return "all"
	case r.Unit == "pages":
		return "pages " + r.Shown
	case r.Unit == "slides":
		return "slides " + r.Shown
	}
	return r.Shown
}

// runJSON runs a sandbox script and decodes the last JSON object it prints.
func runJSON[T any](ctx context.Context, sb sandbox.Sandbox, script string, env map[string]string, timeout time.Duration) (T, error) {
	var zero T
	var stdout, stderr strings.Builder
	cctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: script, Cwd: sandbox.Workspace, Timeout: timeout, Env: env,
		OnStdout: func(t string) {
			if stdout.Len() < 8<<20 {
				stdout.WriteString(t)
			}
		},
		OnStderr: func(t string) {
			if stderr.Len() < 4096 {
				stderr.WriteString(t)
			}
		}})
	if err != nil {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if sandbox.IsGone(err) {
			return zero, err
		}
		return zero, fmt.Errorf("sandbox script failed: %w", err)
	}
	text := strings.TrimSpace(stdout.String())
	if i := strings.LastIndex(text, "\n{"); i >= 0 {
		text = text[i+1:]
	}
	var out T
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return zero, fmt.Errorf("sandbox script gave no result (exit %d): %s", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ExtractText returns up to maxChars characters of a file's text: documents
// through the extraction script (markitdown, pdftotext), text files as they
// are; images and other binaries give "" (the goal judge's deliverable excerpt).
func ExtractText(ctx context.Context, sb sandbox.Sandbox, p string, maxChars int) (string, error) {
	ext := strings.ToLower(path.Ext(p))
	if _, image := imageMIME[ext]; image {
		return "", nil
	}
	var text string
	if documentExt[ext] {
		res, err := runJSON[extracted](ctx, sb, extractScript, map[string]string{"HARNESS_PATH": p}, extractWait)
		if err != nil {
			return "", err
		}
		if res.Error != "" {
			return "", fmt.Errorf("%s", res.Error)
		}
		text = res.Text
	} else {
		data, _, err := sb.ReadFile(ctx, p, int64(maxChars)*4+4)
		if err != nil {
			return "", err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return "", nil
		}
		text = string(data)
	}
	if r := []rune(text); len(r) > maxChars {
		text = string(r[:maxChars]) + "…"
	}
	return text, nil
}
