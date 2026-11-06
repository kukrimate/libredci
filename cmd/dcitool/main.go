// SPDX-License-Identifier: GPL-2.0-only

// dcitool performs JTAG scans on an Intel DCI target over USB.
//
// Raw mode drives a physical chain directly:
//
//	dcitool -chain 0 -ir 8:0x2 -dr 32:0
//
// Model mode addresses TAPs of a platform description by name, taking care
// of IR lengths, TAP splicing, virtual chains and preconditions. The PCH
// and CPU are detected from the IDCODEs of the chains' root TAPs, or named
// with -pch and -cpu:
//
//	dcitool -list
//	dcitool -pch spt -cpu kbl -list
//	dcitool -idcodes
//	dcitool -tap cpu.cbo0 -ir 0x2 -dr 32:0
//	dcitool -tap cpu.core0 -read tap_status
//
// Shell mode does run control (halt, registers, memory, I/O):
//
//	dcitool -shell
//	dcitool -c "halt; regs; md 0x0 64; go"
//
// -ir / -dr / -read may be repeated and are executed in the order given.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"

	"libredci/pkg/dci"
	"libredci/pkg/jtag"
	"libredci/pkg/model"
	"libredci/pkg/platform"
	"libredci/pkg/tapnet"
)

type op struct {
	kind string // "ir", "dr", "read"
	bits int    // 0 = not given
	val  *big.Int
	name string
}

type opFlag struct {
	ops  *[]op
	kind string
}

func (l opFlag) String() string { return "" }

func (l opFlag) Set(s string) error {
	if l.kind == "read" {
		*l.ops = append(*l.ops, op{kind: "read", name: s})
		return nil
	}
	o := op{kind: l.kind}
	v := s
	if n, rest, ok := strings.Cut(s, ":"); ok {
		bits, err := strconv.Atoi(n)
		if err != nil || bits <= 0 {
			return fmt.Errorf("bad bit count in %q (want [n:]value)", s)
		}
		o.bits, v = bits, rest
	}
	val, ok := new(big.Int).SetString(v, 0)
	if !ok || val.Sign() < 0 {
		return fmt.Errorf("bad value in %q", s)
	}
	if o.bits > 0 && val.BitLen() > o.bits {
		return fmt.Errorf("value %s does not fit in %d bits", v, o.bits)
	}
	o.val = val
	*l.ops = append(*l.ops, o)
	return nil
}

func main() {
	var ops []op
	chain := flag.Int("chain", -1, "raw mode: physical JTAG chain (0 = PCH, 1 = CPU)")
	tapName := flag.String("tap", "", "model mode: TAP to address")
	pchName := flag.String("pch", "", "PCH `name` instead of detecting it (none: leave chain 0 out)")
	cpuName := flag.String("cpu", "", "CPU `name` instead of detecting it (none: leave chain 1 out)")
	list := flag.Bool("list", false, "list the platform's TAPs and registers")
	idcodes := flag.Bool("idcodes", false, "read and check the IDCODE of every TAP in the model")
	shellMode := flag.Bool("shell", false, "interactive debug shell (see 'help' inside)")
	script := flag.String("c", "", "run shell `commands` separated by ';' and exit")
	verbose := flag.Bool("v", false, "log transactions (-v) ")
	raw := flag.Bool("vv", false, "also log raw ExI packets")
	flag.Var(opFlag{&ops, "ir"}, "ir", "IR scan `[n:]value` (n optional with -tap)")
	flag.Var(opFlag{&ops, "dr"}, "dr", "DR scan `n:value`")
	flag.Var(opFlag{&ops, "read"}, "read", "read modelled read-only `register` (with -tap)")
	flag.Parse()
	log.SetFlags(log.Lmicroseconds)

	var plat *platform.Platform
	if *pchName != "" || *cpuName != "" {
		pch, err := chipFlag(platform.PCHs, *pchName)
		if err != nil {
			fatal(err)
		}
		cpu, err := chipFlag(platform.CPUs, *cpuName)
		if err != nil {
			fatal(err)
		}
		plat = platform.New(pch, cpu)
		if *list {
			listPlatform(plat.Platform)
			return
		}
	}

	p, err := dci.Open()
	if err != nil {
		fatal(err)
	}
	p.Verbose = *raw
	err = func() (err error) {
		defer p.Close()
		if err := p.Connect(); err != nil {
			return err
		}
		defer func() {
			if derr := p.Disconnect(); derr != nil && err == nil {
				err = derr
			}
		}()
		switch {
		case *chain >= 0:
			return runRaw(p, *chain, ops)
		default:
			if plat == nil {
				if plat, err = detect(p); err != nil {
					return err
				}
			}
			if *list {
				listPlatform(plat.Platform)
				return nil
			}
			s := tapnet.New(plat.Platform, p)
			s.Verbose = *verbose || *raw
			if s.Verbose {
				s.OnTransaction = logTransaction
			}
			defer func() {
				if rerr := s.Release(); rerr != nil && err == nil {
					err = rerr
				}
			}()
			if *idcodes {
				return checkIDCodes(s)
			}
			if *shellMode || *script != "" {
				return runShell(s, plat, *script)
			}
			if *tapName == "" {
				return fmt.Errorf("need -tap NAME (model mode), -chain N (raw mode), -list or -idcodes")
			}
			return runTap(s, *tapName, ops)
		}
	}()
	if err != nil {
		fatal(err)
	}
}

// chipFlag resolves a -pch/-cpu value: empty or "none" leaves the chip out.
func chipFlag(list []*platform.Chip, name string) (*platform.Chip, error) {
	if name == "" || name == "none" {
		return nil, nil
	}
	return platform.Find(list, name)
}

// detect identifies the PCH and CPU from their root IDCODEs.
func detect(d tapnet.Driver) (*platform.Platform, error) {
	pch, cpu, err := platform.Detect(d)
	if err != nil {
		return nil, err
	}
	for _, det := range []platform.Detection{pch, cpu} {
		what := "unknown chip, not modelled"
		switch {
		case det.IDCode&1 == 0:
			what = "no IDCODE, not modelled"
		case det.Chip != nil:
			what = fmt.Sprintf("%s (%s)", det.Chip.Name, det.Chip.Desc)
		}
		fmt.Fprintf(os.Stderr, "chain %d: idcode %#08x: %s\n", det.Chain, det.IDCode, what)
	}
	if pch.Chip == nil && cpu.Chip == nil {
		return nil, fmt.Errorf("no known chip detected; name them with -pch and -cpu")
	}
	return platform.New(pch.Chip, cpu.Chip), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func logTransaction(chain int, scans []*jtag.Scan) {
	var parts []string
	for _, s := range scans {
		parts = append(parts, s.String())
	}
	log.Printf("chain %d: %s", chain, strings.Join(parts, "; "))
}

func hexw(v *big.Int, bits int) string { return fmt.Sprintf("0x%0*x", (bits+3)/4, v) }

func runRaw(p *dci.Probe, chain int, ops []op) error {
	if err := p.SelectChain(chain); err != nil {
		return err
	}
	b := jtag.NewBuilder()
	b.Reset()
	for _, o := range ops {
		if o.bits == 0 {
			return fmt.Errorf("raw mode needs explicit lengths (n:value)")
		}
		switch o.kind {
		case "ir":
			b.IR(o.bits, o.val)
		case "dr":
			b.DR(o.bits, o.val)
		default:
			return fmt.Errorf("-%s needs -tap", o.kind)
		}
	}
	b.Idle(1)
	if err := b.Run(p); err != nil {
		return err
	}
	for _, s := range b.Scans() {
		kind := "dr"
		if s.IR {
			kind = "ir"
		}
		fmt.Printf("%s %d:%#x -> %s\n", kind, s.Bits, s.In, hexw(s.Out, s.Bits))
	}
	return nil
}

func runTap(s *tapnet.Session, name string, ops []op) error {
	tap, err := s.TAP(name)
	if err != nil {
		return err
	}
	var ir *uint64
	for _, o := range ops {
		switch o.kind {
		case "ir":
			if o.bits != 0 && o.bits != tap.IRLen {
				return fmt.Errorf("%s has a %d-bit IR, not %d", tap.Name, tap.IRLen, o.bits)
			}
			if o.val.BitLen() > tap.IRLen {
				return fmt.Errorf("IR value %#x does not fit %s's %d-bit IR", o.val, tap.Name, tap.IRLen)
			}
			v := o.val.Uint64()
			ir = &v
		case "dr":
			if ir == nil {
				return fmt.Errorf("-dr needs a preceding -ir")
			}
			if o.bits == 0 {
				return fmt.Errorf("-dr needs a length (n:value)")
			}
			out, err := s.IRDR(tap, *ir, o.bits, o.val)
			if err != nil {
				return err
			}
			fmt.Printf("%s ir %#x dr %d:%#x -> %s\n", tap.Name, *ir, o.bits, o.val, hexw(out, o.bits))
		case "read":
			reg, ok := tap.Register(o.name)
			if !ok {
				return fmt.Errorf("%s has no register %q (see -list)", tap.Name, o.name)
			}
			out, err := s.Read(tap, reg)
			if err != nil {
				return err
			}
			fmt.Printf("%s.%s = %s\n%s", tap.Name, reg.Name, hexw(out, reg.Bits), reg.Decode(out))
		}
	}
	return nil
}

func checkIDCodes(s *tapnet.Session) error {
	bad := 0
	var err error
	s.Plat.Walk(func(t *model.TAP, depth int) {
		if err != nil || t.IDCode == 0 {
			return
		}
		var v uint32
		v, err = s.ReadIDCode(t)
		if err != nil {
			err = fmt.Errorf("%s: %w", t.Name, err)
			return
		}
		status := "ok"
		switch {
		case t.IDCodeMatches(v):
		case v == 0 && t.Optional:
			status = "absent (optional)"
		default:
			status = fmt.Sprintf("MISMATCH, expected %#08x", t.IDCode)
			bad++
		}
		fmt.Printf("%-*s%-28s %#08x  %s\n", 2*depth, "", t.Name, v, status)
	})
	if err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("%d IDCODE mismatches", bad)
	}
	return nil
}

func listPlatform(p *model.Platform) {
	fmt.Printf("platform %s\n", p.Name)
	p.Walk(func(t *model.TAP, depth int) {
		ind := strings.Repeat("  ", depth)
		var via string
		switch l := t.Via.(type) {
		case nil:
			via = fmt.Sprintf("chain %d root", t.Chain)
		case model.Spliced:
			via = fmt.Sprintf("spliced: ir %#x bit %d/%d", l.SelectIR, l.Bit, l.Width)
		case model.Virtual:
			via = fmt.Sprintf("virtual: ir/dr select %#x/%#x", l.IRSelect, l.DRSelect)
		}
		id := ""
		if t.IDCode != 0 {
			id = fmt.Sprintf(" idcode %#08x", t.IDCode)
		}
		fmt.Printf("%s%s  (ir %d, %s%s)\n", ind, t.Name, t.IRLen, via, id)
		for _, r := range t.Registers {
			acc := map[model.Access]string{model.ReadOnly: "ro", model.ReadWrite: "rw", model.WriteOnly: "wo"}[r.Access]
			fmt.Printf("%s    .%-16s ir %#x, %d bits, %s\n", ind, r.Name, r.IR, r.Bits, acc)
		}
	})
}
