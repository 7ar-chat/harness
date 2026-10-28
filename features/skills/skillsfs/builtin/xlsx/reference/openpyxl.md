# openpyxl recipes

Every code block below was run end to end in this sandbox image; copy it and adapt the paths.

## Model with inputs, formulas, formats, table, conditional formatting, validation, chart

```python
# recipe: model
import datetime as dt
import os
from openpyxl import Workbook
from openpyxl.chart import BarChart, LineChart, Reference
from openpyxl.formatting.rule import CellIsRule, ColorScaleRule
from openpyxl.styles import Alignment, Border, Font, PatternFill, Side
from openpyxl.utils import get_column_letter
from openpyxl.workbook.defined_name import DefinedName
from openpyxl.worksheet.datavalidation import DataValidation
from openpyxl.worksheet.table import Table, TableStyleInfo

os.makedirs("/workspace/.work", exist_ok=True)
wb = Workbook()
inp = wb.active
inp.title = "Inputs"
inp["A1"], inp["B1"] = "Growth rate", 0.05
inp["A2"], inp["B2"] = "Tax rate", 0.25
inp["B1"].number_format = inp["B2"].number_format = "0.0%"
for c in ("B1", "B2"):
    inp[c].font = Font(color="2E6DA4")                     # blue = input
wb.defined_names["growth"] = DefinedName("growth", attr_text="Inputs!$B$1")

ws = wb.create_sheet("Plan")
headers = ["Month", "Revenue", "Cost", "Margin", "Margin %"]
ws.append(headers)
start = dt.date(2026, 1, 1)
for i in range(12):
    r = i + 2
    ws.cell(r, 1, dt.date(2026, i + 1, 1)).number_format = "mmm yyyy"
    ws.cell(r, 2, 1000 if i == 0 else f"=B{r - 1}*(1+growth)").number_format = "#,##0"
    ws.cell(r, 3, f"=B{r}*0.6").number_format = "#,##0"
    ws.cell(r, 4, f"=B{r}-C{r}").number_format = "#,##0"
    ws.cell(r, 5, f"=IFERROR(D{r}/B{r},0)").number_format = "0.0%"
total = 14
ws.cell(total, 1, "Total").font = Font(bold=True)
for col in "BCD":
    ws[f"{col}{total}"] = f"=SUM({col}2:{col}13)"
    ws[f"{col}{total}"].number_format = "#,##0"
    ws[f"{col}{total}"].font = Font(bold=True)
ws[f"E{total}"] = f"=IFERROR(D{total}/B{total},0)"
ws[f"E{total}"].number_format = "0.0%"
ws[f"B{total}"].border = Border(top=Side(style="thin"))

header_fill = PatternFill("solid", fgColor="1B365D")
for c in ws[1]:
    c.font = Font(bold=True, color="FFFFFF")
    c.fill = header_fill
    c.alignment = Alignment(horizontal="center")
ws.freeze_panes = "A2"
for idx, width in enumerate([12, 14, 14, 14, 11], 1):
    ws.column_dimensions[get_column_letter(idx)].width = width

table = Table(displayName="PlanTable", ref="A1:E13")
table.tableStyleInfo = TableStyleInfo(name="TableStyleLight9", showRowStripes=True)
ws.add_table(table)
ws.conditional_formatting.add("E2:E13", CellIsRule(operator="lessThan", formula=["0.38"], font=Font(color="C62828")))
ws.conditional_formatting.add("B2:B13", ColorScaleRule(start_type="min", start_color="EEF1F5", end_type="max", end_color="2E6DA4"))
dv = DataValidation(type="decimal", operator="between", formula1="0", formula2="1", showErrorMessage=True)
inp.add_data_validation(dv)
dv.add("B1:B2")

chart = BarChart()
chart.title = "Revenue and cost"
chart.y_axis.title = "€"
chart.add_data(Reference(ws, min_col=2, max_col=3, min_row=1, max_row=13), titles_from_data=True)
chart.set_categories(Reference(ws, min_col=1, min_row=2, max_row=13))
chart.height, chart.width = 8, 18
line = LineChart()
line.add_data(Reference(ws, min_col=4, min_row=1, max_row=13), titles_from_data=True)
chart += line                                              # combo: bars + line
ws.add_chart(chart, "G2")
ws.print_options.gridLines = False
ws.page_setup.orientation = "landscape"
ws.page_setup.fitToWidth = 1
wb.save("/workspace/.work/model.xlsx")
```
Then: `python3 /skills/xlsx/scripts/recalc.py /workspace/.work/model.xlsx`.

## pandas -> formatted sheet

```python
# recipe: pandas-export
import pandas as pd
from openpyxl import load_workbook
from openpyxl.styles import Font, PatternFill

df = pd.DataFrame({"Region": ["North", "South", "East"], "Revenue": [5100.5, 4200.25, 3900.0],
                   "Growth": [0.09, 0.15, -0.02]})
path = "/workspace/.work/export.xlsx"
with pd.ExcelWriter(path, engine="openpyxl") as xw:
    df.to_excel(xw, sheet_name="Data", index=False)
wb = load_workbook(path)
ws = wb["Data"]
n = ws.max_row
ws[f"A{n + 1}"] = "Total"
ws[f"B{n + 1}"] = f"=SUM(B2:B{n})"
for row in ws.iter_rows(min_row=2, min_col=2, max_col=2):
    row[0].number_format = "#,##0.00"
for row in ws.iter_rows(min_row=2, max_row=n, min_col=3, max_col=3):
    row[0].number_format = "0.0%"
for c in ws[1]:
    c.font, c.fill = Font(bold=True, color="FFFFFF"), PatternFill("solid", fgColor="1B365D")
ws.freeze_panes = "A2"
ws.auto_filter.ref = f"A1:C{n}"
ws.column_dimensions["A"].width = 16
ws.column_dimensions["B"].width = 14
wb.save(path)
```

## Append rows keeping styles; edit values without touching formulas

```python
# recipe: append-keep-styles
from copy import copy
from openpyxl import load_workbook

path = "/workspace/.work/export.xlsx"
wb = load_workbook(path)                    # formulas kept (never data_only=True for editing)
ws = wb["Data"]
total_row = ws.max_row                      # "Total" row from the previous recipe
ws.insert_rows(total_row)                   # new empty row above the total
src = total_row - 1
for col in range(1, ws.max_column + 1):
    s, d = ws.cell(src, col), ws.cell(total_row, col)
    d.font, d.fill, d.border = copy(s.font), copy(s.fill), copy(s.border)
    d.number_format, d.alignment = s.number_format, copy(s.alignment)
ws.cell(total_row, 1, "West")
ws.cell(total_row, 2, 2500)
ws.cell(total_row, 3, 0.04)
# insert_rows does not adjust formulas: rewrite the total explicitly
ws.cell(total_row + 1, 2, f"=SUM(B2:B{total_row})")
wb.save(path)
assert load_workbook(path)["Data"][f"B{total_row + 1}"].value == f"=SUM(B2:B{total_row})"
```

## Read values and formulas side by side

```python
# recipe: read
from openpyxl import load_workbook

formulas = load_workbook("/workspace/.work/export.xlsx")["Data"]
values = load_workbook("/workspace/.work/export.xlsx", data_only=True)["Data"]   # cached results (after recalc.py)
for row in formulas.iter_rows(min_row=1, max_row=3):
    for c in row:
        _ = (c.coordinate, c.value, values[c.coordinate].value, c.number_format)
```

## Images and number formats cheat sheet
- `from openpyxl.drawing.image import Image; ws.add_image(Image("chart.png"), "H2")`
  (PNG from the `charts` skill; size in pixels: `img.width, img.height`).
- Formats: integer `#,##0`; 2 decimals `#,##0.00`; percent `0.0%`; euro `#,##0.00 "€"`;
  thousands of euros `#,##0,"k€"`; negative in red `#,##0;[Red]-#,##0`; date `dd/mm/yyyy`;
  month `mmm yyyy`; text `@`.
