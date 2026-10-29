package sandboxtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

const viewDesc = `Look at a document the way the user will see it: renders pages of a PDF, DOCX, PPTX or XLSX (LibreOffice, then PNG at
110 dpi, at most 6 pages per call) into /workspace/.work/renders/ and shows you the images. pages selects pages or slides ("1-3,7";
default the first 6). Returns JSON {path, pages: [{page, imagePath}], totalPages}. Use it to check layout, overflow, charts and
styling before you call a document done; use read_file for its text. An image file (PNG, JPEG, WEBP, GIF) is shown as is.`

// NoVisionError is the view_file error for models that cannot see images.
const NoVisionError = "this model cannot view images; use read_file and the skill's inspect script"

const (
	maxViewPages = 6
	renderDPI    = 110
	renderWait   = 150 * time.Second
)

var viewableExt = map[string]bool{".pdf": true, ".docx": true, ".pptx": true, ".xlsx": true, ".xlsm": true,
	".odt": true, ".ods": true, ".odp": true, ".doc": true, ".xls": true, ".ppt": true, ".rtf": true}

type viewInput struct {
	Path  string `json:"path" jsonschema_description:"Document or image path, absolute or relative to /workspace"`
	Pages string `json:"pages,omitempty" jsonschema_description:"Pages or slides to render, e.g. \"1-3,7\" (default the first 6; at most 6 per call)"`
}

type viewPage struct {
	Page      int    `json:"page"`
	ImagePath string `json:"imagePath"`
}

type viewOutput struct {
	Path string `json:"path"`
	// RenderID names the render folder (sandbox.RendersDir/<renderId>/p<N>.png;
	// GET /threads/{id}/renders/{renderId}/{N}.png). Empty for an image shown as is.
	RenderID   string     `json:"renderId,omitempty"`
	Pages      []viewPage `json:"pages"`
	TotalPages int        `json:"totalPages"`
	Note       string     `json:"note,omitempty"`
	Notice     string     `json:"notice,omitempty"`
}

type rendered struct {
	TotalPages int        `json:"totalPages"`
	Pages      []viewPage `json:"pages"`
	Error      string     `json:"error"`
	// PastEnd lists the requested pages past the end ("4-5"); Size is the
	// document's length ("3 slides").
	PastEnd string `json:"pastEnd"`
	Size    string `json:"size"`
}

// renderScript renders HARNESS_PATH (converted to PDF by LibreOffice unless it is
// one) to HARNESS_OUT/p<N>.png at HARNESS_DPI for the HARNESS_PAGES selection (at most
// HARNESS_MAX). Harness v3 review round 2 (A4): a flock on HARNESS_OUT serializes
// renders of one file version (parallel view_file calls of the same file in
// one step raced on HARNESS_OUT/<name>.pdf: "LibreOffice could not render …"),
// LibreOffice converts a private copy into a private directory with its own
// profile, a failed conversion is retried once and reported with soffice's
// exit code and output (not the harmless javaldx warning), and pages past the
// end are named with the valid range.
const renderScript = `# work:render
python3 - <<'HARNESS_EOF'
import fcntl, json, os, shutil, subprocess, sys, tempfile, time
p, spec, outdir = os.environ["HARNESS_PATH"], os.environ.get("HARNESS_PAGES", ""), os.environ["HARNESS_OUT"]
dpi, cap = os.environ.get("HARNESS_DPI", "110"), int(os.environ.get("HARNESS_MAX", "6"))
def out(**kw):
    print(json.dumps(kw)); sys.exit(0)
def ranges(nums):
    res, i = [], 0
    while i < len(nums):
        j = i
        while j + 1 < len(nums) and nums[j + 1] == nums[j] + 1:
            j += 1
        res.append(str(nums[i]) if i == j else "%d-%d" % (nums[i], nums[j]))
        i = j + 1
    return ",".join(res)
def why(r):
    lines = [l for l in (r.stdout + b"\n" + r.stderr).decode(errors="replace").splitlines()
             if l.strip() and "javaldx" not in l]
    return "soffice exit %d%s" % (r.returncode, (": " + " ".join(lines))[-300:] if lines else ", no PDF written")
def convert(src, pdf):
    ext = os.path.splitext(src)[1].lower()
    err = ""
    for attempt in (1, 2):
        tmp = tempfile.mkdtemp(prefix=".convert-", dir=os.path.dirname(pdf))  # same file system as pdf: os.replace is atomic
        prof = tempfile.mkdtemp(prefix="work-lo-")  # one LibreOffice profile per conversion: parallel ones never collide
        t0 = time.time()
        try:
            doc = os.path.join(tmp, "document" + ext)  # a private copy: neutral name, no lock file next to the user's file
            shutil.copyfile(src, doc)
            r = subprocess.run(["soffice", "--headless", "--norestore", "--nolockcheck", "-env:UserInstallation=file://" + prof,
                                "--convert-to", "pdf", "--outdir", tmp, doc], capture_output=True, timeout=110)
            made = os.path.join(tmp, "document.pdf")
            if os.path.isfile(made) and os.path.getsize(made) > 0:
                os.replace(made, pdf)
                return ""
            err = why(r)
        except subprocess.TimeoutExpired:
            return "LibreOffice did not finish within 110 s"
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
            shutil.rmtree(prof, ignore_errors=True)
        if time.time() - t0 > 30:
            break  # a slow failure is not worth a second wait
    return err
if not os.path.isfile(p):
    out(error="%s: no such file" % p)
os.makedirs(outdir, exist_ok=True)
lock = open(os.path.join(outdir, ".lock"), "w")
fcntl.flock(lock, fcntl.LOCK_EX)  # one render of this file version at a time; the next one reuses the PDF
pdf = p
if not p.lower().endswith(".pdf"):
    pdf = os.path.join(outdir, "document.pdf")
    if not os.path.exists(pdf) or os.path.getmtime(pdf) < os.path.getmtime(p):
        err = convert(p, pdf)
        if err:
            out(error="LibreOffice could not render %s (%s); check the file opens (the skill's inspect or check script) and call view_file again" % (p, err))
try:
    from pypdf import PdfReader
    n = len(PdfReader(pdf).pages)
except Exception as e:
    out(error="cannot read the rendered PDF: %s" % str(e)[:300])
ext = os.path.splitext(p)[1].lower()
unit = "slides" if ext in (".pptx", ".ppt", ".odp") else "pages"
size = "%d %s" % (n, unit if n != 1 else unit[:-1]) + (" when printed" if ext in (".xlsx", ".xlsm", ".xls", ".ods") else "")
want = []
try:
    if spec.strip():
        for part in spec.split(","):
            a, _, b = part.strip().partition("-")
            lo = int(a); hi = int(b) if b else lo
            if lo < 1 or hi < lo:
                raise ValueError(part)
            want += [i for i in range(lo, hi + 1) if i not in want]
    else:
        want = list(range(1, n + 1))
except ValueError:
    out(error="invalid pages %r" % spec)
pages = [i for i in want if i <= n]
past = sorted(i for i in want if i > n)
if not pages:
    out(error="%s has %s: %s %s past the end (valid pages: %s)" % (p, size, "page" if len(past) == 1 else "pages",
        ranges(past) + (" is" if len(past) == 1 else " are"), "1" if n == 1 else "1-%d" % n), totalPages=n)
res = []
for i in pages[:cap]:
    base = os.path.join(outdir, "p%d" % i)
    if not os.path.exists(base + ".png") or os.path.getmtime(base + ".png") < os.path.getmtime(pdf):
        subprocess.run(["pdftoppm", "-r", dpi, "-png", "-singlefile", "-f", str(i), "-l", str(i), pdf, base], capture_output=True, timeout=60)
    if os.path.exists(base + ".png"):
        res.append({"page": i, "imagePath": base + ".png"})
out(totalPages=n, pages=res, pastEnd=ranges(past), size=size)
HARNESS_EOF
`

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// renderDir is the render directory of a file version: its name and a hash of
// its path, size and modification time.
func renderDir(p string, fi sandbox.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", p, fi.Size, fi.ModifiedAt.UnixNano())))
	name := unsafeName.ReplaceAllString(strings.TrimSuffix(path.Base(p), path.Ext(p)), "_")
	if len(name) > 60 {
		name = name[:60]
	}
	return fmt.Sprintf("%s/%s-%s", sandbox.RendersDir, name, hex.EncodeToString(sum[:])[:10])
}

func (b *binding) view(ctx context.Context, in viewInput) (*viewOutput, error) {
	p, err := resolve(in.Path)
	if err != nil {
		return nil, err
	}
	if !tools.CanViewImages(ctx) {
		return nil, tools.Errorf("%s", NoVisionError)
	}
	ext := strings.ToLower(path.Ext(p))
	if mime, ok := imageMIME[ext]; ok {
		if _, err := b.readImage(ctx, p, mime); err != nil {
			return nil, err
		}
		return &viewOutput{Path: p, Pages: []viewPage{{Page: 1, ImagePath: p}}, TotalPages: 1}, nil
	}
	if !viewableExt[ext] {
		return nil, tools.Errorf("view_file renders PDF, DOCX, PPTX, XLSX (and ODF/legacy Office) documents or shows images; read %s with read_file", p)
	}
	out := &viewOutput{Path: p}
	var images []tools.Image
	err = b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		fi, err := sb.Stat(ctx, p)
		if err != nil {
			return mapFileErr(err, p)
		}
		dir := renderDir(p, fi)
		out.RenderID = path.Base(dir)
		if err := sb.MkdirAll(ctx, dir); err != nil { // the sandbox may predate the layout, or .work was removed
			return err
		}
		res, err := runJSON[rendered](ctx, sb, renderScript, map[string]string{"HARNESS_PATH": p, "HARNESS_PAGES": in.Pages,
			"HARNESS_OUT": dir, "HARNESS_DPI": fmt.Sprint(renderDPI), "HARNESS_MAX": fmt.Sprint(maxViewPages)}, renderWait)
		if err != nil {
			return err
		}
		if res.Error != "" {
			return tools.Errorf("%s", res.Error)
		}
		out.TotalPages = res.TotalPages
		if res.PastEnd != "" {
			what := "pages " + res.PastEnd + " do not exist"
			if !strings.ContainsAny(res.PastEnd, ",-") {
				what = "page " + res.PastEnd + " does not exist"
			}
			out.Note = fmt.Sprintf("%s (%s has %s)", what, path.Base(p), res.Size)
		}
		images = images[:0]
		out.Pages = out.Pages[:0]
		for _, pg := range res.Pages {
			img, _, err := sb.ReadFile(ctx, pg.ImagePath, maxImageBytes)
			if err != nil {
				continue
			}
			images = append(images, tools.Image{Path: pg.ImagePath, MIMEType: "image/png", Data: img})
			out.Pages = append(out.Pages, pg)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out.Pages) == 0 {
		return nil, tools.Errorf("no page of %s could be rendered (pages %q of %d)", p, in.Pages, out.TotalPages)
	}
	for _, img := range images {
		tools.AttachImage(ctx, img)
	}
	if in.Pages == "" && out.TotalPages > maxViewPages && out.Note == "" {
		out.Note = fmt.Sprintf("showing pages 1-%d of %d; ask for others with pages (at most %d per call)", maxViewPages, out.TotalPages, maxViewPages)
	}
	return out, nil
}
