#!/usr/bin/env python3
"""Extract text, tables and metadata from a PDF (pdfplumber + pypdf).

Usage:
  python3 extract.py FILE.pdf                         # JSON: metadata + text per page
  python3 extract.py FILE.pdf --format text           # plain text, pages separated by form feeds
  python3 extract.py FILE.pdf --format markdown       # text + tables as markdown, per page
  python3 extract.py FILE.pdf --tables --pages 2-4    # JSON tables (list of rows) of pages 2..4
  python3 extract.py FILE.pdf --tables --csv-dir /workspace/outputs/tables   # one CSV per table
  python3 extract.py FILE.pdf --layout                # keep the visual layout (columns) in text
  python3 extract.py FILE.pdf --words --pages 1       # words with x0/top/x1/bottom (for forms, layout)
  python3 extract.py --check FILE.pdf                 # deliverable checks (the pdf FINISH GATE)

--check prints {"ok", "file", "checks": [{"name", "ok", "detail"}], "next_step"} and exits 0 when
every check passes, 2 when one fails, 1 on a usage error. Checks: opens, pages (page count and
size), text (a text layer, no blank page), fonts_embedded (every font used for text is embedded),
glyphs (no character without a glyph: U+FFFD, notdef, unmapped cid). The first 100 pages are checked.

Pages are 1-based. Pages with no extractable text but with images are flagged
"scanned": OCR is not available in this sandbox (no tesseract), say so to the user.
"""
from __future__ import annotations

import argparse
import csv
import json
import os
import sys

CHECK_PAGES = 100
STANDARD_14 = {"Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique", "Helvetica", "Helvetica-Bold",
               "Helvetica-Oblique", "Helvetica-BoldOblique", "Times-Roman", "Times-Bold", "Times-Italic",
               "Times-BoldItalic", "Symbol", "ZapfDingbats"}
CMD = "python3 /skills/pdf/scripts/extract.py --check"


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def _resolve(obj):
    return obj.get_object() if hasattr(obj, "get_object") else obj


def font_embedding(page):
    """{BaseFont without subset prefix: embedded?} for the fonts of a pypdf page (+ XObjects)."""
    out = {}

    def visit(resources, depth=0):
        resources = _resolve(resources) or {}
        fonts = _resolve(resources.get("/Font")) or {}
        for ref in fonts.values():
            f = _resolve(ref)
            name = str(f.get("/BaseFont", "?")).lstrip("/")
            name = name.split("+", 1)[-1]
            sub = f.get("/Subtype")
            embedded = sub == "/Type3"
            desc = _resolve(f.get("/FontDescriptor"))
            if sub == "/Type0":
                desc_fonts = _resolve(f.get("/DescendantFonts")) or []
                if desc_fonts:
                    desc = _resolve(_resolve(desc_fonts[0]).get("/FontDescriptor"))
            if desc is not None and any(k in desc for k in ("/FontFile", "/FontFile2", "/FontFile3")):
                embedded = True
            out[name] = out.get(name, False) or embedded
        if depth < 3:
            for x in (_resolve(resources.get("/XObject")) or {}).values():
                x = _resolve(x)
                if x.get("/Subtype") == "/Form" and "/Resources" in x:
                    visit(x["/Resources"], depth + 1)

    visit(page.get("/Resources"))
    return out


def run_checks(path, password=None):
    import pdfplumber
    from pypdf import PdfReader

    checks = []

    def add(name, ok, detail):
        checks.append({"name": name, "ok": bool(ok), "detail": detail})

    try:
        reader = PdfReader(path, password=password)
        if reader.is_encrypted and password is None:
            reader.decrypt("")
        pdf = pdfplumber.open(path, password=password)
        n = len(pdf.pages)
    except Exception as exc:  # noqa: BLE001
        add("opens", False, f"cannot open the file as PDF: {exc}")
        return checks
    add("opens", True, "pypdf and pdfplumber open the file")
    if not n:
        add("pages", False, "the PDF has no page")
        return checks
    first = pdf.pages[0]
    add("pages", True, f"{n} pages, first page {float(first.width):.0f} x {float(first.height):.0f} pt")
    pages = list(range(min(n, CHECK_PAGES)))
    blank, image_only, chars_total, used = [], [], 0, set()
    bad_glyphs = 0
    for i in pages:
        page = pdf.pages[i]
        chars = [c for c in page.chars if c.get("text", "").strip()]
        chars_total += len(chars)
        for c in chars:
            used.add(str(c.get("fontname", "")).split("+", 1)[-1])
            if c.get("text") in ("\ufffd", "\x00") or str(c.get("text")).startswith("(cid:"):
                bad_glyphs += 1
        if not chars:
            if page.images:
                image_only.append(i + 1)
            elif not (page.rects or page.lines or page.curves):
                blank.append(i + 1)
    scope = f" (first {len(pages)} of {n} pages checked)" if len(pages) < n else ""
    if not chars_total:
        add("text", False, "no text layer at all (scanned or rasterised pages): build the PDF from text "
                           "(build_pdf.py, to_pdf.sh) so it can be searched and read" + scope)
    elif blank:
        add("text", False, f"blank pages {blank[:10]}: remove them (pages.py delete) or fix the page breaks" + scope)
    else:
        detail = f"{chars_total} characters of text"
        if image_only:
            detail += f"; pages {image_only[:10]} are images only"
        add("text", True, detail + scope)
    embedding = {}
    for i in pages:
        for name, emb in font_embedding(reader.pages[i]).items():
            embedding[name] = embedding.get(name, False) or emb
    not_embedded = sorted(f for f in used if f in embedding and not embedding[f])
    standard = [f for f in not_embedded if f in STANDARD_14]
    missing = [f for f in not_embedded if f not in STANDARD_14]
    if missing:
        add("fonts_embedded", False, f"fonts not embedded: {', '.join(missing[:6])}: viewers substitute them and the "
                                     "layout changes; use a TrueType font (build_pdf.py does; reportlab: "
                                     "brand.reportlab_fonts() or register LiberationSans with TTFont)")
    else:
        detail = f"{len(used)} fonts used, all embedded" if used else "no text fonts"
        if standard:
            detail = (f"{len(used)} fonts used; standard PDF fonts not embedded: {', '.join(standard[:6])} (every "
                      "viewer has them; they only cover Western European text)")
        add("fonts_embedded", True, detail)
    add("glyphs", not bad_glyphs, f"{bad_glyphs} characters without a glyph (U+FFFD / notdef / cid): the font lacks "
                                  "them (they print as boxes or blanks); use a font that covers the text"
        if bad_glyphs else "every character has a glyph")
    return checks


def check_main(path, password=None):
    path = os.path.abspath(path)  # the checker contract names the file by its absolute path
    if not os.path.isfile(path):
        print(json.dumps({"ok": False, "file": path, "error": f"no such file: {path}"}))
        return 1
    checks = run_checks(path, password)
    ok = all(c["ok"] for c in checks)
    failed = [c["name"] for c in checks if not c["ok"]]
    step = (f"OK: all checks passed. Now call view_file on {path} and look at the pages before you answer." if ok else
            f"NOT DONE: fix the failing checks ({', '.join(failed)}), rebuild the PDF, then run `{CMD} {path}` again "
            "(at most 3 rounds). Do not say the PDF is ready while ok is false.")
    print(json.dumps({"ok": ok, "file": path, "checks": checks, "next_step": step}, ensure_ascii=False, indent=2))
    return 0 if ok else 2


def parse_pages(spec, n):
    if not spec:
        return list(range(1, n + 1))
    pages = []
    for part in spec.split(","):
        part = part.strip()
        if "-" in part:
            a, b = part.split("-", 1)
            a = int(a) if a else 1
            b = int(b) if b else n
            pages.extend(range(a, b + 1))
        elif part:
            pages.append(int(part))
    bad = [p for p in pages if not 1 <= p <= n]
    if bad:
        raise SystemExit(f"error: pages {bad} out of range 1..{n}")
    return pages


def md_table(rows):
    rows = [["" if c is None else str(c).replace("\n", " ").replace("|", "\\|") for c in r] for r in rows if r]
    if not rows:
        return ""
    width = max(len(r) for r in rows)
    rows = [r + [""] * (width - len(r)) for r in rows]
    out = ["| " + " | ".join(rows[0]) + " |", "|" + "---|" * width]
    out += ["| " + " | ".join(r) + " |" for r in rows[1:]]
    return "\n".join(out)


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--check", action="store_true", help="deliverable checks (exit 0 ok, 2 not ok, 1 usage)")
    ap.add_argument("--pages", help='e.g. "1-3,7"')
    ap.add_argument("--format", choices=["json", "text", "markdown"], default="json")
    ap.add_argument("--tables", action="store_true", help="extract tables (JSON)")
    ap.add_argument("--csv-dir", help="with --tables: write each table as CSV here")
    ap.add_argument("--layout", action="store_true", help="layout-preserving text")
    ap.add_argument("--words", action="store_true", help="words with coordinates (JSON)")
    ap.add_argument("--password", default=None)
    args = ap.parse_args(argv)
    if args.check:
        return check_main(args.file, args.password)

    import pdfplumber
    from pypdf import PdfReader

    try:
        reader = PdfReader(args.file, password=args.password)
        if reader.is_encrypted and args.password is None:
            try:
                reader.decrypt("")
            except Exception:  # noqa: BLE001
                pass
        meta = {k.lstrip("/"): str(v) for k, v in (reader.metadata or {}).items()}
        pdf = pdfplumber.open(args.file, password=args.password)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": f"cannot open {args.file}: {exc}"}))
        return 1
    n = len(pdf.pages)
    pages = parse_pages(args.pages, n)
    info = {"file": args.file, "pages": n, "encrypted": reader.is_encrypted, "metadata": meta,
            "has_form": bool(reader.get_fields()), "outline_entries": len(reader.outline) if reader.outline else 0}

    if args.words:
        out = []
        for p in pages:
            page = pdf.pages[p - 1]
            out.append({"page": p, "width": float(page.width), "height": float(page.height),
                        "words": [{k: (round(w[k], 1) if isinstance(w[k], float) else w[k])
                                   for k in ("text", "x0", "top", "x1", "bottom")} for w in page.extract_words()]})
        print(json.dumps({**info, "words": out}, ensure_ascii=False))
        return 0

    if args.tables:
        tables = []
        for p in pages:
            for t_idx, rows in enumerate(pdf.pages[p - 1].extract_tables(), 1):
                entry = {"page": p, "index": t_idx, "rows": rows}
                if args.csv_dir:
                    os.makedirs(args.csv_dir, exist_ok=True)
                    path = os.path.join(args.csv_dir, f"page{p}_table{t_idx}.csv")
                    with open(path, "w", newline="", encoding="utf-8") as fh:
                        csv.writer(fh).writerows(rows)
                    entry["csv"] = path
                tables.append(entry)
        print(json.dumps({**info, "tables_found": len(tables), "tables": tables}, ensure_ascii=False, indent=1))
        return 0

    page_out, scanned = [], []
    for p in pages:
        page = pdf.pages[p - 1]
        text = page.extract_text(layout=args.layout) or ""
        is_scanned = not text.strip() and len(page.images) > 0
        if is_scanned:
            scanned.append(p)
        item = {"page": p, "chars": len(text), "text": text, "scanned": is_scanned}
        if args.format == "markdown":
            item["tables_md"] = [md_table(t) for t in page.extract_tables()]
        page_out.append(item)
    warnings = []
    if scanned:
        warnings.append(f"pages {scanned} look scanned (images, no text layer): OCR is not available in this sandbox")
    if args.format == "text":
        sys.stdout.write("\f".join(i["text"] for i in page_out) + "\n")
        if warnings:
            print("warning: " + "; ".join(warnings), file=sys.stderr)
        return 0
    if args.format == "markdown":
        for i in page_out:
            print(f"\n## Page {i['page']}\n")
            print(i["text"])
            for t in i["tables_md"]:
                if t:
                    print("\n" + t + "\n")
        if warnings:
            print("warning: " + "; ".join(warnings), file=sys.stderr)
        return 0
    print(json.dumps({**info, "warnings": warnings, "content": page_out}, ensure_ascii=False, indent=1))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
