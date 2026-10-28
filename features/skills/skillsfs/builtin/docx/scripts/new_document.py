#!/usr/bin/env python3
"""Build a .docx from a JSON spec with python-docx.

Usage:
  python3 new_document.py --spec spec.json --out /workspace/outputs/report.docx
  python3 new_document.py --example            # print an example spec

Spec (all keys optional except blocks):
{
  "locale": "fr",                 # fr | en  (dates, "Page x sur y", language)
  "template": "path.docx",        # start from an existing document/template instead
  "title": "...", "subtitle": "...", "author": "...",
  "date": "2026-10-06",           # or "14 octobre 2025" / "7 October 2026"; other text is kept as is
  "title_page": true,             # title block + page break (needs "title")
  "header": "text right-aligned in the header",
  "footer": "Report title",       # footer = ONE paragraph "Report title · Page x of y" (default: title)
  "landscape": false,
  "blocks": [
    {"type": "heading", "text": "Introduction", "level": 1},
    {"type": "paragraph", "text": "Supports **bold**, *italic*, `code` and [links](https://x.y).",
     "align": "justify"},
    {"type": "bullets", "items": ["one", "two", {"text": "nested", "level": 1}]},
    {"type": "numbered", "items": ["first", "second"]},          # numbering restarts per list
    {"type": "table", "columns": ["Name", "Value"], "rows": [["a", "1"]],
     "widths_cm": [10, 4], "numeric_cols": [1], "caption": "Table 1 - ..."},
    {"type": "image", "path": "/workspace/.work/chart.png", "width_cm": 16, "caption": "..."},
    {"type": "callout", "text": "Key message", "kind": "note"},   # note | warning
    {"type": "toc", "title": "Sommaire"},                         # filled with fill_toc.py logic
    {"type": "page_break"}
  ]
}
Each block may also be written in shorthand: {"heading": {"text": "Intro", "level": 1}},
{"heading": "Intro"}, {"paragraph": "Text"}, {"bullets": ["a", "b"]}, {"page_break": {}};
a bare string is a paragraph. "blocks" may be the whole spec (a JSON list).
Prints {"out": ..., "paragraphs": n, "tables": n, "images": n, "toc": {...}|null}.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

EXAMPLE = {
    "locale": "en", "title": "Quarterly review", "subtitle": "Q3 2026", "author": "Example",
    "title_page": True, "footer": "Quarterly review - Q3 2026",
    "blocks": [
        {"type": "toc", "title": "Contents"},
        {"type": "heading", "text": "Summary", "level": 1},
        {"type": "paragraph", "text": "Revenue grew **12%** year on year."},
        {"type": "bullets", "items": ["Point one", "Point two"]},
        {"type": "heading", "text": "Figures", "level": 1},
        {"type": "table", "columns": ["Metric", "Value"], "rows": [["Revenue", "1 200"], ["Cost", "800"]],
         "numeric_cols": [1]},
    ],
}


sys.path.insert(0, HERE)
from blocks import SHAPES_HELP as _SHAPES_HELP, normalize_spec  # noqa: E402

SHAPES_HELP = _SHAPES_HELP.replace("{builder}", "python3 /skills/docx/scripts/new_document.py")

INLINE = re.compile(r"(\*\*.+?\*\*|\*[^*]+?\*|`[^`]+?`|\[[^\]]+?\]\([^)]+?\))")


def load_brand():
    """No external brand module: the built-in palette and styles are used."""
    return None


def add_hyperlink(paragraph, url, text):
    from docx.opc.constants import RELATIONSHIP_TYPE as RT
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn

    r_id = paragraph.part.relate_to(url, RT.HYPERLINK, is_external=True)
    link = OxmlElement("w:hyperlink")
    link.set(qn("r:id"), r_id)
    run = OxmlElement("w:r")
    rpr = OxmlElement("w:rPr")
    color = OxmlElement("w:color"); color.set(qn("w:val"), "2E6DA4")
    underline = OxmlElement("w:u"); underline.set(qn("w:val"), "single")
    rpr.append(color); rpr.append(underline)
    run.append(rpr)
    t = OxmlElement("w:t"); t.text = text; t.set(qn("xml:space"), "preserve")
    run.append(t)
    link.append(run)
    paragraph._p.append(link)


def add_inline(paragraph, text):
    """Append text with **bold**, *italic*, `code` and [label](url) to a paragraph."""
    for part in INLINE.split(text or ""):
        if not part:
            continue
        if part.startswith("**") and part.endswith("**"):
            paragraph.add_run(part[2:-2]).bold = True
        elif part.startswith("`") and part.endswith("`"):
            r = paragraph.add_run(part[1:-1])
            r.font.name = "Courier New"
        elif part.startswith("[") and "](" in part:
            label, url = part[1:].split("](", 1)
            add_hyperlink(paragraph, url[:-1], label)
        elif part.startswith("*") and part.endswith("*") and len(part) > 2:
            paragraph.add_run(part[1:-1]).italic = True
        else:
            paragraph.add_run(part)


def restart_numbering(doc, paragraph):
    """Give `paragraph` (style List Number) a fresh w:num so its list restarts at 1."""
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn

    numbering = doc.part.numbering_part.element
    style = doc.styles["List Number"].element
    num_id_el = style.find(qn("w:pPr") + "/" + qn("w:numPr") + "/" + qn("w:numId"))
    if num_id_el is None:
        return None
    base_num = num_id_el.get(qn("w:val"))
    abstract = None
    for num in numbering.findall(qn("w:num")):
        if num.get(qn("w:numId")) == base_num:
            abstract = num.find(qn("w:abstractNumId")).get(qn("w:val"))
    if abstract is None:
        return None
    new_id = str(max(int(n.get(qn("w:numId"))) for n in numbering.findall(qn("w:num"))) + 1)
    num = OxmlElement("w:num"); num.set(qn("w:numId"), new_id)
    an = OxmlElement("w:abstractNumId"); an.set(qn("w:val"), abstract); num.append(an)
    ov = OxmlElement("w:lvlOverride"); ov.set(qn("w:ilvl"), "0")
    so = OxmlElement("w:startOverride"); so.set(qn("w:val"), "1"); ov.append(so); num.append(ov)
    numbering.append(num)
    return new_id


def set_num(paragraph, num_id, level=0):
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn

    ppr = paragraph._p.get_or_add_pPr()
    numpr = OxmlElement("w:numPr")
    il = OxmlElement("w:ilvl"); il.set(qn("w:val"), str(level)); numpr.append(il)
    ni = OxmlElement("w:numId"); ni.set(qn("w:val"), num_id); numpr.append(ni)
    ppr.append(numpr)


def set_col_widths(table, widths):
    """Column widths (python-docx Lengths) on the grid AND on every cell: Word reads the cells,
    LibreOffice (PDF export, previews) reads w:tblGrid/w:gridCol and ignores cell-only widths."""
    from docx.oxml.ns import qn

    table.autofit = False
    tbl = table._tbl
    for grid_col, w in zip(tbl.tblGrid.findall(qn("w:gridCol")), widths):
        grid_col.set(qn("w:w"), str(int(w.twips)))
    for row in table.rows:
        for cell, w in zip(row.cells, widths):
            cell.width = w
    tbl_w = tbl.tblPr.find(qn("w:tblW"))       # python-docx tables always have one
    if tbl_w is not None:
        tbl_w.set(qn("w:type"), "dxa")
        tbl_w.set(qn("w:w"), str(int(sum(w.twips for w in widths))))


def shade_paragraph(paragraph, fill, border_color):
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn

    ppr = paragraph._p.get_or_add_pPr()
    pbdr = OxmlElement("w:pBdr")
    left = OxmlElement("w:left")
    for k, v in (("w:val", "single"), ("w:sz", "24"), ("w:space", "8"), ("w:color", border_color)):
        left.set(qn(k), v)
    pbdr.append(left)
    ppr.append(pbdr)
    shd = OxmlElement("w:shd")
    for k, v in (("w:val", "clear"), ("w:color", "auto"), ("w:fill", fill)):
        shd.set(qn(k), v)
    ppr.append(shd)


def add_toc_field(doc, title, locale):
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn

    if title:
        try:
            doc.add_paragraph(title, style="TOC Heading")
        except KeyError:
            p = doc.add_paragraph()
            p.add_run(title).bold = True
    p = doc.add_paragraph()
    for kind, text in (("begin", None), (None, ' TOC \\o "1-3" \\h \\z \\u '), ("separate", None),
                       (None, "Mettre à jour la table des matières" if locale == "fr" else "Update the table of contents"),
                       ("end", None)):
        r = p.add_run()
        if kind:
            fc = OxmlElement("w:fldChar"); fc.set(qn("w:fldCharType"), kind); r._r.append(fc)
        elif text.startswith(" TOC"):
            it = OxmlElement("w:instrText"); it.set(qn("xml:space"), "preserve"); it.text = text; r._r.append(it)
        else:
            r.text = text
    return p


def build(spec, out):
    spec = normalize_spec(spec, "python3 /skills/docx/scripts/new_document.py")
    import docx
    from docx.enum.section import WD_ORIENT
    from docx.enum.text import WD_ALIGN_PARAGRAPH, WD_BREAK
    from docx.shared import Cm, Pt

    locale = spec.get("locale", "fr")
    doc = docx.Document(spec["template"]) if spec.get("template") else docx.Document()
    brand = load_brand() if spec.get("brand", True) else None
    if brand:
        brand.docx_apply(doc, locale)
    if spec.get("landscape"):
        s = doc.sections[0]
        s.orientation = WD_ORIENT.LANDSCAPE
        s.page_width, s.page_height = s.page_height, s.page_width
    core = doc.core_properties
    if spec.get("title"):
        core.title = spec["title"]
    if spec.get("author"):
        core.author = core.last_modified_by = spec["author"]
    core.created = core.modified = dt.datetime.now(dt.timezone.utc).replace(microsecond=0, tzinfo=None)
    core.revision = 1
    # Any text: a French/English date is written in the locale's long form, anything else as is
    # (brand.date_label); never parsed strictly ('14 octobre 2025' used to crash the build).
    date = spec.get("date") or None

    if spec.get("title") and spec.get("title_page", True):
        if brand:
            brand.docx_title_block(doc, spec["title"], spec.get("subtitle"), spec.get("author"), date, locale)
        else:
            doc.add_paragraph(spec["title"], style="Title")
            if spec.get("subtitle"):
                doc.add_paragraph(spec["subtitle"], style="Subtitle")
            doc.add_paragraph().add_run().add_break(WD_BREAK.PAGE)
    footer_text = spec.get("footer", spec.get("title", ""))
    if brand:
        brand.docx_footer(doc, footer_text, locale, spec.get("header"))
    elif spec.get("footer", True) is not False:
        sys.path.insert(0, HERE)
        from footer import set_footer  # noqa: PLC0415
        set_footer(doc, footer_text if isinstance(footer_text, str) else "", locale, spec.get("header"))

    aligns = {"left": WD_ALIGN_PARAGRAPH.LEFT, "center": WD_ALIGN_PARAGRAPH.CENTER,
              "right": WD_ALIGN_PARAGRAPH.RIGHT, "justify": WD_ALIGN_PARAGRAPH.JUSTIFY}
    stats = {"images": 0, "tables": 0}
    has_toc = False
    first_numbered = True
    for i, block in enumerate(spec.get("blocks", [])):
        kind = block.get("type")
        if kind == "heading":
            doc.add_heading(block["text"], level=int(block.get("level", 1)))
        elif kind == "paragraph":
            p = doc.add_paragraph(style=block.get("style"))
            add_inline(p, block.get("text", ""))
            if block.get("align") in aligns:
                p.alignment = aligns[block["align"]]
        elif kind in ("bullets", "numbered"):
            num_id = None
            for item in block.get("items", []):
                text, level = (item.get("text", ""), int(item.get("level", 0))) if isinstance(item, dict) else (item, 0)
                if kind == "bullets":
                    style = "List Bullet" if level == 0 else f"List Bullet {min(level + 1, 3)}"
                else:
                    style = "List Number" if level == 0 else f"List Number {min(level + 1, 3)}"
                p = doc.add_paragraph(style=style)
                add_inline(p, text)
                if kind == "numbered" and level == 0:
                    if num_id is None and not first_numbered:
                        num_id = restart_numbering(doc, p)
                    if num_id:
                        set_num(p, num_id)
            if kind == "numbered":
                first_numbered = False
        elif kind == "table":
            cols = block.get("columns") or []
            rows = block.get("rows") or []
            ncols = len(cols) or (len(rows[0]) if rows else 1)
            table = doc.add_table(rows=(1 if cols else 0) + len(rows), cols=ncols)
            table.style = doc.styles["Table Grid"] if not brand else table.style
            data = ([cols] if cols else []) + rows
            for r_idx, row in enumerate(data):
                for c_idx in range(ncols):
                    value = row[c_idx] if c_idx < len(row) else ""
                    table.cell(r_idx, c_idx).text = "" if value is None else str(value)
            widths = block.get("widths_cm")
            if widths:
                set_col_widths(table, [Cm(float(w)) for w in widths[:ncols]])
            if brand:
                brand.docx_style_table(table, header=bool(cols), numeric_cols=block.get("numeric_cols"))
            if block.get("caption"):
                doc.add_paragraph(block["caption"], style="Caption")
            stats["tables"] += 1
        elif kind == "image":
            path = block["path"]
            if not os.path.isfile(path):
                raise SystemExit(f"error: image not found: {path} (block {i + 1})")
            doc.add_picture(path, width=Cm(float(block.get("width_cm", 16))))
            doc.paragraphs[-1].alignment = WD_ALIGN_PARAGRAPH.CENTER
            if block.get("caption"):
                doc.add_paragraph(block["caption"], style="Caption")
            stats["images"] += 1
        elif kind == "callout":
            p = doc.add_paragraph()
            add_inline(p, block.get("text", ""))
            warn = block.get("kind") == "warning"
            shade_paragraph(p, "FDF3E1" if warn else "EEF1F5", "E9A23B" if warn else "1B365D")
            p.paragraph_format.space_before = Pt(6)
            p.paragraph_format.space_after = Pt(10)
        elif kind == "toc":
            add_toc_field(doc, block.get("title", "Sommaire" if locale == "fr" else "Contents"), locale)
            if block.get("page_break", True):
                doc.add_paragraph().add_run().add_break(WD_BREAK.PAGE)
            has_toc = True
        elif kind == "page_break":
            doc.add_paragraph().add_run().add_break(WD_BREAK.PAGE)
        else:
            raise SystemExit(f"error: unknown block type {kind!r} (block {i + 1})\n{SHAPES_HELP}")
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    doc.save(out)
    result = {"out": out, "paragraphs": len(doc.paragraphs), "tables": stats["tables"],
              "images": stats["images"], "toc": None}
    if has_toc and spec.get("fill_toc", True):
        res = subprocess.run([sys.executable, os.path.join(HERE, "fill_toc.py"), out, "--inplace"],
                             capture_output=True, text=True)
        if res.returncode == 0:
            toc = json.loads(res.stdout)
            result["toc"] = {"entries": len(toc["entries"]), "missing": toc["missing"]}
        else:
            result["toc"] = {"error": (res.stdout + res.stderr).strip()[-500:]}
    return result


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--spec", help="JSON spec file ('-' for stdin)")
    ap.add_argument("--out", help="output .docx (default: spec 'output')")
    ap.add_argument("--example", action="store_true", help="print an example spec and exit")
    args = ap.parse_args(argv)
    if args.example:
        print(json.dumps(EXAMPLE, indent=2, ensure_ascii=False))
        return 0
    if not args.spec:
        ap.error("--spec is required")
    with (sys.stdin if args.spec == "-" else open(args.spec, encoding="utf-8")) as fh:
        spec = json.load(fh)
    out = args.out or spec.get("output")
    if not out:
        ap.error("--out is required")
    print(json.dumps(build(spec, out), ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
