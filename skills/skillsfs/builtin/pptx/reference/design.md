# Slide design rules (detail)

## Grid (16:9, 13.333 x 7.5 in)
| Zone | Position (in) |
|---|---|
| Side margins | 0.6 left and right; content width 12.13 |
| Title band | top 0.4, height 0.85, text anchored bottom, 28 pt bold, navy |
| Accent rule | 0.9 x 0.05 teal bar under the title |
| Body | top ~1.45 to bottom 6.75 |
| Footer | y 7.05: footer text left (10 pt grey), slide number right |
| Two columns | each (12.13 - 0.5) / 2 = 5.8 wide, gap 0.5 |
| Chart + takeaway | chart 8.2 wide, takeaway panel 3.6 wide on the right |

## Typography
- One family (Arial). Sizes: title slide 40, slide title 28, subtitle/kicker 16-18, body 18-22,
  table 12-16, captions/sources 12, KPI numbers 40-44.
- **Font-size rule (the only one; `fit_text.py` checks it as `min_font`):** no slide text below
  12 pt; the footer and the slide number may be 10 pt. Body text 18 pt or more. If text only
  fits below 12 pt, there is too much of it: shorten it or split the slide.
- Left-align text (centre only short titles on title/section slides and KPI cards).
- Line length <= ~70 characters; break long sentences into two bullets or shorten.
- Bold for the few words that matter; never underline (reads as a link); no ALL CAPS sentences.

## Content
- Title = the message (assertion), body = the evidence.
- Numbers: same decimals within a slide, units in the title/subtitle or the axis, thousands
  separators in the audience's locale.
- Charts: one message per chart, highlight the relevant series/bar in the accent colour and
  mute others, direct labels instead of legends when <= 4 series, no 3D, no pie with > 5 slices
  (use a bar chart), start bar axes at 0.
- Tables: <= 7 rows x 6 columns on a slide; more goes to an appendix or a document.
- Images: high resolution (>= 150 dpi at displayed size), never stretched, cropped to purpose.
- Colour: navy for structure, teal/amber accent for emphasis, greys for secondary info,
  green/red only for good/bad and always with a sign (+/-) or label (colour-blind readers).

## Checklist before delivery
- [ ] Every content slide has a message title; titles are parallel in style.
- [ ] No slide has > 6 bullets; no text below 12 pt (footer 10 pt).
- [ ] `python3 /skills/pptx/scripts/fit_text.py DECK.pptx` prints `"ok": true`.
- [ ] `view_file` on the deck: consistent title position, alignment, colours; no template leftovers.
- [ ] Slide numbers and footer on content slides, not on the title slide.
- [ ] Speaker notes present if the user presents the deck.
- [ ] Sources under every chart/table using external data.
