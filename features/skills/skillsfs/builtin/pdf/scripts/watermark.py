#!/usr/bin/env python3
"""Stamp a text watermark (e.g. DRAFT / CONFIDENTIEL / COPY) or a PDF/PNG overlay on pages.

Usage:
  python3 watermark.py IN.pdf OUT.pdf --text "DRAFT"
  python3 watermark.py IN.pdf OUT.pdf --text "BROUILLON" --opacity 0.15 --angle 45 --size 96 --color 8C96A3
  python3 watermark.py IN.pdf OUT.pdf --text "Copy for J. Doe" --position footer --size 9
  python3 watermark.py IN.pdf OUT.pdf --overlay stamp.pdf            # first page of stamp.pdf on every page
  python3 watermark.py IN.pdf OUT.pdf --overlay logo.png --position top-right --width-pt 90
  options: --pages "1,3-5" (default all), --under (draw below the page content)

Page sizes/rotations are respected (one overlay is generated per distinct page size).
Prints {"out", "pages_stamped"}.
"""
from __future__ import annotations

import argparse
import io
import json
import sys


def parse_pages(spec, n):
    if not spec:
        return set(range(1, n + 1))
    out = set()
    for part in spec.split(","):
        if "-" in part:
            a, b = part.split("-", 1)
            out.update(range(int(a or 1), int(b or n) + 1))
        elif part.strip():
            out.add(int(part))
    return out


def text_overlay(w, h, args):
    from reportlab.lib.colors import HexColor
    from reportlab.pdfgen import canvas

    buf = io.BytesIO()
    c = canvas.Canvas(buf, pagesize=(w, h))
    c.setFillColor(HexColor("#" + args.color.lstrip("#")))
    c.setFillAlpha(args.opacity)
    font = "Helvetica-Bold" if args.position == "center" else "Helvetica"
    if args.position == "center":
        c.saveState()
        c.translate(w / 2, h / 2)
        c.rotate(args.angle)
        size = args.size
        # shrink so the text fits the page diagonal
        while c.stringWidth(args.text, font, size) > (w * w + h * h) ** 0.5 * 0.8 and size > 10:
            size -= 4
        c.setFont(font, size)
        c.drawCentredString(0, -size / 3, args.text)
        c.restoreState()
    else:
        size = args.size if args.size != 96 else 9
        c.setFont(font, size)
        if args.position == "footer":
            c.drawCentredString(w / 2, 14, args.text)
        elif args.position == "header":
            c.drawCentredString(w / 2, h - 20, args.text)
        else:
            c.drawRightString(w - 24, h - 24, args.text)
    c.save()
    buf.seek(0)
    return buf


def image_overlay(w, h, args):
    from reportlab.lib.utils import ImageReader
    from reportlab.pdfgen import canvas

    img = ImageReader(args.overlay)
    iw, ih = img.getSize()
    tw = args.width_pt
    th = tw * ih / iw
    pos = {"top-right": (w - tw - 24, h - th - 24), "top-left": (24, h - th - 24),
           "footer": ((w - tw) / 2, 14), "header": ((w - tw) / 2, h - th - 14),
           "center": ((w - tw) / 2, (h - th) / 2)}[args.position]
    buf = io.BytesIO()
    c = canvas.Canvas(buf, pagesize=(w, h))
    c.setFillAlpha(args.opacity if args.opacity < 1 else 1)
    c.drawImage(img, pos[0], pos[1], tw, th, mask="auto")
    c.save()
    buf.seek(0)
    return buf


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("input")
    ap.add_argument("out")
    ap.add_argument("--text")
    ap.add_argument("--overlay", help="PDF (first page) or PNG/JPG to stamp")
    ap.add_argument("--opacity", type=float, default=0.12)
    ap.add_argument("--angle", type=float, default=45)
    ap.add_argument("--size", type=float, default=96)
    ap.add_argument("--color", default="8C96A3")
    ap.add_argument("--position", choices=["center", "footer", "header", "top-right", "top-left"], default="center")
    ap.add_argument("--width-pt", type=float, default=120, help="image overlay width")
    ap.add_argument("--pages")
    ap.add_argument("--under", action="store_true")
    args = ap.parse_args(argv)
    if not (args.text or args.overlay):
        ap.error("give --text or --overlay")

    from pypdf import PdfReader, PdfWriter

    reader = PdfReader(args.input)
    writer = PdfWriter(clone_from=reader)
    targets = parse_pages(args.pages, len(writer.pages))
    cache = {}
    stamped = 0
    for i, page in enumerate(writer.pages, 1):
        if i not in targets:
            continue
        if page.rotation:
            page.transfer_rotation_to_content()
        w, h = float(page.mediabox.width), float(page.mediabox.height)
        key = (round(w), round(h))
        if key not in cache:
            if args.overlay and args.overlay.lower().endswith(".pdf"):
                cache[key] = PdfReader(args.overlay).pages[0]
            else:
                buf = image_overlay(w, h, args) if args.overlay else text_overlay(w, h, args)
                cache[key] = PdfReader(buf).pages[0]
        page.merge_page(cache[key], over=not args.under)
        stamped += 1
    with open(args.out, "wb") as fh:
        writer.write(fh)
    print(json.dumps({"out": args.out, "pages_stamped": stamped}))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
