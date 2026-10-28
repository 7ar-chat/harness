# Chart recipes

Every code block below was run end to end in this sandbox image; copy it and adapt the paths.

## Heatmap, small multiples, annotation

```python
# recipe: custom
import os
import sys
import numpy as np
import pandas as pd

sys.path.insert(0, "/skills/charts/scripts")
import make_chart as mc

os.makedirs("/workspace/.work", exist_ok=True)
import seaborn as sns

fig, ax = mc.figure("doc")
df = pd.DataFrame(np.random.default_rng(1).uniform(0, 1, (5, 6)), index=list("ABCDE"),
                  columns=[f"M{i}" for i in range(1, 7)])
sns.heatmap(df, ax=ax, cmap="Blues", annot=True, fmt=".0%", cbar=False, linewidths=0.5, linecolor="white")
ax.grid(False)
mc.finish(fig, ax, title="Utilisation by team and month", subtitle="% of capacity")
mc.save(fig, "/workspace/.work/heatmap.png")

import matplotlib.pyplot as plt
mc.setup()
fig, axes = plt.subplots(1, 3, figsize=mc.SIZES["slide"], sharey=True)
x = np.arange(12)
for i, (ax, name) in enumerate(zip(axes, ("North", "South", "East"))):
    y = 100 + np.cumsum(np.random.default_rng(i).normal(1, 3, 12))
    ax.plot(x, y, color=mc.colors()[i])
    ax.set_title(name, fontsize=11)
    ax.annotate(f"{y[-1]:.0f}", (x[-1], y[-1]), xytext=(4, 0), textcoords="offset points", va="center", fontsize=9)
fig.suptitle("All regions grew in 2026", x=0.01, ha="left", fontweight="bold")
fig.tight_layout()
mc.save(fig, "/workspace/.work/multiples.png")
```

## Embedding

```python
# recipe: embedding
import os
import subprocess
import sys

sys.path.insert(0, "/skills/charts/scripts")
import make_chart as mc

os.makedirs("/workspace/.work", exist_ok=True)
spec = {"type": "bar", "title": "Revenue by region", "categories": ["North", "South", "East"],
        "series": [{"name": "2026", "values": [5.1, 4.2, 3.9]}], "highlight": "South", "data_labels": True,
        "decimals": 1}
for size in ("doc", "slide"):
    fig = mc.render(dict(spec, size=size))
    mc.save(fig, f"/workspace/.work/chart_{size}.png")

# Word
from docx import Document
from docx.shared import Cm
doc = Document()
doc.add_paragraph("Figure 1 shows the split.")
doc.add_picture("/workspace/.work/chart_doc.png", width=Cm(16))
doc.add_paragraph("Figure 1 - Revenue by region (M€)", style="Caption")
doc.save("/workspace/.work/with_chart.docx")

# PowerPoint: fit the picture inside the slide body box (a "full width" picture of a wide image
# keeps its ratio and runs off the bottom of the slide); never pass a guessed width AND height
from PIL import Image
from pptx import Presentation
from pptx.util import Inches
prs = Presentation()
prs.slide_width, prs.slide_height = Inches(13.333), Inches(7.5)
slide = prs.slides.add_slide(prs.slide_layouts[6])
tb = slide.shapes.add_textbox(Inches(0.6), Inches(0.4), Inches(12.13), Inches(0.85))
tb.text_frame.text = "South leads the regions"

def add_picture_fit(slide, png, x, y, w, h):
    """Picture inside the box (inches), centred, aspect ratio kept."""
    with Image.open(png) as im:
        iw, ih = im.size
    scale = min(w / iw, h / ih)
    pw, ph = iw * scale, ih * scale
    return slide.shapes.add_picture(png, Inches(x + (w - pw) / 2), Inches(y + (h - ph) / 2), Inches(pw), Inches(ph))

add_picture_fit(slide, "/workspace/.work/chart_slide.png", 0.6, 1.5, 12.13, 5.25)    # body: 0.6 in margins, above the footer
prs.save("/workspace/.work/with_chart.pptx")

# Excel
from openpyxl import Workbook
from openpyxl.drawing.image import Image
wb = Workbook()
img = Image("/workspace/.work/chart_doc.png")
img.width, img.height = img.width * 0.5, img.height * 0.5     # 200-dpi PNG shown at ~100 dpi
wb.active.add_image(img, "B2")
wb.save("/workspace/.work/with_chart.xlsx")
```
