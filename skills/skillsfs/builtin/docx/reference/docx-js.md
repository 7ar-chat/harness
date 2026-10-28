# docx (npm) quick reference

The `docx` package (v9) is installed globally; `NODE_PATH` is set, so `require("docx")` works
from any directory. Use it when you prefer JavaScript or need native footnotes, custom
multi-level numbering or precise section/tab layouts. Save scripts as `.js` (CommonJS) and run
`node script.js`.

```javascript
// recipe-js: report
const fs = require("fs");
const {
  Document, Packer, Paragraph, TextRun, HeadingLevel, AlignmentType, Table, TableRow, TableCell,
  WidthType, ShadingType, Footer, PageNumber, TableOfContents, LevelFormat, ImageRun,
} = require("docx");

const navy = "1B365D";
const cell = (text, opts = {}) => new TableCell({
  width: { size: opts.width || 4500, type: WidthType.DXA },
  shading: opts.header ? { type: ShadingType.CLEAR, fill: navy, color: "auto" } : undefined,
  children: [new Paragraph({
    alignment: opts.right ? AlignmentType.RIGHT : AlignmentType.LEFT,
    children: [new TextRun({ text, bold: !!opts.header, color: opts.header ? "FFFFFF" : undefined })],
  })],
});

const doc = new Document({
  creator: "Studio",
  title: "Example report",
  styles: {
    default: { document: { run: { font: "Arial", size: 21 } } },          // size in half-points
    paragraphStyles: [
      { id: "Heading1", name: "Heading 1", basedOn: "Normal", next: "Normal", quickFormat: true,
        run: { size: 32, bold: true, color: navy }, paragraph: { spacing: { before: 360, after: 120 }, outlineLevel: 0 } },
    ],
  },
  numbering: {
    config: [{ reference: "steps", levels: [{ level: 0, format: LevelFormat.DECIMAL, text: "%1.",
      alignment: AlignmentType.START, style: { paragraph: { indent: { left: 720, hanging: 360 } } } }] }],
  },
  features: { updateFields: true },            // Word refreshes the TOC when the file is opened
  sections: [{
    properties: { page: { size: { width: 11906, height: 16838 },             // A4 in twips
                           margin: { top: 1247, bottom: 1247, left: 1247, right: 1247 } } },
    // footer: ONE paragraph "Title · Page X of Y" (never a second paragraph with another page number)
    footers: { default: new Footer({ children: [new Paragraph({ alignment: AlignmentType.RIGHT,
      children: [new TextRun({ children: ["Report · Page ", PageNumber.CURRENT, " of ", PageNumber.TOTAL_PAGES], size: 16 })] })] }) },
    children: [
      new TableOfContents("Contents", { hyperlink: true, headingStyleRange: "1-3" }),
      new Paragraph({ text: "Summary", heading: HeadingLevel.HEADING_1 }),
      new Paragraph({ children: [new TextRun("Revenue grew "), new TextRun({ text: "12%", bold: true }), new TextRun(".")] }),
      new Paragraph({ text: "First step", numbering: { reference: "steps", level: 0 } }),
      new Paragraph({ text: "Second step", numbering: { reference: "steps", level: 0 } }),
      new Paragraph({ text: "A bullet", bullet: { level: 0 } }),
      new Table({
        width: { size: 9000, type: WidthType.DXA },
        columnWidths: [6000, 3000],                 // the table grid (LibreOffice/PDF use it): same widths as the cells
        rows: [
          new TableRow({ tableHeader: true, children: [cell("Region", { header: true, width: 6000 }),
                                                       cell("Revenue", { header: true, right: true, width: 3000 })] }),
          new TableRow({ children: [cell("North", { width: 6000 }), cell("5 100", { right: true, width: 3000 })] }),
        ],
      }),
    ],
  }],
});

Packer.toBuffer(doc).then((buf) => {
  fs.mkdirSync("/workspace/.work", { recursive: true });
  fs.writeFileSync("/workspace/.work/report_js.docx", buf);
  console.log(JSON.stringify({ out: "/workspace/.work/report_js.docx" }));
});
```

Notes
- Units: font `size` in half-points (21 = 10.5 pt); widths/margins in twips (1 cm = 567).
- Tables: give `columnWidths` (twips, one per column) AND the same `width` on each cell. Word
  reads the cells, LibreOffice (PDF export, previews) reads the grid built from `columnWidths`.
- Images: `new ImageRun({ type: "png", data: fs.readFileSync(path), transformation: { width: 600, height: 300 } })`
  (pixels at 96 dpi; keep the aspect ratio).
- `TableOfContents` is an empty field like in python-docx: run
  `python3 /skills/docx/scripts/fill_toc.py OUT.docx --inplace` afterwards so the PDF/preview
  shows entries.
- Then verify exactly like any other document (SKILL.md §6).
