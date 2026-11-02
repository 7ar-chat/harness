#!/usr/bin/env python3
"""List the figures, dates and named items of a deliverable that appear in none of its sources.

Usage:
  python3 cross_check.py DELIVERABLE SOURCE [SOURCE ...]
  python3 cross_check.py /workspace/outputs/deck.pptx /workspace/suivi.xlsx /workspace/risques.md
  python3 cross_check.py /workspace/.work/answer.md /workspace/report.pdf     # your chat answer, saved to a file

Files: .docx .pptx .xlsx/.xlsm .pdf .csv .md .txt .html .json (deliverable and sources).
Before you finish a document built from files, run it and go through every item it lists:
  - numbers: every figure of the deliverable (amounts, percentages, decimals, years) must be in a
    source - in any notation ("19,5 %" = "19.5%" = 0.195, "618,4 k€" = 618 432) and rounded to the
    precision you print. A figure computed from two figures of the same line or cell that are in a
    source (difference, sum, ratio, change in %) is listed under "derived": check the computation;
  - dates: "2 février 2026", "02/02/2026", "2026-02-02", "février 2026" must be a date of a source
    (Excel date cells included);
  - names: capitalised names, acronyms and short table cells (people, projects, risks, products,
    regions) must be written in a source (case and accents ignored).
An item listed is not necessarily wrong (a total you computed, today's date, a heading you wrote):
fix it if it is wrong, and say in your answer which figures are your own computations.
It cannot see what is missing: also compare the source's key items with the deliverable yourself.

Prints {"file", "sources", "checked": {numbers, dates, names}, "not_in_sources": {numbers, dates,
names: [{"text", "context"}]}, "derived": [...], "all_found"} and exits 0 (1 on a usage error).
It is a helper, not a checker: it never fails a deliverable by itself.
"""
from __future__ import annotations

import argparse
import bisect
import csv
import datetime as dt
import json
import os
import re
import sys
import unicodedata
from decimal import Decimal, InvalidOperation

MAX_LISTED = 40
SPACES = "[ \u00a0\u202f\u2009]"
# A number: not glued to a word before it ("R3", "Q2"); a unit may follow ("39.7m", "12k€").
NUMBER = re.compile(r"(?<![\w.,])[-+−]?(\d{1,3}(?:" + SPACES + r"\d{3})+(?:[.,]\d+)?|\d+(?:[.,]\d+)*)(?![\d]|[.,]\d)")
SCALE = re.compile(r"^\s*(k|K|M|m|Md|Mds|bn|B|Bn|mn|milliers?|millions?|milliards?|thousands?|billions?)(?![^\W\d_])", re.U)
SCALES = {"k": 3, "K": 3, "millier": 3, "milliers": 3, "thousand": 3, "thousands": 3, "M": 6, "m": 6, "mn": 6, "million": 6,
          "millions": 6, "Md": 9, "Mds": 9, "bn": 9, "Bn": 9, "B": 9, "milliard": 9, "milliards": 9, "billion": 9,
          "billions": 9}
MONTHS = {
    "janvier": 1, "février": 2, "fevrier": 2, "mars": 3, "avril": 4, "mai": 5, "juin": 6, "juillet": 7, "août": 8,
    "aout": 8, "septembre": 9, "octobre": 10, "novembre": 11, "décembre": 12, "decembre": 12,
    "january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6, "july": 7, "august": 8,
    "september": 9, "october": 10, "november": 11, "december": 12,
    "jan": 1, "feb": 2, "fév": 2, "fev": 2, "mar": 3, "apr": 4, "avr": 4, "jun": 6, "jul": 7, "juil": 7, "aug": 8,
    "sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12, "déc": 12}
MONTH_RE = "|".join(sorted(MONTHS, key=len, reverse=True))
DATE_DMY = re.compile(r"(?<!\d)(\d{1,2})(?:er)?\.?\s+(" + MONTH_RE + r")\.?\s+(\d{4})(?!\d)", re.I)
DATE_MDY = re.compile(r"(?<![\w])(" + MONTH_RE + r")\.?\s+(\d{1,2}),?\s+(\d{4})(?!\d)", re.I)
DATE_MY = re.compile(r"(?<![\w])(" + MONTH_RE + r")\.?\s+(\d{4})(?!\d)", re.I)
DATE_NUM = re.compile(r"(?<![\d/.-])(\d{1,2})[/.](\d{1,2})[/.](\d{4})(?![\d/-]|\.\d)")  # "20/12/2025." ends a sentence
DATE_ISO = re.compile(r"(?<![\d-])(\d{4})-(\d{2})-(\d{2})(?![\d-])")
NAME = re.compile(r"(?<![\w&+-])((?:[A-ZÀ-Ý][\w&'’+-]*|[A-Z]{2,}[\w+-]*)(?:[ \u00a0](?:(?:de|du|des|la|le|d'|of|the|and|et|&)[ \u00a0])?[A-ZÀ-Ý0-9][\w&'’+-]*)*)")
STOP = set("""a an the this that these those it its we our you your they their he she i in on at of for to from by with
and or but not no yes all any each per as is are was were be been will would can could should may might must
le la les un une des du de d l au aux ce cet cette ces il elle ils elles nous vous on et ou mais ni non oui tous
toutes tout chaque par pour sur sous dans avec sans en est sont était été sera seront peut doit plus moins très
total totals sous-total subtotal moyenne average summary synthèse synthese note notes source sources page pages
slide slides table tableau figure graphique chart annexe appendix objet objectif objectifs contexte context
introduction conclusion recommandations recommendations recommandation recommendation points point key clé
q1 q2 q3 q4 t1 t2 t3 t4 h1 h2 s1 s2 n/a na ok""".split())
# Figures ending a capitalised group or a cell ("Effectif 4512", "Budget 1 670 k€" is not a name).
TRAILING_FIGURES = re.compile(r"(?:[ \u00a0\u202f]+[-+−]?\d[\d.,\u00a0\u202f ]*(?:%|k€|M€|€|k|K|M)?)+\s*$")
NOTE_PREFIX = re.compile(r"^\s*(?:[-*•▪◦·]|\d+[.)]|[a-z][.)])\s*", re.I)


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def fail(message):
    print(json.dumps({"ok": False, "error": message}, ensure_ascii=False))
    sys.exit(1)


# ----------------------------------------------------------------------------- reading
def fold(s):
    """Lower case, no accents, typographic quotes and spaces unified, hyphenation at line ends removed."""
    s = unicodedata.normalize("NFKC", s).replace("\u00ad", "")
    s = re.sub(r"[’‘`´]", "'", s)
    s = re.sub(r"(\w)-\s*\n\s*(\w)", r"\1\2", s)
    s = "".join(c for c in unicodedata.normalize("NFKD", s) if not unicodedata.combining(c))
    return re.sub(r"\s+", " ", s).strip().lower()


def text_units(path):
    """(units, cells, dates): the text units of a file in order (paragraphs, bullets, table cells, lines),
    the short table/sheet cells among them, and the dates of its date-typed cells (Excel)."""
    ext = os.path.splitext(path)[1].lower()
    cells, dates = [], []
    if ext == ".docx":
        import docx

        d = docx.Document(path)
        units = [p.text for p in d.paragraphs]
        for t in d.tables:
            for row in t.rows:
                seen = set()
                for c in row.cells:
                    if id(c._tc) not in seen:
                        seen.add(id(c._tc))
                        units += [p.text for p in c.paragraphs]
                        cells.append(c.text)
        for s in d.sections:
            for part in (s.header, s.footer):
                units += [p.text for p in part.paragraphs]
        return units, cells, dates
    if ext == ".pptx":
        from pptx import Presentation

        units = []
        for slide in Presentation(path).slides:
            for shape in _shapes(slide.shapes):
                if shape.has_text_frame:
                    units += [p.text for p in shape.text_frame.paragraphs]
                if getattr(shape, "has_table", False) and shape.has_table:
                    for row in shape.table.rows:
                        for c in row.cells:
                            units.append(c.text)
                            cells.append(c.text)
                if getattr(shape, "has_chart", False) and shape.has_chart:
                    try:
                        plot = shape.chart.plots[0]
                        units += [str(c) for c in plot.categories]
                        for s in plot.series:
                            units.append("; ".join(_num(v) for v in s.values if v is not None))
                    except Exception:  # noqa: BLE001 - charts without data
                        pass
            if slide.has_notes_slide:
                units += slide.notes_slide.notes_text_frame.text.splitlines()
        return units, cells, dates
    if ext == ".pdf":
        import pdfplumber

        with pdfplumber.open(path) as doc:
            return [line for p in doc.pages for line in (p.extract_text() or "").splitlines()], cells, dates
    if ext in (".xlsx", ".xlsm"):
        import openpyxl

        units = []
        for ws in openpyxl.load_workbook(path, data_only=True, read_only=True).worksheets:
            units.append(ws.title)
            for row in ws.iter_rows(values_only=True):
                for v in row:
                    if isinstance(v, (dt.datetime, dt.date)):
                        dates.append(v.date() if isinstance(v, dt.datetime) else v)
                        units.append(v.strftime("%Y-%m-%d"))
                    elif isinstance(v, bool) or v is None:
                        continue
                    elif isinstance(v, (int, float)):
                        units.append(_num(v))
                    else:
                        units.append(str(v))
                        cells.append(str(v))
        return units, cells, dates
    raw = open(path, encoding="utf-8-sig", errors="replace").read()
    if ext == ".csv":
        try:
            dialect = csv.Sniffer().sniff(raw[:4096], delimiters=",;\t|")
        except csv.Error:
            dialect = csv.excel
        rows = list(csv.reader(raw.splitlines(), dialect))
        flat = [c for r in rows for c in r]
        return flat, [c for c in flat if not NUMBER.fullmatch(c.strip() or "x")], dates
    if ext in (".html", ".htm"):
        raw = re.sub(r"<(br|/p|/li|/td|/th|/h\d|/div|/tr)[^>]*>", "\n", raw, flags=re.I)
        raw = re.sub(r"<[^>]+>", " ", raw)
    lines = raw.splitlines()
    if ext == ".md":  # table cells of markdown tables
        for line in lines:
            if line.strip().startswith("|") and not re.fullmatch(r"[\s|:-]+", line):
                cells += [c.strip() for c in line.strip().strip("|").split("|")]
    return lines, cells, dates


def table_totals(path):
    """Sums of the numeric columns and rows of a source's tables (sheets, CSV, Markdown tables): a total
    printed in a deliverable is derived from them."""
    ext = os.path.splitext(path)[1].lower()
    grids = []
    if ext in (".xlsx", ".xlsm"):
        import openpyxl

        for ws in openpyxl.load_workbook(path, data_only=True, read_only=True).worksheets:
            grids.append([list(r) for r in ws.iter_rows(values_only=True)])
    elif ext in (".csv", ".md"):
        rows = [r.strip().strip("|").split("|") for r in open(path, encoding="utf-8-sig", errors="replace")
                if r.strip().startswith("|")] if ext == ".md" else None
        if ext == ".csv":
            raw = open(path, encoding="utf-8-sig", errors="replace").read()
            try:
                dialect = csv.Sniffer().sniff(raw[:4096], delimiters=",;\t|")
            except csv.Error:
                dialect = csv.excel
            rows = list(csv.reader(raw.splitlines(), dialect))
        grid = []
        for r in rows or []:
            line = []
            for c in r:
                vs = values(c.strip()) if NUMBER.fullmatch(c.strip() or "x") else set()
                line.append(float(min(vs)) if len(vs) == 1 else None)
            grid.append(line)
        grids.append(grid)
    def num(v):
        return isinstance(v, (int, float)) and not isinstance(v, bool)

    totals = set()
    for grid in grids[:20]:
        grid = [r[:30] for r in grid[:5000]]
        width = max((len(r) for r in grid), default=0)
        # the sum of every run of adjacent numeric cells of a row (July..September), and the column
        # totals of those sums (the quarter of every line, then of the whole table)
        ranges = {}
        for r in grid:
            for a in range(width):
                acc, n = 0.0, 0
                for b in range(a, min(width, len(r))):
                    if not num(r[b]):
                        break
                    acc, n = acc + r[b], n + 1
                    if n >= 2:
                        totals.add(Decimal(repr(acc)).normalize())
                    ranges.setdefault((a, b), []).append(acc)
        for sums in ranges.values():
            if len(sums) >= 2:
                totals.add(Decimal(repr(float(sum(sums)))).normalize())
    return totals


def _shapes(shapes):
    for s in shapes:
        if s.shape_type == 6 and hasattr(s, "shapes"):  # group
            yield from _shapes(s.shapes)
        else:
            yield s


def _num(v):
    if isinstance(v, float) and v.is_integer():
        v = int(v)
    return repr(v) if isinstance(v, float) else str(v)


# ----------------------------------------------------------------------------- numbers
def values(token):
    """Possible decimal values of a printed number ("1,234" is 1234 or 1.234)."""
    t = re.sub(SPACES, "", token.lstrip("+-−"))
    out = set()
    if "," in t and "." in t:  # 1,234.5 or 1.234,5
        dec = t[max(t.rfind(","), t.rfind("."))]
        cands = [t.replace("," if dec == "." else ".", "").replace(dec, ".")]
    elif "," in t or "." in t:
        sep = "," if "," in t else "."
        parts = t.split(sep)
        cands = []
        if len(parts) == 2:
            cands.append(parts[0] + "." + parts[1])  # decimal
        if all(len(x) == 3 for x in parts[1:]) and 1 <= len(parts[0]) <= 3:
            cands.append("".join(parts))  # thousands
    else:
        cands = [t]
    for c in cands:
        try:
            out.add(Decimal(c).normalize())
        except InvalidOperation:
            pass
    return out


def numbers_in(text):
    """[(token, {values}, start)] of the numbers in text."""
    return [(m.group(1), values(m.group(1)), m.start(1)) for m in NUMBER.finditer(text)]


def all_values(text):
    """Every value printed in text ("12 500" may also be two numbers)."""
    vals = set()
    for tok, vs, _ in numbers_in(text):
        vals |= vs
        for part in re.split(SPACES, tok):
            vals |= values(part)
    return vals


def scaled_values(text):
    """The values of the abbreviated figures of text in full ("1 232,50 millions" is 1 232 500 000)."""
    vals = set()
    for tok, vs, start in numbers_in(text):
        m = SCALE.match(text[start + len(tok):start + len(tok) + 14])
        if m:
            scale = Decimal(10) ** SCALES[m.group(1)]
            vals |= {(v * scale).normalize() for v in vs}
    return vals


def significant(token, vs, text, start):
    """A figure worth checking: a decimal, a percentage, or a number >= 10 (not a list marker or a small count)."""
    if not vs:
        return False
    after = text[start + len(token):start + len(token) + 3]
    if any(c in token for c in ",.") or "%" in after or "‰" in after:
        return True
    return max(vs) >= 10


def derived(target_vals, known):
    """True when a value equals a difference, sum, ratio or change (%) of two known values, at its precision."""
    ks = [k for k in known if k != 0][:12]
    for t in target_vals:
        exp = t.as_tuple().exponent
        q = Decimal(1).scaleb(exp if isinstance(exp, int) and exp < 0 else 0)

        def close(x):
            try:
                return abs(abs(x) - t) <= q / 2 + Decimal("1e-9")
            except InvalidOperation:
                return False

        for a in ks:
            for b in ks:
                if a != b and (close(a - b) or close(a + b) or close(a / b * 100) or close((a - b) / b * 100) or close(a / b)):
                    return True
    return False


class NumberIndex:
    """The values of the sources, searchable with a tolerance (rounding of the printed figure)."""

    def __init__(self, vals):
        self.vals = sorted(vals)

    def near(self, x, tol):
        i = bisect.bisect_left(self.vals, x - tol)
        return i < len(self.vals) and self.vals[i] <= x + tol

    def has(self, token, vs, text, start):
        """The printed figure, in any of its readings, is a source value rounded (and scaled: k, M, %) as printed."""
        after = text[start + len(token):start + len(token) + 14]
        m = SCALE.match(after)
        scale = Decimal(10) ** SCALES.get(m.group(1), 0) if m else Decimal(1)
        percent = after.lstrip().startswith(("%", "pct", "points", "pts"))
        for v in vs:
            exp = v.as_tuple().exponent
            decimals = -exp if isinstance(exp, int) and exp < 0 else 0
            tol = Decimal(5) / Decimal(10) ** (decimals + 1)
            if self.near(v, Decimal("1e-9")) or self.near(v * scale, tol * scale):
                return True
            if percent and self.near(v / 100, tol / 100):
                return True
            if m is None and self.near(v, tol) and decimals > 0:
                return True
        return False


# ----------------------------------------------------------------------------- dates
def dates_in(text):
    """[(text, {(y, m, d) or (y, m, None)})] of the dates written in text."""
    out, taken = [], []

    def add(m, cands):
        if any(a < m.end() and m.start() < b for a, b in taken):
            return
        cands = {c for c in cands if 1900 <= c[0] <= 2100 and 1 <= c[1] <= 12 and (c[2] is None or 1 <= c[2] <= 31)}
        if cands:
            taken.append((m.start(), m.end()))
            out.append((m.group(0), cands))

    for m in DATE_ISO.finditer(text):
        add(m, {(int(m.group(1)), int(m.group(2)), int(m.group(3)))})
    for m in DATE_DMY.finditer(text):
        add(m, {(int(m.group(3)), MONTHS[m.group(2).lower()], int(m.group(1)))})
    for m in DATE_MDY.finditer(text):
        add(m, {(int(m.group(3)), MONTHS[m.group(1).lower()], int(m.group(2)))})
    for m in DATE_NUM.finditer(text):
        a, b, y = int(m.group(1)), int(m.group(2)), int(m.group(3))
        add(m, {(y, b, a), (y, a, b)})  # dd/mm/yyyy or mm/dd/yyyy
    for m in DATE_MY.finditer(text):
        add(m, {(int(m.group(2)), MONTHS[m.group(1).lower()], None)})
    return out


def date_found(cands, days, months):
    return any((c[0], c[1], c[2]) in days if c[2] else (c[0], c[1]) in months for c in cands)


# ----------------------------------------------------------------------------- names
def names_in(unit, cells):
    """Named items of a text unit: capitalised words or groups not at the start of a sentence, acronyms,
    and the unit itself when it is a short table cell."""
    out = []
    text = NOTE_PREFIX.sub("", unit)
    for m in NAME.finditer(text):
        name = TRAILING_FIGURES.sub("", m.group(1)).strip(" '’-+")  # "Effectif 4512": a label and its figure
        words = name.split()
        start = text[:m.start()].rstrip()
        sentence_start = not start or start[-1] in ".!?:;—–•(\"«“" or start.endswith(" -")
        if sentence_start and len(words) == 1 and not name.isupper():
            continue  # "Le", "The", "Budget" at the start of a sentence
        if name.isupper() and len(words) > 1:
            continue  # a heading in capitals, not a name
        if all(w.lower().strip("'’") in STOP or w.lower() in MONTHS or NUMBER.fullmatch(w) for w in words):
            continue
        if len(name) < 3 or name.lower() in MONTHS:
            continue
        out.append(name)
    stripped = unit.strip()
    if stripped in cells:
        stripped = TRAILING_FIGURES.sub("", stripped)
    else:
        stripped = None
    if stripped and 3 <= len(stripped) <= 60 and 2 <= len(stripped.split()) <= 6 \
            and re.search(r"[^\W\d_]{3}", stripped) and not stripped.endswith((".", "!", "?", ":")):
        out.append(stripped)  # a short table cell: a risk, a milestone, a product (a sentence is not a name)
    return out


# ----------------------------------------------------------------------------- report
def cross_check(deliverable, sources):
    units, cells, _ = text_units(deliverable)
    corpus, src_vals, days, months, totals = [], set(), set(), set(), set()
    for s in sources:
        totals |= table_totals(s)
        su, _, sdates = text_units(s)
        corpus.append(fold("\n".join(su)))
        for u in su:
            src_vals |= all_values(u) | scaled_values(u)
            for _, cands in dates_in(u):
                for c in cands:
                    if c[2]:
                        days.add(c)
                    months.add((c[0], c[1]))
        for d in sdates:
            days.add((d.year, d.month, d.day))
            months.add((d.year, d.month))
    blob = "\n".join(corpus)
    index, total_index = NumberIndex(src_vals), NumberIndex(totals - src_vals)
    seen = set()
    missing = {"numbers": [], "dates": [], "names": []}
    derived_list = []
    counts = {"numbers": 0, "dates": 0, "names": 0}

    def add_derived(text, unit, how):
        key = ("derived", fold(text), fold(unit))
        if key not in seen:
            seen.add(key)
            derived_list.append({"text": text, "context": unit.strip()[:160], "how": how})

    def report(kind, text, unit):
        key = (kind, fold(text))
        if key in seen:
            return
        seen.add(key)
        missing[kind].append({"text": text, "context": unit.strip()[:160]})

    for unit in units:
        if not unit or not unit.strip():
            continue
        date_spans = []
        for text, cands in dates_in(unit):
            counts["dates"] += 1
            i = unit.find(text)
            date_spans.append((i, i + len(text)))
            if not date_found(cands, days, months):
                report("dates", text, unit)
        found_here, pending = set(), []
        for tok, vs, start in numbers_in(unit):
            if any(a <= start < b for a, b in date_spans) or not significant(tok, vs, unit, start):
                continue
            counts["numbers"] += 1
            if index.has(tok, vs, unit, start):
                found_here |= vs
            elif total_index.has(tok, vs, unit, start):
                found_here |= vs
                add_derived(tok, unit, "a total of a source column or row")
            else:
                pending.append((tok, vs))
        for tok, vs in pending:
            if derived(vs, found_here):
                add_derived(tok, unit, "computed from figures of the same line that are in a source")
            else:
                report("numbers", tok, unit)
        for name in names_in(unit, cells):
            counts["names"] += 1
            parts = [p for p in re.split(r"\s+(?:and|et|&|or|ou|vs\.?)\s+|\s*[/,·]\s*", name) if p.strip()]
            if fold(name) not in blob and not all(fold(p) in blob for p in parts):
                report("names", name, unit)
    out = {"file": os.path.abspath(deliverable), "sources": [os.path.abspath(s) for s in sources], "checked": counts,
           "not_in_sources": {k: v[:MAX_LISTED] for k, v in missing.items()},
           "derived": derived_list[:MAX_LISTED], "all_found": not any(missing.values())}
    more = {k: len(v) - MAX_LISTED for k, v in missing.items() if len(v) > MAX_LISTED}
    if more:
        out["not_listed"] = more
    out["note"] = ("every figure, date and name of the deliverable is in a source" if out["all_found"] else
                   "check each item of not_in_sources against the sources (search_files / read_file): fix the wrong "
                   "ones; your own computations and wording may stay - say in your answer what you cross-checked "
                   "and which figures you computed")
    return out


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("deliverable", help="the file to cross-check (.docx .pptx .xlsx .pdf .md .csv ...)")
    ap.add_argument("sources", nargs="+", metavar="SOURCE", help="the files its content comes from")
    args = ap.parse_args(argv)
    for p in [args.deliverable] + args.sources:
        if not os.path.isfile(p):
            fail("file not found: %s" % p)
    try:
        out = cross_check(args.deliverable, args.sources)
    except Exception as exc:  # noqa: BLE001 - unreadable input: usage error
        fail("%s: %s" % (type(exc).__name__, str(exc)[:300]))
    print(json.dumps(out, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
