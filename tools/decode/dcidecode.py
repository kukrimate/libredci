#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-only
"""Decode libusb-shim logs of DCI DbC traffic into 64-byte ExI packets.

usage: dcidecode.py [-H] LOG

With -H, follow the TAP state machine and print IR/DR operations.
"""
import argparse


def packets(line):
    """Yields the 64-byte packets in a log line's hex dump."""
    if " : " not in line:
        return
    hexs = line.split(" : ", 1)[1].replace(" ", "").strip()
    b = bytes.fromhex(hexs)
    for i in range(0, len(b), 64):
        yield b[i : i + 64]


def jtag_nibbles(p):
    """Returns a TCK packet's bit count and its TMS/TDI/TDO nibbles."""
    nbits = int.from_bytes(p[6:8], "little")
    n = nbits // 4
    out = []
    for i in range(n):
        byte = p[8 + i // 2]
        out.append((byte >> (4 * (i % 2))) & 0xF)
    return nbits, out


def fmt_jtag(nibs):
    """Formats nibbles run-length compressed, e.g. "2x10 3 3 2 2"."""
    s, prev, cnt = [], None, 0
    for x in nibs + [None]:
        if x == prev:
            cnt += 1
            continue
        if prev is not None:
            s.append(f"{prev:x}" + (f"x{cnt}" if cnt > 1 else ""))
        prev, cnt = x, 1
    return " ".join(s)


# IEEE 1149.1 TAP state machine: state -> (next if TMS=0, next if TMS=1)
FSM = {
    "TLR": ("RTI", "TLR"),
    "RTI": ("RTI", "SelDR"),
    "SelDR": ("CapDR", "SelIR"),
    "CapDR": ("ShDR", "Ex1DR"),
    "ShDR": ("ShDR", "Ex1DR"),
    "Ex1DR": ("PauDR", "UpdDR"),
    "PauDR": ("PauDR", "Ex2DR"),
    "Ex2DR": ("ShDR", "UpdDR"),
    "UpdDR": ("RTI", "SelDR"),
    "SelIR": ("CapIR", "TLR"),
    "CapIR": ("ShIR", "Ex1IR"),
    "ShIR": ("ShIR", "Ex1IR"),
    "Ex1IR": ("PauIR", "UpdIR"),
    "PauIR": ("PauIR", "Ex2IR"),
    "Ex2IR": ("ShIR", "UpdIR"),
    "UpdIR": ("RTI", "SelDR"),
}


class TapSim:
    """Follows the TCK stream across packets and prints high-level ops."""

    def __init__(self):
        self.state = "TLR"
        self.ops = []
        self.cur = None
        self.idle = 0
        self.tms_run = 0

    def flush_idle(self):
        """Records the pending Run-Test/Idle clocks as one op."""
        if self.idle:
            self.ops.append(f"idle{self.idle}")
            self.idle = 0

    def clock(self, nib):
        """Advances the state machine by one TCK."""
        tms, tdi, cap = nib & 1, (nib >> 1) & 1, (nib >> 2) & 1
        st = self.state
        if st in ("ShIR", "ShDR"):
            if self.cur is None:
                self.cur = [st[2:], [], 0, []]
            self.cur[1].append(tdi)
            self.cur[2] += cap
        if (
            st.startswith("Ex2")
            and FSM[st][tms].startswith("Sh")
            and self.cur is not None
        ):
            self.cur[3].append(len(self.cur[1]))  # pause boundary inside one scan
        if st == "RTI" and not tms:
            self.idle += 1
        nxt = FSM[st][tms]
        if st.startswith("Upd") and self.cur is not None:
            self.flush_idle()
            kind, bits, ncap, cuts = self.cur
            segs, prev = [], 0
            for c in cuts + [len(bits)]:
                seg = bits[prev:c]
                segs.append(
                    f"[{len(seg)}]<-{sum(b << i for i, b in enumerate(seg)):#x}"
                )
                prev = c
            self.ops.append(
                kind
                + "|".join(segs)
                + ("" if ncap in (0, len(bits)) else f"(cap{ncap})")
                + ("" if ncap else "(nocap)")
            )
            self.cur = None
        if nxt == "TLR" and st != "TLR":
            self.flush_idle()
            self.ops.append("TLR")
        self.state = nxt

    def take(self):
        """Returns and clears the ops recorded so far."""
        self.flush_idle()
        o, self.ops = self.ops, []
        return " ".join(o)


def describe(direction, p, sim, high):
    """Describes one packet; TCK packets also drive sim."""
    t = p[0]
    hdr = p[:8].hex(" ")
    if t == 3 and direction == "OUT":
        nbits, nibs = jtag_nibbles(p)
        for x in nibs:
            sim.clock(x)
        hl = sim.take() if p[2] else ""
        raw = "" if high else f": {fmt_jtag(nibs)}"
        return f"JTAG  {hdr} bits={nbits}{raw}" + (f"\n{'':>20}=> {hl}" if hl else "")
    if t == 3 and direction == "IN":
        nbits = int.from_bytes(p[6:8], "little")
        data = int.from_bytes(p[8 : 8 + (nbits + 7) // 8], "little")
        return f"TDO   {hdr} bits={nbits}: {data:#x}"
    body = p[8:].rstrip(b"\0")
    return f"T{t:02x}   {p[:4].hex(' ')} | " + " ".join(
        p[i : i + 4].hex() for i in range(4, 8 + len(body) + (-len(body)) % 4, 4)
    )


# Shim log tags of transfers carrying data. Every transfer is logged as
# ASYNC_*; a synchronous one (libusb_bulk_transfer) also as BULK_*.
DIRECTION = {"ASYNC_OUT": "OUT", "ASYNC_IN": "IN"}


def main():
    """Decodes the log named on the command line."""
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("-H", action="store_true", help="print IR/DR operations")
    parser.add_argument("log")
    args = parser.parse_args()
    sim = TapSim()
    with open(args.log, encoding="latin-1") as log:
        lines = list(log)
    for line in lines:
        if line.startswith("###"):
            print(line.rstrip())
            continue
        parts = line.split()
        if len(parts) < 3 or parts[1] not in DIRECTION:
            if " : " not in line:
                print("   ", line.rstrip())
            continue
        d = DIRECTION[parts[1]]
        ep = parts[2]
        for p in packets(line):
            print(f"{parts[0]} {d:3} {ep} {describe(d, p, sim, args.H)}")


if __name__ == "__main__":
    main()
