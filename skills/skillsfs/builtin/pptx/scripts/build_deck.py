#!/usr/bin/env python3
"""Build a 16:9 .pptx deck from a JSON spec with python-pptx, using a consistent grid
(0.6 in margins, title band, body area, footer) and the Studio palette. Native, editable
charts and tables. Runs the fit_text.py checks at the end (and shrinks overflowing text, never
below fit_text.MIN_PT = 12 pt, unless --no-fix); the result has "ok"/"layout_ok", "checks",
"next_step" and "issues".

Usage:
  python3 build_deck.py --spec deck.json --out /workspace/outputs/deck.pptx
  python3 build_deck.py --example        # print an example spec with every slide type

Spec:
{
  "footer": "Project X - Steering committee",   # footer text on content slides
  "numbers": true,                               # slide numbers on content slides
  "template": "path.pptx",                       # optional: start from this deck/template
  "slides": [
    {"type": "title", "title": "...", "subtitle": "...", "date": "6 octobre 2026"},
    {"type": "section", "title": "...", "subtitle": "..."},
    {"type": "bullets", "title": "...", "subtitle": "optional kicker", "bullets": ["a", {"text": "b", "level": 1}]},
    {"type": "two_column", "title": "...", "left": {"heading": "...", "bullets": [...]},
                                         "right": {"heading": "...", "image": "path.png"}},
    {"type": "image", "title": "...", "image": "path.png", "caption": "Source: ..."},
    {"type": "chart", "title": "...", "chart": {"type": "column", "categories": ["Q1","Q2"],
        "series": [{"name": "2026", "values": [1, 2]}], "number_format": "0.0", "data_labels": true},
        "takeaway": "One sentence that says what the chart shows"},
    {"type": "table", "title": "...", "columns": ["A", "B"], "rows": [["x", "1"]], "numeric_cols": [1],
        "col_widths_in": [8, 4.1], "font_size": 14},   # cells wrap; rows grow to fit the text
    {"type": "kpi", "title": "...", "kpis": [{"value": "12 %", "label": "Growth", "delta": "+2 pts"}]},
    {"type": "quote", "quote": "...", "author": "..."},
    {"type": "text", "title": "...", "text": "Paragraph text. Blank line = new paragraph."}
  ]
}
Every slide accepts "notes" (speaker notes). Chart types: column, stacked_column, bar,
stacked_bar, line, pie, doughnut. A column in "left"/"right" takes one of bullets | text |
image | chart.
Tables are native PowerPoint tables: text wraps inside each cell, column widths come from
"col_widths_in" (inches; scaled down if wider than the slide) or from the text lengths, and
every row height is computed from its wrapped text (font reduced down to 12 pt if the table
is too tall; beyond that the result reports it: split the table over two slides).
Shorthand slides are accepted too: {"bullets": {"title": "...", "bullets": [...]}},
{"section": "Next steps"}, {"quote": "..."}, {"title": "Deck title", "subtitle": "..."},
{"title": "Message", "bullets": [...]} (type taken from the content key).
"""
from __future__ import annotations

import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
BRAND_DIRS = ["/skills/studio-brand/scripts", os.path.join(HERE, "..", "..", "studio-brand", "scripts")]

# Fallback when studio-brand is not installed (same values as studio-brand/scripts/brand.py).
FALLBACK = {
    "PALETTE": {"navy": "1B365D", "blue": "2E6DA4", "sky": "8DB8DE", "teal": "2A9D8F", "amber": "E9A23B",
                "coral": "C8553D", "ink": "1A1F2B", "slate": "4A5568", "grey": "8C96A3", "rule": "D5DAE1",
                "mist": "EEF1F5", "white": "FFFFFF", "positive": "2E7D32", "negative": "C62828"},
    "CHART_COLORS": ["1B365D", "2E6DA4", "2A9D8F", "E9A23B", "C8553D", "7D5BA6", "8C96A3", "8AA05B"],
    "FONT": "Arial",
}

EXAMPLE = {
    "footer": "Studio - Example deck", "numbers": True,
    "slides": [
        {"type": "title", "title": "Quarterly business review", "subtitle": "Q3 2026", "date": "6 October 2026"},
        {"type": "bullets", "title": "Three things to remember", "bullets": [
            "Revenue up 12% year on year", "Costs flat despite inflation", "Two new clients signed"],
         "notes": "Open with the headline numbers."},
        {"type": "chart", "title": "Revenue grew every quarter", "chart": {
            "type": "column", "categories": ["Q1", "Q2", "Q3"],
            "series": [{"name": "2025", "values": [10, 11, 12]}, {"name": "2026", "values": [11, 12.5, 13.4]}],
            "number_format": "0.0", "data_labels": True}, "takeaway": "Q3 2026 is the best quarter on record."},
        {"type": "kpi", "title": "Key figures", "kpis": [
            {"value": "13.4 M€", "label": "Q3 revenue", "delta": "+12%"},
            {"value": "41%", "label": "Gross margin", "delta": "+1 pt"},
            {"value": "2", "label": "New clients"}]},
        {"type": "table", "title": "Detail by region", "columns": ["Region", "Revenue", "Growth"],
         "rows": [["North", "5.1", "+9%"], ["South", "4.2", "+15%"], ["International", "4.1", "+13%"]],
         "numeric_cols": [1, 2]},
        {"type": "section", "title": "Next steps"},
        {"type": "quote", "quote": "Simplicity is the ultimate sophistication.", "author": "Attributed to Leonardo da Vinci"},
    ],
}



SLIDE_TYPES = {  # type -> field filled when the shorthand value is a string / list
    "title": "title", "section": "title", "bullets": "bullets", "two_column": None, "image": "image",
    "chart": "chart", "table": "rows", "kpi": "kpis", "quote": "quote", "text": "text"}
SLIDE_ALIASES = {"cover": "title", "title_slide": "title", "divider": "section", "agenda": "bullets",
                 "list": "bullets", "two_columns": "two_column", "picture": "image", "kpis": "kpi",
                 "paragraph": "text", "body": "text"}
SHAPES_HELP = """Accepted shapes for every slide (pick one):
  {"type": "bullets", "title": "Three things", "bullets": ["a", "b"]}
  {"bullets": {"title": "Three things", "bullets": ["a", "b"]}}
  {"title": "Three things", "bullets": ["a", "b"]}        (type taken from the content key)
  {"section": "Next steps"}                                (a string fills the main field)
Slide types and fields: title{title,subtitle,date}  section{title,subtitle}  bullets{title,bullets}
  two_column{title,left,right}  image{title,image,caption}  chart{title,chart{type,categories,series},takeaway}
  table{title,columns,rows,col_widths_in,numeric_cols}  kpi{title,kpis[{value,label,delta}]}
  quote{quote,author}  text{title,text}       every slide: notes
Short example:
  {"footer": "Deck", "slides": [{"type": "title", "title": "Q3 review"},
   {"type": "bullets", "title": "Revenue grew 12%", "bullets": ["North +9%", "South +15%"]}]}
Full example: python3 /skills/pptx/scripts/build_deck.py --example"""


def _slide_type(name):
    if not isinstance(name, str):
        return None
    key = name.strip().lower().replace("-", "_").replace(" ", "_")
    return key if key in SLIDE_TYPES else SLIDE_ALIASES.get(key)


def normalize_slide(slide, i):
    """Return (slide_with_type, error). Accepts the typed form and the shorthands."""
    if not isinstance(slide, dict):
        return None, f"slide {i + 1}: expected an object, got {json.dumps(slide, ensure_ascii=False)[:80]}"
    shown = json.dumps(slide, ensure_ascii=False)[:120]
    if "type" in slide:
        kind = _slide_type(slide["type"])
        if kind is None:
            return None, f"slide {i + 1}: unknown slide type {slide['type']!r} in {shown}"
        return {**slide, "type": kind}, None
    keys = [k for k in slide if _slide_type(k)]
    if len(keys) > 1 and "title" in keys and isinstance(slide["title"], str):
        keys.remove("title")                          # {"title": "Message", "bullets": [...]}
    guess = ("table" if "columns" in slide or "rows" in slide else "two_column" if "left" in slide or "right" in slide
             else None)
    if guess and keys == ["title"] and isinstance(slide["title"], str):
        keys = []                                     # {"title": "Message", "columns": [...], "rows": [...]}
    if not keys:
        if guess is None:
            return None, f"slide {i + 1}: no slide type in {shown}"
        return {**slide, "type": guess}, None
    if len(keys) > 1:
        return None, f"slide {i + 1}: several slide types ({', '.join(keys)}) in {shown}"
    key = keys[0]
    kind = _slide_type(key)
    value = slide[key]
    rest = {k: v for k, v in slide.items() if k != key}
    field = SLIDE_TYPES[kind]
    if kind == "chart" and isinstance(value, dict) and ("series" in value or "categories" in value):
        return {**rest, "chart": value, "type": kind}, None
    if isinstance(value, dict):
        return {**rest, **value, "type": kind}, None
    if isinstance(value, list) and kind in ("bullets", "table", "kpi"):
        return {**rest, field: value, "type": kind}, None
    if isinstance(value, str) and field:
        return {**rest, field: [value] if kind == "bullets" else value, "type": kind}, None
    return None, f"slide {i + 1}: cannot read the value of {key!r} in {shown}"


def normalize_spec(spec):
    """Validate every slide up front; exit with all problems and the accepted shapes."""
    if isinstance(spec, list):
        spec = {"slides": spec}
    if not isinstance(spec, dict) or not isinstance(spec.get("slides"), list):
        raise SystemExit("error: the spec must be a JSON object with a 'slides' list\n" + SHAPES_HELP)
    out, errors = [], []
    for i, sl in enumerate(spec["slides"]):
        ns, err = normalize_slide(sl, i)
        if err is None and ns["type"] == "table" and not ns.get("columns"):
            err = f"slide {i + 1}: table needs 'columns' (header row) and 'rows'"
        if err is None and ns["type"] == "chart" and not isinstance(ns.get("chart"), dict):
            err = f"slide {i + 1}: chart needs 'chart': {{type, categories, series}}"
        if err:
            errors.append(err)
        else:
            out.append(ns)
    if errors:
        raise SystemExit("error: invalid spec\n  " + "\n  ".join(errors) + "\n" + SHAPES_HELP)
    return {**spec, "slides": out}


def load_brand():
    for d in BRAND_DIRS:
        if os.path.isfile(os.path.join(d, "brand.py")):
            sys.path.insert(0, os.path.abspath(d))
            import brand  # noqa: PLC0415
            return brand
    return None


class Deck:
    W, H, M = 13.333, 7.5, 0.6
    TITLE_TOP, TITLE_H = 0.4, 0.85
    BODY_TOP, BODY_BOTTOM = 1.45, 6.75

    def __init__(self, spec):
        from pptx import Presentation
        from pptx.util import Inches

        self.spec = spec
        self.brand = load_brand()
        if self.brand:
            self.P, self.CC, self.FONT = self.brand.PALETTE, self.brand.CHART_COLORS, self.brand.FONTS["office"]
        else:
            print("warning: studio-brand not installed; using built-in fallback palette", file=sys.stderr)
            self.P, self.CC, self.FONT = FALLBACK["PALETTE"], FALLBACK["CHART_COLORS"], FALLBACK["FONT"]
        if spec.get("template"):
            self.prs = Presentation(spec["template"])
        else:
            self.prs = Presentation()
            self.prs.slide_width, self.prs.slide_height = Inches(self.W), Inches(self.H)
            if self.brand:
                self.brand.pptx_apply_theme(self.prs)
        self.W = self.prs.slide_width / 914400
        self.H = self.prs.slide_height / 914400
        self.BODY_BOTTOM = self.H - 0.75
        self.content_count = 0
        self.warnings = []
        self.resolver = None

    # ------------------------------------------------------------ primitives
    def rgb(self, name):
        from pptx.dml.color import RGBColor
        return RGBColor.from_string(self.P.get(name, name))

    def layout(self, preferred_names, fallback_index):
        for lay in self.prs.slide_layouts:
            if lay.name in preferred_names:
                return lay
        return self.prs.slide_layouts[min(fallback_index, len(self.prs.slide_layouts) - 1)]

    def style_runs(self, tf, size, color="ink", bold=None):
        from pptx.util import Pt
        for p in tf.paragraphs:
            for r in p.runs:
                r.font.size = Pt(size)
                r.font.name = self.FONT
                r.font.color.rgb = self.rgb(color)
                if bold is not None:
                    r.font.bold = bold

    def textbox(self, slide, x, y, w, h, text, size=18, color="ink", bold=False, align="left",
                anchor="top", name=None):
        from pptx.enum.text import MSO_ANCHOR, PP_ALIGN
        from pptx.util import Inches

        tb = slide.shapes.add_textbox(Inches(x), Inches(y), Inches(w), Inches(h))
        if name:
            tb.name = name
        tf = tb.text_frame
        tf.word_wrap = True
        tf.margin_left = tf.margin_right = Inches(0.05)
        tf.vertical_anchor = {"top": MSO_ANCHOR.TOP, "middle": MSO_ANCHOR.MIDDLE, "bottom": MSO_ANCHOR.BOTTOM}[anchor]
        paras = str(text).split("\n\n") if text else [""]
        for i, para in enumerate(paras):
            p = tf.paragraphs[0] if i == 0 else tf.add_paragraph()
            p.alignment = {"left": PP_ALIGN.LEFT, "center": PP_ALIGN.CENTER, "right": PP_ALIGN.RIGHT}[align]
            r = p.add_run()
            r.text = para
            if i > 0:
                from pptx.util import Pt
                p.space_before = Pt(size * 0.5)
        self.style_runs(tf, size, color, bold)
        return tb

    def rect(self, slide, x, y, w, h, fill, line=None, name=None):
        from pptx.enum.shapes import MSO_SHAPE
        from pptx.util import Inches

        shp = slide.shapes.add_shape(MSO_SHAPE.RECTANGLE, Inches(x), Inches(y), Inches(w), Inches(h))
        shp.fill.solid()
        shp.fill.fore_color.rgb = self.rgb(fill)
        if line:
            shp.line.color.rgb = self.rgb(line)
            shp.line.width = 9525
        else:
            shp.line.fill.background()
        shp.shadow.inherit = False
        if name:
            shp.name = name
        return shp

    def set_title(self, slide, text, subtitle=None):
        from pptx.enum.text import MSO_ANCHOR
        from pptx.util import Inches, Pt

        t = slide.shapes.title
        if t is None:
            t = self.textbox(slide, self.M, self.TITLE_TOP, self.W - 2 * self.M, self.TITLE_H, text, 28, "navy", True)
        else:
            t.left, t.top, t.width, t.height = Inches(self.M), Inches(self.TITLE_TOP), Inches(self.W - 2 * self.M), Inches(self.TITLE_H)
            t.text_frame.text = text
            t.text_frame.word_wrap = True
            t.text_frame.vertical_anchor = MSO_ANCHOR.BOTTOM
            t.text_frame.margin_left = Inches(0.05)
            from pptx.enum.text import PP_ALIGN
            for p in t.text_frame.paragraphs:
                p.alignment = PP_ALIGN.LEFT
            self.style_runs(t.text_frame, 28, "navy", True)
        top = self.TITLE_TOP + self.TITLE_H + 0.05
        if subtitle:
            self.textbox(slide, self.M, top, self.W - 2 * self.M, 0.45, subtitle, 16, "slate")
            top += 0.5
        # thin accent rule under the title band
        self.rect(slide, self.M + 0.05, top + 0.02, 0.9, 0.05, "teal", name="Accent rule")
        _ = Pt
        return top + 0.25

    def footer(self, slide):
        self.content_count += 1
        text = self.spec.get("footer", "")
        number = len(self.prs.slides) if self.spec.get("numbers", True) else None
        if self.brand:
            self.brand.pptx_footer(slide, self.prs, text, number)
        else:
            if text:
                self.textbox(slide, self.M, self.H - 0.45, self.W / 2, 0.3, text, 10, "grey", name="Studio footer")
            if number is not None:
                self.textbox(slide, self.W - self.M - 1, self.H - 0.45, 1, 0.3, str(number), 10, "grey",
                             align="right", name="Studio footer number")

    def notes(self, slide, spec):
        if spec.get("notes"):
            slide.notes_slide.notes_text_frame.text = spec["notes"]

    def drop_empty_placeholders(self, slide):
        for ph in list(slide.placeholders):
            if ph.has_text_frame and not ph.text_frame.text.strip():
                ph._element.getparent().remove(ph._element)

    # ------------------------------------------------------------ content blocks
    def bullets(self, slide, x, y, w, h, items, size=None):
        from pptx.util import Inches, Pt
        from pptx.oxml.ns import qn

        n = len(items)
        size = size or (22 if n <= 4 else 20 if n <= 6 else 18)
        tb = slide.shapes.add_textbox(Inches(x), Inches(y), Inches(w), Inches(h))
        tb.name = "Body"
        tf = tb.text_frame
        tf.word_wrap = True
        for i, item in enumerate(items):
            text, level = (item.get("text", ""), int(item.get("level", 0))) if isinstance(item, dict) else (str(item), 0)
            p = tf.paragraphs[0] if i == 0 else tf.add_paragraph()
            p.level = min(level, 4)
            r = p.add_run()
            r.text = text
            sz = size - 2 * level
            r.font.size = Pt(sz)
            r.font.name = self.FONT
            r.font.color.rgb = self.rgb("ink" if level == 0 else "slate")
            p.space_after = Pt(sz * 0.5)
            ppr = p._p.get_or_add_pPr()
            indent = int(Inches(0.3 + 0.3 * level))
            ppr.set("marL", str(indent))
            ppr.set("indent", str(-int(Inches(0.3))))
            for tag in ("a:buNone", "a:buChar", "a:buAutoNum", "a:buClr", "a:buFont"):
                for el in ppr.findall(qn(tag)):
                    ppr.remove(el)
            bu_clr = ppr.makeelement(qn("a:buClr"), {})
            srgb = bu_clr.makeelement(qn("a:srgbClr"), {"val": self.P["teal"] if level == 0 else self.P["grey"]})
            bu_clr.append(srgb)
            ppr.append(bu_clr)
            ppr.append(ppr.makeelement(qn("a:buFont"), {"typeface": "Arial"}))
            ppr.append(ppr.makeelement(qn("a:buChar"), {"char": "•" if level == 0 else "–"}))
        return tb

    def picture(self, slide, path, x, y, w, h):
        from PIL import Image
        from pptx.util import Inches

        if not os.path.isfile(path):
            raise SystemExit(f"error: image not found: {path}")
        with Image.open(path) as im:
            iw, ih = im.size
        scale = min(w / iw, h / ih)
        pw, ph = iw * scale, ih * scale
        return slide.shapes.add_picture(path, Inches(x + (w - pw) / 2), Inches(y + (h - ph) / 2), Inches(pw), Inches(ph))

    def chart(self, slide, spec, x, y, w, h):
        from pptx.chart.data import CategoryChartData
        from pptx.enum.chart import XL_CHART_TYPE, XL_LEGEND_POSITION, XL_LABEL_POSITION
        from pptx.util import Inches, Pt

        types = {"column": XL_CHART_TYPE.COLUMN_CLUSTERED, "stacked_column": XL_CHART_TYPE.COLUMN_STACKED,
                 "bar": XL_CHART_TYPE.BAR_CLUSTERED, "stacked_bar": XL_CHART_TYPE.BAR_STACKED,
                 "line": XL_CHART_TYPE.LINE_MARKERS, "pie": XL_CHART_TYPE.PIE, "doughnut": XL_CHART_TYPE.DOUGHNUT}
        kind = spec.get("type", "column")
        if kind not in types:
            raise SystemExit(f"error: chart type {kind!r} not in {sorted(types)}")
        data = CategoryChartData(number_format=spec.get("number_format", "General"))
        data.categories = spec["categories"]
        for s in spec["series"]:
            data.add_series(s["name"], s["values"])
        gf = slide.shapes.add_chart(types[kind], Inches(x), Inches(y), Inches(w), Inches(h), data)
        ch = gf.chart
        ch.font.size = Pt(12)
        ch.font.name = self.FONT
        ch.font.color.rgb = self.rgb("slate")
        ch.has_title = False
        multi = len(spec["series"]) > 1 or kind in ("pie", "doughnut")
        ch.has_legend = spec.get("legend", multi)
        if ch.has_legend:
            ch.legend.position = XL_LEGEND_POSITION.BOTTOM
            ch.legend.include_in_layout = False
            ch.legend.font.size = Pt(12)
        plot = ch.plots[0]
        if kind in ("pie", "doughnut"):
            for i, point in enumerate(plot.series[0].points):
                point.format.fill.solid()
                point.format.fill.fore_color.rgb = self.rgb(self.CC[i % len(self.CC)])
        else:
            for i, s in enumerate(plot.series):
                color = s_color = self.rgb(self.CC[i % len(self.CC)])
                if kind == "line":
                    s.format.line.color.rgb = color
                    s.format.line.width = Pt(2.5)
                    s.smooth = False
                    s.marker.format.fill.solid()
                    s.marker.format.fill.fore_color.rgb = s_color
                    s.marker.format.line.color.rgb = s_color
                else:
                    s.format.fill.solid()
                    s.format.fill.fore_color.rgb = color
            if kind in ("column", "bar", "stacked_column", "stacked_bar"):
                plot.gap_width = 60
                if kind.startswith("stacked"):
                    plot.overlap = 100
            va = ch.value_axis
            values = [v for se in spec["series"] for v in se["values"] if v is not None]
            if kind != "line" and values and min(values) >= 0:
                va.minimum_scale = 0                  # bars always start at zero
            va.has_major_gridlines = True
            va.major_gridlines.format.line.color.rgb = self.rgb("rule")
            va.format.line.fill.background()
            va.tick_labels.font.size = Pt(12)
            if spec.get("number_format"):
                va.tick_labels.number_format = spec["number_format"]
                va.tick_labels.number_format_is_linked = False
            ca = ch.category_axis
            ca.format.line.color.rgb = self.rgb("grey")
            ca.tick_labels.font.size = Pt(12)
            ca.has_major_gridlines = False
        if spec.get("data_labels", False):
            plot.has_data_labels = True
            dl = plot.data_labels
            dl.font.size = Pt(12)
            dl.font.color.rgb = self.rgb("ink")
            if spec.get("number_format"):
                dl.number_format = spec["number_format"]
                dl.number_format_is_linked = False
            if kind in ("column", "bar"):
                dl.position = XL_LABEL_POSITION.OUTSIDE_END
            if kind in ("pie", "doughnut"):
                dl.show_percentage = spec.get("show_percent", True)
                dl.show_value = not dl.show_percentage
        return gf

    def table_widths(self, columns, rows, w, widths):
        """Column widths in inches: the requested ones (scaled down to fit w) or by text length."""
        n = len(columns)
        if widths:
            widths = [float(x) for x in widths[:n]] + [0.0] * max(0, n - len(widths))
            missing = [i for i, x in enumerate(widths) if x <= 0]
            if missing:
                rest = max(0.5 * len(missing), w - sum(widths))
                for i in missing:
                    widths[i] = rest / len(missing)
            if sum(widths) > w:
                widths = [x * w / sum(widths) for x in widths]
            return widths
        lengths = []
        for c in range(n):
            texts = [str(columns[c])] + [str(r[c]) for r in rows if c < len(r) and r[c] is not None]
            lengths.append(min(60, max(len(t) for t in texts)) + 6)
        return [w * x / sum(lengths) for x in lengths]

    def table(self, slide, columns, rows, x, y, w, h, numeric_cols=None, widths=None, font_size=None):
        """Native table: wrapped cells, column widths, row heights computed from the text."""
        import fit_text  # noqa: PLC0415
        from pptx.enum.text import MSO_ANCHOR
        from pptx.util import Emu, Inches, Pt

        n_rows = len(rows) + 1
        col_w = self.table_widths(columns, rows, w, widths)
        gf = slide.shapes.add_table(n_rows, len(columns), Inches(x), Inches(y), Inches(sum(col_w)),
                                    Inches(0.4 * n_rows))
        gf.name = "Table"
        tbl = gf.table
        for c, wi in enumerate(col_w):
            tbl.columns[c].width = Inches(wi)
        for c, name in enumerate(columns):
            tbl.cell(0, c).text = str(name)
        for r, row in enumerate(rows, 1):
            for c in range(len(columns)):
                tbl.cell(r, c).text = "" if c >= len(row) or row[c] is None else str(row[c])
        tbl.first_row = True
        tbl.horz_banding = False
        for row in tbl.rows:
            for cell in row.cells:
                cell.margin_top = cell.margin_bottom = Inches(0.05)
                cell.margin_left = cell.margin_right = Inches(0.08)
                cell.text_frame.word_wrap = True
                cell.vertical_anchor = MSO_ANCHOR.MIDDLE
        if self.resolver is None:
            self.resolver = fit_text.Resolver(self.prs)
        size = font_size or (16 if n_rows <= 6 else 14 if n_rows <= 10 else 12)
        floor = min(size, fit_text.MIN_PT)
        min_row = Inches(0.4)
        while True:
            if self.brand:
                self.brand.pptx_style_table(tbl, font_size=size, numeric_cols=numeric_cols or [])
            else:
                for r_idx, row in enumerate(tbl.rows):
                    for cell in row.cells:
                        for p in cell.text_frame.paragraphs:
                            for run in p.runs:
                                run.font.size = Pt(size)
                                run.font.name = self.FONT
                                run.font.bold = r_idx == 0
            needed, _ = fit_text.table_rows_needed(gf, slide, self.resolver)
            heights = [max(min_row, int(nd * 1.08) + Inches(0.04)) for nd in needed]   # safety margin
            if sum(heights) <= Inches(h) or size <= floor:
                break
            size -= 1
        for row, hh in zip(tbl.rows, heights):
            row.height = Emu(hh)
        gf.height = Emu(sum(heights))
        if sum(heights) > Inches(h):
            self.warnings.append({"slide": len(self.prs.slides), "type": "overflow", "shape": gf.name,
                                  "detail": f"table needs {sum(heights) / 914400:.2f} in, {h:.2f} in available even at "
                                            f"{size} pt: split the rows over two slides or shorten the text"})
        return gf

    def column_block(self, slide, block, x, y, w, h):
        if block.get("heading"):
            self.textbox(slide, x, y, w, 0.5, block["heading"], 18, "navy", True)
            y += 0.6
            h -= 0.6
        if block.get("bullets"):
            self.bullets(slide, x, y, w, h, block["bullets"], size=block.get("size", 18))
        elif block.get("text"):
            self.textbox(slide, x, y, w, h, block["text"], block.get("size", 18))
        elif block.get("image"):
            self.picture(slide, block["image"], x, y, w, h)
        elif block.get("chart"):
            self.chart(slide, block["chart"], x, y, w, h)

    # ------------------------------------------------------------ slide types
    def add(self, s):
        kind = s.get("type", "bullets")
        m, W, H = self.M, self.W, self.H
        if kind == "title":
            slide = self.prs.slides.add_slide(self.layout(["Title Slide"], 0))
            self.rect(slide, 0, 0, 0.35, H, "navy", name="Brand band")
            t = slide.shapes.title
            from pptx.util import Inches
            from pptx.enum.text import MSO_ANCHOR, PP_ALIGN
            t.left, t.top, t.width, t.height = Inches(1.1), Inches(2.2), Inches(W - 2.2), Inches(1.7)
            t.text_frame.text = s.get("title", "")
            t.text_frame.word_wrap = True
            t.text_frame.vertical_anchor = MSO_ANCHOR.BOTTOM
            for p in t.text_frame.paragraphs:
                p.alignment = PP_ALIGN.LEFT
            self.style_runs(t.text_frame, 40, "navy", True)
            self.rect(slide, 1.15, 4.05, 1.2, 0.07, "teal", name="Accent rule")
            sub = None
            for ph in slide.placeholders:
                if ph.placeholder_format.idx == 1:
                    sub = ph
            if s.get("subtitle") and sub is not None:
                sub.left, sub.top, sub.width, sub.height = Inches(1.1), Inches(4.3), Inches(W - 2.2), Inches(0.9)
                sub.text_frame.text = s["subtitle"]
                sub.text_frame.vertical_anchor = MSO_ANCHOR.TOP
                for p in sub.text_frame.paragraphs:
                    p.alignment = PP_ALIGN.LEFT
                self.style_runs(sub.text_frame, 20, "slate")
            meta = " · ".join(x for x in (s.get("author"), s.get("date")) if x)
            if meta:
                self.textbox(slide, 1.1, H - 1.3, W - 2.2, 0.4, meta, 14, "grey")
            self.drop_empty_placeholders(slide)
        elif kind == "section":
            slide = self.prs.slides.add_slide(self.layout(["Title Only"], 5))
            slide.background.fill.solid()
            slide.background.fill.fore_color.rgb = self.rgb("navy")
            for ph in list(slide.placeholders):
                ph._element.getparent().remove(ph._element)
            self.textbox(slide, 1.1, 2.6, W - 2.2, 1.4, s.get("title", ""), 36, "white", True, anchor="bottom")
            self.rect(slide, 1.15, 4.15, 1.2, 0.07, "teal", name="Accent rule")
            if s.get("subtitle"):
                self.textbox(slide, 1.1, 4.4, W - 2.2, 0.8, s["subtitle"], 20, "sky")
        elif kind == "quote":
            slide = self.prs.slides.add_slide(self.layout(["Blank"], 6))
            self.rect(slide, m, 2.0, 0.08, 2.6, "teal", name="Quote bar")
            self.textbox(slide, m + 0.4, 1.8, W - 2 * m - 1.0, 2.9, "“" + s.get("quote", "") + "”", 30, "navy",
                         anchor="middle")
            if s.get("author"):
                self.textbox(slide, m + 0.4, 4.9, W - 2 * m - 1.0, 0.5, "— " + s["author"], 16, "slate")
            self.footer(slide)
        else:
            slide = self.prs.slides.add_slide(self.layout(["Title Only"], 5))
            top = self.set_title(slide, s.get("title", ""), s.get("subtitle"))
            bottom = self.BODY_BOTTOM
            bw, bh = W - 2 * m, bottom - top
            if kind == "bullets":
                items = s.get("bullets", [])
                if len(items) > 6:
                    self.warnings.append({"slide": len(self.prs.slides), "type": "dense",
                                          "detail": f"{len(items)} bullets (max 6): split the slide or use a table"})
                self.bullets(slide, m, top, bw, bh, items, s.get("size"))
            elif kind == "text":
                self.textbox(slide, m, top, bw, bh, s.get("text", ""), s.get("size", 20))
            elif kind == "two_column":
                gap = 0.5
                cw = (bw - gap) / 2
                self.column_block(slide, s.get("left", {}), m, top, cw, bh)
                self.column_block(slide, s.get("right", {}), m + cw + gap, top, cw, bh)
            elif kind == "image":
                cap_h = 0.4 if s.get("caption") else 0
                self.picture(slide, s["image"], m, top, bw, bh - cap_h)
                if cap_h:
                    self.textbox(slide, m, bottom - cap_h, bw, cap_h, s["caption"], 12, "slate")
            elif kind == "chart":
                take = s.get("takeaway")
                cw = bw - 3.9 if take else bw
                cap_h = 0.4 if s.get("caption") else 0
                self.chart(slide, s["chart"], m, top, cw, bh - cap_h)
                if take:
                    self.rect(slide, m + cw + 0.3, top + 0.2, 3.6, bh - 0.4, "mist", name="Takeaway panel")
                    self.textbox(slide, m + cw + 0.5, top + 0.4, 3.2, bh - 0.8, take, 18, "navy", True)
                if cap_h:
                    self.textbox(slide, m, bottom - cap_h, cw, cap_h, s["caption"], 12, "slate")
            elif kind == "table":
                self.table(slide, s["columns"], s.get("rows", []), m, top, bw, bh, s.get("numeric_cols"),
                           s.get("col_widths_in") or s.get("widths_in"), s.get("font_size"))
            elif kind == "kpi":
                kpis = s.get("kpis", [])[:4]
                n = max(1, len(kpis))
                gap = 0.35
                cw = (bw - gap * (n - 1)) / n
                ch = min(3.2, bh - 0.4)
                y = top + (bh - ch) / 2
                for i, k in enumerate(kpis):
                    x = m + i * (cw + gap)
                    self.rect(slide, x, y, cw, ch, "mist", name=f"KPI card {i + 1}")
                    self.rect(slide, x, y, cw, 0.08, "navy", name=f"KPI bar {i + 1}")
                    self.textbox(slide, x + 0.2, y + 0.35, cw - 0.4, 1.2, k.get("value", ""), 40, "navy", True,
                                 align="center", anchor="middle")
                    self.textbox(slide, x + 0.2, y + 1.6, cw - 0.4, 0.8, k.get("label", ""), 16, "slate",
                                 align="center")
                    if k.get("delta"):
                        d = str(k["delta"])
                        color = "negative" if d.strip().startswith(("-", "−")) else "positive"
                        self.textbox(slide, x + 0.2, y + 2.4, cw - 0.4, 0.5, d, 14, color, True, align="center")
            else:
                raise SystemExit(f"error: unknown slide type {kind!r}")
            self.footer(slide)
        self.notes(slide, s)
        return slide


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--spec")
    ap.add_argument("--out")
    ap.add_argument("--example", action="store_true")
    ap.add_argument("--no-fix", action="store_true", help="report overflow but do not shrink text")
    args = ap.parse_args(argv)
    if args.example:
        print(json.dumps(EXAMPLE, indent=2, ensure_ascii=False))
        return 0
    if not args.spec or not args.out:
        ap.error("--spec and --out are required")
    spec = normalize_spec(json.load(sys.stdin if args.spec == "-" else open(args.spec, encoding="utf-8")))
    deck = Deck(spec)
    for s in spec.get("slides", []):
        deck.add(s)
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    import fit_text  # noqa: PLC0415

    deck.prs.core_properties.identifier = fit_text.BUILT_MARKER  # its overlaps fail the check (agent's layout)
    deck.prs.save(args.out)

    from pptx import Presentation

    prs = Presentation(args.out)
    resolver = fit_text.Resolver(prs)
    report = [fit_text.check_slide(sl, n, prs, resolver, fit_text.MIN_PT, not args.no_fix, fit_text.MIN_PT)
              for n, sl in enumerate(prs.slides, 1)]
    if not args.no_fix:
        prs.save(args.out)
    issues = [dict(i, slide=s["slide"]) for s in report for i in s["issues"]] + deck.warnings
    blocking = [i for i in issues if i["type"] in fit_text.BLOCKING]
    counts = {}
    for i in blocking:
        counts[i["type"]] = counts.get(i["type"], 0) + 1
    checks = fit_text.build_checks([{"slide": s["slide"], "issues": s["issues"]} for s in report])
    if deck.warnings:
        tall = [w for w in deck.warnings if w["type"] in fit_text.BLOCKING]
        if tall:
            checks.append({"name": "tables_fit_slide", "ok": False, "detail": "; ".join(w["detail"] for w in tall)})
    print(json.dumps({"ok": not blocking, "layout_ok": not blocking, "file": args.out, "out": args.out,
                      "slides": len(prs.slides), "checks": checks, "next_step": fit_text.next_step(counts, args.out),
                      "issues": issues}, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}",
                          "hint": "check the spec against: python3 /skills/pptx/scripts/build_deck.py --example"},
                         ensure_ascii=False))
        sys.exit(1)
