"""LibreOffice / poppler helpers shared by the scripts of this skill (import, not a CLI).

convert(src, "pdf") runs soffice headless with a private throw-away profile, so it never
collides with another soffice process in the sandbox, and enforces a timeout.
"""
from __future__ import annotations

import os
import shutil
import subprocess
import tempfile


class OfficeError(RuntimeError):
    pass


def convert(src: str, fmt: str = "pdf", outdir: str | None = None, timeout: int = 180) -> str:
    """Convert `src` with soffice --convert-to `fmt` (e.g. "pdf", "docx", "xlsx",
    'xlsx:"Calc MS Excel 2007 XML"'). Returns the output path."""
    if not os.path.isfile(src):
        raise OfficeError(f"input not found: {src}")
    outdir = outdir or os.path.dirname(os.path.abspath(src))
    os.makedirs(outdir, exist_ok=True)
    profile = tempfile.mkdtemp(prefix="lo_profile.", dir="/tmp")
    try:
        cmd = ["soffice", f"-env:UserInstallation=file://{profile}", "--headless", "--norestore",
               "--nolockcheck", "--nodefault", "--convert-to", fmt, "--outdir", outdir, src]
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        except subprocess.TimeoutExpired as exc:
            raise OfficeError(f"soffice timed out after {timeout}s converting {src}") from exc
        ext = fmt.split(":")[0]
        out = os.path.join(outdir, os.path.splitext(os.path.basename(src))[0] + "." + ext)
        if res.returncode != 0 or not os.path.isfile(out) or os.path.getsize(out) == 0:
            raise OfficeError(f"soffice failed for {src}: {res.stdout.strip()} {res.stderr.strip()}")
        return out
    finally:
        shutil.rmtree(profile, ignore_errors=True)


def pdf_page_count(pdf: str) -> int:
    from pypdf import PdfReader

    return len(PdfReader(pdf).pages)


def pdf_to_pngs(pdf: str, outdir: str, dpi: int = 80, first: int | None = None,
                last: int | None = None, prefix: str = "page") -> list[str]:
    """Render PDF pages to PNG with pdftoppm. Returns sorted image paths."""
    os.makedirs(outdir, exist_ok=True)
    cmd = ["pdftoppm", "-png", "-r", str(dpi)]
    if first:
        cmd += ["-f", str(first)]
    if last:
        cmd += ["-l", str(last)]
    root = os.path.join(outdir, prefix)
    cmd += [pdf, root]
    res = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
    if res.returncode != 0:
        raise OfficeError(f"pdftoppm failed: {res.stderr.strip()}")
    images = sorted(os.path.join(outdir, f) for f in os.listdir(outdir)
                    if f.startswith(prefix + "-") and f.endswith(".png"))
    return images


def contact_sheet(images: list[str], out: str, cols: int = 3, thumb_width: int = 400,
                  labels: bool = True, label_start: int = 1) -> str:
    """Grid of thumbnails with page/slide numbers, white background, thin borders."""
    from PIL import Image, ImageDraw, ImageFont

    if not images:
        raise OfficeError("no images for the contact sheet")
    thumbs = []
    for path in images:
        im = Image.open(path).convert("RGB")
        ratio = thumb_width / im.width
        thumbs.append(im.resize((thumb_width, max(1, int(im.height * ratio)))))
    cell_h = max(t.height for t in thumbs)
    pad, label_h = 16, 22 if labels else 0
    rows = (len(thumbs) + cols - 1) // cols
    sheet = Image.new("RGB", (cols * (thumb_width + pad) + pad, rows * (cell_h + pad + label_h) + pad), "white")
    draw = ImageDraw.Draw(sheet)
    try:
        font = ImageFont.truetype("/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf", 14)
    except OSError:
        font = ImageFont.load_default()
    for i, t in enumerate(thumbs):
        x = pad + (i % cols) * (thumb_width + pad)
        y = pad + (i // cols) * (cell_h + pad + label_h)
        if labels:
            draw.text((x, y), str(label_start + i), fill=(74, 85, 104), font=font)
        sheet.paste(t, (x, y + label_h))
        draw.rectangle([x - 1, y + label_h - 1, x + t.width, y + label_h + t.height], outline=(180, 186, 196))
    sheet.save(out)
    return out
