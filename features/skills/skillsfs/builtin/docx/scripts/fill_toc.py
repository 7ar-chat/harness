#!/usr/bin/env python3
"""Fill (or insert) the table of contents of a .docx with real entries and page numbers.

python-docx and the docx npm package can only insert an EMPTY TOC field ("update field"
placeholder): Word fills it when the user presses F9, LibreOffice / PDF export never does.
This script computes the entries from the heading styles, gets the page numbers from a
LibreOffice PDF render (PDF outline), writes them as the field's result, and repeats until
the page numbers are stable. The field stays a real TOC field: Word can still update it.

Usage:
  python3 fill_toc.py IN.docx --inplace                     # fill the existing TOC field
  python3 fill_toc.py IN.docx --out OUT.docx
  python3 fill_toc.py IN.docx --inplace --insert-before 3 --title "Sommaire" --page-break
      # no TOC yet: insert one before body paragraph 3 (index from inspect_docx.py --text)
  python3 fill_toc.py IN.docx --inplace --levels 1-2        # override the \\o "1-3" levels

Output (JSON): {"out", "entries": [{"level","text","page"}], "passes", "missing": [...]}
Exit 1 if there is no TOC field (and no --insert-before) or LibreOffice fails.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
import tempfile
import zipfile

from lxml import etree

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lo  # noqa: E402

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
XML_SPACE = "{http://www.w3.org/XML/1998/namespace}space"


def q(tag):
    return "{%s}%s" % (W, tag)


def el(tag, attrs=None, text=None):
    e = etree.Element(q(tag), {q(k): str(v) for k, v in (attrs or {}).items()})
    if text is not None:
        e.text = text
        e.set(XML_SPACE, "preserve")
    return e


def norm(s):
    return re.sub(r"\s+", " ", s or "").strip().lower()


def owning_paragraph(node):
    while node is not None and node.tag != q("p"):
        node = node.getparent()
    return node


def owning_run(node):
    while node is not None and node.tag != q("r"):
        node = node.getparent()
    return node


def style_levels(styles_root):
    levels = {}
    for st in styles_root.findall(q("style")):
        if st.get(q("type")) != "paragraph":
            continue
        sid = st.get(q("styleId"))
        name = st.find(q("name"))
        name = name.get(q("val")) if name is not None else ""
        m = re.match(r"^heading\s*(\d)$", name or "", re.I)
        lvl = st.find(f"{q('pPr')}/{q('outlineLvl')}")
        if m:
            levels[sid] = int(m.group(1))
        elif lvl is not None and lvl.get(q("val")) is not None and int(lvl.get(q("val"))) < 9:
            levels[sid] = int(lvl.get(q("val"))) + 1
    return levels


def para_level(p, levels):
    ppr = p.find(q("pPr"))
    if ppr is not None:
        ol = ppr.find(q("outlineLvl"))
        if ol is not None and ol.get(q("val")) is not None:
            v = int(ol.get(q("val")))
            return v + 1 if v < 9 else None
        ps = ppr.find(q("pStyle"))
        if ps is not None:
            return levels.get(ps.get(q("val")))
    return None


def find_toc_field(body):
    """Return (begin, separate, end, instr) of the first TOC complex field."""
    stack = []
    for node in body.iter(q("fldChar"), q("instrText")):
        if node.tag == q("instrText"):
            if stack and stack[-1]["sep"] is None:
                stack[-1]["instr"] += node.text or ""
            continue
        kind = node.get(q("fldCharType"))
        if kind == "begin":
            stack.append({"begin": node, "sep": None, "instr": ""})
        elif kind == "separate" and stack:
            stack[-1]["sep"] = node
        elif kind == "end" and stack:
            f = stack.pop()
            if f["instr"].strip().upper().startswith("TOC"):
                return f["begin"], f["sep"], node, f["instr"]
    return None


SDT_KINDS = ("equation", "comboBox", "date", "docPartObj", "docPartList", "dropDownList", "picture", "richText",
             "text", "citation", "group", "bibliography")


def mark_toc_control(begin):
    """If the TOC field sits in a content control (w:sdt) that is not declared as a table of
    contents, declare it (docPartGallery "Table of Contents", as Word writes it). The docx npm
    package writes a bare w:sdt: LibreOffice then prints the field code ('TOC \\h \\o "1-3"')
    in front of the entries. Returns True when the control was changed."""
    node = begin
    while node is not None and node.tag != q("sdtContent"):
        node = node.getparent()
    if node is None:
        return False
    sdt_pr = node.getparent().find(q("sdtPr"))
    if sdt_pr is None:
        sdt_pr = el("sdtPr")
        node.addprevious(sdt_pr)
    if any(sdt_pr.find(q(k)) is not None for k in SDT_KINDS):
        return False
    obj = el("docPartObj")
    obj.append(el("docPartGallery", {"val": "Table of Contents"}))
    obj.append(el("docPartUnique"))
    sdt_pr.append(obj)
    return True


def simple_to_complex(body):
    """Convert a w:fldSimple TOC into a complex field (begin/instr/separate/end)."""
    for fs in body.iter(q("fldSimple")):
        instr = fs.get(q("instr")) or ""
        if instr.strip().upper().startswith("TOC"):
            runs = [el("r"), el("r"), el("r"), el("r")]
            runs[0].append(el("fldChar", {"fldCharType": "begin"}))
            runs[1].append(el("instrText", text=f" {instr.strip()} "))
            runs[2].append(el("fldChar", {"fldCharType": "separate"}))
            runs[3].append(el("fldChar", {"fldCharType": "end"}))
            parent, idx = fs.getparent(), fs.getparent().index(fs)
            parent.remove(fs)
            for i, r in enumerate(runs):
                parent.insert(idx + i, r)
            return True
    return False


def insert_toc(body, before_index, title, page_break, title_style):
    paras = [p for p in body if p.tag == q("p")]
    if before_index < 0 or before_index > len(paras):
        raise SystemExit(f"error: --insert-before {before_index} out of range (0..{len(paras)})")
    anchor = paras[before_index] if before_index < len(paras) else body.find(q("sectPr"))
    new = []
    if title:
        tp = el("p")
        ppr = el("pPr")
        ppr.append(el("pStyle", {"val": title_style}))
        tp.append(ppr)
        r = el("r")
        r.append(el("t", text=title))
        tp.append(r)
        new.append(tp)
    p = el("p")
    for child in (el("fldChar", {"fldCharType": "begin"}), el("instrText", text=' TOC \\o "1-3" \\h \\z \\u '),
                  el("fldChar", {"fldCharType": "separate"}), el("fldChar", {"fldCharType": "end"})):
        r = el("r")
        r.append(child)
        p.append(r)
    new.append(p)
    if page_break:
        bp = el("p")
        r = el("r")
        r.append(el("br", {"type": "page"}))
        bp.append(r)
        new.append(bp)
    for n in new:
        anchor.addprevious(n)


def text_width_twips(body):
    sect = body.find(q("sectPr"))
    try:
        pg = sect.find(q("pgSz"))
        mar = sect.find(q("pgMar"))
        return int(pg.get(q("w"))) - int(mar.get(q("left"))) - int(mar.get(q("right")))
    except Exception:  # noqa: BLE001
        return 9026  # A4 with 2.54 cm margins


def ensure_toc_styles(styles_root, max_level):
    existing = {s.get(q("styleId")) for s in styles_root.findall(q("style"))}
    for n in range(1, max_level + 1):
        sid = f"TOC{n}"
        if sid in existing:
            continue
        st = el("style", {"type": "paragraph", "styleId": sid})
        st.append(el("name", {"val": f"toc {n}"}))
        st.append(el("basedOn", {"val": "Normal"}))
        st.append(el("next", {"val": "Normal"}))
        st.append(el("uiPriority", {"val": "39"}))
        st.append(el("unhideWhenUsed"))
        ppr = el("pPr")
        ppr.append(el("spacing", {"before": "60" if n == 1 else "0", "after": "60"}))
        ppr.append(el("ind", {"left": str((n - 1) * 284)}))
        st.append(ppr)
        if n == 1:
            rpr = el("rPr")
            rpr.append(el("b"))
            st.append(rpr)
        styles_root.append(st)


def entry_paragraph(level, text, page, width):
    p = el("p")
    ppr = el("pPr")
    ppr.append(el("pStyle", {"val": f"TOC{level}"}))
    tabs = el("tabs")
    tabs.append(el("tab", {"val": "right", "leader": "dot", "pos": str(width)}))
    ppr.append(tabs)
    p.append(ppr)
    for child in (el("t", text=text), el("tab"), el("t", text=str(page))):
        r = el("r")
        r.append(child)
        p.append(r)
    return p


def rebuild(body, entries, width):
    """Replace the TOC field result by `entries` [(level, text, page)]."""
    found = find_toc_field(body)
    begin, sep, end, _ = found
    begin_p, end_p = owning_paragraph(begin), owning_paragraph(end)
    sep_run, end_run = owning_run(sep), owning_run(end)
    # 1. remove the old result
    if begin_p is end_p:
        node = sep_run.getnext()
        while node is not None and node is not end_run:
            nxt = node.getnext()
            begin_p.remove(node)
            node = nxt
        end_p.remove(end_run)
    else:
        node = sep_run.getnext()
        while node is not None:
            nxt = node.getnext()
            begin_p.remove(node)
            node = nxt
        node = begin_p.getnext()
        while node is not None and node is not end_p:
            nxt = node.getnext()
            node.getparent().remove(node)
            node = nxt
        node = end_p.find(q("pPr"))
        node = end_p[0] if node is None else node.getnext()
        while node is not None and node is not end_run:
            nxt = node.getnext()
            end_p.remove(node)
            node = nxt
        end_p.remove(end_run)
        has_content = any(c.tag != q("pPr") for c in end_p)
        if not has_content:
            end_p.getparent().remove(end_p)
    # 2. write the new result
    if not entries:
        entries = [(1, "(no headings found)", "")]
    first = entry_paragraph(*entries[0], width)
    ppr = begin_p.find(q("pPr"))
    if ppr is not None:
        begin_p.remove(ppr)
    begin_p.insert(0, first.find(q("pPr")))
    for child in [c for c in first if c.tag != q("pPr")]:
        begin_p.append(child)
    last = begin_p
    for entry in entries[1:]:
        p = entry_paragraph(*entry, width)
        last.addnext(p)
        last = p
    r = el("r")
    r.append(el("fldChar", {"fldCharType": "end"}))
    last.append(r)


def collect_headings(body, levels, lo_level, hi_level):
    found = find_toc_field(body)
    begin_p, end_p = owning_paragraph(found[0]), owning_paragraph(found[2])
    toc_paras = set()
    if begin_p is not None:
        node = begin_p
        while node is not None:
            toc_paras.add(node)
            if node is end_p:
                break
            node = node.getnext()
    out = []
    for p in body.iter(q("p")):
        if p in toc_paras:
            continue
        lvl = para_level(p, levels)
        if lvl is None or not (lo_level <= lvl <= hi_level):
            continue
        text = "".join(t.text or "" for t in p.iter(q("t"))).strip()
        if text:
            out.append((lvl, text))
    return out


def pdf_outline_pages(pdf):
    from pypdf import PdfReader

    reader = PdfReader(pdf)
    items = []

    def walk(nodes):
        for n in nodes:
            if isinstance(n, list):
                walk(n)
            else:
                try:
                    items.append((norm(n.title), reader.get_destination_page_number(n) + 1))
                except Exception:  # noqa: BLE001
                    pass
    walk(reader.outline)
    return items


def match_pages(headings, outline):
    pages, missing, cursor = [], [], 0
    for _, text in headings:
        key = norm(text)
        hit = None
        for i in range(cursor, len(outline)):
            title = outline[i][0]
            if title == key or title.endswith(key) or (key and key in title):
                hit = i
                break
        if hit is None:
            pages.append("")
            missing.append(text)
        else:
            pages.append(outline[hit][1])
            cursor = hit + 1
    return pages, missing


def write_docx(src_zip, out, parts):
    fd, tmp = tempfile.mkstemp(suffix=".docx", dir=os.path.dirname(os.path.abspath(out)) or ".")
    os.close(fd)
    with zipfile.ZipFile(src_zip) as zin, zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zout:
        for item in zin.infolist():
            data = parts.get(item.filename)
            zout.writestr(item, data if data is not None else zin.read(item.filename))
    shutil.move(tmp, out)


def ser(root):
    return etree.tostring(root, xml_declaration=True, encoding="UTF-8", standalone=True)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("input")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    ap.add_argument("--insert-before", type=int)
    ap.add_argument("--title", help="title paragraph above an inserted TOC (e.g. Sommaire / Contents)")
    ap.add_argument("--page-break", action="store_true", help="page break after an inserted TOC")
    ap.add_argument("--levels", help='heading levels, e.g. "1-2" (default: from the field, else 1-3)')
    ap.add_argument("--max-passes", type=int, default=4)
    args = ap.parse_args(argv)
    if not (args.out or args.inplace):
        ap.error("give --out OUT.docx or --inplace")
    out = args.input if args.inplace else args.out

    with zipfile.ZipFile(args.input) as z:
        doc_root = etree.fromstring(z.read("word/document.xml"))
        styles_root = etree.fromstring(z.read("word/styles.xml"))
    body = doc_root.find(q("body"))
    levels = style_levels(styles_root)
    if args.insert_before is not None:
        title_style = next((sid for sid in ("TOCHeading", "Heading1") if any(
            s.get(q("styleId")) == sid for s in styles_root.findall(q("style")))), "Normal")
        insert_toc(body, args.insert_before, args.title, args.page_break, title_style)
    if find_toc_field(body) is None and not simple_to_complex(body):
        print(json.dumps({"error": "no TOC field in the document; use --insert-before N"}))
        return 1
    mark_toc_control(find_toc_field(body)[0])
    instr = find_toc_field(body)[3]
    lo_level, hi_level = 1, 3
    spec = args.levels or (re.search(r'\\o\s+"(\d)-(\d)"', instr) and "-".join(re.search(r'\\o\s+"(\d)-(\d)"', instr).groups()))
    if spec:
        a, b = spec.split("-")
        lo_level, hi_level = int(a), int(b)
    ensure_toc_styles(styles_root, hi_level)
    width = text_width_twips(body)
    headings = collect_headings(body, levels, lo_level, hi_level)

    work = tempfile.mkdtemp(prefix="fill_toc.", dir="/tmp")
    try:
        pages = ["0"] * len(headings)
        missing, passes = [], 0
        for passes in range(1, args.max_passes + 1):
            rebuild(body, [(lvl, text, pages[i]) for i, (lvl, text) in enumerate(headings)], width)
            tmp_docx = os.path.join(work, "measure.docx")
            write_docx(args.input, tmp_docx, {"word/document.xml": ser(doc_root), "word/styles.xml": ser(styles_root)})
            try:
                pdf = lo.convert(tmp_docx, "pdf", work)
            except lo.OfficeError as exc:
                print(json.dumps({"error": str(exc)}))
                return 1
            new_pages, missing = match_pages(headings, pdf_outline_pages(pdf))
            if new_pages == pages:
                break
            pages = new_pages
        rebuild(body, [(lvl, text, pages[i]) for i, (lvl, text) in enumerate(headings)], width)
        write_docx(args.input, out, {"word/document.xml": ser(doc_root), "word/styles.xml": ser(styles_root)})
    finally:
        shutil.rmtree(work, ignore_errors=True)
    result = {"out": out, "passes": passes, "levels": [lo_level, hi_level],
              "entries": [{"level": lvl, "text": t, "page": pages[i]} for i, (lvl, t) in enumerate(headings)],
              "missing": missing}
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if missing:
        print(f"warning: no page found for {len(missing)} heading(s)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
