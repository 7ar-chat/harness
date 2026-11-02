#!/usr/bin/env python3
"""Check the page citations of a deliverable against the source PDF(s).

Usage:
  python3 check_citations.py SOURCE.pdf DELIVERABLE                # .docx .pptx .md .txt .html .pdf .xlsx
  python3 check_citations.py SOURCE.pdf OTHER.pdf DELIVERABLE      # several sources: any of them may hold the claim
  python3 check_citations.py SOURCE.pdf /workspace/.work/answer.md # your chat answer: write it to a file first

Every claim followed by a page citation - "(p. 12)", "(pp. 12-13)", "(p.12, 14)", "(page 12)",
"(Rapport, p. 12)" - is checked against the text of that page of the source:
  - numbers: every figure of the claim (percentages, amounts, decimals, years; "19,5 %" = "19.5%",
    "1 234" = "1,234") must be printed on the cited page; a figure computed from figures of the
    claim that are on the page (difference, sum, ratio, change in %) is accepted as derived;
  - quotes: text in quotation marks ("...", “...”, « ... ») must be on the cited page;
  - pages: the cited page exists (printed page labels count too).
The claim of a citation is the text before it, back to the previous citation or the start of its
paragraph, bullet, table cell or line.

Prints the checker contract {"ok", "file", "checks": [{"name", "ok", "detail"}], "next_step"?,
"citations": [...]} and exits 0 when every citation holds, 2 when one does not, 1 on a usage
error (missing or unreadable file). "file" is the deliverable. A figure found on another page is
reported with that page, so you can fix the citation; a figure on no page is not in the source.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cross_check import all_values as page_values  # noqa: E402 - shared with cross_check.py (same folder)
from cross_check import derived, fold, numbers_in, significant, text_units, values  # noqa: E402

CMD = "python3 /skills/pdf/scripts/check_citations.py"
MAX_DETAIL = 8

PAREN = re.compile(r"\(([^()]{1,160})\)")
PAGEREF = re.compile(r"(?<![\w])(?:pp?\.|pages?|S\.)\s*(\d{1,4}(?:\s*(?:[-–—]|,|et|and|&)\s*\d{1,4})*)", re.I)
QUOTE = re.compile(r"[\"“”«»]\s*([^\"“”«»]{12,400}?)\s*[\"“”«»]")


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def fail(message):
    print(json.dumps({"ok": False, "error": message}, ensure_ascii=False))
    sys.exit(1)


# ----------------------------------------------------------------------------- text
def units_of(path):
    """Text units of the deliverable (paragraphs, bullets, table cells, lines), in order."""
    return text_units(path)[0]


def page_texts(path):
    """The text of every page of a PDF: pdftotext (poppler, ~100x faster on long documents) when it gives
    one text per page, else pdfplumber."""
    from pypdf import PdfReader

    n = len(PdfReader(path).pages)
    exe = shutil.which("pdftotext")
    if exe:
        try:
            res = subprocess.run([exe, "-q", "-enc", "UTF-8", path, "-"], capture_output=True, timeout=120)
            texts = res.stdout.decode("utf-8", "replace").split("\f")  # one form feed after every page
            if res.returncode == 0 and len(texts) == n + 1 and not texts[-1].strip():
                return texts[:n]
        except (OSError, subprocess.SubprocessError):
            pass
    import pdfplumber

    with pdfplumber.open(path) as doc:
        return [page.extract_text() or "" for page in doc.pages]


def source_pages(paths):
    """[(source path, page number, label, folded text, figures, own)] of every page of the sources: each page
    is read and its figures computed once; own are the values of its page number and label."""
    from pypdf import PdfReader

    pages = []
    for p in paths:
        try:
            labels = list(PdfReader(p).page_labels)
        except Exception:  # noqa: BLE001 - labels are optional
            labels = []
        for i, raw in enumerate(page_texts(p), 1):
            label = labels[i - 1] if i - 1 < len(labels) else str(i)
            text = fold(raw)
            own = values(str(i)) | (values(label) if label.isdigit() else set())
            pages.append((p, i, label, text, page_values(text), own))
    return pages


# ----------------------------------------------------------------------------- citations
def cited_pages(spec):
    pages = []
    for part in re.split(r"\s*(?:,|et|and|&)\s*", spec.strip(), flags=re.I):
        m = re.match(r"(\d+)\s*[-–—]\s*(\d+)$", part.strip())
        if m:
            lo, hi = int(m.group(1)), int(m.group(2))
            if hi < lo:  # "pp. 112-14"
                hi = int(str(lo)[: len(str(lo)) - len(str(hi))] + str(hi))
            pages += list(range(lo, min(hi, lo + 20) + 1))
        elif part.strip().isdigit():
            pages.append(int(part.strip()))
    return pages


def citations(units):
    """[(claim, [pages], citation text)] in document order."""
    out = []
    for unit in units:
        prev = 0
        for m in PAREN.finditer(unit):
            pages = [p for ref in PAGEREF.findall(m.group(1)) for p in cited_pages(ref)]
            if not pages:
                continue
            claim = unit[prev:m.start()].strip(" \t-–—•:;,")
            prev = m.end()
            out.append((claim, sorted(set(pages)), m.group(0)))
    return out


def found_on(pages, vs):
    """Labels of the pages printing one of the values (a page's own number, in its header or footer, does not count)."""
    hits = {label for _, _, label, _, figures, own in pages if vs & (figures - own)}
    return sorted(hits, key=lambda x: (len(x), x))


def check(sources, deliverable):
    cits = citations(units_of(deliverable))
    if not cits:  # nothing to check: the sources are not even read (the runtime runs this on every document)
        return {"ok": True, "file": os.path.abspath(deliverable), "citations": [],
                "checks": [{"name": "citations", "ok": True, "detail": "no (p. N) citation found: nothing to check"}]}
    pages = source_pages(sources)
    by_page, figures_of = {}, {}
    for src, i, label, text, figures, _ in pages:
        for key in {str(i), label}:
            by_page.setdefault(key, []).append(text)
            figures_of.setdefault(key, set()).update(figures)
    total = max((i for _, i, _, _, _, _ in pages), default=0)
    results, bad_pages, bad_numbers, bad_quotes, derived_notes = [], [], [], [], []
    for claim, cited, raw in cits:
        texts = [t for p in cited for t in by_page.get(str(p), [])]
        item = {"citation": raw, "pages": cited, "claim": claim[-300:], "ok": True}
        missing_pages = [p for p in cited if str(p) not in by_page]
        if missing_pages:
            item["ok"] = False
            item["missing_pages"] = missing_pages
            bad_pages.append("%s: the source has %d pages" % (raw, total))
        if texts:
            on_page = set().union(*(figures_of.get(str(p), set()) for p in cited))
            found_known, missing = set(), []
            nums = [(tok, vs, s) for tok, vs, s in numbers_in(claim) if significant(tok, vs, claim, s)]
            for tok, vs, _ in nums:
                if vs & on_page:
                    found_known |= vs & on_page
                else:
                    missing.append((tok, vs))
            not_found = []
            for tok, vs in missing:
                if derived(vs, found_known):
                    derived_notes.append("%s %s (computed from the page's figures)" % (tok, raw))
                    continue
                elsewhere = found_on(pages, vs)
                not_found.append(tok)
                where = ("found on p. %s" % ", ".join(elsewhere[:5])) if elsewhere else "on no page of the source"
                bad_numbers.append("%s %s: not on p. %s (%s) - \"%s\"" % (
                    tok, raw, ",".join(map(str, cited)), where, claim[-120:]))
            quotes = []
            for q in QUOTE.findall(claim):
                nq = fold(q)
                words = [w for w in re.findall(r"\w{3,}", nq)]
                if any(nq in t for t in texts):
                    continue
                if words and sum(1 for w in words if any(re.search(r"\b" + re.escape(w), t) for t in texts)) >= 0.9 * len(words):
                    continue
                quotes.append(q)
                bad_quotes.append("\"%s\" %s: not on p. %s" % (q[:80], raw, ",".join(map(str, cited))))
            if not_found or quotes:
                item["ok"] = False
                item["not_on_page"] = not_found + quotes
        results.append(item)
    checks = [
        {"name": "citations", "ok": True, "detail": "%d page citations checked" % len(cits)},
        {"name": "pages_exist", "ok": not bad_pages, "detail": "; ".join(bad_pages[:MAX_DETAIL])},
        {"name": "numbers_on_page", "ok": not bad_numbers,
         "detail": "; ".join(bad_numbers[:MAX_DETAIL]) + (" (+%d more)" % (len(bad_numbers) - MAX_DETAIL) if len(bad_numbers) > MAX_DETAIL else "")
         or ("derived: " + "; ".join(derived_notes[:4]) if derived_notes else "")},
        {"name": "quotes_on_page", "ok": not bad_quotes, "detail": "; ".join(bad_quotes[:MAX_DETAIL])},
    ]
    ok = all(c["ok"] for c in checks)
    report = {"ok": ok, "file": os.path.abspath(deliverable), "checks": checks, "citations": results}
    if not ok:
        report["next_step"] = ("for each claim listed, read the cited page (read_file pages=N on the source) and use the "
                               "figure or wording printed there, cite the page where it is printed, or remove the claim; "
                               "then run %s again" % CMD)
    return report


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("sources", nargs="+", metavar="SOURCE.pdf", help="the PDF(s) the citations refer to")
    ap.add_argument("deliverable", help="the file with the (p. N) citations")
    args = ap.parse_args(argv)
    for p in args.sources + [args.deliverable]:
        if not os.path.isfile(p):
            fail("file not found: %s" % p)
    for p in args.sources:
        if not p.lower().endswith(".pdf"):
            fail("a source must be a PDF: %s" % p)
    try:
        report = check(args.sources, args.deliverable)
    except Exception as exc:  # noqa: BLE001 - unreadable input: usage error
        fail("%s: %s" % (type(exc).__name__, str(exc)[:300]))
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report["ok"] else 2


if __name__ == "__main__":
    sys.exit(main())
