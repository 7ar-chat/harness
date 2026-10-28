# PDF forms

## Create a fillable form (AcroForm) with reportlab

```python
# recipe: make-form
import os
from reportlab.lib.pagesizes import A4
from reportlab.pdfgen import canvas

os.makedirs("/workspace/.work", exist_ok=True)
c = canvas.Canvas("/workspace/.work/form.pdf", pagesize=A4)
w, h = A4
c.setFont("Helvetica", 11)
c.drawString(60, h - 80, "Full name")
c.acroForm.textfield(name="full_name", tooltip="Full name", x=160, y=h - 92, width=300, height=20)
c.drawString(60, h - 130, "Country")
c.acroForm.choice(name="country", tooltip="Country", value="France", options=["France", "Germany", "Italy"],
                  x=160, y=h - 142, width=200, height=20)
c.drawString(60, h - 180, "I agree")
c.acroForm.checkbox(name="agree", tooltip="Agreement", x=160, y=h - 185, size=16, checked=False)
c.drawString(60, h - 230, "Plan")
for i, option in enumerate(("basic", "pro")):
    c.acroForm.radio(name="plan", value=option, selected=(i == 0), x=160 + i * 90, y=h - 235, size=16)
    c.drawString(182 + i * 90, h - 230, option)
c.showPage()
c.save()
```
Then: `fill_form.py /workspace/.work/form.pdf --list`.

## Fill with pypdf directly (what fill_form.py does)

```python
# recipe: fill-pypdf
from pypdf import PdfReader, PdfWriter

reader = PdfReader("/workspace/.work/form.pdf")
writer = PdfWriter(clone_from=reader)
writer.update_page_form_field_values(None, {"full_name": "Jeanne Martin", "country": "Italy",
                                            "agree": "/Yes", "plan": "/pro"}, auto_regenerate=False)
writer.set_need_appearances_writer(True)
with open("/workspace/.work/form_filled.pdf", "wb") as fh:
    writer.write(fh)
fields = PdfReader("/workspace/.work/form_filled.pdf").get_fields()
assert fields["full_name"]["/V"] == "Jeanne Martin"
```
Checkbox "on" values are form-specific (`/Yes`, `/On`, `/1`...): `fill_form.py --list` shows
them in `options`; `fill_form.py` maps `true` to the right one.

## Flat forms (no fields): write text at coordinates

```python
# recipe: flat-form
import io
import pdfplumber
from pypdf import PdfReader, PdfWriter
from reportlab.pdfgen import canvas

src = "/workspace/.work/letter.pdf"                 # any PDF; here the canvas recipe's letter
with pdfplumber.open(src) as pdf:
    page = pdf.pages[0]
    words = page.extract_words()
    label = next(wd for wd in words if wd["text"].startswith("Objet"))
    height = float(page.height)
# pdfplumber: top-left origin; reportlab: bottom-left -> y = height - bottom
x, y = label["x1"] + 120, height - label["bottom"]
buf = io.BytesIO()
c = canvas.Canvas(buf, pagesize=(float(page.width), height))
c.setFont("Helvetica", 10)
c.drawString(x, y + 1, "Ref. 2026-104")
c.save()
buf.seek(0)
writer = PdfWriter(clone_from=PdfReader(src))
writer.pages[0].merge_page(PdfReader(buf).pages[0])
with open("/workspace/.work/letter_filled.pdf", "wb") as fh:
    writer.write(fh)
```
Call `view_file` on the result and check the text sits on the line (or compare `extract.py --words` positions).

## Cropping a region before extracting text

```python
# recipe: crop
import pdfplumber

with pdfplumber.open("/workspace/.work/letter.pdf") as pdf:
    page = pdf.pages[0]
    left_half = page.crop((0, 0, float(page.width) / 2, float(page.height)))
    text = left_half.extract_text() or ""
```
