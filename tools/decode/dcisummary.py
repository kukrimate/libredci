#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-only
"""Summarize sections of `dcidecode.py -H` output.

usage: dcisummary.py [--hide-pir] DECODED SECTION

Prints the IR/DR operations, config packets and TDO results of every
section whose `###` marker line contains SECTION. Repeated operations
(polls) are collapsed into one line with a count, and the 180-bit TDO of a
tap_status poll of the four KBL cores is split per core. With --hide-pir,
probe-instruction scans (sub_pir_write, PDR reads) and their TDO are left
out.
"""
import argparse

# One tap_status poll of all four KBL core TAPs, as decoded.
POLL4 = (
    "IR[9]<-0x110(nocap) DR[8]<-0x60(nocap) IR[9]<-0x111(nocap) DR[45]<-0x0 "
    "idle5 IR[9]<-0x114"
)
# Scans of probe instructions and PDR reads on a core TAP.
PIR_MARKS = ("DR[8]<-0x83", "DR[8]<-0x84", "DR[8]<-0x85", "DR[142]")


def split4(value):
    """Splits a 180-bit four-core tap_status capture per core."""
    v = int(value, 16)
    return " ".join(
        f"c{i}={(v >> (45 * i + 8)) & ((1 << 37) - 1):#x}" for i in range(4)
    )


def entries(path, want, hide_pir):
    """Yields the summarized lines of the selected sections."""
    section, hidden = None, False
    with open(path, encoding="latin-1") as f:
        for line in f:
            if line.startswith("###"):
                section = line.strip()
                if want in section:
                    yield section
                continue
            if not section or want not in section:
                continue
            s = line.strip()
            if s.startswith("=>"):
                s = s[3:]
                hidden = hide_pir and any(m in s for m in PIR_MARKS)
                if hidden:
                    continue
                yield "poll tap_status c0..c3" if POLL4 in s else s
            elif " TDO " in s:
                bits, value = s.split("bits=")[1].split(": ")
                if bits == "0" or hidden:
                    continue
                yield f"  tdo[{bits}] " + (split4(value) if bits == "180" else value)
            elif " OUT " in s and " T01 " in s:
                yield "cfg " + s.split("|")[1].strip()


def collapse(lines):
    """Prints lines, collapsing repeated lines and repeated pairs (scan +
    result) with a count."""
    i = 0
    while i < len(lines):
        n = 1
        while i + n < len(lines) and lines[i + n] == lines[i]:
            n += 1
        if n > 1 and not lines[i].startswith("###"):
            print(f"[x{n}]", lines[i])
            i += n
            continue
        pair = lines[i : i + 2]
        n = 1
        while (
            len(pair) == 2
            and pair[0] != pair[1]
            and not pair[0].startswith("###")
            and lines[i + 2 * n : i + 2 * n + 2] == pair
        ):
            n += 1
        if n > 1:
            print(f"[x{n}]", pair[0])
            print(f"[x{n}]", pair[1])
            i += 2 * n
        else:
            print(lines[i])
            i += 1


def main():
    """Summarizes the sections named on the command line."""
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("decoded", help="output of dcidecode.py -H")
    parser.add_argument("section", help="text contained in the ### marker lines")
    parser.add_argument(
        "--hide-pir",
        action="store_true",
        help="leave out probe-instruction scans and PDR reads",
    )
    args = parser.parse_args()
    collapse(list(entries(args.decoded, args.section, args.hide_pir)))


if __name__ == "__main__":
    main()
