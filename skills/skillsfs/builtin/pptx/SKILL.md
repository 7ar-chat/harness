---
name: pptx
description: Create, edit and check PowerPoint decks (.pptx) - presentations, pitches, steering-committee or training decks, speaker notes. Use it to build a new deck (JSON spec builder with native charts and tables, Studio grid and fonts), to change an existing deck while keeping its formatting (cross-run text replace, add/duplicate/reorder/delete slides, charts, tables, images, templates), and to check the layout (overflow, overlaps, off-slide or stretched images, font sizes, titles). To only read or summarise a .pptx, call read_file on it; you do not need this skill for that.
license: Internal (Studio)
requires: [studio-brand]
---

# PowerPoint decks (.pptx)

**FINISH GATE - do this before you say anything about the deck to the user.**
1. Check: `python3 /skills/pptx/scripts/fit_text.py /workspace/outputs/DECK.pptx`.
   The runtime already runs it after every tool call that changes a file in `/workspace/outputs/` and adds the
   verdict to that call's result (`verification`): when the verdict of your last change is there, do not run it again.
2. If it is `"ok": false` (exit code 2): fix every check with `"ok": false` (the `slides` list
   says which slide and shape), save, and check again. At most 3 rounds.
3. When it is `"ok": true`: call `view_file` on `/workspace/outputs/DECK.pptx` once (6 slides per
   call, `pages: "7-12"` for the next ones) and look: nothing cut, overlapping or unreadable,
   titles aligned, no leftover template text. Fix what looks wrong (then steps 1-3 again).
   If `view_file` answers that this model cannot view images, skip this step and say so; if it cannot
   render the deck, say "could not view: <its reason>".
4. Cross-check the content against the files it comes from:
   `python3 /skills/pptx/scripts/cross_check.py /workspace/outputs/DECK.pptx SOURCE [SOURCE ...]` lists the
   figures, dates and names (milestones, risks, owners) that are in none of them. Check each one in the
   source (`search_files` / `read_file`) and fix the wrong ones; also check that every item the user
   asked for (each milestone, the top risks, the decisions) is on a slide. Page citations of a PDF
   ("(p. 12)"): `python3 /skills/pdf/scripts/check_citations.py SOURCE.pdf /workspace/outputs/DECK.pptx`.
5. Never say the deck is done, ready or created while `"ok": false`. After 3 rounds, tell the
   user exactly which slides still fail which check. In your answer, say what you cross-checked
   against which file (and which figures are your own computations).

Only a changed file needs a new check or view: a passing check and a `view_file` after a file's last change stay
valid. Never check or view again a file you did not change since (one `view_file` per file after its last change).

**Files.** Save the final deck in `/workspace/outputs/` (or the path the user asked for).
Never put a deliverable in `/workspace/.work/` and never link a file there: it is scratch space
(specs, chart PNGs) and is not kept. In your answer, only link files under `/workspace/outputs/`.

**Reading.** `read_file` returns the text of a .pptx (slides and notes) directly. Use
`inspect_pptx.py` only for shapes, positions, placeholders and layouts.

Scripts are in `/skills/pptx/scripts/`; run them with `python3` from `/workspace`. They print
JSON; on bad input they exit non-zero with an `error` message.

## 0. Decide

| The user wants | Do |
|---|---|
| A new deck | §2 plan, then §3a `build_deck.py` |
| Changes to an existing deck or a template | §4 |
| Always, at the end | the FINISH GATE (§5), then §6 Deliver |

## 1. Structure of an existing deck

```bash
python3 /skills/pptx/scripts/inspect_pptx.py DECK.pptx --outline   # one line per slide (+ [notes])
python3 /skills/pptx/scripts/inspect_pptx.py DECK.pptx             # shapes, text, tables, charts, positions
python3 /skills/pptx/scripts/inspect_pptx.py DECK.pptx --layouts   # template layouts and placeholders
```

## 2. Plan first

Write a slide-by-slide plan: one **message title** per slide (a sentence with the takeaway:
"Costs fell 8% after the migration", not "Costs"), the content type, the speaker notes.
1. One idea per slide; at most 6 bullets of about 12 words, 2 levels.
2. **Font sizes (one rule, checked as `min_font`): nothing below 12 pt; the footer and slide
   number may be 10 pt. Body text 18 pt or more, titles 28 pt.** If text only fits below 12 pt,
   shorten it or split the slide.
3. Numbers -> a chart, a table or KPI cards rather than bullets.
4. Same grid on every slide (0.6 in margins, same title position, fonts, colours). Max 3 colours + greys.
5. Speaker notes on every content slide when the user will present.
Details: `reference/design.md`.

## 3. Build

### 3a. Default for every new deck: JSON spec -> `build_deck.py`
Native, editable charts and tables, wrapped table cells with row heights from the text, the
Studio grid, footer and slide numbers; it runs the same checks as `fit_text.py` at the end.
```json
{
  "footer": "Renewable energy - briefing",
  "slides": [
    {"type": "title", "title": "Renewable energy: why it pays off", "subtitle": "Briefing for the board", "date": "6 October 2026"},
    {"type": "bullets", "title": "Clean power now beats fossil fuels on cost",
     "bullets": ["Solar and onshore wind are the cheapest new power", "No fuel bill, so prices are stable",
                 {"text": "Storage covers the evening peak", "level": 1}],
     "notes": "Start with cost: it is the argument that convinces."},
    {"type": "table", "title": "Renewables create local value long before the savings",
     "columns": ["Benefit", "How it helps", "Typical impact"],
     "rows": [["Jobs and investment", "Construction, operations, maintenance and manufacturing create local employment.", "Boosts local economies"],
              ["Energy security", "Diversifies supply and reduces exposure to imported fuels.", "Improves resilience"],
              ["Cost stability", "Wind and sunlight have no fuel cost.", "Less price volatility"]],
     "col_widths_in": [2.8, 5.8, 3.5]},
    {"type": "chart", "title": "Renewables keep growing their share",
     "chart": {"type": "column", "categories": ["2022", "2023", "2024", "2025"],
               "series": [{"name": "Share of new capacity (%)", "values": [78, 83, 86, 90]}],
               "number_format": "0", "data_labels": true},
     "takeaway": "Nine in ten new plants are now renewable."}
  ]
}
```
```bash
python3 /skills/pptx/scripts/build_deck.py --spec /workspace/.work/deck.json --out /workspace/outputs/deck.pptx
python3 /skills/pptx/scripts/build_deck.py --example     # every slide type
```
- Slide types: `title`, `section`, `bullets`, `two_column` (left/right: bullets | text | image |
  chart), `image`, `chart` (column, stacked_column, bar, stacked_bar, line, pie, doughnut;
  optional `takeaway`), `table` (`columns`, `rows`, `col_widths_in`, `numeric_cols`, `font_size`),
  `kpi` (up to 4 cards), `quote`, `text`. Every slide takes `notes`. Top level: `footer`, `numbers`, `template`.
- Shorthand works: `{"bullets": {"title": "...", "bullets": [...]}}`, `{"section": "Next steps"}`.
  A bad spec stops with the accepted shapes: fix the spec.
- Tables: give `col_widths_in` (total <= 12.1) with the widest column for the longest text. If
  the result reports the table too tall, split the rows over two slides.
- The result has `ok`, `checks`, `next_step` and `issues`: `"ok": false` means fix the spec
  (shorter text, split the slide) and rebuild.
- Image charts (heatmaps, complex plots): a PNG from the `charts` skill (size `slide` or
  `slide-half`) in `/workspace/.work/`, used in an `image` slide or column (fitted, ratio kept).

### 3b. Hand-written python-pptx code: only for edits
Hand-written python-pptx code is only for editing an existing deck (§4) or a layout the builder cannot
express. Tested patterns in `reference/python-pptx.md`. Rules: 16:9 (`prs.slide_width = Inches(13.333)`,
`prs.slide_height = Inches(7.5)`) before adding slides; native tables (`shapes.add_table`), never
rectangles + text boxes; `text_frame.word_wrap = True` on every text box; pictures with
`add_picture_fit` (fitted inside their box: a full-width picture runs off the slide, a guessed
width and height stretches it); delete placeholders you do not fill. A deck you create from scratch
this way gets `prs.core_properties.identifier = "studio-work:agent-deck"` (as `build_deck.py` does):
`fit_text.py` then fails its overlaps; in decks without it (uploads) overlaps are only warnings.

### 3c. pptxgenjs alternative
`reference/pptxgenjs.md` (layout, text, charts, tables, images with `fitImage`, notes). Same rules, same FINISH GATE.

## 4. Edit an existing deck

1. Work on a copy in `/workspace/outputs/`; never overwrite the upload.
2. `inspect_pptx.py DECK.pptx` to see shape names and placeholder idx on each slide.
3. Tools:
   ```bash
   python3 /skills/pptx/scripts/replace_text.py DECK.pptx --inplace --find "Q2 2026" --replace "Q3 2026"   # keeps run formatting
   python3 /skills/pptx/scripts/replace_text.py DECK.pptx --inplace --pairs /workspace/.work/pairs.json --notes
   python3 /skills/pptx/scripts/slides.py DECK.pptx list
   python3 /skills/pptx/scripts/slides.py DECK.pptx duplicate 3 --inplace      # copy after slide 3
   python3 /skills/pptx/scripts/slides.py DECK.pptx move 7 2 --inplace
   python3 /skills/pptx/scripts/slides.py DECK.pptx delete 4,5 --inplace
   python3 /skills/pptx/scripts/slides.py DECK.pptx reorder 1,3,2,4,5 --inplace
   python3 /skills/pptx/scripts/slides.py DECK.pptx keep 1,2,6 --out /workspace/outputs/short.pptx
   ```
   Anything else (chart data with `chart.replace_data(...)`, a table cell, a slide from a layout):
   python-pptx code. To change text but keep its formatting set `run.text`;
   `text_frame.text = ...` resets it.
4. New text follows the deck's fonts and sizes (read them with `inspect_pptx.py`), not Studio's.
5. Run the FINISH GATE after every text change: translations grow and overflow.

## 5. Verify (the FINISH GATE in detail)

`fit_text.py DECK.pptx` (same result: `inspect_pptx.py --check DECK.pptx`) runs these checks:

| Check | Fails when | Fix |
|---|---|---|
| `opens` | not a readable .pptx, or no slide | rebuild it |
| `text_fits` | text taller than its box, or no-wrap / long word wider than it | shorten, wrap on, bigger box, split the slide |
| `tables_fit` | wrapped cell text taller than its row | row heights from the text (`build_deck.py`), wider column, fewer rows |
| `inside_slide` | a shape, picture or group child outside the slide | move or resize it; fit pictures in their box |
| `overlap` | two shapes overlap (one not inside the other) in a deck the agent made (`build_deck.py`, or identifier `studio-work:agent-deck`, §3b); in other decks (uploaded) it is a warning (`"detail": "warning: ..."`), and with `fit_text.py --baseline ORIGINAL.pptx` only overlaps the original did not have fail | move them apart or align them |
| `images` | a picture is stretched (ratio differs from the image) | width and height from the image ratio |
| `min_font` | text below 12 pt (footer below 10 pt); a smaller size inherited from the template's layout/master is only a warning | bigger font, less text, split the slide |
| `titles` | a slide without any text, or an empty title placeholder | write the message title |
| `placeholders` | an empty placeholder ("Click to add text") | fill it or delete it |
| `slide_numbers` | a slide-number field still stores another slide's number (copied with a template slide) | `fit_text.py DECK.pptx --fix --inplace` rewrites it |

When you edit a deck the user gave you, keep a copy of the original (`cp DECK.pptx /workspace/.work/original.pptx`)
and check with `fit_text.py DECK.pptx --baseline /workspace/.work/original.pptx`: only the overlaps you added fail.
The runtime's own check does this by itself with the uploaded deck (same name, or the upload your file was copied from).
`--fix --inplace` shrinks overflowing text (never below 12 pt); check again afterwards. The text
height is an estimate (+/- one line): look at the slides with `view_file`.

## 6. Deliver

Summarise the deck (slide count, structure) and link it:
`[deck.pptx](workspace-file:///workspace/outputs/deck.pptx)`. Link only files that exist under
`/workspace/outputs/`. A PDF on request:
`soffice --headless --convert-to pdf --outdir /workspace/outputs /workspace/outputs/deck.pptx`, then link it too.

## Pitfalls
- python-pptx's default template is 4:3; layout placeholders keep their 4:3 positions: set every position explicitly.
- `add_textbox` does not wrap by default.
- Chart number formats are Excel formats (`'0.0'`, `'0%'`); set `number_format_is_linked = False`.
- Use Arial (studio-brand) or the template's fonts: other fonts are substituted at render time.
