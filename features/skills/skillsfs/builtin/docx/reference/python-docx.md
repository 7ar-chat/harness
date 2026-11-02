# python-docx recipes

Each block is self-contained and was run end to end in this sandbox image. Paths use
`/workspace/.work/` (scratch); write your deliverable to `/workspace/outputs/`.

## Fonts for every script, page setup, styles

```python
# recipe: setup
import os
from docx import Document
from docx.enum.section import WD_ORIENT
from docx.oxml.ns import qn
from docx.shared import Cm, Pt, RGBColor

os.makedirs("/workspace/.work", exist_ok=True)

def set_font(font_owner, name):
    """font_owner: a style (doc.styles['Normal']) or a run. Sets ascii/hAnsi/eastAsia/cs and
    removes the theme-font attributes, which win over w:ascii (Heading/Title styles of the
    default template use the theme fonts: without this they stay Calibri Light / Calibri)."""
    font_owner.font.name = name
    rpr = font_owner.element.get_or_add_rPr() if hasattr(font_owner, "element") else font_owner._r.get_or_add_rPr()
    rfonts = rpr.find(qn("w:rFonts"))
    if rfonts is None:
        rfonts = rpr.makeelement(qn("w:rFonts"), {})
        rpr.insert(0, rfonts)
    for attr in ("w:ascii", "w:hAnsi", "w:eastAsia", "w:cs"):
        rfonts.set(qn(attr), name)
    for attr in ("w:asciiTheme", "w:hAnsiTheme", "w:eastAsiaTheme", "w:cstheme"):
        rfonts.attrib.pop(qn(attr), None)

doc = Document()
sec = doc.sections[0]
sec.page_width, sec.page_height = Cm(21), Cm(29.7)          # A4
for side in ("left_margin", "right_margin", "top_margin", "bottom_margin"):
    setattr(sec, side, Cm(2.2))
normal = doc.styles["Normal"]
set_font(normal, "Arial")
normal.font.size = Pt(10.5)
normal.paragraph_format.space_after = Pt(6)
h1 = doc.styles["Heading 1"]
set_font(h1, "Arial")
h1.font.size, h1.font.bold, h1.font.color.rgb = Pt(16), True, RGBColor(0x1B, 0x36, 0x5D)
doc.add_heading("Introduction", level=1)
p = doc.add_paragraph("Body text with a ")
p.add_run("bold").bold = True
p.add_run(" word.")
doc.save("/workspace/.work/setup.docx")
```

## Insert after a paragraph (not at the end)

```python
# recipe: insert-after
import copy
from docx import Document
from docx.text.paragraph import Paragraph

doc = Document()
doc.add_heading("Section A", 1)
anchor = doc.add_paragraph("Existing paragraph.")
doc.add_heading("Section B", 1)

def insert_paragraph_after(paragraph, text="", style=None):
    new_p = paragraph._p.makeelement(paragraph._p.tag, {})
    paragraph._p.addnext(new_p)
    para = Paragraph(new_p, paragraph._parent)
    if text:
        para.add_run(text)
    if style is not None:
        para.style = style
    return para

new = insert_paragraph_after(anchor, "Inserted right after.", style=anchor.style)
bullet = insert_paragraph_after(new, "A bullet in the middle", style="List Bullet")
# move an existing table/paragraph: element.addnext(other_element) moves it
table = doc.add_table(rows=1, cols=2)
bullet._p.addnext(table._tbl)
doc.save("/workspace/.work/insert_after.docx")
assert [p.text for p in Document("/workspace/.work/insert_after.docx").paragraphs][2:4] == \
    ["Inserted right after.", "A bullet in the middle"]
```

Delete a paragraph: `p._p.getparent().remove(p._p)`. Delete a table: `t._tbl.getparent().remove(t._tbl)`.

## Tables: widths, merged cells, shading, header repeat, repeat a template row

```python
# recipe: tables
import copy
from docx import Document
from docx.enum.table import WD_TABLE_ALIGNMENT
from docx.enum.text import WD_ALIGN_PARAGRAPH
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Cm, Pt

doc = Document()
rows = [("North", 5100.0), ("South", 4200.5)]
t = doc.add_table(rows=1, cols=3)
t.style = "Table Grid"
t.alignment = WD_TABLE_ALIGNMENT.CENTER
for cell, text in zip(t.rows[0].cells, ("Region", "Revenue (k€)", "Share")):
    cell.text = text
    cell.paragraphs[0].runs[0].bold = True
total = sum(v for _, v in rows)
for name, value in rows:
    cells = t.add_row().cells
    cells[0].text = name
    cells[1].text = f"{value:,.1f}".replace(",", " ").replace(".", ",")
    cells[2].text = f"{value / total:.0%}"
    for c in cells[1:]:
        c.paragraphs[0].alignment = WD_ALIGN_PARAGRAPH.RIGHT
# column widths: Word reads the cell widths, LibreOffice (PDF export, previews) reads the
# table grid (w:gridCol): set both, plus the total table width.
def set_col_widths(table, widths):
    table.autofit = False
    for grid_col, w in zip(table._tbl.tblGrid.findall(qn("w:gridCol")), widths):
        grid_col.set(qn("w:w"), str(int(w.twips)))
    for row in table.rows:
        for cell, w in zip(row.cells, widths):
            cell.width = w
    tbl_w = table._tbl.tblPr.find(qn("w:tblW"))     # total width (python-docx tables have one)
    if tbl_w is not None:
        tbl_w.set(qn("w:type"), "dxa"); tbl_w.set(qn("w:w"), str(int(sum(w.twips for w in widths))))

set_col_widths(t, (Cm(7), Cm(5), Cm(4)))
# shade header + repeat it on every page
for cell in t.rows[0].cells:
    shd = OxmlElement("w:shd"); shd.set(qn("w:val"), "clear"); shd.set(qn("w:fill"), "1B365D")
    cell._tc.get_or_add_tcPr().append(shd)
    cell.paragraphs[0].runs[0].font.color.rgb = None
trpr = t.rows[0]._tr.get_or_add_trPr()
hdr = OxmlElement("w:tblHeader"); hdr.set(qn("w:val"), "true"); trpr.append(hdr)
# merged total row
row = t.add_row()
merged = row.cells[0].merge(row.cells[1])
merged.text = "Merged cell"
# repeat a template row (keeps its formatting): deep-copy the <w:tr>
template_tr = t.rows[1]._tr
new_tr = copy.deepcopy(template_tr)
template_tr.addnext(new_tr)
doc.save("/workspace/.work/tables.docx")
assert len(Document("/workspace/.work/tables.docx").tables[0].rows) == 5
```

## Images, captions, landscape section, headers/footers with page numbers

```python
# recipe: images-sections-footer
from docx import Document
from docx.enum.section import WD_ORIENT, WD_SECTION
from docx.enum.text import WD_ALIGN_PARAGRAPH
from docx.shared import Cm
from PIL import Image

Image.new("RGB", (800, 400), (46, 109, 164)).save("/workspace/.work/pic.png")
doc = Document()
doc.add_picture("/workspace/.work/pic.png", width=Cm(15))
doc.paragraphs[-1].alignment = WD_ALIGN_PARAGRAPH.CENTER
doc.add_paragraph("Figure 1 - Example", style="Caption")

# a landscape section for a wide table, then back to portrait
land = doc.add_section(WD_SECTION.NEW_PAGE)
land.orientation = WD_ORIENT.LANDSCAPE
land.page_width, land.page_height = land.page_height, land.page_width
doc.add_paragraph("Wide content here")
port = doc.add_section(WD_SECTION.NEW_PAGE)
port.orientation = WD_ORIENT.PORTRAIT
port.page_width, port.page_height = port.page_height, port.page_width
doc.add_paragraph("Back to portrait")

# Footer: ONE paragraph "Title · Page X of Y" on every section. set_footer() first deletes
# whatever the footer held, so a second "Page x of y" or a number glued to the title
# ("...Energy2") cannot happen. Never add your own PAGE field next to it.
import sys
sys.path.insert(0, "/skills/docx/scripts")
from footer import set_footer
set_footer(doc, "Project X report", "en", header="Project X - internal")   # fr -> "... · Page X sur Y"
doc.save("/workspace/.work/sections.docx")
```
New sections inherit ("link to previous") the header/footer: call `set_footer(doc, ...)`
after adding the sections (it rewrites the footer of every section). Existing file:
`python3 /skills/docx/scripts/footer.py FILE.docx --text "Title" --locale en --inplace`.
Do not hand-build page-number fields: a bare PAGE field after text renders as "Title2", and
appending a paragraph to a footer that already has one duplicates the page number.

## Table of contents field

```python
# recipe: toc
import subprocess
from docx import Document
from docx.oxml import OxmlElement
from docx.oxml.ns import qn

doc = Document()
doc.add_paragraph("Contents", style="TOC Heading")
p = doc.add_paragraph()
for kind, text in (("begin", None), ("instr", ' TOC \\o "1-3" \\h \\z \\u '), ("separate", None),
                   ("text", "Update the table of contents"), ("end", None)):
    run = p.add_run()
    if kind == "instr":
        it = OxmlElement("w:instrText"); it.set(qn("xml:space"), "preserve"); it.text = text; run._r.append(it)
    elif kind == "text":
        run.text = text
    else:
        fc = OxmlElement("w:fldChar"); fc.set(qn("w:fldCharType"), kind); run._r.append(fc)
for title in ("Context", "Analysis", "Recommendations"):
    doc.add_heading(title, 1)
    doc.add_paragraph("Text. " * 40)
doc.save("/workspace/.work/toc.docx")
# fill it with entries + page numbers (otherwise PDF/preview show the placeholder text)
subprocess.run(["python3", "/skills/docx/scripts/fill_toc.py", "/workspace/.work/toc.docx", "--inplace"], check=True)
```

## Hyperlink, page break, keep with next, line spacing

```python
# recipe: misc
from docx import Document
from docx.enum.text import WD_BREAK
from docx.opc.constants import RELATIONSHIP_TYPE as RT
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Pt

doc = Document()
p = doc.add_paragraph("See ")
r_id = p.part.relate_to("https://example.com", RT.HYPERLINK, is_external=True)
link = OxmlElement("w:hyperlink"); link.set(qn("r:id"), r_id)
run = OxmlElement("w:r"); rpr = OxmlElement("w:rPr")
style = OxmlElement("w:rStyle"); style.set(qn("w:val"), "Hyperlink"); rpr.append(style)
run.append(rpr); t = OxmlElement("w:t"); t.text = "example.com"; run.append(t); link.append(run)
p._p.append(link)
h = doc.add_heading("Stays with the next paragraph", 2)
h.paragraph_format.keep_with_next = True
body = doc.add_paragraph("Line spacing 1.15, 6 pt after.")
body.paragraph_format.line_spacing = 1.15
body.paragraph_format.space_after = Pt(6)
doc.add_paragraph().add_run().add_break(WD_BREAK.PAGE)
doc.add_paragraph("Next page")
doc.save("/workspace/.work/misc.docx")
```

## Reading details
- Iterate body content in order (paragraphs AND tables):
  `for el in doc.element.body.iterchildren(): el.tag.endswith('}p') / '}tbl'`.
- Style of a paragraph: `p.style.name`; outline level comes from heading styles.
- Core properties: `doc.core_properties.title/author/created`.
- Comments (python-docx >= 1.2): `doc.comments` / `doc.add_comment(runs, text, author)`.
