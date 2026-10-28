#!/usr/bin/env python3
"""Summarise a .pptx: slide size, layouts (with their placeholders), and per slide the
layout, shapes (kind, name, placeholder idx, position/size in inches, text), pictures,
tables, charts and speaker notes.

Usage:
  python3 inspect_pptx.py DECK.pptx               # JSON
  python3 inspect_pptx.py DECK.pptx --layouts     # only the layouts of the template
  python3 inspect_pptx.py DECK.pptx --outline     # one line per slide: index, layout, title
  python3 inspect_pptx.py --check DECK.pptx       # the deliverable checks (same as fit_text.py DECK.pptx):
                                                  # {"ok", "file", "checks", "next_step", ...}, exit 0/2/1

Slide numbers in the output are 1-based (as shown in PowerPoint); python-pptx lists are
0-based (`prs.slides[n - 1]`).
"""
from __future__ import annotations

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

EMU_PER_IN = 914400


def inches(v):
    return None if v is None else round(v / EMU_PER_IN, 2)


def shape_kind(sh):
    if sh.is_placeholder:
        return "placeholder"
    if sh.has_text_frame and sh.shape_type is not None and int(sh.shape_type) == 17:
        return "textbox"
    if getattr(sh, "has_chart", False) and sh.has_chart:
        return "chart"
    if getattr(sh, "has_table", False) and sh.has_table:
        return "table"
    if sh.shape_type is not None and int(sh.shape_type) == 13:
        return "picture"
    if sh.shape_type is not None and int(sh.shape_type) == 6:
        return "group"
    return "shape"


def font_sizes(tf):
    sizes = set()
    for p in tf.paragraphs:
        for r in p.runs:
            if r.font.size:
                sizes.add(r.font.size.pt)
    return sorted(sizes)


def describe_shape(sh, depth=0):
    d = {"id": sh.shape_id, "name": sh.name, "kind": shape_kind(sh),
         "left": inches(sh.left), "top": inches(sh.top), "width": inches(sh.width), "height": inches(sh.height)}
    if sh.is_placeholder:
        pf = sh.placeholder_format
        d["placeholder"] = {"idx": pf.idx, "type": str(pf.type).split(".")[-1].split(" ")[0]}
    if sh.has_text_frame and sh.text_frame.text.strip():
        d["text"] = sh.text_frame.text[:500]
        d["paragraphs"] = len(sh.text_frame.paragraphs)
        fs = font_sizes(sh.text_frame)
        if fs:
            d["font_sizes_pt"] = fs
    if d["kind"] == "table":
        tbl = sh.table
        d["table"] = {"rows": len(tbl.rows), "cols": len(tbl.columns),
                      "first_row": [c.text[:40] for c in tbl.rows[0].cells] if len(tbl.rows) else []}
    if d["kind"] == "chart":
        ch = sh.chart
        d["chart"] = {"type": str(ch.chart_type).split(".")[-1].split(" ")[0],
                      "series": [s.name for s in ch.plots[0].series] if len(ch.plots) else [],
                      "title": ch.chart_title.text_frame.text if ch.has_title and ch.chart_title.has_text_frame else None}
    if d["kind"] == "picture":
        try:
            d["image"] = {"content_type": sh.image.content_type, "px": list(sh.image.size)}
        except Exception:  # noqa: BLE001
            pass
    if d["kind"] == "group" and depth < 3:
        d["children"] = [describe_shape(c, depth + 1) for c in sh.shapes]
    return d


def layouts_info(prs):
    out = []
    for m_idx, master in enumerate(prs.slide_masters):
        for l_idx, layout in enumerate(master.slide_layouts):
            out.append({"master": m_idx, "index": l_idx, "name": layout.name,
                        "placeholders": [{"idx": ph.placeholder_format.idx,
                                          "type": str(ph.placeholder_format.type).split(".")[-1].split(" ")[0],
                                          "name": ph.name, "left": inches(ph.left), "top": inches(ph.top),
                                          "width": inches(ph.width), "height": inches(ph.height)}
                                         for ph in layout.placeholders]})
    return out


def slide_title(slide):
    try:
        if slide.shapes.title is not None and slide.shapes.title.text.strip():
            return slide.shapes.title.text.strip()
    except Exception:  # noqa: BLE001
        pass
    for sh in slide.shapes:
        if sh.has_text_frame and sh.text_frame.text.strip():
            return sh.text_frame.text.strip().split("\n")[0][:120]
    return ""


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1 (argparse would exit 2)
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--check", action="store_true", help="deliverable checks (exit 0 ok, 2 not ok, 1 usage error)")
    ap.add_argument("--layouts", action="store_true")
    ap.add_argument("--outline", action="store_true")
    args = ap.parse_args(argv)
    args.file = os.path.abspath(args.file)  # the checker contract names the file by its absolute path
    if not os.path.isfile(args.file):
        print(json.dumps({"ok": False, "file": args.file, "error": f"no such file: {args.file}"}))
        return 1
    if args.check:
        import fit_text  # noqa: PLC0415

        report, code = fit_text.check_deck(args.file)
        print(json.dumps(report, indent=2, ensure_ascii=False))
        return code
    try:
        from pptx import Presentation
        prs = Presentation(args.file)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": f"cannot open {args.file}: {exc}"}))
        return 1

    if args.outline:
        for i, s in enumerate(prs.slides, 1):
            notes = " [notes]" if s.has_notes_slide and s.notes_slide.notes_text_frame.text.strip() else ""
            print(f"{i:>3}  ({s.slide_layout.name})  {slide_title(s)}{notes}")
        return 0
    if args.layouts:
        print(json.dumps(layouts_info(prs), indent=2, ensure_ascii=False))
        return 0

    slides = []
    for i, s in enumerate(prs.slides, 1):
        notes = s.notes_slide.notes_text_frame.text if s.has_notes_slide else ""
        slides.append({"slide": i, "layout": s.slide_layout.name, "title": slide_title(s),
                       "shapes": [describe_shape(sh) for sh in s.shapes], "notes": notes[:1000]})
    out = {"file": args.file, "slide_width_in": inches(prs.slide_width), "slide_height_in": inches(prs.slide_height),
           "aspect": "16:9" if abs(prs.slide_width / prs.slide_height - 16 / 9) < 0.01 else
           "4:3" if abs(prs.slide_width / prs.slide_height - 4 / 3) < 0.01 else "other",
           "slides_count": len(slides), "layouts": [{"index": l["index"], "name": l["name"]} for l in layouts_info(prs)],
           "slides": slides}
    print(json.dumps(out, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
