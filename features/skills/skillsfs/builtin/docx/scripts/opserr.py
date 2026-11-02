"""Structured failures and bounded text for the skills' op scripts (build_xlsx.py, edit_xlsx.py,
edit_docx.py, inspect_*.py).

The model fixes a failure in one round when the error names what is valid, so every rejected
input carries a stable `reason` (never parse `error`), what exists (`available`), the closest
spelling (`did_you_mean`) and the usage line of the op:

  {"ok": false, "error": "no sheet 'Sales'", "reason": "sheet_not_found",
   "failures": [{"index": 2, "op": "set_cell", "reason": "sheet_not_found", "error": "...",
                 "available": ["Data", "Summary"], "did_you_mean": "Data", "usage": "set_cell {...}"}],
   "next_step": "fix the failing op(s) and resend the whole batch: nothing was applied"}

Shared by the xlsx and docx skills (shared/opserr.py is the source: edit it, then run sync_shared.sh).
"""
from __future__ import annotations

import difflib
import json
import sys

CLIP = "…(+%d chars)"


def clip(text, limit):
    """text cut to `limit` characters, ending in the marker …(+n chars) when it was longer."""
    text = "" if text is None else str(text)
    if limit is None or limit <= 0 or len(text) <= limit:
        return text
    return text[:limit] + CLIP % (len(text) - limit)


def did_you_mean(word, candidates, n=1):
    """The closest candidate(s) to word (None when nothing is close)."""
    pool = [str(c) for c in candidates]
    found = difflib.get_close_matches(str(word), pool, n=n, cutoff=0.6)
    if not found:
        found = [c for c in pool if str(word).lower() == c.lower()][:n]
    return (found[0] if n == 1 else found) if found else None


class OpError(Exception):
    """A rejected input: reason is a stable code, extra keys go into the failure record."""

    def __init__(self, reason, message, **extra):
        super().__init__(message)
        self.reason, self.message, self.extra = reason, message, extra

    def record(self, index=None, op=None, usage=None):
        out = {}
        if index is not None:
            out["index"] = index
        if op is not None:
            out["op"] = op
        out.update({"reason": self.reason, "error": self.message})
        out.update({k: v for k, v in self.extra.items() if v not in (None, [], "")})
        if usage:
            out["usage"] = usage
        return out


def unknown(kind, word, candidates, reason, **extra):
    """OpError for a name that is not in candidates: lists them and the closest one."""
    candidates = list(candidates)
    return OpError(reason, f"unknown {kind} {word!r}", available=candidates[:40],
                   did_you_mean=did_you_mean(word, candidates), **extra)


def fail(failures, next_step=None, file=None, applied=0, code=1):
    """Print the failure report (stdout, one JSON object) and exit with `code`."""
    first = failures[0] if failures else {"reason": "error", "error": "failed"}
    report = {"ok": False, "error": first["error"], "reason": first["reason"]}
    if file:
        report["file"] = file
    report["failures"] = failures
    report["applied"] = applied
    report["next_step"] = next_step or ("fix the failing op(s) and resend the whole batch: nothing was applied "
                                        "(the file is unchanged)")
    print(json.dumps(report, indent=2, ensure_ascii=False))
    sys.exit(code)
