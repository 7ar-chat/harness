#!/usr/bin/env python3
"""Find/replace text in a .docx even when Word split the text across several runs.
Keeps the formatting of the run where each match starts. Covers body, tables, text boxes,
headers, footers, footnotes, endnotes and comments. Every other part of the package is
copied byte for byte.

Usage:
  python3 replace.py IN.docx --out OUT.docx --find "2025" --replace "2026"
  python3 replace.py IN.docx --out OUT.docx --pairs pairs.json      # {"old": "new", ...} or [["old","new"], ...]
  python3 replace.py IN.docx --inplace --find "ACME" --replace "Contoso" --track --author "Studio"
  python3 replace.py IN.docx --dry-run --find "\\bQ(\\d)\\b" --replace "T\\1" --regex

Options:
  --regex          patterns are Python regular expressions (replacement may use \\1, \\g<name>)
  --ignore-case    case-insensitive matching
  --track          record each replacement as a tracked change (w:del + w:ins) by --author
  --strict         exit 3 if any pattern matched nothing
Output (stdout, JSON): {"out": path, "total": N, "counts": {pattern: n}, "not_found": [...]}.
Limits: matches never span paragraphs; tabs/line breaks inside a run are not part of the
searchable text; deleted (tracked) text is never matched.
"""
from __future__ import annotations

import argparse
import copy
import datetime as dt
import json
import os
import re
import shutil
import sys
import tempfile
import zipfile

from lxml import etree

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
XML_SPACE = "{http://www.w3.org/XML/1998/namespace}space"
PART_RE = re.compile(r"^word/(document|header\d*|footer\d*|footnotes|endnotes|comments)\.xml$")


def q(tag: str) -> str:
    return "{%s}%s" % (W, tag)


def owning_paragraph(el):
    node = el.getparent()
    while node is not None and node.tag != q("p"):
        node = node.getparent()
    return node


def text_nodes(p):
    """w:t elements whose nearest paragraph is p (excludes nested text-box paragraphs)."""
    return [t for t in p.iter(q("t")) if owning_paragraph(t) is p and t.getparent().tag == q("r")]


def locate(ts, pos):
    """Map a character offset of the joined text to (t index, offset inside t)."""
    acc = 0
    for i, t in enumerate(ts):
        n = len(t.text or "")
        if pos < acc + n or (pos == acc + n and i == len(ts) - 1):
            return i, pos - acc
        acc += n
    return len(ts) - 1, len(ts[-1].text or "")


def set_text(t, value):
    t.text = value
    t.set(XML_SPACE, "preserve")


def split_run_at(t, offset):
    """Split the run containing w:t `t` so that a new run starts at t.text[offset:]."""
    r = t.getparent()
    text = t.text or ""
    content = [c for c in r if c.tag != q("rPr")]
    idx = content.index(t)
    if offset == 0 and idx == 0:
        return  # already at the start of the run
    if offset >= len(text) and idx == len(content) - 1:
        return  # nothing after the split point in this run
    r2 = copy.deepcopy(r)
    content2 = [c for c in r2 if c.tag != q("rPr")]
    # r keeps content[:idx] + t[:offset]
    for c in content[idx + 1:]:
        r.remove(c)
    if offset > 0:
        set_text(t, text[:offset])
    else:
        r.remove(t)
    # r2 keeps t[offset:] + content[idx+1:]
    for c in content2[:idx]:
        r2.remove(c)
    t2 = content2[idx]
    if offset < len(text):
        set_text(t2, text[offset:])
    else:
        r2.remove(t2)
    r.addnext(r2)
    if not [c for c in r if c.tag != q("rPr")]:
        r.getparent().remove(r)


class Ids:
    def __init__(self, start):
        self.n = start

    def next(self):
        self.n += 1
        return str(self.n)


def replace_in_paragraph(p, pattern, repl, regex, track, author, date, ids):
    count = 0
    pos = 0
    while True:
        ts = text_nodes(p)
        if not ts:
            return count
        full = "".join(t.text or "" for t in ts)
        m = pattern.search(full, pos)
        if not m or m.end() == m.start():
            return count
        start, end = m.start(), m.end()
        new_text = m.expand(repl) if regex else repl
        if not track:
            i, oi = locate(ts, start)
            j, oj = locate(ts, end)
            if i == j:
                s = ts[i].text or ""
                set_text(ts[i], s[:oi] + new_text + s[oj:])
            else:
                set_text(ts[i], (ts[i].text or "")[:oi] + new_text)
                for k in range(i + 1, j):
                    set_text(ts[k], "")
                set_text(ts[j], (ts[j].text or "")[oj:])
        else:
            j, oj = locate(ts, end)
            split_run_at(ts[j], oj)
            ts = text_nodes(p)
            i, oi = locate(ts, start)
            split_run_at(ts[i], oi)
            ts = text_nodes(p)
            # runs fully covering [start, end)
            runs, acc = [], 0
            for t in ts:
                n = len(t.text or "")
                if acc >= start and acc + n <= end and n > 0:
                    r = t.getparent()
                    if r not in runs:
                        runs.append(r)
                acc += n
            if not runs:
                pos = end
                continue
            first_rpr = runs[0].find(q("rPr"))
            for r in runs:
                d = etree.Element(q("del"), {q("id"): ids.next(), q("author"): author, q("date"): date})
                r.addprevious(d)
                d.append(r)
                for t in r.findall(q("t")):
                    t.tag = q("delText")
                for it in r.findall(q("instrText")):
                    it.tag = q("delInstrText")
            if new_text:
                ins = etree.Element(q("ins"), {q("id"): ids.next(), q("author"): author, q("date"): date})
                nr = etree.SubElement(ins, q("r"))
                if first_rpr is not None:
                    nr.append(copy.deepcopy(first_rpr))
                nt = etree.SubElement(nr, q("t"))
                set_text(nt, new_text)
                runs[-1].getparent().addnext(ins)
        count += 1
        pos = start + len(new_text)


def load_pairs(args):
    pairs = []
    if args.pairs:
        with open(args.pairs, encoding="utf-8") as fh:
            data = json.load(fh)
        pairs += list(data.items()) if isinstance(data, dict) else [tuple(x) for x in data]
    if len(args.find) != len(args.replace):
        raise SystemExit("error: --find and --replace must be given the same number of times")
    pairs += list(zip(args.find, args.replace))
    if not pairs:
        raise SystemExit("error: nothing to replace (use --find/--replace or --pairs)")
    return pairs


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("input")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    ap.add_argument("--find", action="append", default=[])
    ap.add_argument("--replace", action="append", default=[])
    ap.add_argument("--pairs")
    ap.add_argument("--regex", action="store_true")
    ap.add_argument("--ignore-case", action="store_true")
    ap.add_argument("--track", action="store_true")
    ap.add_argument("--author", default="Studio")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--strict", action="store_true")
    args = ap.parse_args(argv)

    if not (args.out or args.inplace or args.dry_run):
        ap.error("give --out OUT.docx, --inplace or --dry-run")
    pairs = load_pairs(args)
    flags = re.IGNORECASE if args.ignore_case else 0
    compiled = [(re.compile(f if args.regex else re.escape(f), flags), r, f) for f, r in pairs]
    date = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")

    zin = zipfile.ZipFile(args.input)
    counts = {f: 0 for _, _, f in compiled}
    new_parts = {}
    for name in zin.namelist():
        if not PART_RE.match(name):
            continue
        root = etree.fromstring(zin.read(name))
        max_id = max([int(v) for v in root.xpath("//@w:id", namespaces={"w": W}) if v.lstrip("-").isdigit()] or [0])
        ids = Ids(max(max_id, 0) + 1000)
        changed = False
        for p in list(root.iter(q("p"))):
            for pat, repl, key in compiled:
                n = replace_in_paragraph(p, pat, repl, args.regex, args.track, args.author, date, ids)
                if n:
                    counts[key] += n
                    changed = True
        if changed:
            new_parts[name] = etree.tostring(root, xml_declaration=True, encoding="UTF-8", standalone=True)

    total = sum(counts.values())
    not_found = [k for k, v in counts.items() if v == 0]
    out_path = None
    if not args.dry_run and new_parts:
        out_path = args.input if args.inplace else args.out
        fd, tmp = tempfile.mkstemp(suffix=".docx", dir=os.path.dirname(os.path.abspath(out_path)) or ".")
        os.close(fd)
        with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zout:
            for item in zin.infolist():
                data = new_parts.get(item.filename)
                if data is None:
                    data = zin.read(item.filename)
                zout.writestr(item, data)
        zin.close()
        shutil.move(tmp, out_path)
    elif not args.dry_run and args.out and not args.inplace:
        zin.close()
        shutil.copyfile(args.input, args.out)
        out_path = args.out
    result = {"out": out_path, "total": total, "counts": counts, "not_found": not_found,
              "tracked": bool(args.track)}
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if not_found:
        print("warning: no match for: " + ", ".join(repr(x) for x in not_found)
              + " (check spelling, typographic quotes ’ “ ”, non-breaking spaces; inspect_docx.py --text)",
              file=sys.stderr)
        if args.strict:
            return 3
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
