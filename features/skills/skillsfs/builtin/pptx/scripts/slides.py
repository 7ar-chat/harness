#!/usr/bin/env python3
"""Reorganise slides of a .pptx with python-pptx: list, duplicate, delete, move, reorder,
keep a subset. Slide numbers are 1-based. Duplicates copy shapes, pictures, charts (as an
independent chart part), hyperlinks and speaker notes.

Usage:
  python3 slides.py DECK.pptx list
  python3 slides.py DECK.pptx duplicate 3 --out NEW.pptx          # copy inserted right after 3
  python3 slides.py DECK.pptx duplicate 3 --to 8 --inplace         # copy placed at position 8
  python3 slides.py DECK.pptx delete 2,5 --inplace
  python3 slides.py DECK.pptx move 6 2 --inplace                   # slide 6 becomes slide 2
  python3 slides.py DECK.pptx reorder 1,3,2,4 --inplace            # full new order (all slides)
  python3 slides.py DECK.pptx keep 1,2,7 --out SHORT.pptx          # drop all other slides

Prints the resulting outline as JSON: {"out", "slides": [{"slide", "title"}]}.
"""
from __future__ import annotations

import argparse
import copy
import json
import sys

R_NS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"


def parse_list(text, n_slides):
    nums = [int(x) for x in str(text).replace(" ", "").split(",") if x]
    for x in nums:
        if not 1 <= x <= n_slides:
            raise SystemExit(f"error: slide {x} out of range 1..{n_slides}")
    return nums


def sld_ids(prs):
    return prs.slides._sldIdLst


def delete_slides(prs, numbers):
    lst = sld_ids(prs)
    items = list(lst)
    for n in sorted(set(numbers), reverse=True):
        sld = items[n - 1]
        prs.part.drop_rel(sld.rId)
        lst.remove(sld)


def move_slide(prs, src, dst):
    lst = sld_ids(prs)
    items = list(lst)
    el = items[src - 1]
    lst.remove(el)
    lst.insert(dst - 1, el)


def duplicate_slide(prs, number, to=None):
    from pptx.opc.constants import RELATIONSHIP_TYPE as RT
    from pptx.opc.packuri import PackURI
    from pptx.parts.chart import ChartPart
    from pptx.parts.embeddedpackage import EmbeddedXlsxPart

    src = prs.slides[number - 1]
    new = prs.slides.add_slide(src.slide_layout)
    # drop the placeholders created from the layout, then copy the source shape tree
    for shp in list(new.shapes):
        shp._element.getparent().remove(shp._element)
    rid_map = {}
    for rid, rel in src.part.rels.items():
        if rel.reltype in (RT.SLIDE_LAYOUT, RT.NOTES_SLIDE):
            continue
        if rel.is_external:
            rid_map[rid] = new.part.relate_to(rel.target_ref, rel.reltype, is_external=True)
        elif rel.reltype == RT.CHART:
            old = rel.target_part
            partname = prs.part.package.next_partname("/ppt/charts/chart%d.xml")
            chart = ChartPart.load(PackURI(partname), old.content_type, prs.part.package, old.blob)
            id_map = {}
            for crid, crel in old.rels.items():
                if crel.is_external:
                    id_map[crid] = chart.relate_to(crel.target_ref, crel.reltype, is_external=True)
                elif crel.reltype == RT.PACKAGE:
                    xlsx = EmbeddedXlsxPart.new(crel.target_part.blob, prs.part.package)
                    id_map[crid] = chart.relate_to(xlsx, crel.reltype)
                else:
                    id_map[crid] = chart.relate_to(crel.target_part, crel.reltype)
            for node in chart._element.iter():
                for attr in list(node.attrib):
                    if attr.startswith("{%s}" % R_NS) and node.attrib[attr] in id_map:
                        node.attrib[attr] = id_map[node.attrib[attr]]
            rid_map[rid] = new.part.relate_to(chart, rel.reltype)
        else:
            rid_map[rid] = new.part.relate_to(rel.target_part, rel.reltype)
    sp_tree = new.shapes._spTree
    for el in src.shapes._spTree:
        tag = el.tag.split("}")[-1]
        if tag in ("nvGrpSpPr", "grpSpPr"):
            continue
        clone = copy.deepcopy(el)
        for node in clone.iter():
            for attr in list(node.attrib):
                if attr.startswith("{%s}" % R_NS) and node.attrib[attr] in rid_map:
                    node.attrib[attr] = rid_map[node.attrib[attr]]
        sp_tree.append(clone)
    # background
    src_bg = src._element.cSld.bg
    if src_bg is not None:
        new._element.cSld.insert(0, copy.deepcopy(src_bg))
    if src.has_notes_slide:
        text = src.notes_slide.notes_text_frame.text
        if text:
            new.notes_slide.notes_text_frame.text = text
    move_slide(prs, len(prs.slides), to or number + 1)
    return new


def outline(prs):
    out = []
    for i, s in enumerate(prs.slides, 1):
        title = ""
        try:
            if s.shapes.title is not None:
                title = s.shapes.title.text
        except Exception:  # noqa: BLE001
            pass
        if not title:
            for sh in s.shapes:
                if sh.has_text_frame and sh.text_frame.text.strip():
                    title = sh.text_frame.text.strip().split("\n")[0]
                    break
        out.append({"slide": i, "layout": s.slide_layout.name, "title": title[:120]})
    return out


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("action", choices=["list", "duplicate", "delete", "move", "reorder", "keep"])
    ap.add_argument("args", nargs="*")
    ap.add_argument("--to", type=int, help="target position for duplicate")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    a = ap.parse_args(argv)
    from pptx import Presentation

    prs = Presentation(a.file)
    n = len(prs.slides)
    if a.action != "list" and not (a.out or a.inplace):
        ap.error("give --out NEW.pptx or --inplace")
    if a.action == "duplicate":
        (num,) = parse_list(a.args[0], n)
        duplicate_slide(prs, num, a.to)
    elif a.action == "delete":
        nums = parse_list(a.args[0], n)
        if len(set(nums)) >= n:
            raise SystemExit("error: refusing to delete every slide")
        delete_slides(prs, nums)
    elif a.action == "move":
        src, dst = parse_list(a.args[0], n)[0], parse_list(a.args[1], n)[0]
        move_slide(prs, src, dst)
    elif a.action == "reorder":
        order = parse_list(a.args[0], n)
        if sorted(order) != list(range(1, n + 1)):
            raise SystemExit(f"error: reorder needs every slide 1..{n} exactly once (use keep/delete to drop)")
        lst = sld_ids(prs)
        items = list(lst)
        for el in items:
            lst.remove(el)
        for x in order:
            lst.append(items[x - 1])
    elif a.action == "keep":
        keep = set(parse_list(a.args[0], n))
        delete_slides(prs, [x for x in range(1, n + 1) if x not in keep])
    out = None
    if a.action != "list":
        out = a.file if a.inplace else a.out
        prs.save(out)
        prs = Presentation(out)
    print(json.dumps({"out": out, "slides": outline(prs)}, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
