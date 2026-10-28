#!/usr/bin/env python3
"""Publication-quality static charts (matplotlib) in the default style, sized for slides
and documents. Use as a CLI with a JSON spec or CSV, or import the helpers.

CLI:
  python3 make_chart.py --spec chart.json --out /workspace/outputs/chart.png
  python3 make_chart.py --csv data.csv --x month --y revenue,cost --type line --out chart.png \
      --title "Revenue and cost" --size doc --y-format eur
  python3 make_chart.py --example            # print an example spec
  python3 make_chart.py --check chart.png    # checker: readable size and dpi, not blank

--check prints {"ok", "file", "checks": [{"name", "ok", "detail"}], "next_step"} and exits 0 when
every check passes, 2 when one fails, 1 on a usage error. Checks: opens, size (>= 600 x 300 px),
dpi (>= 150 when recorded), not_blank (some ink on the background), aspect (between 1:4 and 4:1).
SVG files: opens (valid SVG with drawing elements) only. Images make_chart.py did not render (a logo,
a photo; charts are marked with CHART_MARKER in their metadata) only get opens and not_blank.

Spec:
{"type": "bar",               # bar | barh | stacked_bar | line | area | scatter | pie | donut | waterfall
 "title": "Revenue grew 12%",   # say the message, not "Revenue by quarter"
 "subtitle": "M€, 2025-2026",   # unit / scope
 "categories": ["Q1","Q2","Q3"],
 "series": [{"name": "2025", "values": [10, 11, 12]}, {"name": "2026", "values": [11, 12.5, 13.4]}],
 "x_label": null, "y_label": null,
 "y_format": "number",        # number | percent (values are ratios: 0.12) | eur
 "decimals": 0, "locale": "fr",
 "size": "slide",             # slide | slide-half | doc | doc-half | square | [width_in, height_in]
 "data_labels": true, "legend": true,
 "highlight": "Q3",           # optional: emphasise one category, others muted (single series)
 "source": "Source: internal data",
 "ylim": [0, null]}
Output: PNG at 200 dpi (default) or SVG if --out ends with .svg. Prints
{"out", "width_px", "height_px", "dpi", "size_in"}.

Import:
  import sys; sys.path.insert(0, "/skills/charts/scripts"); import make_chart as mc
  fig, ax = mc.figure("doc"); ax.plot(x, y); mc.finish(fig, ax, title="...", source="..."); mc.save(fig, "out.png")
"""
from __future__ import annotations

import argparse
import csv
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# Default palette.
CHART_COLORS = ["1B365D", "2E6DA4", "2A9D8F", "E9A23B", "C8553D", "7D5BA6", "8C96A3", "8AA05B"]
PALETTE = {"ink": "1A1F2B", "slate": "4A5568", "grey": "8C96A3", "rule": "D5DAE1", "mist": "EEF1F5",
           "navy": "1B365D", "muted": "C9D1DB", "positive": "2E7D32", "negative": "C62828"}
SIZES = {"slide": (11.5, 5.2), "slide-half": (5.8, 4.8), "doc": (6.5, 3.6), "doc-half": (3.2, 2.6), "square": (5.0, 5.0)}
DPI = 200
EXAMPLE = {"type": "bar", "title": "Revenue grew every quarter", "subtitle": "Revenue, M€",
           "categories": ["Q1", "Q2", "Q3", "Q4"],
           "series": [{"name": "2025", "values": [10.2, 11.0, 12.1, 12.9]},
                      {"name": "2026", "values": [11.4, 12.5, 13.4, 14.2]}],
           "y_format": "number", "decimals": 1, "size": "slide", "data_labels": True,
           "source": "Source: example data"}

_brand = None


def brand():
    global _brand
    if _brand is None:
        _brand = False  # no external brand module: the built-in palette and style are used
    return _brand or None


def colors():
    b = brand()
    return ["#" + c for c in (b.CHART_COLORS if b else CHART_COLORS)]


def setup():
    """Apply the matplotlib style (Agg backend, fonts, colours, grid)."""
    import matplotlib

    matplotlib.use("Agg")
    b = brand()
    if b:
        b.mpl_apply()
    else:
        from cycler import cycler

        matplotlib.rcParams.update({
            "font.family": "sans-serif", "font.sans-serif": ["Liberation Sans", "DejaVu Sans"],
            "font.size": 10, "axes.titlesize": 13, "axes.titleweight": "bold", "axes.titlelocation": "left",
            "axes.spines.top": False, "axes.spines.right": False, "axes.grid": True, "axes.grid.axis": "y",
            "axes.axisbelow": True, "grid.color": "#" + PALETTE["rule"], "axes.edgecolor": "#" + PALETTE["grey"],
            "axes.prop_cycle": cycler(color=colors()), "legend.frameon": False, "savefig.dpi": DPI,
            "savefig.bbox": "tight", "svg.fonttype": "none", "xtick.color": "#" + PALETTE["slate"],
            "ytick.color": "#" + PALETTE["slate"]})
    return matplotlib


def figure(size="doc"):
    setup()
    import matplotlib.pyplot as plt

    w, h = SIZES[size] if isinstance(size, str) else size
    fig, ax = plt.subplots(figsize=(w, h))
    return fig, ax


def fmt_value(v, kind="number", decimals=0, locale="fr"):
    b = brand()
    if b:
        if kind == "percent":
            return b.format_percent(v, decimals, locale)
        if kind == "eur":
            return b.format_currency(v, "EUR", decimals, locale)
        return b.format_number(v, decimals, locale)
    if kind == "percent":
        return f"{v * 100:.{decimals}f}%"
    return f"{v:,.{decimals}f}" + (" €" if kind == "eur" else "")


def axis_formatter(kind="number", decimals=0, locale="fr"):
    from matplotlib.ticker import FuncFormatter

    return FuncFormatter(lambda x, _p=None: fmt_value(x, kind, decimals, locale))


def finish(fig, ax, title=None, subtitle=None, source=None, legend=None):
    """Title (left, bold) + subtitle (unit) above the plot, source note bottom-left."""
    if title:
        ax.set_title(title, loc="left", pad=22 if subtitle else 10)
    if subtitle:
        ax.text(0, 1.02, subtitle, transform=ax.transAxes, fontsize=9.5, color="#" + PALETTE["slate"], va="bottom")
    if legend is None:
        handles, _ = ax.get_legend_handles_labels()
        legend = len(handles) > 1
    if legend:
        ax.legend(loc="upper left", bbox_to_anchor=(0, -0.08 if not ax.get_xlabel() else -0.16), ncol=6, frameon=False)
    if source:
        fig.text(0.01, -0.02, source, fontsize=8, color="#" + PALETTE["grey"], ha="left", va="top",
                 transform=fig.transFigure)
    fig.tight_layout()
    return fig


# Written into every chart make_chart.py saves (PNG "Software" text chunk, JPEG comment, SVG dc:creator):
# --check applies the chart checks (size, dpi, aspect) only to marked images.
CHART_MARKER = "harness make_chart.py"


def save(fig, out, dpi=DPI):
    import matplotlib.pyplot as plt

    os.makedirs(os.path.dirname(os.path.abspath(out)) or ".", exist_ok=True)
    ext = os.path.splitext(out)[1].lower()
    kw = {}
    if ext == ".png":
        kw["metadata"] = {"Software": CHART_MARKER}
    elif ext == ".svg":
        kw["metadata"] = {"Creator": CHART_MARKER}
    elif ext in (".jpg", ".jpeg"):
        kw["pil_kwargs"] = {"comment": CHART_MARKER.encode()}
    fig.savefig(out, dpi=dpi, bbox_inches="tight", facecolor="white", **kw)
    w_in, h_in = fig.get_size_inches()
    plt.close(fig)
    info = {"out": out, "dpi": dpi, "size_in": [round(w_in, 2), round(h_in, 2)]}
    if out.lower().endswith(".png"):
        from PIL import Image

        with Image.open(out) as im:
            info["width_px"], info["height_px"] = im.size
    return info


def label_bars(ax, bars, kind, decimals, locale, horizontal=False):
    for bar in bars:
        v = bar.get_width() if horizontal else bar.get_height()
        if v is None:
            continue
        text = fmt_value(v, kind, decimals, locale)
        if horizontal:
            ax.annotate(text, (bar.get_x() + v, bar.get_y() + bar.get_height() / 2), xytext=(3, 0),
                        textcoords="offset points", va="center", fontsize=8.5, color="#" + PALETTE["ink"])
        else:
            ax.annotate(text, (bar.get_x() + bar.get_width() / 2, bar.get_y() + v), xytext=(0, 3),
                        textcoords="offset points", ha="center", va="bottom", fontsize=8.5, color="#" + PALETTE["ink"])


def render(spec):
    import numpy as np

    kind = spec.get("type", "bar")
    cats = list(spec.get("categories") or [])
    series = spec.get("series") or []
    yfmt, dec, loc = spec.get("y_format", "number"), int(spec.get("decimals", 0)), spec.get("locale", "fr")
    labels = spec.get("data_labels", False)
    fig, ax = figure(spec.get("size", "doc"))
    cols = colors()
    highlight = spec.get("highlight")

    if kind in ("bar", "stacked_bar", "barh"):
        n = max(1, len(series))
        x = np.arange(len(cats))
        horizontal = kind == "barh"
        if horizontal:
            ax.grid(axis="x")
            ax.grid(axis="y", visible=False)
        width = 0.8 / n if kind != "stacked_bar" else 0.6
        bottom = np.zeros(len(cats))
        for i, s in enumerate(series):
            vals = np.array([np.nan if v is None else v for v in s["values"]], dtype=float)
            color = s.get("color") and "#" + s["color"].lstrip("#") or cols[i % len(cols)]
            bar_colors = color
            if highlight is not None and n == 1:
                idx = cats.index(highlight) if highlight in cats else int(highlight)
                bar_colors = ["#" + PALETTE["muted"]] * len(cats)
                bar_colors[idx] = color
            if kind == "stacked_bar":
                bars = ax.bar(x, vals, width, bottom=bottom, label=s.get("name"), color=bar_colors)
                bottom = bottom + np.nan_to_num(vals)
            elif horizontal:
                bars = ax.barh(x + (i - (n - 1) / 2) * width, vals, width * 0.95, label=s.get("name"), color=bar_colors)
            else:
                bars = ax.bar(x + (i - (n - 1) / 2) * width, vals, width * 0.95, label=s.get("name"), color=bar_colors)
            if labels and kind != "stacked_bar":
                label_bars(ax, bars, yfmt, dec, loc, horizontal)
        if horizontal:
            ax.set_yticks(x, cats)
            ax.invert_yaxis()
            ax.xaxis.set_major_formatter(axis_formatter(yfmt, dec, loc))
        else:
            ax.set_xticks(x, cats)
            ax.yaxis.set_major_formatter(axis_formatter(yfmt, dec, loc))
        ax.tick_params(axis="both", length=0)
    elif kind in ("line", "area"):
        x = np.arange(len(cats))
        for i, s in enumerate(series):
            vals = np.array([np.nan if v is None else v for v in s["values"]], dtype=float)
            color = s.get("color") and "#" + s["color"].lstrip("#") or cols[i % len(cols)]
            if kind == "area":
                ax.fill_between(x, vals, alpha=0.18, color=color)
            ax.plot(x, vals, label=s.get("name"), color=color, marker="o" if len(cats) <= 12 else None, markersize=4)
            if labels and len(vals):
                ax.annotate(fmt_value(vals[-1], yfmt, dec, loc), (x[-1], vals[-1]), xytext=(5, 0),
                            textcoords="offset points", va="center", fontsize=8.5, color=color)
        step = max(1, len(cats) // 12)
        ax.set_xticks(x[::step], [cats[j] for j in range(0, len(cats), step)])
        ax.yaxis.set_major_formatter(axis_formatter(yfmt, dec, loc))
        ax.tick_params(axis="both", length=0)
    elif kind == "scatter":
        for i, s in enumerate(series):
            ax.scatter(s["x"], s["values"], label=s.get("name"), color=cols[i % len(cols)], alpha=0.85, s=28)
        ax.grid(axis="both")
        ax.yaxis.set_major_formatter(axis_formatter(yfmt, dec, loc))
    elif kind in ("pie", "donut"):
        vals = series[0]["values"]
        wedges, *_ = ax.pie(vals, labels=None, colors=cols[:len(vals)], startangle=90, counterclock=False,
                            wedgeprops={"width": 0.38 if kind == "donut" else 1, "edgecolor": "white", "linewidth": 1.5})
        total = float(sum(vals)) or 1.0
        ax.legend(wedges, [f"{c}  {fmt_value(v / total, 'percent', dec, loc)}" for c, v in zip(cats, vals)],
                  loc="center left", bbox_to_anchor=(1.0, 0.5), frameon=False)
        ax.set_aspect("equal")
        ax.grid(False)
        spec = dict(spec, legend=False)
    elif kind == "waterfall":
        vals = [float(v) for v in series[0]["values"]]
        running, bottoms, colors_ = 0.0, [], []
        for i, v in enumerate(vals):
            is_total = spec.get("total_last", True) and i == len(vals) - 1
            if is_total:
                bottoms.append(0)
                colors_.append("#" + PALETTE["navy"])
                v = running if spec.get("compute_total", False) else v
                vals[i] = v
            else:
                bottoms.append(running if v >= 0 else running + v)
                colors_.append("#" + (PALETTE["positive"] if v >= 0 else PALETTE["negative"]) if i else "#" + PALETTE["navy"])
                running += v
        heights = [abs(v) for v in vals]
        bars = ax.bar(range(len(vals)), heights, bottom=bottoms, color=colors_, width=0.6)
        if labels:
            for b_, v in zip(bars, vals):
                ax.annotate(fmt_value(v, yfmt, dec, loc), (b_.get_x() + b_.get_width() / 2, b_.get_y() + b_.get_height()),
                            xytext=(0, 3), textcoords="offset points", ha="center", fontsize=8.5)
        ax.set_xticks(range(len(vals)), cats)
        ax.yaxis.set_major_formatter(axis_formatter(yfmt, dec, loc))
        ax.tick_params(axis="both", length=0)
        spec = dict(spec, legend=False)
    else:
        raise SystemExit(f"error: unknown chart type {kind!r}")

    if spec.get("x_label"):
        ax.set_xlabel(spec["x_label"])
    if spec.get("y_label"):
        ax.set_ylabel(spec["y_label"])
    if spec.get("ylim"):
        lo, hi = spec["ylim"]
        ax.set_ylim(bottom=lo, top=hi)
    finish(fig, ax, spec.get("title"), spec.get("subtitle"), spec.get("source"), spec.get("legend"))
    return fig


MIN_W, MIN_H, MIN_DPI = 600, 300, 150
CHECK_CMD = "python3 /skills/charts/scripts/make_chart.py --check"


def is_chart(info):
    """Whether an image's metadata (PIL info) carries CHART_MARKER (rendered by make_chart.py)."""
    for v in info.values():
        if isinstance(v, bytes):
            v = v.decode("utf-8", "replace")
        if isinstance(v, str) and CHART_MARKER in v:
            return True
    return False


def check_image(path):
    """Checker contract: (report, exit code) for a chart image; other images (not rendered by
    make_chart.py) only get opens and not_blank."""
    checks = []

    def add(name, ok, detail):
        checks.append({"name": name, "ok": bool(ok), "detail": detail})

    if path.lower().endswith(".svg"):
        try:
            from lxml import etree

            root = etree.parse(path).getroot()
            marks = sum(1 for el in root.iter() if isinstance(el.tag, str) and el.tag.split("}")[-1] in
                        ("path", "rect", "line", "polyline", "polygon", "circle", "ellipse", "text", "use", "image"))
            is_svg = root.tag.split("}")[-1] == "svg"
            if CHART_MARKER in etree.tostring(root, encoding="unicode"):
                add("opens", is_svg and marks > 5,
                    f"SVG with {marks} drawing elements" if marks > 5 else "SVG without drawing elements")
            else:  # not a make_chart chart (a logo, an icon): it opens and draws something
                add("opens", is_svg, "valid SVG" if is_svg else "not an SVG document")
                add("not_blank", marks > 0, f"{marks} drawing elements" if marks else "SVG without drawing elements")
        except Exception as exc:  # noqa: BLE001
            add("opens", False, f"not a valid SVG: {exc}")
    else:
        try:
            from PIL import Image

            with Image.open(path) as im:
                im.load()
                fmt, (w, h), info = im.format, im.size, dict(im.info)
                rgb = im.convert("RGB")
        except Exception as exc:  # noqa: BLE001
            add("opens", False, f"cannot open the image: {exc}")
            rgb = None
        chart = rgb is not None and is_chart(info)
        if rgb is not None:
            add("opens", True, f"{fmt} {w} x {h} px")
        if chart:
            add("size", w >= MIN_W and h >= MIN_H,
                f"{w} x {h} px" + ("" if w >= MIN_W and h >= MIN_H else
                                   f" is too small (min {MIN_W} x {MIN_H}): render it at its final size "
                                   "(size doc / slide) with dpi 200"))
            dpi = info.get("dpi")
            dpi = round(float(dpi[0])) if isinstance(dpi, (tuple, list)) and dpi and dpi[0] else None
            if dpi is None:
                add("dpi", True, "no dpi recorded; pixel size checked instead")
            else:
                add("dpi", dpi >= MIN_DPI, f"{dpi} dpi" + ("" if dpi >= MIN_DPI else
                                                           f" (min {MIN_DPI}): save with dpi=200 (make_chart default)"))
        if rgb is not None:
            small = rgb.copy()
            small.thumbnail((300, 300))
            counts = small.getcolors(small.width * small.height) or []
            bg = max(counts)[1] if counts else (255, 255, 255)
            ink = sum(n for n, px in counts if sum(abs(a - b) for a, b in zip(px, bg)) > 60) / max(
                1, small.width * small.height)
            add("not_blank", ink >= 0.005, f"{ink:.1%} of the pixels differ from the background"
                + ("" if ink >= 0.005 else ": the image is empty (no data plotted?)"))
        if chart:
            ratio = w / h if h else 0
            add("aspect", 0.25 <= ratio <= 4, f"{ratio:.2f}:1" + ("" if 0.25 <= ratio <= 4 else
                                                                 " is extreme: check the figure size"))
    ok = all(c["ok"] for c in checks)
    failed = [c["name"] for c in checks if not c["ok"]]
    step = (f"OK: all checks passed. Now call view_file on {path} and look at it (title, labels, legend, units) "
            "before you use it." if ok else
            f"NOT DONE: fix the failing checks ({', '.join(failed)}), re-render, then run `{CHECK_CMD} {path}` "
            "again (at most 3 rounds).")
    return {"ok": ok, "file": path, "checks": checks, "next_step": step}, (0 if ok else 2)


class Parser(argparse.ArgumentParser):
    def error(self, message):  # usage error: one JSON object, exit 1 (argparse would exit 2)
        print(json.dumps({"ok": False, "error": message, "usage": self.format_usage().strip()}))
        sys.exit(1)


def spec_from_csv(args):
    with open(args.csv, newline="", encoding="utf-8-sig") as fh:
        rows = list(csv.DictReader(fh))
    if not rows:
        raise SystemExit("error: empty CSV")
    x = args.x or list(rows[0].keys())[0]
    ys = args.y.split(",") if args.y else [k for k in rows[0] if k != x]

    def num(v):
        v = (v or "").strip().replace("\u00a0", "").replace(" ", "")
        if not v:
            return None
        if "," in v and "." not in v:
            v = v.replace(",", ".")
        return float(v.replace(",", ""))

    return {"type": args.type, "categories": [r[x] for r in rows],
            "series": [{"name": y, "values": [num(r[y]) for r in rows]} for y in ys]}


def main(argv=None) -> int:
    ap = Parser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--check", metavar="IMAGE", help="check a chart image (exit 0 ok, 2 not ok, 1 usage error)")
    ap.add_argument("--spec")
    ap.add_argument("--csv")
    ap.add_argument("--x")
    ap.add_argument("--y")
    ap.add_argument("--type", default="bar")
    ap.add_argument("--title")
    ap.add_argument("--subtitle")
    ap.add_argument("--source")
    ap.add_argument("--size")
    ap.add_argument("--y-format")
    ap.add_argument("--decimals", type=int)
    ap.add_argument("--locale")
    ap.add_argument("--dpi", type=int, default=DPI)
    ap.add_argument("--out")
    ap.add_argument("--example", action="store_true")
    args = ap.parse_args(argv)
    if args.example:
        print(json.dumps(EXAMPLE, indent=2, ensure_ascii=False))
        return 0
    if args.check:
        args.check = os.path.abspath(args.check)  # the checker contract names the file by its absolute path
        if not os.path.isfile(args.check):
            print(json.dumps({"ok": False, "file": args.check, "error": f"no such file: {args.check}"}))
            return 1
        report, code = check_image(args.check)
        print(json.dumps(report, indent=2, ensure_ascii=False))
        return code
    if not args.out or not (args.spec or args.csv):
        ap.error("--out and one of --spec / --csv are required")
    spec = json.load(open(args.spec, encoding="utf-8")) if args.spec else spec_from_csv(args)
    for key in ("title", "subtitle", "source", "size", "locale", "decimals"):
        if getattr(args, key) is not None:
            spec[key] = getattr(args, key)
    if args.y_format:
        spec["y_format"] = args.y_format
    fig = render(spec)
    print(json.dumps(save(fig, args.out, args.dpi)))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
