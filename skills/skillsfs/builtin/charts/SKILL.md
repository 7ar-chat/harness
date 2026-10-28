---
name: charts
description: Make clear static charts (bar, stacked, horizontal bar, line, area, scatter, pie/donut, waterfall, or any custom matplotlib/seaborn figure) as PNG (200 dpi) or SVG, sized for slides and A4 documents, in the Studio palette, check them (size, dpi, not blank) and embed them in Word, PowerPoint, Excel or PDF deliverables. Use it whenever the user asks for a chart, graph, plot or visualisation, or a document or deck needs a figure. For editable charts inside PowerPoint or Excel see the pptx/xlsx skills (native charts).
license: Internal (Studio)
requires: [studio-brand]
---

# Charts (matplotlib / seaborn)

**FINISH GATE - do this for every chart before you embed it or show it to the user.**
1. Run `python3 /skills/charts/scripts/make_chart.py --check /workspace/.work/CHART.png`.
2. If it prints `"ok": false` (exit code 2): fix every check with `"ok": false`, render again, and run
   the same command again. At most 3 rounds.
3. When it prints `"ok": true`: call `view_file` on the PNG once and look: title readable, no clipped
   labels or legend, numbers in the user's locale, units present, no overlapping tick labels.
   If `view_file` answers that this model cannot view images, skip this step and say so.
4. After embedding, run the host document's FINISH GATE (docx, pptx or pdf skill).

**Files.** A chart that only goes inside a document or deck is an intermediate file: save it in
`/workspace/.work/`, embed it, and do not link it. A chart the user wants as a file is a deliverable:
save it in `/workspace/outputs/`. Never put a deliverable in `/workspace/.work/` and never link a file
there (it is scratch space and is not kept). In your answer, only link files under `/workspace/outputs/`.

Script: `/skills/charts/scripts/make_chart.py` (CLI + importable helpers). It uses the
`studio-brand` palette and fonts when that skill is installed, an identical built-in fallback otherwise.

## 0. Image chart or native chart?

| Need | Use |
|---|---|
| Figure in a Word document or PDF | PNG from this skill |
| Chart the user will edit in PowerPoint / Excel | native chart: `pptx` skill (`build_deck.py`) / `xlsx` skill (openpyxl charts) |
| Heatmap, distribution, small multiples, annotations | this skill, custom matplotlib/seaborn (§3) |
| Interactive HTML | plotly `fig.write_html`, only if the user asks |

## 1. Choose the chart from the message

- Compare categories -> bar (`barh` when labels are long or > 7 categories). Change over time ->
  line (bars for few periods). Share of a whole -> stacked bar, or a donut for <= 5 parts.
  Bridge from A to B -> waterfall. Relationship -> scatter.
- The title states the finding ("Q3 revenue up 12%"), the subtitle gives unit and scope
  ("M€, 2025-2026"), a source line when the data is external.
- Highlight what matters (`highlight`), mute the rest. No 3D, no dual axes unless asked, bar axes
  start at zero, at most 6-8 series.

## 2. Make it (CLI)

```bash
python3 /skills/charts/scripts/make_chart.py --example > /workspace/.work/chart.json
python3 /skills/charts/scripts/make_chart.py --spec /workspace/.work/chart.json --out /workspace/.work/revenue.png
python3 /skills/charts/scripts/make_chart.py --csv data.csv --x month --y revenue,cost --type line \
    --title "Revenue outpaces cost" --subtitle "k€" --size doc --y-format number --out /workspace/.work/rc.png
```
Spec keys: `type` (bar, barh, stacked_bar, line, area, scatter, pie, donut, waterfall), `title`,
`subtitle`, `categories`, `series` [{name, values, color?}] (scatter: `x` per series), `y_format`
number|percent (ratios)|eur, `decimals`, `locale` fr|en, `size`, `data_labels`, `legend`,
`highlight`, `source`, `ylim`, `x_label`, `y_label`. Waterfall: first value = start, last = end total.

Sizes (inches, 200 dpi): `slide` 11.5 x 5.2, `slide-half` 5.8 x 4.8, `doc` 6.5 x 3.6 (A4 text
width), `doc-half` 3.2 x 2.6, `square` 5 x 5, or `[w, h]`. Make the figure at its final size:
never shrink a big figure into a small slot or stretch it.

## 3. Custom figures (Python)

```python
import sys
sys.path.insert(0, "/skills/charts/scripts")
import make_chart as mc
fig, ax = mc.figure("doc")                         # Studio style, Agg backend
ax.plot(x, y, label="Actual")
ax.yaxis.set_major_formatter(mc.axis_formatter("percent", 0, "fr"))
mc.finish(fig, ax, title="Margin back above target", subtitle="%", source="Source: finance")
info = mc.save(fig, "/workspace/.work/margin.png")  # {"out", "width_px", "height_px", "dpi"}
```
seaborn works on top (`sns.heatmap(df, ax=ax, cmap="Blues")`) after `mc.setup()` or `mc.figure`.
Colours: `mc.colors()`. More patterns: `reference/recipes.md`.

## 4. Embed

- Word: `new_document.py` `image` block `{"path": png, "width_cm": 16, "caption": "..."}`, or
  python-docx `doc.add_picture(png, width=Cm(16))` + a `Caption` paragraph (7.8 cm for half width).
- PowerPoint: `build_deck.py` `image` slide or `two_column` with `"image": png`; python-pptx: fit the
  picture inside the body box (`add_picture_fit` in `reference/recipes.md` "Embedding"): a picture
  given only the slide width keeps its ratio and runs off the slide.
- Excel: `ws.add_image(Image(png), "H2")` with `img.width, img.height = img.width * 0.5, img.height * 0.5` for a 200-dpi PNG.
- PDF: `build_pdf.py` `image` block, or reportlab `Image(png, width=15*cm, height=...)` from the ratio.

## 5. Verify (the FINISH GATE in detail)

`make_chart.py --check IMAGE` checks: `opens` (a PNG/JPEG, or an SVG with drawing elements), `size`
(>= 600 x 300 px), `dpi` (>= 150 when recorded), `not_blank` (something is plotted), `aspect`
(between 1:4 and 4:1). Fix: render at the final size with dpi 200, plot the data, fix the figure size.
Images not rendered by `make_chart.py` (a logo, a photo) only get `opens` and `not_blank`.

## 6. Deliver

Link the image only if the chart itself is the deliverable (in `/workspace/outputs/`):
`[revenue.png](workspace-file:///workspace/outputs/revenue.png)`; otherwise link only the document
that contains it. Never link the PNG in `/workspace/.work/`.

## Pitfalls
- `mc.setup()` sets the Agg backend (there is no display); `mc.save` closes the figure.
- Percent data as ratios (0.12) with `y_format: percent`; do not multiply by 100.
- Boxes instead of characters: the font lacks them; Liberation Sans / DejaVu Sans cover Latin,
  Greek and Cyrillic, not CJK.
- SVG keeps text as text: use PNG for Office documents.
