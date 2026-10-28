#!/usr/bin/env python3
"""List and fill the fields of a fillable PDF form (AcroForm) with pypdf.

Usage:
  python3 fill_form.py FORM.pdf --list                       # fields: name, type, value, options, page
  python3 fill_form.py FORM.pdf --template > values.json     # {"field name": current value} to edit
  python3 fill_form.py FORM.pdf --values values.json --out FILLED.pdf
  python3 fill_form.py FORM.pdf --values values.json --out FILLED.pdf --flatten   # not editable any more

values.json: {"full_name": "Jane Doe", "agree": true, "country": "France", "plan": "pro"}
  text fields  -> string;  checkboxes -> true/false (mapped to the box's on-state, e.g. /Yes)
  radio groups -> the option name (see "options" in --list);  choice/list -> one of "options".
Field names are the fully qualified names shown by --list ("section.name" for nested fields).
After filling, the script re-reads the output and reports every field whose value did not
stick ("mismatches"); exit 2 if any. Unknown field names -> exit 2 as well.
PDFs without AcroForm fields (flat forms): there is nothing to fill; write text at the
right coordinates instead (extract.py --words gives label positions; see reference/forms.md).
"""
from __future__ import annotations

import argparse
import json
import sys


def field_type(f):
    ft = f.get("/FT")
    flags = int(f.get("/Ff", 0) or 0)
    if ft == "/Tx":
        return "text"
    if ft == "/Btn":
        if flags & (1 << 15):
            return "radio"
        if flags & (1 << 16):
            return "button"
        return "checkbox"
    if ft == "/Ch":
        return "combo" if flags & (1 << 17) else "list"
    if ft == "/Sig":
        return "signature"
    return str(ft)


def on_states(reader, name):
    """Possible 'on' appearance names of a checkbox / radio group (from widget /AP /N)."""
    states = []
    for page in reader.pages:
        for annot in page.get("/Annots") or []:
            a = annot.get_object()
            parent = a.get("/Parent")
            fq = []
            node = a
            while node is not None:
                if "/T" in node:
                    fq.append(str(node["/T"]))
                node = node.get("/Parent").get_object() if node.get("/Parent") is not None else None
            if ".".join(reversed(fq)) != name:
                continue
            ap = a.get("/AP")
            if ap and "/N" in ap:
                for k in ap["/N"].keys():
                    if k != "/Off" and k not in states:
                        states.append(k)
            _ = parent
    return states


def field_pages(reader):
    pages = {}
    for i, page in enumerate(reader.pages, 1):
        for annot in page.get("/Annots") or []:
            a = annot.get_object()
            fq, node = [], a
            while node is not None:
                if "/T" in node:
                    fq.append(str(node["/T"]))
                node = node.get("/Parent").get_object() if node.get("/Parent") is not None else None
            if fq:
                pages.setdefault(".".join(reversed(fq)), i)
    return pages


def list_fields(reader):
    fields = reader.get_fields() or {}
    pages = field_pages(reader)
    out = []
    for name, f in fields.items():
        kind = field_type(f)
        if kind in ("button",) or (f.get("/Kids") and not f.get("/FT")):
            continue
        item = {"name": name, "type": kind, "value": str(f.get("/V")) if f.get("/V") is not None else None,
                "page": pages.get(name)}
        if kind in ("checkbox", "radio"):
            item["options"] = on_states(reader, name)
        if kind in ("combo", "list"):
            opts = f.get("/Opt") or []
            item["options"] = [str(o[1] if isinstance(o, list) else o) for o in opts]
        if f.get("/TU"):
            item["tooltip"] = str(f.get("/TU"))
        if f.get("/MaxLen"):
            item["max_len"] = int(f.get("/MaxLen"))
        out.append(item)
    return out


def flatten(writer):
    """Draw every widget's current appearance into the page content, then remove the
    widgets and the AcroForm (pypdf's own flatten mixes up radio-button appearances)."""
    from pypdf.generic import ArrayObject, DictionaryObject, NameObject, StreamObject

    count = 0
    for page in writer.pages:
        annots = page.get("/Annots")
        if not annots:
            continue
        ops = []
        keep = ArrayObject()
        resources = page.get("/Resources")
        resources = resources.get_object() if resources is not None else DictionaryObject()
        page[NameObject("/Resources")] = resources
        xobjects = resources.get("/XObject")
        xobjects = xobjects.get_object() if xobjects is not None else DictionaryObject()
        resources[NameObject("/XObject")] = xobjects
        for ref in annots:
            annot = ref.get_object()
            if annot.get("/Subtype") != "/Widget":
                keep.append(ref)
                continue
            if int(annot.get("/F", 0)) & 2:  # hidden
                continue
            ap = annot.get("/AP")
            normal = ap.get_object().get("/N") if ap is not None else None
            if normal is None:
                continue
            normal_obj = normal.get_object()
            if not isinstance(normal_obj, StreamObject):  # states dict (checkbox / radio)
                state = annot.get("/AS", "/Off")
                normal = normal_obj.get(state)
                if normal is None:
                    continue
            stream = normal.get_object()
            x0, y0, x1, y1 = [float(v) for v in annot["/Rect"]]
            bbox = [float(v) for v in stream.get("/BBox", [0, 0, x1 - x0, y1 - y0])]
            bw, bh = (bbox[2] - bbox[0]) or 1, (bbox[3] - bbox[1]) or 1
            sx, sy = (x1 - x0) / bw, (y1 - y0) / bh
            name = f"/FlatField{count}"
            count += 1
            xobjects[NameObject(name)] = normal if hasattr(normal, "idnum") else writer._add_object(stream)
            ops.append(f"q {sx:.4f} 0 0 {sy:.4f} {x0 - bbox[0] * sx:.4f} {y0 - bbox[1] * sy:.4f} cm {name} Do Q")
        if ops:
            content = StreamObject()
            content.set_data(("\n".join(ops) + "\n").encode())
            existing = page.get("/Contents")
            parts = ArrayObject()
            if existing is not None:
                existing_obj = existing.get_object()
                if isinstance(existing_obj, ArrayObject):
                    parts.extend(existing_obj)
                else:
                    parts.append(existing)
            parts.append(writer._add_object(content))
            page[NameObject("/Contents")] = parts
        if keep:
            page[NameObject("/Annots")] = keep
        else:
            del page[NameObject("/Annots")]
    if "/AcroForm" in writer._root_object:
        del writer._root_object[NameObject("/AcroForm")]
    return count


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--list", action="store_true")
    ap.add_argument("--template", action="store_true")
    ap.add_argument("--values")
    ap.add_argument("--out")
    ap.add_argument("--flatten", action="store_true")
    args = ap.parse_args(argv)

    from pypdf import PdfReader, PdfWriter

    reader = PdfReader(args.file)
    fields = list_fields(reader)
    if args.list or not args.values:
        if args.template:
            print(json.dumps({f["name"]: f["value"] for f in fields}, indent=2, ensure_ascii=False))
        else:
            print(json.dumps({"file": args.file, "count": len(fields), "fields": fields,
                              "note": None if fields else "no fillable AcroForm fields in this PDF"},
                             indent=2, ensure_ascii=False))
        return 0
    if not args.out:
        ap.error("--out is required with --values")
    values = json.load(open(args.values, encoding="utf-8"))
    by_name = {f["name"]: f for f in fields}
    unknown = [k for k in values if k not in by_name]
    if unknown:
        print(json.dumps({"error": "unknown field names", "unknown": unknown,
                          "known": sorted(by_name)}, indent=2, ensure_ascii=False))
        return 2
    prepared = {}
    for name, value in values.items():
        f = by_name[name]
        if f["type"] == "checkbox":
            on = (f.get("options") or ["/Yes"])[0]
            prepared[name] = on if value in (True, "true", "True", "yes", "Yes", 1, "1", "on", on, on.lstrip("/")) else "/Off"
        elif f["type"] == "radio":
            v = str(value)
            prepared[name] = v if v.startswith("/") else "/" + v
        else:
            prepared[name] = "" if value is None else str(value)

    writer = PdfWriter(clone_from=reader)
    writer.set_need_appearances_writer(True)
    writer.update_page_form_field_values(None, prepared, auto_regenerate=False)
    if args.flatten:
        flatten(writer)
    with open(args.out, "wb") as fh:
        writer.write(fh)

    check = PdfReader(args.out)
    mismatches = []
    if not args.flatten:
        after = {f["name"]: f["value"] for f in list_fields(check)}
        for name, v in prepared.items():
            if str(after.get(name)) != str(v) and not (v == "" and after.get(name) in (None, "")):
                mismatches.append({"name": name, "wanted": v, "got": after.get(name)})
    else:
        text = "\n".join(p.extract_text() or "" for p in check.pages)
        for name, v in prepared.items():
            if by_name[name]["type"] == "text" and v and v not in text:
                mismatches.append({"name": name, "wanted": v, "got": "(not found in flattened page text)"})
    print(json.dumps({"out": args.out, "filled": len(prepared), "flattened": args.flatten,
                      "mismatches": mismatches}, indent=2, ensure_ascii=False))
    return 2 if mismatches else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 - bad input: one JSON error on stdout, no traceback
        print(json.dumps({"ok": False, "error": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
        sys.exit(1)
