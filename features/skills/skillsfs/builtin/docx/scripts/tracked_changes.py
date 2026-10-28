#!/usr/bin/env python3
"""List, accept or reject tracked changes (revisions) in a .docx; toggle "Track changes";
strip comments. Works directly on the WordprocessingML, other parts are copied unchanged.

Usage:
  python3 tracked_changes.py IN.docx --list
  python3 tracked_changes.py IN.docx --accept-all --out CLEAN.docx
  python3 tracked_changes.py IN.docx --reject-all --out ORIGINAL.docx
  python3 tracked_changes.py IN.docx --enable-tracking --inplace   # Word records later edits
  python3 tracked_changes.py IN.docx --accept-all --strip-comments --out FINAL.docx

Handles: inserted/deleted runs (w:ins / w:del), moves (w:moveFrom / w:moveTo), inserted or
deleted paragraph marks (paragraph merge), formatting changes (w:rPrChange, w:pPrChange,
w:sectPrChange, w:tblPrChange, w:trPrChange, w:tcPrChange). Table cell insert/delete
(w:cellIns / w:cellDel) is reported but left untouched.
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

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
PART_RE = re.compile(r"^word/(document|header\d*|footer\d*|footnotes|endnotes|comments)\.xml$")
PR_CHANGES = ("rPrChange", "pPrChange", "sectPrChange", "tblPrChange", "trPrChange", "tcPrChange",
              "tblGridChange", "tblPrExChange", "numberingChange")
KEEP_IN_PR = {"rPr", "sectPr", "ins", "del", "moveFrom", "moveTo"}


def q(tag: str) -> str:
    return "{%s}%s" % (W, tag)


def local(el) -> str:
    return etree.QName(el).localname


def unwrap(el):
    parent = el.getparent()
    idx = parent.index(el)
    for child in list(el):
        parent.insert(idx, child)
        idx += 1
    parent.remove(el)


def remove(el):
    el.getparent().remove(el)


def text_of(el, tags=("t", "delText")) -> str:
    return "".join(n.text or "" for n in el.iter() if isinstance(n.tag, str) and local(n) in tags)


def merge_with_next_paragraph(p):
    """Paragraph mark of p removed: move p's content to the start of the next paragraph."""
    nxt = p.getnext()
    while nxt is not None and nxt.tag != q("p"):
        nxt = nxt.getnext()
    if nxt is None:
        return
    nppr = nxt.find(q("pPr"))
    insert_at = 1 if nppr is not None else 0
    for child in [c for c in p if c.tag != q("pPr")]:
        nxt.insert(insert_at, child)
        insert_at += 1
    remove(p)


def restore_old_properties(change):
    """Reject a *PrChange: replace the current properties by the recorded old ones."""
    parent = change.getparent()
    old = next(iter(change), None)  # the old w:rPr / w:pPr / ... element
    for child in list(parent):
        if child is change or local(child) in KEEP_IN_PR:
            continue
        parent.remove(child)
    remove(change)
    if old is not None:
        for i, child in enumerate(list(old)):
            parent.insert(i, child)


def paragraph_mark_marker(el) -> bool:
    par = el.getparent()
    return par is not None and par.tag == q("rPr") and par.getparent() is not None and par.getparent().tag == q("pPr")


def process(root, mode):
    """mode: 'accept' or 'reject'. Returns counts."""
    n = {"insertions": 0, "deletions": 0, "moves": 0, "format_changes": 0, "paragraph_marks": 0}
    merge = []
    for el in list(root.iter(q("ins"), q("del"), q("moveFrom"), q("moveTo"))):
        if el.getparent() is None:
            continue
        kind = local(el)
        if el.getparent().tag == q("trPr"):  # inserted / deleted table row
            row = el.getparent().getparent()
            n["insertions" if kind == "ins" else "deletions"] += 1
            drop_row = (kind == "del" and mode == "accept") or (kind == "ins" and mode == "reject")
            remove(el)
            if drop_row and row.getparent() is not None:
                remove(row)
            continue
        if paragraph_mark_marker(el):
            n["paragraph_marks"] += 1
            p = el.getparent().getparent().getparent()
            removed_mark = (kind == "del" and mode == "accept") or (kind == "ins" and mode == "reject")
            remove(el)
            if removed_mark:
                merge.append(p)
            continue
        if kind in ("ins", "moveTo"):
            n["insertions" if kind == "ins" else "moves"] += 1
            unwrap(el) if mode == "accept" else remove(el)
        else:
            n["deletions" if kind == "del" else "moves"] += 1
            if mode == "accept":
                remove(el)
            else:
                for t in el.iter(q("delText")):
                    t.tag = q("t")
                for t in el.iter(q("delInstrText")):
                    t.tag = q("instrText")
                unwrap(el)
    for tag in ("moveFromRangeStart", "moveFromRangeEnd", "moveToRangeStart", "moveToRangeEnd"):
        for el in list(root.iter(q(tag))):
            remove(el)
    for tag in PR_CHANGES:
        for el in list(root.iter(q(tag))):
            n["format_changes"] += 1
            remove(el) if mode == "accept" else restore_old_properties(el)
    for p in merge:
        if p.getparent() is not None:
            merge_with_next_paragraph(p)
    return n


def list_changes(zf):
    items, other = [], {"cellIns": 0, "cellDel": 0}
    for name in zf.namelist():
        if not PART_RE.match(name):
            continue
        root = etree.fromstring(zf.read(name))
        for el in root.iter(q("ins"), q("del"), q("moveFrom"), q("moveTo")):
            items.append({"part": name, "type": local(el), "author": el.get(q("author")),
                          "date": el.get(q("date")),
                          "paragraph_mark": paragraph_mark_marker(el),
                          "text": text_of(el)[:200]})
        for tag in PR_CHANGES:
            for el in root.iter(q(tag)):
                items.append({"part": name, "type": tag, "author": el.get(q("author")),
                              "date": el.get(q("date")), "text": text_of(el.getparent().getparent())[:80]})
        for tag in other:
            other[tag] += len(list(root.iter(q(tag))))
    comments = []
    if "word/comments.xml" in zf.namelist():
        croot = etree.fromstring(zf.read("word/comments.xml"))
        for c in croot.iter(q("comment")):
            comments.append({"id": c.get(q("id")), "author": c.get(q("author")), "text": text_of(c, ("t",))[:300]})
    settings_on = False
    if "word/settings.xml" in zf.namelist():
        settings_on = etree.fromstring(zf.read("word/settings.xml")).find(q("trackRevisions")) is not None
    authors = sorted({i["author"] for i in items if i.get("author")})
    return {"track_changes_on": settings_on, "count": len(items), "authors": authors, "changes": items,
            "unsupported": other, "comments": comments}


def strip_comments(root):
    n = 0
    for tag in ("commentRangeStart", "commentRangeEnd"):
        for el in list(root.iter(q(tag))):
            remove(el)
            n += 1
    for ref in list(root.iter(q("commentReference"))):
        r = ref.getparent()
        if r is not None and r.tag == q("r"):
            remove(r)
    return n


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("input")
    g = ap.add_mutually_exclusive_group()
    g.add_argument("--list", action="store_true")
    g.add_argument("--accept-all", action="store_true")
    g.add_argument("--reject-all", action="store_true")
    ap.add_argument("--enable-tracking", action="store_true")
    ap.add_argument("--disable-tracking", action="store_true")
    ap.add_argument("--strip-comments", action="store_true")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    args = ap.parse_args(argv)

    zf = zipfile.ZipFile(args.input)
    if args.list or not (args.accept_all or args.reject_all or args.enable_tracking
                         or args.disable_tracking or args.strip_comments):
        print(json.dumps(list_changes(zf), ensure_ascii=False, indent=2))
        return 0
    if not (args.out or args.inplace):
        ap.error("give --out OUT.docx or --inplace")

    new_parts, totals = {}, {}
    for name in zf.namelist():
        if PART_RE.match(name) or (name == "word/settings.xml" and (args.enable_tracking or args.disable_tracking)):
            root = etree.fromstring(zf.read(name))
            if name == "word/settings.xml":
                existing = root.find(q("trackRevisions"))
                if args.disable_tracking and existing is not None:
                    root.remove(existing)
                if args.enable_tracking and existing is None:
                    root.insert(0, etree.Element(q("trackRevisions")))
            else:
                if args.accept_all or args.reject_all:
                    for k, v in process(root, "accept" if args.accept_all else "reject").items():
                        totals[k] = totals.get(k, 0) + v
                if args.strip_comments and name != "word/comments.xml":
                    totals["comment_ranges_removed"] = totals.get("comment_ranges_removed", 0) + strip_comments(root)
            new_parts[name] = etree.tostring(root, xml_declaration=True, encoding="UTF-8", standalone=True)

    out_path = args.input if args.inplace else args.out
    fd, tmp = tempfile.mkstemp(suffix=".docx", dir=os.path.dirname(os.path.abspath(out_path)) or ".")
    os.close(fd)
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zout:
        for item in zf.infolist():
            if args.strip_comments and item.filename == "word/comments.xml":
                # keep the part (relationships point to it) but empty it
                root = etree.fromstring(zf.read(item.filename))
                for c in list(root):
                    root.remove(c)
                zout.writestr(item, etree.tostring(root, xml_declaration=True, encoding="UTF-8", standalone=True))
                continue
            zout.writestr(item, new_parts.get(item.filename, zf.read(item.filename)))
    zf.close()
    shutil.move(tmp, out_path)
    remaining = list_changes(zipfile.ZipFile(out_path))
    print(json.dumps({"out": out_path, "processed": totals, "remaining_changes": remaining["count"],
                      "track_changes_on": remaining["track_changes_on"]}, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
