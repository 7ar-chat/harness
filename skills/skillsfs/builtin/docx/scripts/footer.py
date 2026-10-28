#!/usr/bin/env python3
"""Set (or repair) the page footer of a .docx: ONE paragraph "Title · Page X of Y" on every
section ("Titre · Page X sur Y" in French). Everything already in the footer is removed
first, so it can never end up with a second page number or a number glued to the title
("...Energy2").

Usage:
  python3 footer.py IN.docx --text "Annual report" --locale en --inplace
  python3 footer.py IN.docx --out OUT.docx                  # text = document title (core properties)
  python3 footer.py IN.docx --text "Note" --header "Internal" --inplace

In a python-docx script:
  import sys; sys.path.insert(0, "/skills/docx/scripts")
  from footer import set_footer
  set_footer(doc, "Annual report", "en")

Prints {"out": ..., "sections": n, "footer": "Annual report · Page {PAGE} of {NUMPAGES}"}.
"""
from __future__ import annotations

import argparse
import json
import sys

LABELS = {"en": ("Page", "of"), "fr": ("Page", "sur")}
GREY, RULE = "8C96A3", "D5DAE1"


def _field(paragraph, instr):
    """Append a complete PAGE / NUMPAGES field (begin, instr, separate, placeholder, end)."""
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn

    runs = []
    for kind in ("begin", "instr", "separate", "text", "end"):
        r = paragraph.add_run()
        if kind == "instr":
            it = OxmlElement("w:instrText")
            it.set(qn("xml:space"), "preserve")
            it.text = f" {instr} "
            r._r.append(it)
        elif kind == "text":
            r.text = "1"
        else:
            fc = OxmlElement("w:fldChar")
            fc.set(qn("w:fldCharType"), kind)
            r._r.append(fc)
        runs.append(r)
    return runs


def _clear(part):
    """Remove every paragraph/table of a header/footer and return one empty paragraph."""
    el = part._element
    for child in list(el):
        el.remove(child)
    return part.add_paragraph()


def footer_label(text, locale="fr"):
    page, of = LABELS.get((locale or "fr")[:2].lower(), LABELS["en"])
    prefix = f"{text.strip()} · " if text and text.strip() else ""
    return prefix, page, of


def set_footer(doc, text="", locale="fr", header=None, size_pt=8, align="right", rule=True):
    """Replace the footer of every section with one paragraph: "<text> · Page X of Y"."""
    from docx.enum.text import WD_ALIGN_PARAGRAPH
    from docx.oxml.ns import qn
    from docx.shared import Pt, RGBColor

    prefix, page, of = footer_label(text, locale)
    aligns = {"left": WD_ALIGN_PARAGRAPH.LEFT, "center": WD_ALIGN_PARAGRAPH.CENTER, "right": WD_ALIGN_PARAGRAPH.RIGHT}
    for section in doc.sections:
        section.footer.is_linked_to_previous = False
        p = _clear(section.footer)
        p.alignment = aligns[align]
        if rule:
            ppr = p._p.get_or_add_pPr()
            pbdr = ppr.makeelement(qn("w:pBdr"), {})
            pbdr.append(pbdr.makeelement(qn("w:top"), {qn("w:val"): "single", qn("w:sz"): "4",
                                                       qn("w:space"): "4", qn("w:color"): RULE}))
            ppr.append(pbdr)
        runs = [p.add_run(f"{prefix}{page} ")]
        runs += _field(p, "PAGE")
        runs.append(p.add_run(f" {of} "))
        runs += _field(p, "NUMPAGES")
        for r in runs:
            r.font.size = Pt(size_pt)
            r.font.color.rgb = RGBColor.from_string(GREY)
        if header:
            section.header.is_linked_to_previous = False
            hp = _clear(section.header)
            hp.alignment = WD_ALIGN_PARAGRAPH.RIGHT
            hr = hp.add_run(header)
            hr.font.size = Pt(size_pt)
            hr.font.color.rgb = RGBColor.from_string(GREY)
    return f"{prefix}{page} {{PAGE}} {of} {{NUMPAGES}}"


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--text", help="text before the page number (default: the document title)")
    ap.add_argument("--locale", default="fr", help="fr (Page X sur Y) | en (Page X of Y)")
    ap.add_argument("--header", help="optional header text (right-aligned)")
    ap.add_argument("--align", default="right", choices=["left", "center", "right"])
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    args = ap.parse_args(argv)
    if not (args.out or args.inplace):
        ap.error("--out OUT.docx or --inplace is required")
    try:
        import docx
        doc = docx.Document(args.file)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": f"cannot open {args.file}: {exc}"}))
        return 1
    text = args.text if args.text is not None else (doc.core_properties.title or "")
    label = set_footer(doc, text, args.locale, args.header, align=args.align)
    out = args.file if args.inplace else args.out
    doc.save(out)
    print(json.dumps({"out": out, "sections": len(doc.sections), "footer": label}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
