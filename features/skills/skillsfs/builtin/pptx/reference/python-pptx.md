# python-pptx recipes

Every code block below was run end to end in this sandbox image. Units: `Inches`, `Pt`,
`Emu` from `pptx.util`.

## New 16:9 deck, title-only slide, text box with bullets, notes

```python
# recipe: basics
import os
from pptx import Presentation
from pptx.dml.color import RGBColor
from pptx.enum.text import MSO_ANCHOR, PP_ALIGN
from pptx.util import Inches, Pt

os.makedirs("/workspace/.work", exist_ok=True)
prs = Presentation()
prs.slide_width, prs.slide_height = Inches(13.333), Inches(7.5)        # 16:9 BEFORE adding slides
NAVY, INK = RGBColor(0x1B, 0x36, 0x5D), RGBColor(0x1A, 0x1F, 0x2B)

slide = prs.slides.add_slide(prs.slide_layouts[5])                      # "Title Only"
title = slide.shapes.title
title.left, title.top, title.width, title.height = Inches(0.6), Inches(0.4), Inches(12.13), Inches(0.85)
title.text_frame.text = "Costs fell 8% after the migration"
title.text_frame.vertical_anchor = MSO_ANCHOR.BOTTOM
for p in title.text_frame.paragraphs:
    p.alignment = PP_ALIGN.LEFT
    for r in p.runs:
        r.font.size, r.font.bold, r.font.color.rgb, r.font.name = Pt(28), True, NAVY, "Arial"

box = slide.shapes.add_textbox(Inches(0.6), Inches(1.6), Inches(12.13), Inches(4.8))
tf = box.text_frame
tf.word_wrap = True                                                     # textboxes do not wrap by default
for i, (text, level) in enumerate([("Licences consolidated", 0), ("-35% on support", 1), ("Fewer incidents", 0)]):
    p = tf.paragraphs[0] if i == 0 else tf.add_paragraph()
    p.level = level
    run = p.add_run()
    run.text = ("• " if level == 0 else "– ") + text   # simple visible bullets in a textbox
    run.font.size = Pt(22 if level == 0 else 18)
    run.font.color.rgb = INK
    run.font.name = "Arial"
    p.space_after = Pt(8)

slide.notes_slide.notes_text_frame.text = "Explain where the savings come from."
prs.save("/workspace/.work/basics.pptx")
```
Real bullets (with hanging indent) need XML: see `build_deck.py` `Deck.bullets()` (`a:buChar`,
`marL`, `indent`). In a template's body placeholder, bullets come from the layout: just set
`p.level`.

## Native chart (editable in PowerPoint) and a table

```python
# recipe: chart-table
from pptx import Presentation
from pptx.chart.data import CategoryChartData
from pptx.dml.color import RGBColor
from pptx.enum.chart import XL_CHART_TYPE, XL_LEGEND_POSITION
from pptx.util import Inches, Pt

prs = Presentation()
prs.slide_width, prs.slide_height = Inches(13.333), Inches(7.5)
slide = prs.slides.add_slide(prs.slide_layouts[6])                      # Blank
data = CategoryChartData(number_format="0.0")
data.categories = ["Q1", "Q2", "Q3"]
data.add_series("2025", (10.2, 11.0, 12.1))
data.add_series("2026", (11.4, 12.5, 13.4))
gf = slide.shapes.add_chart(XL_CHART_TYPE.COLUMN_CLUSTERED, Inches(0.6), Inches(1.5), Inches(7.5), Inches(5), data)
chart = gf.chart
chart.has_legend = True
chart.legend.position = XL_LEGEND_POSITION.BOTTOM
chart.legend.include_in_layout = False
chart.font.size, chart.font.name = Pt(12), "Arial"
for series, color in zip(chart.plots[0].series, ("1B365D", "2E6DA4")):
    series.format.fill.solid()
    series.format.fill.fore_color.rgb = RGBColor.from_string(color)
chart.value_axis.has_major_gridlines = True
chart.value_axis.major_gridlines.format.line.color.rgb = RGBColor.from_string("D5DAE1")
chart.value_axis.tick_labels.number_format = "0"
chart.value_axis.tick_labels.number_format_is_linked = False
plot = chart.plots[0]
plot.has_data_labels = True
plot.data_labels.number_format, plot.data_labels.number_format_is_linked = "0.0", False

# Tables: always a native table (never rectangles + text boxes). Cells wrap; rows grow with
# the text, so set row heights that fit it - or build the slide with build_deck.py, which
# computes them. fit_text.py reports cells taller than their row as `table_overflow`.
rows = [("Region", "Revenue"), ("North", "5.1"), ("South", "4.2")]
tbl = slide.shapes.add_table(len(rows), 2, Inches(8.6), Inches(1.5), Inches(4.1), Inches(1.5)).table
tbl.columns[0].width, tbl.columns[1].width = Inches(2.6), Inches(1.5)
for r, row in enumerate(rows):
    for c, value in enumerate(row):
        cell = tbl.cell(r, c)
        cell.text = value
        cell.text_frame.word_wrap = True
        para = cell.text_frame.paragraphs[0]
        para.runs[0].font.size = Pt(14)
        if r == 0:
            cell.fill.solid(); cell.fill.fore_color.rgb = RGBColor.from_string("1B365D")
            para.runs[0].font.bold = True
            para.runs[0].font.color.rgb = RGBColor.from_string("FFFFFF")
prs.save("/workspace/.work/chart_table.pptx")

# Edit: update the data of an existing chart, keeping its formatting
prs = Presentation("/workspace/.work/chart_table.pptx")
chart = next(sh.chart for sh in prs.slides[0].shapes if sh.has_chart)
new = CategoryChartData(number_format="0.0")
new.categories = ["Q1", "Q2", "Q3", "Q4"]
new.add_series("2025", (10.2, 11.0, 12.1, 12.9))
new.add_series("2026", (11.4, 12.5, 13.4, 14.2))
chart.replace_data(new)
prs.save("/workspace/.work/chart_table.pptx")
```

## Picture keeping its aspect ratio, background colour, shapes

```python
# recipe: picture-shapes
from PIL import Image
from pptx import Presentation
from pptx.dml.color import RGBColor
from pptx.enum.shapes import MSO_SHAPE
from pptx.util import Inches

Image.new("RGB", (1600, 900), (42, 157, 143)).save("/workspace/.work/photo.png")
prs = Presentation()
prs.slide_width, prs.slide_height = Inches(13.333), Inches(7.5)
slide = prs.slides.add_slide(prs.slide_layouts[6])
title = slide.shapes.add_textbox(Inches(0.6), Inches(0.4), Inches(12.13), Inches(0.85))
title.text_frame.text = "The new site opens in March"

def add_picture_fit(slide, path, x, y, w, h):
    """Fit inside the box (inches), centred, aspect ratio kept. Use it for every picture: a picture
    given only a width (e.g. the full slide width) keeps its ratio but can run off the slide, and a
    guessed width AND height stretches it (fit_text.py: offslide / stretched)."""
    with Image.open(path) as im:
        iw, ih = im.size
    scale = min(w / iw, h / ih)
    pw, ph = iw * scale, ih * scale
    return slide.shapes.add_picture(path, Inches(x + (w - pw) / 2), Inches(y + (h - ph) / 2), Inches(pw), Inches(ph))

add_picture_fit(slide, "/workspace/.work/photo.png", 0.6, 1.5, 7.0, 5.0)
card = slide.shapes.add_shape(MSO_SHAPE.ROUNDED_RECTANGLE, Inches(8.0), Inches(1.5), Inches(4.7), Inches(5.0))
card.fill.solid(); card.fill.fore_color.rgb = RGBColor.from_string("EEF1F5")
card.line.fill.background()
card.shadow.inherit = False
card.text_frame.text = "Text inside a shape"
card.text_frame.word_wrap = True
section = prs.slides.add_slide(prs.slide_layouts[6])
section.background.fill.solid()
section.background.fill.fore_color.rgb = RGBColor.from_string("1B365D")
heading = section.shapes.add_textbox(Inches(1.1), Inches(2.6), Inches(11), Inches(1.4)).text_frame
heading.text = "Next steps"
heading.paragraphs[0].runs[0].font.color.rgb = RGBColor.from_string("FFFFFF")
prs.save("/workspace/.work/pictures.pptx")
```

## Work with a corporate template

```python
# recipe: template
from pptx import Presentation
from pptx.util import Inches

# stand-in for the user's template: any .pptx works the same way
Presentation().save("/workspace/.work/template.pptx")
prs = Presentation("/workspace/.work/template.pptx")
layouts = {l.name: l for l in prs.slide_layouts}            # inspect_pptx.py --layouts shows the same
slide = prs.slides.add_slide(layouts.get("Title and Content", prs.slide_layouts[1]))
slide.shapes.title.text = "Message title"
body = next(ph for ph in slide.placeholders if ph.placeholder_format.idx == 1)
body.text_frame.text = "First point"                         # inherits template bullets and sizes
p = body.text_frame.add_paragraph(); p.text = "Detail"; p.level = 1
# remove placeholders you do not fill (otherwise "Click to add text" shows in PowerPoint)
for ph in list(slide.placeholders):
    if ph.has_text_frame and not ph.text_frame.text.strip():
        ph._element.getparent().remove(ph._element)
prs.save("/workspace/.work/from_template.pptx")
```

## Edit text but keep formatting

```python
# recipe: edit-runs
from pptx import Presentation

prs = Presentation("/workspace/.work/basics.pptx")
for slide in prs.slides:
    for shape in slide.shapes:
        if not shape.has_text_frame:
            continue
        for p in shape.text_frame.paragraphs:
            for r in p.runs:
                if "8%" in r.text:
                    r.text = r.text.replace("8%", "9%")        # run keeps font, size, colour
prs.save("/workspace/.work/basics_edited.pptx")
```
For replacements that may span runs use `scripts/replace_text.py`.
