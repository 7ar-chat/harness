#!/usr/bin/env python3
"""Find/replace text in a .pptx across split runs, keeping the formatting of the run
where each match starts. Covers text boxes, placeholders, tables, grouped shapes and
SmartArt-free shapes on slides; optionally speaker notes and layouts/masters.
Everything else in the package is copied byte for byte.

Usage:
  python3 replace_text.py DECK.pptx --out NEW.pptx --find "Q2" --replace "Q3"
  python3 replace_text.py DECK.pptx --inplace --pairs pairs.json      # {"old": "new"} or [["old","new"]]
  python3 replace_text.py DECK.pptx --out NEW.pptx --find "{{client}}" --replace "Contoso" --slides 1,4
  python3 replace_text.py DECK.pptx --dry-run --find "20\\d\\d" --replace "2026" --regex --notes

Output (JSON): {"out", "total", "counts", "per_slide", "not_found"}. Use --strict to exit 3
when a pattern matched nothing. Chart titles/labels live in chart parts: edit them with
python-pptx (chart.chart_title, chart.replace_data).
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

A = "http://schemas.openxmlformats.org/drawingml/2006/main"


def q(tag):
    return "{%s}%s" % (A, tag)


def text_nodes(p):
    return [t for t in p.iter(q("t")) if t.getparent().tag == q("r")]


def locate(ts, pos):
    acc = 0
    for i, t in enumerate(ts):
        n = len(t.text or "")
        if pos < acc + n or (pos == acc + n and i == len(ts) - 1):
            return i, pos - acc
        acc += n
    return len(ts) - 1, len(ts[-1].text or "")


def replace_in_paragraph(p, pattern, repl, regex):
    count, pos = 0, 0
    while True:
        ts = text_nodes(p)
        if not ts:
            return count
        full = "".join(t.text or "" for t in ts)
        m = pattern.search(full, pos)
        if not m or m.end() == m.start():
            return count
        new = m.expand(repl) if regex else repl
        i, oi = locate(ts, m.start())
        j, oj = locate(ts, m.end())
        if i == j:
            s = ts[i].text or ""
            ts[i].text = s[:oi] + new + s[oj:]
        else:
            ts[i].text = (ts[i].text or "")[:oi] + new
            for k in range(i + 1, j):
                ts[k].text = ""
            ts[j].text = (ts[j].text or "")[oj:]
        count += 1
        pos = m.start() + len(new)


def slide_order(zf):
    """Map slide part name -> 1-based slide number (presentation order)."""
    pres = etree.fromstring(zf.read("ppt/presentation.xml"))
    rels = etree.fromstring(zf.read("ppt/_rels/presentation.xml.rels"))
    rid_target = {r.get("Id"): r.get("Target") for r in rels}
    ns = {"p": "http://schemas.openxmlformats.org/presentationml/2006/main",
          "r": "http://schemas.openxmlformats.org/officeDocument/2006/relationships"}
    order = {}
    for n, sld in enumerate(pres.findall(".//p:sldIdLst/p:sldId", ns), 1):
        target = rid_target.get(sld.get("{%s}id" % ns["r"]), "")
        part = target.lstrip("/") if target.startswith("/") else "ppt/" + target
        order[part] = n
    # notes slide -> slide number via the notes slide rels
    for name in zf.namelist():
        m = re.match(r"ppt/notesSlides/_rels/(notesSlide\d+\.xml)\.rels$", name)
        if m:
            for r in etree.fromstring(zf.read(name)):
                if r.get("Type", "").endswith("/slide"):
                    target = "ppt/slides/" + os.path.basename(r.get("Target"))
                    if target in order:
                        order["ppt/notesSlides/" + m.group(1)] = order[target]
    return order


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("input")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--find", action="append", default=[])
    ap.add_argument("--replace", action="append", default=[])
    ap.add_argument("--pairs")
    ap.add_argument("--regex", action="store_true")
    ap.add_argument("--ignore-case", action="store_true")
    ap.add_argument("--slides", help="1-based slide numbers, comma-separated")
    ap.add_argument("--notes", action="store_true", help="also replace in speaker notes")
    ap.add_argument("--masters", action="store_true", help="also replace in layouts and masters")
    ap.add_argument("--strict", action="store_true")
    args = ap.parse_args(argv)
    if not (args.out or args.inplace or args.dry_run):
        ap.error("give --out NEW.pptx, --inplace or --dry-run")
    pairs = []
    if args.pairs:
        data = json.load(open(args.pairs, encoding="utf-8"))
        pairs += list(data.items()) if isinstance(data, dict) else [tuple(x) for x in data]
    if len(args.find) != len(args.replace):
        ap.error("--find and --replace must be given the same number of times")
    pairs += list(zip(args.find, args.replace))
    if not pairs:
        ap.error("nothing to replace")
    flags = re.IGNORECASE if args.ignore_case else 0
    compiled = [(re.compile(f if args.regex else re.escape(f), flags), r, f) for f, r in pairs]
    wanted = {int(x) for x in args.slides.split(",") if x.strip()} if args.slides else None

    zin = zipfile.ZipFile(args.input)
    order = slide_order(zin)
    counts = {f: 0 for _, _, f in compiled}
    per_slide = {}
    new_parts = {}
    for name in zin.namelist():
        is_slide = re.match(r"ppt/slides/slide\d+\.xml$", name)
        is_notes = args.notes and re.match(r"ppt/notesSlides/notesSlide\d+\.xml$", name)
        is_master = args.masters and re.match(r"ppt/(slideLayouts/slideLayout|slideMasters/slideMaster)\d+\.xml$", name)
        if not (is_slide or is_notes or is_master):
            continue
        num = order.get(name)
        if wanted and (is_slide or is_notes) and num not in wanted:
            continue
        root = etree.fromstring(zin.read(name))
        changed = 0
        for p in root.iter(q("p")):
            for pat, repl, key in compiled:
                n = replace_in_paragraph(p, pat, repl, args.regex)
                counts[key] += n
                changed += n
        if changed:
            label = str(num) if num else os.path.basename(name)
            per_slide[label] = per_slide.get(label, 0) + changed
            new_parts[name] = etree.tostring(root, xml_declaration=True, encoding="UTF-8", standalone=True)
    out = None
    if not args.dry_run:
        out = args.input if args.inplace else args.out
        fd, tmp = tempfile.mkstemp(suffix=".pptx", dir=os.path.dirname(os.path.abspath(out)) or ".")
        os.close(fd)
        with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zout:
            for item in zin.infolist():
                zout.writestr(item, new_parts.get(item.filename, zin.read(item.filename)))
        zin.close()
        shutil.move(tmp, out)
    not_found = [k for k, v in counts.items() if v == 0]
    print(json.dumps({"out": out, "total": sum(counts.values()), "counts": counts, "per_slide": per_slide,
                      "not_found": not_found}, indent=2, ensure_ascii=False))
    if not_found:
        print("warning: no match for " + ", ".join(map(repr, not_found)), file=sys.stderr)
        if args.strict:
            return 3
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
