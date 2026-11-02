# pptxgenjs quick reference

pptxgenjs 4 is installed globally (`require("pptxgenjs")`). Coordinates are inches on a
13.33 x 7.5 slide with `LAYOUT_WIDE`. Run with `node deck.js`.

```javascript
// recipe-js: deck
const fs = require("fs");
const pptxgen = require("pptxgenjs");

const pptx = new pptxgen();
pptx.layout = "LAYOUT_WIDE";                         // 16:9, 13.33 x 7.5 in
pptx.title = "Example deck";
const NAVY = "1B365D", TEAL = "2A9D8F", INK = "1A1F2B", GREY = "8C96A3";
const font = "Arial";

pptx.defineSlideMaster({
  title: "CONTENT",
  background: { color: "FFFFFF" },
  objects: [
    { rect: { x: 0.65, y: 1.32, w: 0.9, h: 0.05, fill: { color: TEAL } } },
    { text: { text: "Example", options: { x: 0.6, y: 7.05, w: 6, h: 0.3, fontFace: font, fontSize: 10, color: GREY } } },
  ],
  slideNumber: { x: 12.2, y: 7.05, w: 0.5, h: 0.3, fontFace: font, fontSize: 10, color: GREY, align: "right" },
});

let s = pptx.addSlide();
s.addShape(pptx.ShapeType.rect, { x: 0, y: 0, w: 0.35, h: 7.5, fill: { color: NAVY } });
s.addText("Quarterly review", { x: 1.1, y: 2.2, w: 11, h: 1.6, fontFace: font, fontSize: 40, bold: true, color: NAVY, valign: "bottom" });
s.addText("Q3 2026", { x: 1.1, y: 4.3, w: 11, h: 0.8, fontFace: font, fontSize: 20, color: "4A5568" });

s = pptx.addSlide({ masterName: "CONTENT" });
s.addText("Revenue grew every quarter", { x: 0.6, y: 0.4, w: 12.13, h: 0.85, fontFace: font, fontSize: 28, bold: true, color: NAVY, valign: "bottom" });
s.addChart(pptx.ChartType.bar, [
  { name: "2025", labels: ["Q1", "Q2", "Q3"], values: [10.2, 11.0, 12.1] },
  { name: "2026", labels: ["Q1", "Q2", "Q3"], values: [11.4, 12.5, 13.4] },
], { x: 0.6, y: 1.5, w: 8, h: 5, barDir: "col", chartColors: [NAVY, "2E6DA4"], showLegend: true, legendPos: "b",
     showValue: true, dataLabelFormatCode: "0.0", valGridLine: { color: "D5DAE1", size: 0.5 }, catAxisLabelFontFace: font,
     valAxisLabelFontFace: font, dataLabelFontSize: 11 });
s.addText([
  { text: "Best quarter on record", options: { bullet: true, breakLine: true } },
  { text: "Two new clients", options: { bullet: true } },
], { x: 9, y: 1.6, w: 3.7, h: 4.5, fontFace: font, fontSize: 18, color: INK, valign: "top" });
s.addNotes("Mention the two new clients by name.");

s = pptx.addSlide({ masterName: "CONTENT" });
s.addText("Detail by region", { x: 0.6, y: 0.4, w: 12.13, h: 0.85, fontFace: font, fontSize: 28, bold: true, color: NAVY, valign: "bottom" });
const head = { bold: true, color: "FFFFFF", fill: { color: NAVY } };
s.addTable([
  [{ text: "Region", options: head }, { text: "Revenue", options: { ...head, align: "right" } }],
  ["North", { text: "5.1", options: { align: "right" } }],
  ["South", { text: "4.2", options: { align: "right" } }],
], { x: 0.6, y: 1.6, w: 7, colW: [4.5, 2.5], fontFace: font, fontSize: 14, border: { type: "solid", color: "D5DAE1", pt: 0.5 } });

fs.mkdirSync("/workspace/.work", { recursive: true });
pptx.writeFile({ fileName: "/workspace/.work/deck_js.pptx" }).then((f) => console.log(JSON.stringify({ out: f })));
```

## Images: keep the aspect ratio

pptxgenjs cannot read an image's size: `addImage({ path, x, y, w, h })` with a guessed `w`/`h`
stretches the picture (`fit_text.py` reports it as `stretched`). Read the real size with
`sharp` (installed) and fit the image inside its box:

```javascript
// recipe-js: images
const fs = require("fs");
const pptxgen = require("pptxgenjs");
const sharp = require("sharp");

// x, y, w, h of `path` fitted inside `box` (inches), centred, ratio kept
async function fitImage(path, box) {
  const { width, height } = await sharp(path).metadata();
  const scale = Math.min(box.w / width, box.h / height);
  const w = width * scale, h = height * scale;
  return { path, x: box.x + (box.w - w) / 2, y: box.y + (box.h - h) / 2, w, h };
}

(async () => {
  fs.mkdirSync("/workspace/.work", { recursive: true });
  await sharp({ create: { width: 1600, height: 900, channels: 3, background: "#2A9D8F" } }).png().toFile("/workspace/.work/photo_wide.png");
  await sharp({ create: { width: 600, height: 900, channels: 3, background: "#1B365D" } }).png().toFile("/workspace/.work/photo_tall.png");
  const pptx = new pptxgen();
  pptx.layout = "LAYOUT_WIDE";
  const s = pptx.addSlide();
  s.addText("Both pictures keep their proportions", { x: 0.6, y: 0.4, w: 12.13, h: 0.85, fontFace: "Arial", fontSize: 28, bold: true, color: "1B365D", valign: "bottom" });
  s.addImage(await fitImage("/workspace/.work/photo_wide.png", { x: 0.6, y: 1.5, w: 7.6, h: 5.2 }));
  s.addImage(await fitImage("/workspace/.work/photo_tall.png", { x: 8.6, y: 1.5, w: 4.1, h: 5.2 }));
  const out = await pptx.writeFile({ fileName: "/workspace/.work/images_js.pptx" });
  console.log(JSON.stringify({ out }));
})();
```

Notes
- Never pass `w` and `h` you did not compute from the image; do not rely on `sizing` either
  (it needs the real size too).
- Then run `python3 /skills/pptx/scripts/fit_text.py DECK.pptx` exactly as for python-pptx decks.
