#!/usr/bin/env python3
"""Fixture of TestTheFlagParserReadsOnlyArgparseDefinitions: options defined in several ways,
and option-like strings that are not options (see --not-an-option in this docstring)."""
import argparse
import sys

NOTE = "--quoted-elsewhere"


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("file")
    ap.add_argument("-o", "--out", help="where to write (see --mentioned-in-help)")
    ap.add_argument('--single-quoted', action="store_true")
    g = ap.add_mutually_exclusive_group()
    g.add_argument(
        "--in-group",
        "--alias-in-group",
        default="--a-default",
    )
    sub = ap.add_subparsers(dest="cmd")
    s = sub.add_parser("render", help="--not-a-flag either")
    s.add_argument("--dpi", type=int)
    args = ap.parse_args()
    print(NOTE, args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
