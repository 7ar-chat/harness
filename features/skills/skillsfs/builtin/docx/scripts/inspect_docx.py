#!/usr/bin/env python3
"""Check or summarise a .docx.

Checker (the FINISH GATE of the docx skill):
  python3 inspect_docx.py --check FILE.docx
  -> {"ok": bool, "file", "checks": [{"name", "ok", "detail"}], "next_step"}
  exit 0 = every check passed, 2 = at least one check failed (fix and re-run), 1 = usage error
  (no such file, bad arguments).
  Checks: opens, content (body not empty), headings (a document of more than 500 words has
  Heading styles), empty_paragraphs (no 3+ empty paragraphs in a row used as spacing),
  footer (no page number glued to text, no duplicate or hand-typed page numbers), toc (a TOC
  field shows real entries, not the "update the table" placeholder), table_widths (every table
  has a column grid that fits the text width: LibreOffice and PDF export read w:gridCol),
  images (inside the text area of their page), placeholders (no {{...}}, Lorem ipsum, TODO,
  TBD, FIXME, [Insert ...]).

Summary / reading helpers:
  python3 inspect_docx.py FILE.docx            # JSON summary: outline, styles, tables, fields,
                                               # tracked changes, fonts, + "ok"/"problems"/"checks"
  python3 inspect_docx.py FILE.docx --text     # numbered paragraph listing: [index] (style) text
  python3 inspect_docx.py FILE.docx --text --limit 200
  python3 inspect_docx.py FILE.docx --text --range 40-60        # paragraphs 40..60 only (the index edit_docx.py uses)
  python3 inspect_docx.py FILE.docx --text --range 12 --full    # one paragraph, whole text

--text cuts each paragraph at --max-chars (default 200) with the marker …(+n chars) and ends with the range that
continues a cut listing; --full removes the cut. A range outside the document answers with the valid range.

Paragraph indices are those of python-docx `Document(path).paragraphs` (body only, tables
excluded), so you can edit `doc.paragraphs[i]` afterwards.
"""
from __future__ import annotations

import argparse
import collections
import json
import os
import re
import sys
import zipfile

from lxml import etree

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from opserr import clip  # noqa: E402

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
NS = {"w": W,
      "wp": "http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing",
      "a": "http://schemas.openxmlformats.org/drawingml/2006/main",
      "v": "urn:schemas-microsoft-com:vml"}
EMU_PER_CM = 360000
EMU_PER_TWIP = 635
LONG_DOC_WORDS = 500
EMPTY_RUN = 3
PLACEHOLDERS = re.compile(r"\{\{[^}]{0,40}\}\}|\b[Ll]orem ipsum\b|\bLOREM\b|\bTODO\b|\bTBD\b|\bFIXME\b|"
                          r"\[(?:insert|Insert|INSERT)[^\]]{0,40}\]")
TOC_PLACEHOLDER = re.compile(r"Update the table of contents|Mettre à jour la table des matières|"
                             r"No table of contents entries|Aucune entrée de table des matières|"
                             r"\(no headings found\)|Right-click to update", re.I)
CMD = "python3 /skills/docx/scripts/inspect_docx.py --check"


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1 (argparse would exit 2)
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def q(tag: str) -> str:
    return "{%s}%s" % (W, tag)


def cm(length) -> float | None:
    return None if length is None else round(length / EMU_PER_CM, 2)


def para_text(p) -> str:
    return "".join(t.text or "" for t in p.iter(q("t")))


def page_number_problems(root, part_name):
    """Problems of a header/footer part: several page numbers, a page number glued to the
    preceding text (e.g. 'Annual Energy' + PAGE renders 'Annual Energy2'), or page numbers
    typed as plain text."""
    problems = []
    page_fields = 0
    for p in root.iter(q("p")):
        last_text = ""
        for node in p.iter(q("t"), q("instrText"), q("fldSimple")):
            if node.tag == q("t"):
                last_text += node.text or ""
                continue
            instr = (node.text if node.tag == q("instrText") else node.get(q("instr"))) or ""
            if instr.strip().split(" ")[0].upper() != "PAGE":
                continue
            page_fields += 1
            if last_text and last_text[-1].isalnum():
                problems.append(f"{part_name}: page number glued to the text {last_text[-30:]!r} (renders like "
                                f"'{last_text[-12:]}2'); rebuild the footer with python3 /skills/docx/scripts/footer.py "
                                "FILE --text \"Title\" --inplace")
    if page_fields > 1:
        problems.append(f"{part_name}: {page_fields} page numbers in one footer/header (duplicate 'Page x of y'); "
                        "rebuild it with python3 /skills/docx/scripts/footer.py FILE --text \"Title\" --inplace")
    text = " ".join(para_text(p) for p in root.iter(q("p")))
    if not page_fields and re.search(r"\b(Page|p\.)\s*\d+\s*(of|sur|/)\s*\d+", text):
        problems.append(f"{part_name}: page numbers typed as text ({text.strip()[:40]!r}) are wrong on every other "
                        "page; use python3 /skills/docx/scripts/footer.py FILE --text \"Title\" --inplace")
    return problems


def toc_fields(body):
    """[(instr, result_text)] of every TOC field of the body (complex and simple fields)."""
    out, stack = [], []
    for node in body.iter(q("fldChar"), q("instrText"), q("t"), q("fldSimple")):
        if node.tag == q("fldSimple"):
            instr = node.get(q("instr")) or ""
            if instr.strip().upper().startswith("TOC"):
                out.append((instr, "".join(t.text or "" for t in node.iter(q("t")))))
            continue
        if node.tag == q("instrText"):
            if stack and stack[-1]["sep"] is False:
                stack[-1]["instr"] += node.text or ""
            continue
        if node.tag == q("t"):
            for f in stack:
                if f["sep"]:
                    f["result"] += (node.text or "")
            continue
        kind = node.get(q("fldCharType"))
        if kind == "begin":
            stack.append({"instr": "", "sep": False, "result": ""})
        elif kind == "separate" and stack:
            stack[-1]["sep"] = True
        elif kind == "end" and stack:
            f = stack.pop()
            if f["instr"].strip().upper().startswith("TOC"):
                out.append((f["instr"], f["result"] if f["sep"] else ""))
    return out


def section_geometry(doc):
    """[(text_width_emu, text_height_emu, page_width_emu, page_height_emu)] per section."""
    out = []
    for s in doc.sections:
        pw, ph = s.page_width or 0, s.page_height or 0
        tw = pw - (s.left_margin or 0) - (s.right_margin or 0)
        th = ph - (s.top_margin or 0) - (s.bottom_margin or 0)
        out.append((tw, th, pw, ph))
    return out or [(int(16 * EMU_PER_CM), int(25 * EMU_PER_CM), int(21 * EMU_PER_CM), int(29.7 * EMU_PER_CM))]


def body_items(body):
    """(element, section index) of every direct child of the body, in order."""
    idx = 0
    for child in body:
        if child.tag == q("sectPr"):
            continue
        yield child, idx
        ppr = child.find(q("pPr")) if child.tag == q("p") else None
        if ppr is not None and ppr.find(q("sectPr")) is not None:
            idx += 1


def is_empty_paragraph(p):
    if para_text(p).strip():
        return False
    for tag in ("drawing", "pict", "object", "br", "fldChar", "fldSimple", "tab", "sym"):
        if p.find(".//" + q(tag)) is not None:
            return False
    ppr = p.find(q("pPr"))
    if ppr is not None and (ppr.find(q("sectPr")) is not None or ppr.find(q("pBdr")) is not None
                            or ppr.find(q("shd")) is not None):
        return False
    return True


def table_grid_problem(tbl, text_w_emu, top_level):
    grid = tbl.find(q("tblGrid"))
    cols = grid.findall(q("gridCol")) if grid is not None else []
    n_cols = 0
    for tr in tbl.findall(q("tr")):
        n = 0
        for tc in tr.findall(q("tc")):
            span = tc.find(f"{q('tcPr')}/{q('gridSpan')}")
            n += int(span.get(q("val"))) if span is not None and (span.get(q("val")) or "").isdigit() else 1
        n_cols = max(n_cols, n)
    if not cols or len(cols) < n_cols:
        return f"{len(cols)} gridCol for {n_cols} columns: set the widths with set_col_widths (reference/python-docx.md)"
    widths = [int(c.get(q("w")) or 0) for c in cols]
    narrow = [i + 1 for i, w in enumerate(widths) if w < 300]
    if narrow:
        return (f"columns {narrow} are narrower than 0.5 cm in the grid (w:gridCol {widths}): LibreOffice and the PDF "
                "export squeeze them; set real widths (widths_cm / set_col_widths / docx columnWidths)")
    total_emu = sum(widths) * EMU_PER_TWIP
    if top_level and total_emu > text_w_emu * 1.02 + 20 * EMU_PER_TWIP:
        return (f"table is {total_emu / EMU_PER_CM:.1f} cm wide, text width {text_w_emu / EMU_PER_CM:.1f} cm: it is cut "
                "at the page edge; reduce the column widths")
    return None


def image_problems(body, geometry):
    problems = []
    n = 0
    for child, sec in body_items(body):
        tw, th, pw, ph = geometry[min(sec, len(geometry) - 1)]
        for kind in ("inline", "anchor"):
            for el in child.iter("{%s}%s" % (NS["wp"], kind)):
                n += 1
                ext = el.find("wp:extent", NS)
                if ext is None:
                    continue
                cx, cy = int(ext.get("cx") or 0), int(ext.get("cy") or 0)
                lim_w, lim_h = (tw, th) if kind == "inline" else (pw, ph)
                tol = 0.1 * EMU_PER_CM
                if cx > lim_w + tol or cy > lim_h + tol:
                    where = "text area" if kind == "inline" else "page"
                    problems.append(f"image {n} is {cx / EMU_PER_CM:.1f} x {cy / EMU_PER_CM:.1f} cm, {where} "
                                    f"{lim_w / EMU_PER_CM:.1f} x {lim_h / EMU_PER_CM:.1f} cm: give it a width that fits "
                                    "(width_cm / Cm(...))")
    return problems, n


def run_checks(path):
    """Return (checks, context) for a .docx. context holds the parsed objects for the summary."""
    checks = []

    def add(name, ok, detail):
        checks.append({"name": name, "ok": bool(ok), "detail": detail})

    try:
        import docx
        doc = docx.Document(path)
        zf = zipfile.ZipFile(path)
        body_root = etree.fromstring(zf.read("word/document.xml"))
    except Exception as exc:  # noqa: BLE001
        add("opens", False, f"cannot open the file as .docx: {exc}")
        return checks, None
    add("opens", True, "python-docx opens the file")
    names = zf.namelist()
    body = body_root.find(q("body"))

    words = sum(len(p.text.split()) for p in doc.paragraphs)
    for t in doc.tables:
        for row in t.rows:
            for c in row.cells:
                words += len(c.text.split())
    images_total = len(body_root.findall(".//wp:inline", NS)) + len(body_root.findall(".//wp:anchor", NS))
    add("content", words or images_total or doc.tables,
        f"{words} words, {len(doc.tables)} tables, {images_total} images" if words or images_total or doc.tables
        else "the document body is empty")

    headings = 0
    for p in doc.paragraphs:
        name = p.style.name if p.style is not None else ""
        ppr = p._p.pPr
        if re.match(r"^(Heading|Titre)\s*\d$", name, re.I) or name == "Title" or (
                ppr is not None and ppr.find(q("outlineLvl")) is not None):
            headings += 1
    if words > LONG_DOC_WORDS and not headings:
        add("headings", False, f"{words} words and no heading: structure it with Heading 1-3 styles "
                               "(heading blocks / doc.add_heading), never bold Normal text")
    else:
        add("headings", True, f"{headings} headings")

    empty_runs, run_start, run_len, p_index = [], None, 0, -1
    for child in list(body) + [None]:
        if child is not None and child.tag == q("p"):
            p_index += 1
            if is_empty_paragraph(child):
                if run_len == 0:
                    run_start = p_index
                run_len += 1
                continue
        if run_len >= EMPTY_RUN:
            empty_runs.append((run_start, run_len))
        run_len = 0
    if empty_runs:
        where = ", ".join(f"{n} at paragraphs {s}-{s + n - 1}" for s, n in empty_runs[:5])
        add("empty_paragraphs", False, f"empty paragraphs used as spacing ({where}): delete them and use "
                                       "space_before/space_after or a page break")
    else:
        add("empty_paragraphs", True, f"no run of {EMPTY_RUN}+ empty paragraphs")

    footer_problems, parts = [], 0
    for name in names:
        if re.match(r"word/(header|footer)\d*\.xml$", name):
            parts += 1
            label = ("footer" if "footer" in name else "header") + f" ({name.split('/')[-1]})"
            footer_problems += page_number_problems(etree.fromstring(zf.read(name)), label)
    add("footer", not footer_problems, "; ".join(footer_problems) if footer_problems
        else f"{parts} header/footer parts, page numbers sane")

    tocs = toc_fields(body_root)
    body_text = "\n".join(p.text for p in doc.paragraphs)
    if TOC_PLACEHOLDER.search(body_text) and not tocs:
        tocs = [("TOC", TOC_PLACEHOLDER.search(body_text).group(0))]
    bad = [r for _, r in tocs if not r.strip() or TOC_PLACEHOLDER.search(r) or not re.search(r"\d", r)]
    if bad:
        add("toc", False, "table of contents not filled (shows "
                          f"{(bad[0].strip()[:60] or 'nothing')!r}): run python3 /skills/docx/scripts/fill_toc.py FILE "
                          "--inplace")
    else:
        add("toc", True, f"{len(tocs)} filled TOC" if tocs else "no TOC field")

    geometry = section_geometry(doc)
    table_problems, n_tables = [], 0
    for child, sec in body_items(body):
        tables = [(child, True)] if child.tag == q("tbl") else []
        tables += [(t, False) for t in child.iter(q("tbl")) if t is not child]
        for t, top in tables:
            n_tables += 1
            pb = table_grid_problem(t, geometry[min(sec, len(geometry) - 1)][0], top)
            if pb:
                table_problems.append(f"table {n_tables}: {pb}")
    add("table_widths", not table_problems, "; ".join(table_problems[:5]) if table_problems
        else f"{n_tables} tables with a column grid that fits")

    img_problems, n_images = image_problems(body, geometry)
    add("images", not img_problems, "; ".join(img_problems[:5]) if img_problems else f"{n_images} images inside the page")

    texts = [body_text, "\n".join(c.text for t in doc.tables for row in t.rows for c in row.cells)]
    for name in names:
        if re.match(r"word/(header|footer)\d*\.xml$", name):
            texts.append(" ".join(para_text(p) for p in etree.fromstring(zf.read(name)).iter(q("p"))))
    leftovers = sorted(set(PLACEHOLDERS.findall("\n".join(texts))))
    add("placeholders", not leftovers, f"leftover placeholders: {', '.join(leftovers[:8])}" if leftovers
        else "no placeholder text")
    return checks, {"doc": doc, "zf": zf, "body": body_root, "words": words}


def next_step(path, checks):
    failed = [c["name"] for c in checks if not c["ok"]]
    if not failed:
        return (f"OK: all checks passed. Now call view_file on {path} and look at the pages (footer, tables, images, "
                "TOC) before you answer.")
    return (f"NOT DONE: fix the failing checks ({', '.join(failed)}), save, then run `{CMD} {path}` again (at most 3 "
            "rounds). Do not say the document is ready while ok is false; after 3 rounds tell the user exactly what is "
            "still wrong.")


def summary(path, checks, ctx):
    doc, zf, body = ctx["doc"], ctx["zf"], ctx["body"]
    names = zf.namelist()
    warnings: list[str] = []
    problems = [f"{c['name']}: {c['detail']}" for c in checks if not c["ok"]]

    styles_used = collections.Counter(p.style.name if p.style is not None else "-" for p in doc.paragraphs)
    outline = []
    for i, p in enumerate(doc.paragraphs):
        name = p.style.name if p.style is not None else ""
        m = re.match(r"^(Heading|Titre)\s*(\d)$", name, re.I)
        if m or name in ("Title",):
            outline.append({"index": i, "level": int(m.group(2)) if m else 0, "style": name,
                            "text": p.text[:200]})

    sections = []
    for s_idx, s in enumerate(doc.sections):
        def part_text(hf):
            try:
                if hf.is_linked_to_previous:
                    return None
                return "\n".join(p.text for p in hf.paragraphs).strip()
            except Exception:  # noqa: BLE001
                return None
        sections.append({
            "index": s_idx,
            "orientation": "landscape" if s.orientation == 1 else "portrait",
            "page_width_cm": cm(s.page_width), "page_height_cm": cm(s.page_height),
            "margins_cm": {"top": cm(s.top_margin), "bottom": cm(s.bottom_margin),
                           "left": cm(s.left_margin), "right": cm(s.right_margin)},
            "different_first_page": bool(s.different_first_page_header_footer),
            "header_text": part_text(s.header), "footer_text": part_text(s.footer),
        })

    tables = []
    for t_idx, t in enumerate(doc.tables):
        try:
            header = [c.text.strip()[:60] for c in t.rows[0].cells] if t.rows else []
        except Exception:  # noqa: BLE001
            header = []
        tables.append({"index": t_idx, "rows": len(t.rows), "cols": len(t.columns),
                       "style": t.style.name if t.style is not None else None, "first_row": header})

    fields = collections.Counter()
    all_xml_parts = [n for n in names if re.match(r"word/(document|header\d*|footer\d*|footnotes|endnotes)\.xml$", n)]
    ins = dels = moves = fmt_changes = 0
    fonts = collections.Counter()
    for name in all_xml_parts:
        root = body if name == "word/document.xml" else etree.fromstring(zf.read(name))
        for instr in root.iter(q("instrText")):
            word = (instr.text or "").strip().split(" ")[0].upper()
            if word:
                fields[word] += 1
        for fs in root.iter(q("fldSimple")):
            word = (fs.get(q("instr")) or "").strip().split(" ")[0].upper()
            if word:
                fields[word] += 1
        ins += len(root.findall(".//w:ins", NS))
        dels += len(root.findall(".//w:del", NS))
        moves += len(root.findall(".//w:moveFrom", NS)) + len(root.findall(".//w:moveTo", NS))
        fmt_changes += sum(len(root.findall(f".//w:{t}", NS)) for t in ("rPrChange", "pPrChange", "sectPrChange", "tblPrChange"))
        for rf in root.iter(q("rFonts")):
            f = rf.get(q("ascii")) or rf.get(q("hAnsi"))
            if f:
                fonts[f] += 1

    comments = []
    if "word/comments.xml" in names:
        croot = etree.fromstring(zf.read("word/comments.xml"))
        for c in croot.findall("w:comment", NS):
            comments.append({"id": c.get(q("id")), "author": c.get(q("author")),
                             "text": " ".join(para_text(p) for p in c.findall(".//w:p", NS))[:300]})

    inline_images = len(body.findall(".//wp:inline", NS))
    anchored_images = len(body.findall(".//wp:anchor", NS))
    vml_images = len(body.findall(".//v:imagedata", NS))
    text_boxes = len(body.findall(".//w:txbxContent", NS))
    settings_track = False
    if "word/settings.xml" in names:
        sroot = etree.fromstring(zf.read("word/settings.xml"))
        settings_track = sroot.find("w:trackRevisions", NS) is not None

    if ins or dels or moves:
        warnings.append("document contains tracked changes: python-docx `.text` shows inserted text and hides "
                        "deleted text; use tracked_changes.py --list / --accept-all / --reject-all")
    if text_boxes:
        warnings.append(f"{text_boxes} text box(es): their text is not in doc.paragraphs (replace.py handles them)")
    if any(n.startswith("word/embeddings/") for n in names):
        warnings.append("embedded OLE objects present (charts/spreadsheets): python-docx cannot edit them")
    warnings += problems

    cp = doc.core_properties
    return {
        "file": path,
        "ok": not problems,
        "problems": problems,
        "checks": checks,
        "next_step": next_step(path, checks),
        "core": {"title": cp.title, "subject": cp.subject, "author": cp.author,
                 "last_modified_by": cp.last_modified_by,
                 "created": cp.created.isoformat() if cp.created else None,
                 "modified": cp.modified.isoformat() if cp.modified else None},
        "counts": {"paragraphs": len(doc.paragraphs), "words": ctx["words"], "tables": len(doc.tables),
                   "inline_images": inline_images, "anchored_images": anchored_images + vml_images,
                   "text_boxes": text_boxes, "comments": len(comments), "sections": len(doc.sections),
                   "tracked_insertions": ins, "tracked_deletions": dels, "tracked_moves": moves,
                   "tracked_format_changes": fmt_changes,
                   "footnotes": max(0, len(etree.fromstring(zf.read("word/footnotes.xml")).findall("w:footnote", NS)) - 2)
                   if "word/footnotes.xml" in names else 0},
        "track_changes_on": settings_track,
        "fields": dict(fields),
        "styles_used": dict(styles_used.most_common()),
        "fonts_used": dict(fonts.most_common(10)),
        "outline": outline,
        "sections": sections,
        "tables": tables,
        "comments": comments[:50],
        "warnings": warnings,
    }


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--check", action="store_true", help="run the deliverable checks (exit 0 ok, 2 not ok)")
    ap.add_argument("--text", action="store_true", help="print numbered paragraphs instead of JSON")
    ap.add_argument("--limit", type=int, default=0, help="max paragraphs for --text (0 = all)")
    ap.add_argument("--range", help="with --text: paragraphs A-B (or one index) to list")
    ap.add_argument("--max-chars", type=int, default=200, help="with --text: cut each paragraph at this many characters")
    ap.add_argument("--full", action="store_true", help="with --text: no cut")
    args = ap.parse_args(argv)
    args.file = os.path.abspath(args.file)  # the checker contract names the file by its absolute path
    if not os.path.isfile(args.file):
        print(json.dumps({"ok": False, "file": args.file, "error": f"no such file: {args.file}"}))
        return 1

    if args.text:
        try:
            import docx
            doc = docx.Document(args.file)
        except Exception as exc:  # noqa: BLE001
            print(json.dumps({"ok": False, "error": f"cannot open {args.file}: {exc}"}))
            return 1
        total = len(doc.paragraphs)
        lo, hi = 0, total - 1
        if args.range:
            a, _, b = args.range.partition("-")
            try:
                lo, hi = int(a), int(b or a)
            except ValueError:
                lo, hi = -1, -1
            if not (0 <= lo <= hi < total):
                print(json.dumps({"ok": False, "reason": "out_of_range", "error": f"--range {args.range} out of bounds",
                                  "valid_range": [0, total - 1]}))
                return 1
        if args.limit:
            hi = min(hi, lo + args.limit - 1)
        limit = None if args.full else args.max_chars
        for i in range(lo, hi + 1):
            p = doc.paragraphs[i]
            print(f"[{i}] ({p.style.name if p.style is not None else '-'}) {clip(p.text, limit)}")
        if hi < total - 1 and not args.range:
            print(f"... {total - 1 - hi} more paragraphs of {total}; continue with --range {hi + 1}-{min(total - 1, hi + 40)}")
        if args.range:
            return 0
        for ti, table in enumerate(doc.tables):
            print(f"--- table {ti}: {len(table.rows)}x{len(table.columns)}")
            for ri, row in enumerate(table.rows[:50]):
                print(f"  r{ri}: " + " | ".join(c.text.replace("\n", " ") for c in row.cells))
        return 0

    checks, ctx = run_checks(args.file)
    ok = all(c["ok"] for c in checks)
    if args.check or ctx is None:
        print(json.dumps({"ok": ok, "file": args.file, "checks": checks, "next_step": next_step(args.file, checks)},
                         ensure_ascii=False, indent=2))
        return 0 if ok else 2
    print(json.dumps(summary(args.file, checks, ctx), ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
