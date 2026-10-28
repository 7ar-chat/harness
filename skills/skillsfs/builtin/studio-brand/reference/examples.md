# Applying the brand - tested examples

Every code block below was run end to end in this sandbox image; copy it and adapt the paths.

```python
# recipe: brand-docx
import datetime as dt
import os
import sys

sys.path.insert(0, "/skills/studio-brand/scripts")
import brand
from docx import Document

os.makedirs("/workspace/.work", exist_ok=True)
doc = brand.docx_apply(Document(), "fr")
brand.docx_title_block(doc, "Note de synthèse", "Comité du 6 octobre 2026", "Équipe Studio", dt.date(2026, 10, 6), "fr")
brand.docx_footer(doc, "Note de synthèse", "fr")
doc.add_heading("Contexte", 1)
doc.add_paragraph(f"Le budget atteint {brand.format_currency(1234567.5, 'EUR', 0, 'fr')}, "
                  f"soit {brand.format_percent(0.125, 1, 'fr')} de plus.")
table = doc.add_table(rows=3, cols=2)
for r, (k, v) in enumerate([("Poste", "Montant"), ("Licences", brand.format_number(52000, 0)), ("Support", brand.format_number(18500, 0))]):
    table.cell(r, 0).text, table.cell(r, 1).text = k, v
brand.docx_style_table(table, numeric_cols=[1])
doc.save("/workspace/.work/brand.docx")
```

```python
# recipe: brand-pptx
import sys

sys.path.insert(0, "/skills/studio-brand/scripts")
import brand
from pptx.util import Inches

prs = brand.pptx_new()
slide = prs.slides.add_slide(prs.slide_layouts[5])
title = slide.shapes.title
title.left, title.top, title.width, title.height = Inches(0.6), Inches(0.4), Inches(12.13), Inches(0.85)
title.text = "Les coûts baissent de 8 %"
brand.pptx_style_text(title.text_frame, 28, "navy", bold=True)
box = slide.shapes.add_textbox(Inches(0.6), Inches(1.6), Inches(12.13), Inches(4))
box.text_frame.word_wrap = True
box.text_frame.text = "Consolidation des licences"
brand.pptx_style_text(box.text_frame, 20)
brand.pptx_footer(slide, prs, "Comité de pilotage", 2)
prs.save("/workspace/.work/brand.pptx")
```

```python
# recipe: brand-xlsx-mpl
import sys

sys.path.insert(0, "/skills/studio-brand/scripts")
import brand
from openpyxl import Workbook

wb = Workbook()
ws = wb.active
ws.append(["Mois", "Revenu", "Marge %"])
for m, rev in enumerate([1000, 1100, 1250], 1):
    ws.append([f"2026-{m:02d}", rev, f"=1-600/B{m + 1}"])
for row in ws.iter_rows(min_row=2):
    row[1].number_format = brand.XLSX_FORMATS["eur0"]
    row[2].number_format = brand.XLSX_FORMATS["percent"]
brand.xlsx_header(ws)
brand.xlsx_body_font(ws)
brand.xlsx_autofit(ws)
wb.save("/workspace/.work/brand.xlsx")

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
brand.mpl_apply()
fig, ax = plt.subplots(figsize=brand.FIGURE_SIZES["doc"])
ax.bar(["T1", "T2", "T3"], [0.31, 0.35, 0.39])
ax.yaxis.set_major_formatter(brand.mpl_number_formatter("percent", 0, "fr"))
ax.set_title("La marge progresse")
fig.savefig("/workspace/.work/brand_chart.png")
plt.close(fig)
```

Colours by hand: each library wants its own form of the palette colour (`brand.hex_()` with its
"#" is for matplotlib / xlsxwriter / reportlab only; `brand.rgb()` is a plain tuple for Pillow).

```python
# recipe: brand-colours
import sys

sys.path.insert(0, "/skills/studio-brand/scripts")
import brand
from docx import Document
from openpyxl import Workbook
from openpyxl.styles import Border, Font, PatternFill, Side
from pptx.util import Inches

# openpyxl: aRGB strings ("FF1B365D"), or the ready-made fill / font
wb = Workbook()
ws = wb.active
ws["A1"] = "Synthèse"
ws["A1"].font = Font(name="Arial", size=14, bold=True, color=brand.xlsx_color("navy"))
ws["A2"] = "Total"
ws["A2"].fill = PatternFill("solid", fgColor=brand.xlsx_color("mist"))
ws["A2"].border = Border(bottom=Side(style="thin", color=brand.xlsx_color("rule")))
ws["B2"] = 1250
ws["B2"].fill = brand.xlsx_fill("teal")              # a PatternFill, never a colour string
ws["B2"].font = brand.xlsx_font("white", bold=True)
wb.save("/workspace/.work/colours.xlsx")

# python-docx: RGBColor objects
doc = Document()
run = doc.add_paragraph().add_run("Source : export comptable")
run.font.color.rgb = brand.docx_rgb("slate")
doc.save("/workspace/.work/colours.docx")

# python-pptx: its own RGBColor
prs = brand.pptx_new()
slide = prs.slides.add_slide(prs.slide_layouts[6])
card = slide.shapes.add_textbox(Inches(0.6), Inches(0.6), Inches(4), Inches(1))
card.fill.solid()
card.fill.fore_color.rgb = brand.pptx_rgb("navy")
card.text_frame.text = "KPI"
card.text_frame.paragraphs[0].runs[0].font.color.rgb = brand.pptx_rgb("white")
prs.save("/workspace/.work/colours.pptx")
```
