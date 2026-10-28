#!/usr/bin/env python3
"""Build a PDF report directly with reportlab (platypus) from a JSON spec - the same block
format as docx/scripts/new_document.py, so one spec can produce both a .docx and a .pdf.

Usage:
  python3 build_pdf.py --spec spec.json --out /workspace/outputs/report.pdf
  python3 build_pdf.py --example

Spec keys: locale (fr|en), title, subtitle, author, date ("2026-10-06", "14 octobre 2025", any
text: dates are written in the locale's long form, other text as is), title_page (bool), footer,
header, landscape, blocks: heading{text,level 1-3} | paragraph{text, align} |
bullets{items} | numbered{items} | table{columns, rows, widths_cm, numeric_cols, caption} |
image{path, width_cm, caption} | callout{text, kind note|warning} | toc{title} | page_break.
Every block may be written in the docx builder's shorthand too: {"heading": "Intro"},
{"heading": {"text": "Intro", "level": 2}}, {"paragraph": "Text"}, {"bullets": ["a", "b"]},
{"page_break": {}}, or a bare string (paragraph); "blocks" may be the whole spec (a JSON list).
Inline markup in text: **bold**, *italic*, `code`, [label](https://url).
Footer: footer text left, "Page x sur y" / "Page x of y" right. Headings become PDF bookmarks.
Prints {"out", "pages"}.

Page x of y in your own reportlab code:
  import sys; sys.path.insert(0, "/skills/pdf/scripts"); from build_pdf import numbered_canvas
  doc.build(story, canvasmaker=numbered_canvas("Report title", locale="en", font="StudioSans"))
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import sys
from xml.sax.saxutils import escape

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from blocks import normalize_spec  # noqa: E402  (same normalizer as docx/scripts/new_document.py)

BRAND_DIRS = ["/skills/studio-brand/scripts", os.path.join(HERE, "..", "..", "studio-brand", "scripts")]
FALLBACK = {"navy": "1B365D", "blue": "2E6DA4", "teal": "2A9D8F", "amber": "E9A23B", "ink": "1A1F2B",
            "slate": "4A5568", "grey": "8C96A3", "rule": "D5DAE1", "mist": "EEF1F5", "white": "FFFFFF"}
EXAMPLE = {"locale": "en", "title": "Monthly report", "subtitle": "September 2026", "author": "Studio",
           "footer": "Monthly report", "blocks": [
               {"type": "toc"}, {"type": "heading", "text": "Highlights", "level": 1},
               {"type": "paragraph", "text": "Activity grew **8%** over the month."},
               {"type": "bullets", "items": ["First point", "Second point"]},
               {"type": "heading", "text": "Figures", "level": 1},
               {"type": "table", "columns": ["Metric", "Value"], "rows": [["Users", "1,204"], ["Tickets", "87"]],
                "numeric_cols": [1]}]}
INLINE = re.compile(r"(\*\*.+?\*\*|\*[^*]+?\*|`[^`]+?`|\[[^\]]+?\]\([^)]+?\))")


LIBERATION = "/usr/share/fonts/truetype/liberation/"


def register_fonts():
    """(sans, mono) embedded TrueType families: Liberation Sans as "StudioSans" (+ -Bold,
    -Italic, -BoldItalic) and Liberation Mono as "StudioMono". The built-in Helvetica/Courier
    are not embedded and miss characters such as œ ’ – (extract.py --check fails on them)."""
    from reportlab.lib.fonts import addMapping
    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont

    families = {"StudioSans": "LiberationSans", "StudioMono": "LiberationMono"}
    out = []
    for name, base in families.items():
        if name not in pdfmetrics.getRegisteredFontNames():
            faces = {"": "Regular", "-Bold": "Bold", "-Italic": "Italic", "-BoldItalic": "BoldItalic"}
            if not all(os.path.isfile(f"{LIBERATION}{base}-{f}.ttf") for f in faces.values()):
                out.append("Helvetica" if name == "StudioSans" else "Courier")
                continue
            for suffix, face in faces.items():
                pdfmetrics.registerFont(TTFont(name + suffix, f"{LIBERATION}{base}-{face}.ttf"))
            addMapping(name, 0, 0, name)
            addMapping(name, 1, 0, name + "-Bold")
            addMapping(name, 0, 1, name + "-Italic")
            addMapping(name, 1, 1, name + "-BoldItalic")
        out.append(name)
    return tuple(out)


def load_brand():
    for d in BRAND_DIRS:
        if os.path.isfile(os.path.join(d, "brand.py")):
            sys.path.insert(0, os.path.abspath(d))
            import brand  # noqa: PLC0415
            return brand
    return None


def numbered_canvas(footer="", header=None, locale="en", font="Helvetica", margin=None,
                    rule_color="#D5DAE1", text_color="#8C96A3"):
    """reportlab canvas class that prints a footer rule, `footer` on the left and "Page x of y"
    ("Page x sur y" for fr) on the right of every page, plus `header` top right. It defers the
    page output to know the total: pass it as `doc.build(story, canvasmaker=numbered_canvas(...))`."""
    from reportlab.lib import colors
    from reportlab.lib.units import cm
    from reportlab.pdfgen import canvas as rl_canvas

    word = "sur" if str(locale).startswith("fr") else "of"
    edge = 2.2 * cm if margin is None else margin

    class NumberedCanvas(rl_canvas.Canvas):
        def __init__(self, *a, **k):
            super().__init__(*a, **k)
            self._saved = []

        def showPage(self):
            self._saved.append(dict(self.__dict__))
            self._startPage()

        def save(self):
            total = len(self._saved)
            for state in self._saved:
                self.__dict__.update(state)
                width, height = self._pagesize
                self.saveState()
                self.setStrokeColor(colors.HexColor(rule_color))
                self.setLineWidth(0.5)
                self.line(edge, 1.6 * cm, width - edge, 1.6 * cm)
                self.setFont(font, 8)
                self.setFillColor(colors.HexColor(text_color))
                self.drawString(edge, 1.15 * cm, (footer or "")[:120])
                self.drawRightString(width - edge, 1.15 * cm, f"Page {self._pageNumber} {word} {total}")
                if header:
                    self.drawRightString(width - edge, height - 1.2 * cm, header[:120])
                self.restoreState()
                super().showPage()
            super().save()

    return NumberedCanvas


def markup(text, mono):
    out = []
    for part in INLINE.split(text or ""):
        if not part:
            continue
        if part.startswith("**") and part.endswith("**"):
            out.append(f"<b>{escape(part[2:-2])}</b>")
        elif part.startswith("`") and part.endswith("`"):
            out.append(f'<font name="{mono}">{escape(part[1:-1])}</font>')
        elif part.startswith("[") and "](" in part:
            label, url = part[1:].split("](", 1)
            out.append(f'<a href="{escape(url[:-1])}" color="#2E6DA4"><u>{escape(label)}</u></a>')
        elif part.startswith("*") and part.endswith("*") and len(part) > 2:
            out.append(f"<i>{escape(part[1:-1])}</i>")
        else:
            out.append(escape(part))
    return "".join(out)


def build(spec, out):
    spec = normalize_spec(spec, "python3 /skills/pdf/scripts/build_pdf.py")
    from reportlab.lib import colors
    from reportlab.lib.enums import TA_CENTER, TA_JUSTIFY, TA_LEFT, TA_RIGHT
    from reportlab.lib.pagesizes import A4, landscape
    from reportlab.lib.styles import ParagraphStyle
    from reportlab.lib.units import cm
    from reportlab.platypus import (Image, KeepTogether, ListFlowable, ListItem, PageBreak, Paragraph,
                                    SimpleDocTemplate, Spacer, Table, TableStyle)
    from reportlab.platypus.tableofcontents import TableOfContents

    brand = load_brand()
    P = brand.PALETTE if brand else FALLBACK
    sans, mono = register_fonts()
    font = brand.reportlab_fonts() if brand else sans
    builtin = font == "Helvetica"
    bold = "Helvetica-Bold" if builtin else font + "-Bold"
    locale = spec.get("locale", "fr")
    labels = {"fr": ("Page", "sur", "Sommaire", "Préparé par : "), "en": ("Page", "of", "Contents", "Prepared by: ")}[
        "fr" if locale.startswith("fr") else "en"]

    def c(name):
        return colors.HexColor("#" + P[name])

    base = ParagraphStyle("Body", fontName=font, fontSize=10.5, leading=14.5, textColor=c("ink"), spaceAfter=6)
    styles = {
        "body": base,
        "title": ParagraphStyle("Title", base, fontName=bold, fontSize=26, leading=31, textColor=c("navy"), spaceAfter=8),
        "subtitle": ParagraphStyle("Subtitle", base, fontSize=14, leading=18, textColor=c("slate"), spaceAfter=18),
        "meta": ParagraphStyle("Meta", base, textColor=c("slate")),
        1: ParagraphStyle("H1", base, fontName=bold, fontSize=16, leading=20, textColor=c("navy"), spaceBefore=16, spaceAfter=6),
        2: ParagraphStyle("H2", base, fontName=bold, fontSize=13, leading=17, textColor=c("navy"), spaceBefore=12, spaceAfter=4),
        3: ParagraphStyle("H3", base, fontName=bold, fontSize=11.5, leading=15, textColor=c("blue"), spaceBefore=10, spaceAfter=3),
        "caption": ParagraphStyle("Caption", base, fontSize=9, leading=12, textColor=c("slate"), alignment=TA_CENTER,
                                  fontName="Helvetica-Oblique" if builtin else font + "-Italic"),
        "cell": ParagraphStyle("Cell", base, fontSize=9.5, leading=12, spaceAfter=0),
        "toc_title": ParagraphStyle("TocTitle", base, fontName=bold, fontSize=16, leading=20, textColor=c("navy"), spaceAfter=10),
    }
    aligns = {"left": TA_LEFT, "center": TA_CENTER, "right": TA_RIGHT, "justify": TA_JUSTIFY}
    pagesize = landscape(A4) if spec.get("landscape") else A4
    margin = 2.2 * cm
    text_width = pagesize[0] - 2 * margin
    footer_text = spec.get("footer", spec.get("title", ""))
    header_text = spec.get("header")

    NumberedCanvas = numbered_canvas(footer_text, header_text, locale, font, margin, "#" + P["rule"], "#" + P["grey"])

    class Doc(SimpleDocTemplate):
        def afterFlowable(self, flowable):
            level = getattr(flowable, "_heading_level", None)
            if level is not None:
                text = flowable.getPlainText()
                key = f"h{id(flowable)}"
                self.canv.bookmarkPage(key)
                self.canv.addOutlineEntry(text, key, level=level - 1, closed=False)
                self.notify("TOCEntry", (level - 1, text, self.page, key))

    story = []
    date = spec.get("date") or None  # any text (see brand.date_label); never parsed strictly
    if spec.get("title") and spec.get("title_page", True):
        story += [Spacer(1, 6 * cm), Paragraph(markup(spec["title"], mono), styles["title"])]
        if spec.get("subtitle"):
            story.append(Paragraph(markup(spec["subtitle"], mono), styles["subtitle"]))
        rule = Table([[""]], colWidths=[text_width], rowHeights=[2])
        rule.setStyle(TableStyle([("LINEBELOW", (0, 0), (-1, -1), 1.5, c("navy"))]))
        story += [rule, Spacer(1, 10)]
        if spec.get("author"):
            story.append(Paragraph(escape(labels[3] + spec["author"]), styles["meta"]))
        label = brand.date_label(date, locale) if brand else str(date or dt.date.today().isoformat())
        story.append(Paragraph(escape(label), styles["meta"]))
        story.append(PageBreak())

    has_toc = False
    for i, b in enumerate(spec["blocks"]):
        kind = b.get("type")
        if kind == "heading":
            level = max(1, min(3, int(b.get("level", 1))))
            p = Paragraph(markup(b["text"], mono), styles[level])
            p._heading_level = level
            story.append(p)
        elif kind == "paragraph":
            st = ParagraphStyle(f"p{i}", styles["body"], alignment=aligns.get(b.get("align", "left"), TA_LEFT))
            story.append(Paragraph(markup(b.get("text", ""), mono), st))
        elif kind in ("bullets", "numbered"):
            items = []
            for it in b.get("items", []):
                text, level = (it.get("text", ""), int(it.get("level", 0))) if isinstance(it, dict) else (it, 0)
                items.append(ListItem(Paragraph(markup(text, mono), styles["body"]), leftIndent=14 + 14 * level,
                                      bulletColor=c("teal") if kind == "bullets" else c("ink")))
            story.append(ListFlowable(items, bulletType="bullet" if kind == "bullets" else "1",
                                      start="•" if kind == "bullets" else None, bulletFontName=font,
                                      bulletFormat=None if kind == "bullets" else "%s.",
                                      bulletFontSize=10, leftIndent=14))
        elif kind == "table":
            cols = b.get("columns") or []
            rows = b.get("rows") or []
            numeric = set(b.get("numeric_cols") or [])
            head = ParagraphStyle("th", styles["cell"], fontName=bold, textColor=colors.white)
            data = []
            if cols:
                data.append([Paragraph(markup(str(x), mono), ParagraphStyle(f"th{j}", head, alignment=TA_RIGHT if j in numeric else TA_LEFT))
                             for j, x in enumerate(cols)])
            for r in rows:
                data.append([Paragraph(markup("" if v is None else str(v), mono),
                                       ParagraphStyle(f"td{j}", styles["cell"], alignment=TA_RIGHT if j in numeric else TA_LEFT))
                             for j, v in enumerate(r)])
            widths = [w * cm for w in b["widths_cm"]] if b.get("widths_cm") else None
            t = Table(data, colWidths=widths or [text_width / max(1, len(data[0]))] * len(data[0]), repeatRows=1 if cols else 0)
            st = [("LINEBELOW", (0, 0), (-1, -1), 0.4, c("rule")), ("VALIGN", (0, 0), (-1, -1), "TOP"),
                  ("TOPPADDING", (0, 0), (-1, -1), 3), ("BOTTOMPADDING", (0, 0), (-1, -1), 3)]
            if cols:
                st.append(("BACKGROUND", (0, 0), (-1, 0), c("navy")))
            for r_idx in range(1 if cols else 0, len(data)):
                if (r_idx - (1 if cols else 0)) % 2 == 1:
                    st.append(("BACKGROUND", (0, r_idx), (-1, r_idx), c("mist")))
            t.setStyle(TableStyle(st))
            story.append(t)
            if b.get("caption"):
                story.append(Paragraph(markup(b["caption"], mono), styles["caption"]))
            story.append(Spacer(1, 8))
        elif kind == "image":
            path = b["path"]
            if not os.path.isfile(path):
                raise SystemExit(f"error: image not found: {path} (block {i + 1})")
            from PIL import Image as PILImage
            with PILImage.open(path) as im:
                iw, ih = im.size
            w = min(text_width, float(b.get("width_cm", 16)) * cm)
            flow = [Image(path, width=w, height=w * ih / iw)]
            if b.get("caption"):
                flow.append(Paragraph(markup(b["caption"], mono), styles["caption"]))
            story.append(KeepTogether(flow))
            story.append(Spacer(1, 8))
        elif kind == "callout":
            warn = b.get("kind") == "warning"
            t = Table([[Paragraph(markup(b.get("text", ""), mono), styles["body"])]], colWidths=[text_width])
            t.setStyle(TableStyle([("BACKGROUND", (0, 0), (-1, -1), colors.HexColor("#FDF3E1") if warn else c("mist")),
                                   ("LINEBEFORE", (0, 0), (0, -1), 3, c("amber") if warn else c("navy")),
                                   ("LEFTPADDING", (0, 0), (-1, -1), 10), ("TOPPADDING", (0, 0), (-1, -1), 6),
                                   ("BOTTOMPADDING", (0, 0), (-1, -1), 4)]))
            story += [t, Spacer(1, 8)]
        elif kind == "toc":
            toc = TableOfContents()
            toc.dotsMinLevel = 0
            toc.levelStyles = [ParagraphStyle(f"toc{n}", styles["body"], leftIndent=14 * n, firstLineIndent=0,
                                              fontName=bold if n == 0 else font, spaceAfter=2) for n in range(3)]
            story += [Paragraph(escape(b.get("title", labels[2])), styles["toc_title"]), toc]
            if b.get("page_break", True):
                story.append(PageBreak())
            has_toc = True
        elif kind == "page_break":
            story.append(PageBreak())
        else:
            raise SystemExit(f"error: unknown block type {kind!r} (block {i + 1})")

    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    doc = Doc(out, pagesize=pagesize, leftMargin=margin, rightMargin=margin, topMargin=margin, bottomMargin=2.4 * cm,
              title=spec.get("title", ""), author=spec.get("author", ""), lang="fr-FR" if locale == "fr" else "en-GB")
    if has_toc:
        doc.multiBuild(story, canvasmaker=NumberedCanvas)
    else:
        doc.build(story, canvasmaker=NumberedCanvas)
    from pypdf import PdfReader
    return {"out": out, "pages": len(PdfReader(out).pages)}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--spec")
    ap.add_argument("--out")
    ap.add_argument("--example", action="store_true")
    args = ap.parse_args(argv)
    if args.example:
        print(json.dumps(EXAMPLE, indent=2, ensure_ascii=False))
        return 0
    if not args.spec or not args.out:
        ap.error("--spec and --out are required")
    spec = json.load(sys.stdin if args.spec == "-" else open(args.spec, encoding="utf-8"))
    print(json.dumps(build(spec, args.out), ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
