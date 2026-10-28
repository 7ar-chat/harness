# xlsxwriter quick reference (create-only)

xlsxwriter writes new files only (it cannot open an existing workbook). Strengths: speed,
`constant_memory` mode for big data, rich chart formatting, sparklines, data bars, comments.

```python
# recipe: xlsxwriter
import os
import xlsxwriter

os.makedirs("/workspace/.work", exist_ok=True)
wb = xlsxwriter.Workbook("/workspace/.work/report_xw.xlsx")
ws = wb.add_worksheet("Sales")
hdr = wb.add_format({"bold": True, "font_color": "#FFFFFF", "bg_color": "#1B365D", "align": "center", "font_name": "Arial"})
num = wb.add_format({"num_format": "#,##0", "font_name": "Arial"})
pct = wb.add_format({"num_format": "0.0%", "font_name": "Arial"})
rows = [("North", 5100, 4600), ("South", 4200, 3650), ("East", 3900, 4010)]
ws.write_row(0, 0, ["Region", "2026", "2025", "Growth"], hdr)
for i, (region, cur, prev) in enumerate(rows, start=1):
    ws.write(i, 0, region)
    ws.write_number(i, 1, cur, num)
    ws.write_number(i, 2, prev, num)
    ws.write_formula(i, 3, f"=IFERROR(B{i + 1}/C{i + 1}-1,0)", pct)
ws.write(4, 0, "Total", wb.add_format({"bold": True}))
ws.write_formula(4, 1, "=SUM(B2:B4)", num)
ws.write_formula(4, 2, "=SUM(C2:C4)", num)
ws.write_formula(4, 3, "=IFERROR(B5/C5-1,0)", pct)
ws.set_column("A:A", 14)
ws.set_column("B:D", 12)
ws.freeze_panes(1, 0)
ws.autofilter("A1:D4")
ws.conditional_format("D2:D4", {"type": "cell", "criteria": "<", "value": 0, "format": wb.add_format({"font_color": "#C62828"})})
chart = wb.add_chart({"type": "column"})
for col, color in (("B", "#1B365D"), ("C", "#8DB8DE")):
    chart.add_series({"name": f"=Sales!${col}$1", "categories": "=Sales!$A$2:$A$4",
                      "values": f"=Sales!${col}$2:${col}$4", "fill": {"color": color}})
chart.set_title({"name": "Revenue by region"})
chart.set_legend({"position": "bottom"})
chart.set_y_axis({"num_format": "#,##0", "major_gridlines": {"visible": True, "line": {"color": "#D5DAE1"}}})
ws.insert_chart("F2", chart, {"x_scale": 1.3, "y_scale": 1.1})
wb.close()
```
- xlsxwriter stores 0 as the result of every formula (even `=A2&" units"`): until `recalc.py` runs,
  previews, pandas and `read_file` show 0. Always run `recalc.py` afterwards.
- `ws.write(row, col, "5100")` writes TEXT, and `=SUM(...)` over text is 0 (`recalc.py` reports
  `numbers_as_text`). Use `write_number`, real `int`/`float` values, or
  `xlsxwriter.Workbook(path, {"strings_to_numbers": True})`.
- `wb = xlsxwriter.Workbook(path, {"constant_memory": True})` for very large sheets (write rows
  in order).
