"""Block spec normalizer shared by docx/scripts/new_document.py and pdf/scripts/build_pdf.py.

The two files docx/scripts/blocks.py and pdf/scripts/blocks.py are identical copies (each skill
is installed on its own, so neither can import the other); tests/test_pdf.py checks they match.

  from blocks import normalize_spec
  spec = normalize_spec(json.load(fh), "python3 scripts/new_document.py")   # the calling builder

Accepted block shapes: {"type": "heading", "text": "Intro", "level": 1},
{"heading": {"text": "Intro", "level": 1}}, {"heading": "Intro"}, a bare string (paragraph).
An invalid spec raises SystemExit with every problem, the accepted shapes and an example.
"""
from __future__ import annotations

import json

# block type -> field filled by the string / list shorthand ({"heading": "Intro"}, {"bullets": [...]})
BLOCK_TYPES = {"heading": "text", "paragraph": "text", "bullets": "items", "numbered": "items",
               "table": "rows", "image": "path", "callout": "text", "toc": "title", "page_break": None}
ALIASES = {"h1": ("heading", {"level": 1}), "h2": ("heading", {"level": 2}), "h3": ("heading", {"level": 3}),
           "title": ("heading", {"level": 1}), "subheading": ("heading", {"level": 2}),
           "para": ("paragraph", {}), "p": ("paragraph", {}), "text": ("paragraph", {}),
           "bullet": ("bullets", {}), "list": ("bullets", {}), "ul": ("bullets", {}),
           "ol": ("numbered", {}), "numbered_list": ("numbered", {}), "picture": ("image", {}),
           "note": ("callout", {}), "pagebreak": ("page_break", {}), "break": ("page_break", {})}
REQUIRED = {"heading": "text", "image": "path", "bullets": "items", "numbered": "items"}
SHAPES_HELP = """Accepted shapes for every block (pick one):
  {"type": "heading", "text": "Introduction", "level": 1}
  {"heading": {"text": "Introduction", "level": 1}}
  {"heading": "Introduction"}
Block types and fields: heading{text,level}  paragraph{text,align}  bullets{items}  numbered{items}
  table{columns,rows,widths_cm,numeric_cols,caption}  image{path,width_cm,caption}
  callout{text,kind}  toc{title}  page_break
Short example:
  {"title": "Report", "blocks": [{"heading": "Summary"}, {"paragraph": "Sales grew **12%**."},
   {"bullets": ["North", "South"]}, {"table": {"columns": ["Region", "Sales"], "rows": [["North", "5"]]}}]}
Full example: {builder} --example"""


def _canonical(name):
    if not isinstance(name, str):
        return None, {}
    key = name.strip().lower().replace("-", "_").replace(" ", "_")
    if key in BLOCK_TYPES:
        return key, {}
    return ALIASES.get(key, (None, {}))


def normalize_block(block, i):
    """Return (block_with_type, error). Accepts the typed form and the shorthands."""
    if isinstance(block, str):
        return {"type": "paragraph", "text": block}, None
    if not isinstance(block, dict):
        return None, f"block {i + 1}: expected an object, got {json.dumps(block, ensure_ascii=False)[:80]}"
    shown = json.dumps(block, ensure_ascii=False)[:120]
    if "type" in block:
        kind, defaults = _canonical(block["type"])
        if kind is None:
            return None, f"block {i + 1}: unknown block type {block['type']!r} in {shown}"
        out = {**defaults, **block, "type": kind}
    else:
        keys = [k for k in block if _canonical(k)[0]]
        if len(keys) != 1:
            # field-based guess: {"items": [...]}, {"columns": .., "rows": ..}, {"path": ...}
            guess = ("bullets" if "items" in block else "table" if ("rows" in block or "columns" in block)
                     else "image" if "path" in block else None)
            if guess is None or keys:
                why = "several block types" if keys else "no block type"
                return None, f"block {i + 1}: {why} in {shown}"
            out = {**block, "type": guess}
        else:
            key = keys[0]
            kind, defaults = _canonical(key)
            value = block[key]
            rest = {k: v for k, v in block.items() if k != key}
            field = BLOCK_TYPES[kind]
            if isinstance(value, dict):
                out = {**defaults, **rest, **value, "type": kind}
            elif value is None or value is True or (field is None and not isinstance(value, (list, dict))):
                out = {**defaults, **rest, "type": kind}
            elif isinstance(value, list) and kind in ("bullets", "numbered", "table"):
                out = {**defaults, **rest, field: value, "type": kind}
            elif isinstance(value, (str, int, float)) and field and kind not in ("bullets", "numbered", "table"):
                out = {**defaults, **rest, field: str(value), "type": kind}
            elif isinstance(value, str) and kind in ("bullets", "numbered"):
                out = {**defaults, **rest, "items": [value], "type": kind}
            else:
                return None, f"block {i + 1}: cannot read the value of {key!r} in {shown}"
    need = REQUIRED.get(out["type"])
    if need and not out.get(need):
        return None, f"block {i + 1}: {out['type']} needs {need!r} in {shown}"
    if out["type"] == "table" and not (out.get("columns") or out.get("rows")):
        return None, f"block {i + 1}: table needs 'columns' and/or 'rows' in {shown}"
    return out, None


def normalize_spec(spec, builder="python3 scripts/new_document.py"):
    """Validate the whole spec up front; exit with every problem and the accepted shapes."""
    shapes_help = SHAPES_HELP.replace("{builder}", builder)
    if isinstance(spec, list):
        spec = {"blocks": spec}
    if not isinstance(spec, dict):
        raise SystemExit("error: the spec must be a JSON object with a 'blocks' list\n" + shapes_help)
    blocks = spec.get("blocks", spec.get("content"))
    if not isinstance(blocks, list):
        raise SystemExit("error: the spec needs a 'blocks' list\n" + shapes_help)
    out, errors = [], []
    for i, b in enumerate(blocks):
        nb, err = normalize_block(b, i)
        if err:
            errors.append(err)
        else:
            out.append(nb)
    if errors:
        raise SystemExit("error: invalid spec\n  " + "\n  ".join(errors) + "\n" + shapes_help)
    return {**spec, "blocks": out}
