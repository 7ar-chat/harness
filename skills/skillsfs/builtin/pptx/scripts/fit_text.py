#!/usr/bin/env python3
"""Layout checker for .pptx decks (the FINISH GATE of the pptx skill): text that overflows its
box, table rows too short for their text, shapes or images outside the slide, overlapping
shapes, stretched images, text below the minimum size, missing titles, empty placeholders.
Optionally shrinks overflowing text to fit.

Text is measured with the same fonts LibreOffice uses in this sandbox (fontconfig
substitution, e.g. Arial -> Liberation Sans), wrapping words to the box width minus its
insets. A placeholder's insets, wrap and autofit are inherited from its layout and master
placeholders like PowerPoint does (templates often set zero insets on the master), and the
leading below the last line (not drawn) is not counted. The estimate is close (+/- one
line), so treat results near the limit as risky.

Usage:
  python3 fit_text.py DECK.pptx                          # check, deck unchanged
  python3 fit_text.py DECK.pptx --fix --out FIXED.pptx   # shrink overflowing text (not below --min-pt)
  python3 fit_text.py DECK.pptx --fix --inplace
  python3 fit_text.py DECK.pptx --slides 2,5
  python3 fit_text.py DECK.pptx --baseline ORIGINAL.pptx   # overlaps already in the original only warn

Output: {"ok", "file", "checks": [{"name", "ok", "detail"}], "next_step", "summary": {type: n},
"slides": [{"slide": n, "issues": [{"type", "shape", "detail"}]}]}.
Exit code: 0 = every check passed, 2 = at least one check failed (fix, re-run), 1 = usage error.
Checks (issue types): text_fits (overflow: text taller than its box; overflow_x: no-wrap text or
a long word wider than its box or table cell), tables_fit (table_overflow: wrapped cell text
taller than its row, the table grows into what is below), inside_slide (offslide: a shape,
picture or group child outside the slide), overlap, images (stretched: picture aspect ratio
differs from the image), min_font (small_font), titles (no_title: a slide without any text;
empty_title: an empty title placeholder), placeholders (empty_placeholder: "Click to add text"),
slide_numbers (stale_slide_number: a slide-number field whose stored text is not the slide's
position, typically copied with a template slide; --fix rewrites it).
Overlaps fail only in decks the agent made (BUILT_MARKER in the core properties' identifier, set by
build_deck.py):
in other decks (uploaded, edited) they are warnings (the overlap check stays ok, its detail starts with
"warning:"), unless --baseline names the original deck: then only the overlaps it already had warn.
Shapes inside groups are checked too ("Group > Shape" names); native table cells are measured
with their column width and cell margins. "fixed" (after --fix) is informational.

Font-size rule (one rule for the whole pptx skill): no slide text below MIN_PT = 12 pt; the
footer, slide number and date may go down to FOOTER_MIN_PT = 10 pt. Body text should be 18 pt
or more (design rule, not checked).
"""
from __future__ import annotations

import argparse
import functools
import json
import os
import re
import subprocess
import sys

EMU_PER_PT = 12700
MIN_PT = 12
FOOTER_MIN_PT = 10
CHECKS = {"text_fits": ("overflow", "overflow_x"), "tables_fit": ("table_overflow",), "inside_slide": ("offslide",),
          "overlap": ("overlap",), "images": ("stretched",), "min_font": ("small_font",),
          "titles": ("no_title", "empty_title"), "placeholders": ("empty_placeholder",),
          "slide_numbers": ("stale_slide_number",)}
BLOCKING = tuple(t for types in CHECKS.values() for t in types)
# Non-blocking issue types reported in a check's detail ("warning: ...").
WARNINGS = {"overlap": ("overlap_warning",), "min_font": ("small_font_inherited",)}
# build_deck.py (and the agent's own python-pptx code, pptx SKILL.md 3b) writes it into
# core_properties.identifier: the agent laid the deck out, so its overlaps fail.
BUILT_MARKER = "studio-work:agent-deck"
FOOTER_PH = ("SLIDE_NUMBER", "FOOTER", "DATE", "HEADER")
CMD = "python3 /skills/pptx/scripts/fit_text.py"
A = "http://schemas.openxmlformats.org/drawingml/2006/main"
P = "http://schemas.openxmlformats.org/presentationml/2006/main"
NS = {"a": A, "p": P}
FALLBACK_FONT = "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf"


@functools.lru_cache(maxsize=64)
def font_file(family: str, bold: bool) -> str:
    pattern = f"{family}:bold" if bold else family
    try:
        out = subprocess.run(["fc-match", "-f", "%{file}", pattern], capture_output=True, text=True, timeout=10)
        if out.returncode == 0 and out.stdout.strip():
            return out.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return FALLBACK_FONT


@functools.lru_cache(maxsize=256)
def pil_font(path: str, size_pt: float):
    from PIL import ImageFont

    return ImageFont.truetype(path, max(1, int(round(size_pt * 10))))


def text_width_pt(text: str, family: str, bold: bool, size_pt: float) -> float:
    return pil_font(font_file(family, bold), size_pt).getlength(text) / 10.0


# ---------------------------------------------------------------- inheritance helpers
def lvl_pPr(container, level):
    """a:lvlNpPr inside a lstStyle / titleStyle / bodyStyle element."""
    if container is None:
        return None
    return container.find(f"a:lvl{level + 1}pPr", NS)


def ph_key(shape):
    pf = shape.placeholder_format
    return pf.idx, str(pf.type).split(".")[-1].split(" ")[0]


def find_inherited_placeholder(owner_shapes, idx, ptype):
    by_idx = by_type = None
    for sh in owner_shapes:
        if not sh.is_placeholder:
            continue
        i, t = ph_key(sh)
        if i == idx and by_idx is None:
            by_idx = sh
        if t == ptype and by_type is None:
            by_type = sh
    return by_idx or by_type


# a:bodyPr defaults (EMU) when no level of the chain sets an inset.
DEFAULT_INSETS = {"lIns": 91440, "tIns": 45720, "rIns": 91440, "bIns": 45720}
AUTOFIT_TAGS = ("normAutofit", "spAutoFit", "noAutofit")


def body_prs(shape, slide=None):
    """a:bodyPr of the shape, then of the layout and master placeholders it inherits from
    (PowerPoint resolves insets, wrap and autofit through that chain)."""
    out = []
    bp = shape.text_frame._txBody.find("a:bodyPr", NS)
    if bp is not None:
        out.append(bp)
    if slide is not None and shape.is_placeholder:
        idx, ptype = ph_key(shape)
        layout = slide.slide_layout
        for owner in (layout, layout.slide_master):
            ph = find_inherited_placeholder(owner.placeholders, idx, ptype)
            b = ph._element.find(".//a:bodyPr", NS) if ph is not None else None
            if b is not None:
                out.append(b)
    return out


def body_attr(chain, name, default=None):
    for bp in chain:
        if bp.get(name) is not None:
            return bp.get(name)
    return default


def insets(shape, slide=None):
    """(left, top, right, bottom) text insets in EMU, inherited through the placeholder chain."""
    chain = body_prs(shape, slide)
    return tuple(int(body_attr(chain, k, DEFAULT_INSETS[k])) for k in ("lIns", "tIns", "rIns", "bIns"))


class Resolver:
    """Resolve font size / family / left margin for a paragraph through the placeholder,
    layout, master and theme chain."""

    def __init__(self, prs):
        self.prs = prs
        self.theme = {}
        try:
            from lxml import etree
            from pptx.opc.constants import RELATIONSHIP_TYPE as RT

            master = prs.slide_masters[0]
            theme = etree.fromstring(master.part.part_related_by(RT.THEME).blob)
            major = theme.find(".//a:fontScheme/a:majorFont/a:latin", NS)
            minor = theme.find(".//a:fontScheme/a:minorFont/a:latin", NS)
            self.theme = {"major": major.get("typeface") if major is not None else "Arial",
                          "minor": minor.get("typeface") if minor is not None else "Arial"}
        except Exception:  # noqa: BLE001
            self.theme = {"major": "Arial", "minor": "Arial"}

    def chain(self, shape, slide):
        """lstStyle-like containers from most to least specific, plus master txStyle."""
        out = []
        txb = shape._element.find(".//p:txBody", NS)
        if txb is None:
            txb = shape._element.find(".//a:txBody", NS)
        if txb is not None:
            out.append(txb.find("a:lstStyle", NS))
        kind = "other"
        if shape.is_placeholder:
            idx, ptype = ph_key(shape)
            kind = "title" if ptype in ("TITLE", "CENTER_TITLE") else "body" if ptype in (
                "BODY", "OBJECT", "SUBTITLE", "VERTICAL_BODY", "VERTICAL_OBJECT") else "other"
            layout = slide.slide_layout
            for owner in (layout, layout.slide_master):
                ph = find_inherited_placeholder(owner.placeholders, idx, ptype)
                if ph is not None:
                    t = ph._element.find(".//p:txBody", NS)
                    if t is not None:
                        out.append(t.find("a:lstStyle", NS))
        master = slide.slide_layout.slide_master._element
        tx = master.find("p:txStyles", NS)
        if tx is not None:
            out.append(tx.find({"title": "p:titleStyle", "body": "p:bodyStyle"}.get(kind, "p:otherStyle"), NS))
        return [c for c in out if c is not None], kind

    def resolve(self, shape, slide, paragraph, run=None):
        level = paragraph.level
        chain, kind = self.chain(shape, slide)
        size = family = None
        bold = False
        if run is not None:
            if run.font.size:
                size = run.font.size.pt
            if run.font.name:
                family = run.font.name
            bold = bool(run.font.bold)
        ppr = paragraph._p.find("a:pPr", NS)
        marl = int(ppr.get("marL")) if ppr is not None and ppr.get("marL") else None
        pdef = ppr.find("a:defRPr", NS) if ppr is not None else None
        if pdef is not None:                          # paragraph.font.size / .name
            if size is None and pdef.get("sz"):
                size = int(pdef.get("sz")) / 100
            lat = pdef.find("a:latin", NS)
            if family is None and lat is not None and lat.get("typeface"):
                family = lat.get("typeface")
        for c in chain:
            lp = lvl_pPr(c, level)
            if lp is None:
                continue
            d = lp.find("a:defRPr", NS)
            if size is None and d is not None and d.get("sz"):
                size = int(d.get("sz")) / 100
            if family is None and d is not None:
                lat = d.find("a:latin", NS)
                if lat is not None and lat.get("typeface"):
                    family = lat.get("typeface")
            if marl is None and lp.get("marL"):
                marl = int(lp.get("marL"))
        size = size or 18.0
        family = family or ("+mj-lt" if kind == "title" else "+mn-lt")
        if family.startswith("+mj"):
            family = self.theme["major"]
        elif family.startswith("+mn"):
            family = self.theme["minor"]
        return size, family, bold, (marl or 0) / EMU_PER_PT, kind


# ---------------------------------------------------------------- measurement
def wrap_lines(text, width_pt, family, bold, size):
    lines = 0
    widest = 0.0
    for hard in (text.split("\n") if text else [""]):
        words = hard.split(" ")
        cur = ""
        n = 1
        for w in words:
            trial = (cur + " " + w) if cur else w
            if text_width_pt(trial, family, bold, size) <= width_pt or not cur:
                cur = trial
            else:
                widest = max(widest, text_width_pt(cur, family, bold, size))
                n += 1
                cur = w
        widest = max(widest, text_width_pt(cur, family, bold, size))
        lines += n
    return lines, widest


def text_height(tf, shape, slide, resolver, avail_w, wrap=True, scale=1.0, trim_trailing=False, stats=None):
    """Height (pt) of the paragraphs of a text frame laid out in avail_w points.
    trim_trailing: leave out the leading below the last line (renderers do not draw it).
    stats (dict, optional) receives "lines" (laid-out lines) and "max_size" (largest font, pt).
    Returns (height_pt, widest_line_pt, smallest_font_pt, widest_word_pt)."""
    total = 0.0
    trailing = 0.0
    widest = 0.0
    widest_word = 0.0
    min_font = None
    paragraphs = tf.paragraphs
    for i, p in enumerate(paragraphs):
        runs = p.runs
        text = "".join(r.text for r in runs) if runs else ""
        size, family, bold, marl, _ = resolver.resolve(shape, slide, p, runs[0] if runs else None)
        if runs:
            sizes = [resolver.resolve(shape, slide, p, r)[0] for r in runs]
            size = max(sizes)
            min_font = min([min_font] + sizes) if min_font is not None else min(sizes)
        size *= scale
        width = max(10.0, avail_w - marl)
        if wrap:
            lines, w = wrap_lines(text, width, family, bold, size)
            for word in text.split():
                widest_word = max(widest_word, text_width_pt(word, family, bold, size) + marl)
        else:
            lines, w = len(text.split("\n")), max(text_width_pt(t, family, bold, size) for t in text.split("\n"))
        widest = max(widest, w + marl)
        ls = p.line_spacing
        if ls is None:
            line_h = size * 1.2
        elif isinstance(ls, float):
            line_h = size * 1.2 * ls
        else:
            line_h = ls.pt
        total += lines * line_h
        trailing = max(0.0, line_h - size) if lines else 0.0
        if stats is not None:
            stats["lines"] = stats.get("lines", 0) + lines
            stats["max_size"] = max(stats.get("max_size", 0.0), size)
        if i > 0 and p.space_before is not None:
            total += p.space_before.pt
        if p.space_after is not None and i < len(paragraphs) - 1:
            total += p.space_after.pt
    if trim_trailing:
        total -= trailing
    return total, widest, min_font, widest_word


def measure(shape, slide, resolver, scale=1.0, width=None, height=None, stats=None):
    """Return (needed_height_pt, available_height_pt, widest_line_pt, available_width_pt,
    min_font, wrap). width/height (EMU) override the shape size (grouped shapes); stats: see
    text_height."""
    tf = shape.text_frame
    wrap = body_attr(body_prs(shape, slide), "wrap") != "none"
    width = shape.width if width is None else width
    height = shape.height if height is None else height
    left, top, right, bottom = insets(shape, slide)
    avail_w = (width - left - right) / EMU_PER_PT
    avail_h = (height - top - bottom) / EMU_PER_PT
    total, widest, min_font, _ = text_height(tf, shape, slide, resolver, avail_w, wrap, scale, trim_trailing=True, stats=stats)
    return total, avail_h, widest, avail_w, min_font, wrap


def table_rows_needed(frame, slide, resolver, col_widths=None):
    """Needed height (EMU) of every row of a native table (wrapped text + cell margins),
    plus cell-level problems [(row, col, kind, detail)]."""
    tbl = frame.table
    widths = col_widths or [c.width for c in tbl.columns]
    needed = [0] * len(tbl.rows)
    problems = []
    for r, row in enumerate(tbl.rows):
        for c, cell in enumerate(row.cells):
            if cell.is_spanned:
                continue
            span_w = cell.span_width if cell.is_merge_origin else 1
            span_h = cell.span_height if cell.is_merge_origin else 1
            width = sum(widths[c:c + span_w])
            avail_w = (width - cell.margin_left - cell.margin_right) / EMU_PER_PT
            h_pt, _, _, word = text_height(cell.text_frame, frame, slide, resolver, avail_w)
            if not cell.text_frame.text.strip():
                h_pt = 0
            if word > avail_w + 2:
                problems.append((r, c, "overflow_x", f"word {word:.0f} pt wide, cell {avail_w:.0f} pt"))
            h = int(h_pt * EMU_PER_PT) + cell.margin_top + cell.margin_bottom
            if span_h == 1:
                needed[r] = max(needed[r], h)
    return needed, problems


def group_children(group, xf):
    """Child shapes of a group with their transform to slide coordinates."""
    xfrm = group._element.find("p:grpSpPr/a:xfrm", NS)
    if xfrm is None:
        return [(ch, xf) for ch in group.shapes]
    off, ext = xfrm.find("a:off", NS), xfrm.find("a:ext", NS)
    choff, chext = xfrm.find("a:chOff", NS), xfrm.find("a:chExt", NS)
    if None in (off, ext, choff, chext):
        return [(ch, xf) for ch in group.shapes]
    sx = int(ext.get("cx")) / max(1, int(chext.get("cx")))
    sy = int(ext.get("cy")) / max(1, int(chext.get("cy")))
    local = (int(off.get("x")), int(off.get("y")), int(choff.get("x")), int(choff.get("y")), sx, sy)
    return [(ch, [local] + xf) for ch in group.shapes]


def apply_xf(xf, l, t, r, b):
    for ox, oy, cx, cy, sx, sy in xf:                   # innermost group first
        l, r = ox + (l - cx) * sx, ox + (r - cx) * sx
        t, b = oy + (t - cy) * sy, oy + (b - cy) * sy
    return l, t, r, b


def flatten(shapes, prefix="", xf=None):
    """(shape, display name, (l, t, r, b) in slide EMU) for every non-group shape, recursively."""
    xf = xf or []
    for sh in shapes:
        if sh.shape_type is not None and int(sh.shape_type) == 6:          # MSO_SHAPE_TYPE.GROUP
            for child, cxf in group_children(sh, xf):
                yield from flatten([child], prefix + sh.name + " > ", cxf)
            continue
        if sh.width is None or sh.height is None:
            continue
        l, t, r, b = rect(sh)
        yield sh, prefix + sh.name, tuple(int(v) for v in apply_xf(xf, l, t, r, b))


def autofit_kind(shape, slide=None):
    """Autofit of a text frame: the nearest level of the placeholder chain that sets one."""
    for bp in body_prs(shape, slide):
        for tag in AUTOFIT_TAGS:
            if bp.find(f"a:{tag}", NS) is not None:
                return tag
    return None


def rect(sh):
    return sh.left or 0, sh.top or 0, (sh.left or 0) + (sh.width or 0), (sh.top or 0) + (sh.height or 0)


def is_footer(shape, name):
    if "footer" in name.lower() or "slide number" in name.lower():
        return True
    return bool(shape.is_placeholder and ph_key(shape)[1] in FOOTER_PH)


def picture_issue(shape, name):
    """'stretched' issue when a picture is shown with another aspect ratio than its image."""
    try:
        px_w, px_h = shape.image.size
    except Exception:  # noqa: BLE001
        return None
    crop_w = 1 - (shape.crop_left or 0) - (shape.crop_right or 0)
    crop_h = 1 - (shape.crop_top or 0) - (shape.crop_bottom or 0)
    if not (px_w and px_h and shape.width and shape.height and crop_w > 0 and crop_h > 0):
        return None
    native = (px_w * crop_w) / (px_h * crop_h)
    shown = shape.width / shape.height
    if abs(shown / native - 1) <= 0.03:
        return None
    return {"type": "stretched", "shape": name,
            "detail": f"shown at {shown:.2f}:1, image is {native:.2f}:1 ({abs(shown / native - 1):.0%} distortion): "
                      "keep the ratio (compute the height from the width, or fit the image inside its box)"}


def text_issues(slide):
    """Missing title / empty placeholders of one slide."""
    issues = []
    any_text = False
    for shape in slide.shapes:
        if shape.has_text_frame and shape.text_frame.text.strip():
            any_text = True
        if getattr(shape, "has_table", False) and any(c.text.strip() for row in shape.table.rows for c in row.cells):
            any_text = True
        if not shape.is_placeholder or not shape.has_text_frame or shape.text_frame.text.strip():
            continue
        kind = ph_key(shape)[1]
        if kind in FOOTER_PH:
            continue
        if kind in ("TITLE", "CENTER_TITLE"):
            issues.append({"type": "empty_title", "shape": shape.name,
                           "detail": "empty title placeholder ('Click to add title'): write the slide's message title"})
        else:
            issues.append({"type": "empty_placeholder", "shape": shape.name,
                           "detail": f"empty {kind.lower()} placeholder ('Click to add text'): fill it or delete it "
                                     "(ph._element.getparent().remove(ph._element))"})
    if not any_text:
        issues.append({"type": "no_title", "shape": "-", "detail": "the slide has no text at all: give it a title "
                                                                    "that states its message"})
    return issues


def slide_number_issues(slide, n, fix=False):
    """Slide-number fields whose stored text is not the slide's position (n). PowerPoint and
    LibreOffice recompute the field when they draw it, but previews, search and text
    extraction read the stored text (e.g. "12" on slide 3, copied with a template slide)."""
    issues = []
    for fld in slide._element.iter(f"{{{A}}}fld"):
        if fld.get("type") != "slidenum":
            continue
        t = fld.find("a:t", NS)
        cur = (t.text or "").strip() if t is not None else ""
        if cur == str(n):
            continue
        sp = fld.getparent()
        while sp is not None and sp.tag != f"{{{P}}}sp":
            sp = sp.getparent()
        cnv = sp.find(".//p:cNvPr", NS) if sp is not None else None
        name = cnv.get("name") if cnv is not None else "-"
        if fix and t is not None:
            t.text = str(n)
            issues.append({"type": "fixed", "shape": name, "detail": f"slide number field text {cur!r} -> {str(n)!r}"})
        else:
            issues.append({"type": "stale_slide_number", "shape": name,
                           "detail": f"slide number field stores {cur!r} on slide {n} (copied from another slide): "
                                     f"run {CMD} DECK.pptx --fix --inplace, or set the field's a:t text to {str(n)!r}"})
    return issues


def explicit_min_size(shape):
    """Smallest font size set on the shape's own runs / paragraphs (None: every size is inherited
    from the layout, master or theme, i.e. the template's design)."""
    sizes = []
    for p in shape.text_frame.paragraphs:
        ppr = p._p.find("a:pPr", NS)
        d = ppr.find("a:defRPr", NS) if ppr is not None else None
        if d is not None and d.get("sz"):
            sizes.append(int(d.get("sz")) / 100)
        sizes += [r.font.size.pt for r in p.runs if r.font.size]
    return min(sizes) if sizes else None


def check_slide(slide, n, prs, resolver, small_pt=MIN_PT, fix=False, min_pt=MIN_PT):
    issues = text_issues(slide) + slide_number_issues(slide, n, fix)
    sw, sh_ = prs.slide_width, prs.slide_height
    tol = 2 * EMU_PER_PT
    boxes = []
    for shape, name, (l, t, r, b) in flatten(slide.shapes):
        if shape.shape_type is not None and int(shape.shape_type) == 13:
            pic = picture_issue(shape, name)
            if pic:
                issues.append(pic)
        if getattr(shape, "has_table", False):
            needed, problems = table_rows_needed(shape, slide, resolver)
            heights = [row.height for row in shape.table.rows]
            grown = [max(h, nd) for h, nd in zip(heights, needed)]
            b = max(b, t + sum(grown))           # renderers lay rows out by row height, growing to fit
            for ri, (h, nd) in enumerate(zip(heights, needed)):
                if nd > h + tol:
                    issues.append({"type": "table_overflow", "shape": name, "row": ri + 1,
                                   "detail": f"row {ri + 1} needs {nd / 914400:.2f} in for its wrapped text, row height "
                                             f"{h / 914400:.2f} in: the row grows and the table ends at {b / 914400:.2f} in. "
                                             "Set the row heights from the text (or build_deck.py table slide), "
                                             "shorten the text, widen the column or split the table"})
            for ri, ci, kind, detail in problems:
                issues.append({"type": kind, "shape": f"{name} cell r{ri + 1}c{ci + 1}", "detail": detail})
        if l < -tol or t < -tol or r > sw + tol or b > sh_ + tol:
            issues.append({"type": "offslide", "shape": name,
                           "detail": f"box {l / 914400:.2f},{t / 914400:.2f} -> {r / 914400:.2f},{b / 914400:.2f} in; "
                                     f"slide {sw / 914400:.2f}x{sh_ / 914400:.2f} in"})
        area = (r - l) * (b - t)
        has_text = shape.has_text_frame and shape.text_frame.text.strip()
        if area < 0.8 * sw * sh_ and (has_text or getattr(shape, "has_chart", False) or getattr(shape, "has_table", False)
                                       or (shape.shape_type is not None and int(shape.shape_type) == 13)):
            boxes.append((name, (l, t, r, b)))
        if not has_text:
            continue
        w_emu, h_emu = r - l, b - t
        stats = {}
        needed, avail, widest, avail_w, min_font, wrap = measure(shape, slide, resolver, width=w_emu, height=h_emu,
                                                                 stats=stats)
        fit = autofit_kind(shape, slide)
        floor = min(small_pt, FOOTER_MIN_PT) if is_footer(shape, name) else small_pt
        explicit = explicit_min_size(shape)
        if min_font is not None and min_font < floor and (explicit is None or explicit >= floor):
            issues.append({"type": "small_font_inherited", "shape": name,
                           "detail": f"{min_font:g} pt, inherited from the layout/master (the template's design, "
                                     "not set on this text): keep it unless the user asked for larger text"})
        elif min_font is not None and min_font < floor:
            issues.append({"type": "small_font", "shape": name,
                           "detail": f"{min_font:g} pt < {floor:g} pt (rule: slide text >= {small_pt:g} pt, footer and "
                                     f"slide number >= {FOOTER_MIN_PT} pt): enlarge it, shorten the text or split "
                                     "the slide"})
        if not wrap and widest > avail_w + 2:
            issues.append({"type": "overflow_x", "shape": name,
                           "detail": f"line {widest:.0f} pt wide, box {avail_w:.0f} pt (wrap is off: set "
                                     "text_frame.word_wrap = True, or use a native table for tabular content)"})
        # One line that fits the width can only spill part of its own height below the box
        # (a template title drawn larger than its placeholder): nothing is hidden, not an overflow.
        one_line = stats.get("lines") == 1 and widest <= avail_w + 2
        slack = max(2.0, 0.25 * stats.get("max_size", 0.0)) if one_line else 2.0
        if needed > avail + slack:
            detail = {"type": "overflow", "shape": name, "needed_pt": round(needed), "available_pt": round(avail),
                      "autofit": fit, "text": shape.text_frame.text[:80]}
            if fit == "spAutoFit":
                # "resize shape to fit text": the box grows downwards over whatever is below it
                grow = (needed - avail) * EMU_PER_PT
                off = b + grow > sh_ + tol
                detail["detail"] = (f"auto-grow box: text extends {(needed - avail) / 72:.2f} in below the box"
                                    + (" and off the slide" if off else ""))
            if fix:
                scale = 1.0
                while scale > 0.3:
                    scale -= 0.04
                    n_needed, *_ = measure(shape, slide, resolver, scale, w_emu, h_emu)
                    if n_needed <= avail:
                        break
                applied = shrink(shape, slide, resolver, scale, min_pt)
                after, *_ = measure(shape, slide, resolver, width=w_emu, height=h_emu)
                detail["type"] = "fixed" if after <= avail + 2 else "overflow"
                detail["detail"] = f"font scaled x{applied:.2f}; now needs {after:.0f} pt of {avail:.0f}"
                if detail["type"] == "overflow":
                    detail["detail"] += f" (min {min_pt} pt reached: shorten the text or split the slide)"
            issues.append(detail)
    for i in range(len(boxes)):
        for j in range(i + 1, len(boxes)):
            (n1, a), (n2, b) = boxes[i], boxes[j]
            ix = max(0, min(a[2], b[2]) - max(a[0], b[0]))
            iy = max(0, min(a[3], b[3]) - max(a[1], b[1]))
            inter = ix * iy
            if not inter:
                continue
            area_a = (a[2] - a[0]) * (a[3] - a[1])
            area_b = (b[2] - b[0]) * (b[3] - b[1])
            smaller = max(1, min(area_a, area_b))
            contains = inter >= 0.98 * smaller
            if inter / smaller > 0.1 and not contains:
                issues.append({"type": "overlap", "shape": f"{n1} / {n2}", "shapes": [n1, n2],
                               "detail": f"{inter / smaller:.0%} of the smaller shape"})
    return {"slide": n, "issues": issues}


def shrink(shape, slide, resolver, scale, min_pt):
    """Apply explicit font sizes = resolved size * scale (not below min_pt). Returns the
    effective scale applied to the largest font."""
    from pptx.util import Pt

    applied = scale
    for p in shape.text_frame.paragraphs:
        for r in p.runs:
            size = resolver.resolve(shape, slide, p, r)[0]
            new = max(min_pt, round(size * scale * 2) / 2)
            applied = min(applied, new / size) if size else applied
            r.font.size = Pt(new)
    bp = shape.text_frame._txBody.find("a:bodyPr", NS)
    if bp is not None:
        for tag in ("normAutofit",):
            el = bp.find(f"a:{tag}", NS)
            if el is not None:
                bp.remove(el)
    return applied


def next_step(blocking, path="DECK.pptx"):
    if not blocking:
        return (f"OK: all checks passed. Now call view_file on {path} and look at the slides before you answer.")
    n = sum(blocking.values())
    return (f"NOT DONE: {n} blocking issue(s) ({', '.join(sorted(blocking))}). Fix every one (shorter text, word wrap "
            "on, native table with row heights from the text, wider column, image fitted to its box, split the slide), "
            f"save, and run `{CMD} {path}` again - at most 3 rounds. Do not tell the user the deck is ready while ok is "
            "false; after 3 rounds, say exactly which slides still have problems.")


def build_checks(report, opened=True, error=None):
    """Contract checks from the per-slide issues."""
    if not opened:
        return [{"name": "opens", "ok": False, "detail": f"cannot open the file as .pptx: {error}"}]
    checks = [{"name": "opens", "ok": True, "detail": "python-pptx opens the file"}]
    def describe(found):
        detail = "; ".join(f"slide {n} {i['shape']!r}: {i.get('detail') or i['type']}" for n, i in found[:6])
        if len(found) > 6:
            detail += f"; ... {len(found) - 6} more"
        return detail

    for name, types in CHECKS.items():
        found = [(s["slide"], i) for s in report for i in s["issues"] if i["type"] in types]
        warned = [(s["slide"], i) for s in report for i in s["issues"] if i["type"] in WARNINGS.get(name, ())]
        if found:
            detail = describe(found)
        elif warned:
            detail = ("warning: " + describe(warned) + " (the deck was not built by build_deck.py, or the overlap "
                      "is already in the --baseline deck: move the shapes apart only if you placed them)")
        else:
            detail = "ok"
        checks.append({"name": name, "ok": not found, "detail": detail})
    return checks


def overlap_keys(path, slides=None):
    """(slide, shape names) of the overlaps of a deck (the --baseline deck)."""
    from pptx import Presentation

    prs = Presentation(path)
    resolver = Resolver(prs)
    keys = set()
    for n, slide in enumerate(prs.slides, 1):
        if slides and n not in slides:
            continue
        for i in check_slide(slide, n, prs, resolver)["issues"]:
            if i["type"] == "overlap":
                keys.add((n, frozenset(i["shapes"])))
    return keys


def soften_overlaps(prs, report, baseline=None):
    """Overlaps of a deck not built by build_deck.py are warnings; with a baseline deck only the
    overlaps it already had are (new ones stay blocking)."""
    built = (prs.core_properties.identifier or "") == BUILT_MARKER
    if built and baseline is None:
        return
    known = overlap_keys(baseline) if baseline else None
    for s in report:
        for i in s["issues"]:
            if i["type"] == "overlap" and (known is None or (s["slide"], frozenset(i["shapes"])) in known):
                i["type"] = "overlap_warning"


def check_deck(path, slides=None, fix=False, out=None, min_pt=MIN_PT, small_pt=MIN_PT, baseline=None):
    """Run every check; returns (report dict, exit code). baseline: the original deck (its overlaps only warn)."""
    try:
        from pptx import Presentation
        prs = Presentation(path)
    except Exception as exc:  # noqa: BLE001
        checks = build_checks([], False, exc)
        return {"ok": False, "file": path, "checks": checks, "next_step": f"NOT DONE: {path} is not a readable .pptx; "
                "rebuild it", "summary": {}, "slides": []}, 2
    if not len(prs.slides):
        return {"ok": False, "file": path, "checks": [{"name": "opens", "ok": False, "detail": "the deck has no slide"}],
                "next_step": "NOT DONE: the deck has no slide", "summary": {}, "slides": []}, 2
    resolver = Resolver(prs)
    report = []
    for n, slide in enumerate(prs.slides, 1):
        if slides and n not in slides:
            continue
        report.append(check_slide(slide, n, prs, resolver, small_pt, fix, min_pt))
    soften_overlaps(prs, report, baseline)
    summary = {}
    for s in report:
        for i in s["issues"]:
            summary[i["type"]] = summary.get(i["type"], 0) + 1
    blocking = {k: v for k, v in summary.items() if k in BLOCKING}
    if fix:
        prs.save(out or path)
    checks = build_checks(report)
    ok = all(c["ok"] for c in checks)
    return {"ok": ok, "file": path, "checks": checks, "next_step": next_step(blocking, out or path),
            "summary": summary, "out": (out or path) if fix else None,
            "slides": [s for s in report if s["issues"]]}, (0 if ok else 2)


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1 (argparse would exit 2)
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--slides", help="comma-separated 1-based slide numbers")
    ap.add_argument("--fix", action="store_true", help="shrink overflowing text, rewrite stale slide-number fields")
    ap.add_argument("--out")
    ap.add_argument("--inplace", action="store_true")
    ap.add_argument("--min-pt", type=float, default=MIN_PT, help=f"--fix never shrinks below this (default {MIN_PT})")
    ap.add_argument("--small-pt", type=float, default=MIN_PT, help=f"min_font threshold (default {MIN_PT})")
    ap.add_argument("--baseline", help="the original deck: overlaps it already had are warnings, new ones fail")
    args = ap.parse_args(argv)
    if args.fix and not (args.out or args.inplace):
        ap.error("--fix needs --out FIXED.pptx or --inplace")
    args.file = os.path.abspath(args.file)  # the checker contract names the file by its absolute path
    if not os.path.isfile(args.file):
        print(json.dumps({"ok": False, "file": args.file, "error": f"no such file: {args.file}"}))
        return 1
    if args.baseline and not os.path.isfile(args.baseline):
        print(json.dumps({"ok": False, "file": args.file, "error": f"no such baseline deck: {args.baseline}"}))
        return 1
    wanted = {int(x) for x in re.split(r"[,\s]+", args.slides) if x} if args.slides else None
    report, code = check_deck(args.file, wanted, args.fix, args.out if not args.inplace else None,
                              args.min_pt, args.small_pt, args.baseline)
    print(json.dumps(report, indent=2, ensure_ascii=False))
    return code


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
