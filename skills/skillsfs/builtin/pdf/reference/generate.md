# Generating PDFs with reportlab

Every code block below was run end to end in this sandbox image; copy it and adapt the paths.

## Platypus document with Unicode fonts, table, image, page numbers

```python
# recipe: platypus
import os
import sys
from PIL import Image as PILImage
from reportlab.lib import colors
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle
from reportlab.lib.units import cm
from reportlab.platypus import Image, Paragraph, SimpleDocTemplate, Spacer, Table, TableStyle

sys.path.insert(0, "/skills/studio-brand/scripts")
try:
    import brand
    FONT = brand.reportlab_fonts()            # Liberation Sans TTF as "StudioSans" (+ -Bold/-Italic)
except ImportError:                           # studio-brand not loaded: register the TTF yourself
    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont
    pdfmetrics.registerFont(TTFont("StudioSans", "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf"))
    pdfmetrics.registerFont(TTFont("StudioSans-Bold", "/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf"))
    FONT = "StudioSans"

os.makedirs("/workspace/.work", exist_ok=True)
PILImage.new("RGB", (900, 400), (27, 54, 93)).save("/workspace/.work/fig.png")
body = ParagraphStyle("body", fontName=FONT, fontSize=10.5, leading=14.5)
h1 = ParagraphStyle("h1", body, fontName=FONT + "-Bold", fontSize=16, leading=20, textColor=colors.HexColor("#1B365D"),
                    spaceBefore=12, spaceAfter=6)

def footer(canvas, doc):
    canvas.saveState()
    canvas.setFont(FONT, 8)
    canvas.setFillColor(colors.HexColor("#8C96A3"))
    canvas.drawRightString(A4[0] - 2.2 * cm, 1.2 * cm, f"Page {doc.page}")
    canvas.restoreState()

story = [Paragraph("Synthèse – œuvre, « guillemets », 1 234,50 €", h1),
         Paragraph("Texte courant avec <b>gras</b> et <i>italique</i>.", body), Spacer(1, 8)]
data = [["Indicateur", "Valeur"], ["CA", "1 200"], ["Marge", "400"]]
t = Table(data, colWidths=[10 * cm, 4 * cm], repeatRows=1)
t.setStyle(TableStyle([("FONTNAME", (0, 0), (-1, -1), FONT), ("FONTNAME", (0, 0), (-1, 0), FONT + "-Bold"),
                       ("BACKGROUND", (0, 0), (-1, 0), colors.HexColor("#1B365D")),
                       ("TEXTCOLOR", (0, 0), (-1, 0), colors.white), ("ALIGN", (1, 0), (1, -1), "RIGHT"),
                       ("LINEBELOW", (0, 0), (-1, -1), 0.4, colors.HexColor("#D5DAE1"))]))
story += [t, Spacer(1, 12), Image("/workspace/.work/fig.png", width=15 * cm, height=15 * cm * 400 / 900)]
doc = SimpleDocTemplate("/workspace/.work/platypus.pdf", pagesize=A4, leftMargin=2.2 * cm, rightMargin=2.2 * cm,
                        topMargin=2.2 * cm, bottomMargin=2.2 * cm, title="Exemple", author="Studio")
doc.build(story, onFirstPage=footer, onLaterPages=footer)
```
"Page x of y" needs the total page count, so the canvas must defer its pages. Import the
ready-made canvas of `build_pdf.py`:

```python
# recipe: page-x-of-y
import sys
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle
from reportlab.platypus import PageBreak, Paragraph, SimpleDocTemplate

sys.path.insert(0, "/skills/pdf/scripts")
from build_pdf import numbered_canvas, register_fonts

sans, mono = register_fonts()                # embedded Liberation Sans / Mono ("StudioSans", "StudioMono")
body = ParagraphStyle("body", fontName=sans, fontSize=10.5, leading=14.5)
story = [Paragraph("First page", body), PageBreak(), Paragraph("Second page", body)]
doc = SimpleDocTemplate("/workspace/.work/numbered.pdf", pagesize=A4)
doc.build(story, canvasmaker=numbered_canvas("Monthly report", locale="en", font=sans))   # "Page 1 of 2"
```

## Canvas: letter / certificate layout

```python
# recipe: canvas
from reportlab.lib.pagesizes import A4
from reportlab.lib.units import cm
from reportlab.pdfgen import canvas

c = canvas.Canvas("/workspace/.work/letter.pdf", pagesize=A4)
w, h = A4                                       # 595 x 842 pt, origin bottom-left
c.setFont("Helvetica-Bold", 14)
c.drawString(2.2 * cm, h - 3 * cm, "Objet : confirmation")
c.setFont("Helvetica", 10.5)
text = c.beginText(2.2 * cm, h - 4.2 * cm)
for line in ["Madame, Monsieur,", "", "Nous vous confirmons la date du 6 octobre 2026."]:
    text.textLine(line)
c.drawText(text)
c.line(2.2 * cm, 3 * cm, w - 2.2 * cm, 3 * cm)
c.showPage()
c.save()
```
