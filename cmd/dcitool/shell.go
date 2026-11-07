// SPDX-License-Identifier: GPL-2.0-only
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/arch/x86/x86asm"
	"libredci/pkg/dci"
	"libredci/pkg/ip/iosfsb"
	"libredci/pkg/platform"
	"libredci/pkg/runctl"
	_ "libredci/pkg/runctl/bigcore"
	_ "libredci/pkg/runctl/lakemont"
	"libredci/pkg/tapnet"
)

const shellHelp = `commands:
  threads                    list threads
  thread NAME                select thread (default: first)
  status                     running or halted
  halt                       stop the thread
  go                         resume it
  attach                     adopt a thread that is already halted
  forcego                    resume an attached thread (restore its state first!)
  step                       execute one instruction
  stepmode targeted|all      big cores: step only this thread (default), or
                             run every core and hold all of them after it
  cont [MS]                  resume and wait for a stop (default 10000 ms;
                             the thread keeps running after a timeout)
  wait [MS]                  wait for a running thread to stop
  bp SLOT ADDR [x|w|rw|io] [LEN]  hardware breakpoint (linear address)
  bpc SLOT                   clear breakpoint (a running thread is halted,
                             cleared and resumed)
  trace N                    single-step N times, printing each instruction
  resetbreak on|off          stop at the reset vector on the next reset (CSME:
                             a platform reset completes on go)
  regs                       show registers
  reg NAME [VALUE]           read / write a register
  md ADDR [LEN]              dump physical memory (LEN bytes, default 64)
  mw ADDR VALUE [SIZE]       write SIZE (1/2/4, default 4) bytes
  v2p LIN                    translate a linear address (program's paging)
  vmd LIN [LEN]              dump memory at a linear address
  vmw LIN VALUE [SIZE]       write memory at a linear address
  dis LIN [LEN]              disassemble at a linear address (needs objdump)
  rdmsr INDEX [force]        read model-specific register (known MSRs only
                             unless forced: unimplemented ones can kill the platform)
  wrmsr INDEX VALUE [force]  write model-specific register
  ior PORT [SIZE]            read I/O port
  iow PORT VALUE [SIZE]      write I/O port
  csr DEV FN OFF [SIZE] [VAL]      uncore CSR via the VCU (no halt needed)
  mmio DEV FN BAR OFF [SIZE] [VAL] device MMIO via the VCU
  sb SPACE PORT FID BAR OFF [SIZE] [VAL]
                             IOSF sideband register via the PCH's TPSB;
                             SPACE is mem, io, cfg or cr, SIZE 1/2/4/8
  scan TAP IR N:VALUE        raw IR/DR scan on a modelled TAP
  idcodes                    check all TAP IDCODEs
  pins                       show debug pin state (PREQ, raw snapshot)
  preq on|off                drive PREQ to the CPU
  reset                      reset the target platform (ends the session)
  detach                     exit leaving halted threads halted
  quit                       exit (resumes threads halted in this session)
`

type pinDriver interface {
	PulseReset() error
	SetPREQ(bool) error
	ReadPins() (dci.Pins, error)
}

type shell struct {
	s       *tapnet.Session
	plat    *platform.Platform
	cur     runctl.Thread
	threads map[string]runctl.Thread
	halted  map[string]bool // halted by us
	detach  bool
}

func runShell(s *tapnet.Session, plat *platform.Platform, script string) (err error) {
	sh := &shell{s: s, plat: plat, threads: map[string]runctl.Thread{}, halted: map[string]bool{}}
	names, _ := runctl.Threads(s.Plat)
	if len(names) > 0 {
		if err := sh.selectThread(names[0]); err != nil {
			fmt.Fprintln(os.Stderr, "warning:", err)
		}
	}
	defer func() {
		if sh.detach {
			return
		}
		// Disarm breakpoints so nothing of ours stays programmed. Halted
		// threads drop them when resumed; running ones are then halted and
		// resumed one at a time.
		var armed []string
		for name, t := range sh.threads {
			a, ok := t.(interface{ Armed() bool })
			if !ok || !a.Armed() {
				continue
			}
			d := t.(runctl.Debugger)
			for i := 0; i < 4; i++ {
				d.ClearBreakpoint(i)
			}
			if !sh.halted[name] {
				armed = append(armed, name)
			}
		}
		sh.dropRunning()
		resume := func(name string) {
			t := sh.threads[name]
			if a, ok := t.(runctl.Attacher); ok && !a.Owned() {
				fmt.Printf("leaving %s halted: its pre-halt state is not known to this session\n", name)
				return
			}
			fmt.Printf("resuming %s\n", name)
			if rerr := t.Resume(); rerr != nil {
				fmt.Printf("could not resume %s: %v (left halted)\n", name, rerr)
				if err == nil {
					err = rerr
				}
			}
		}
		for name := range sh.halted {
			resume(name)
		}
		for _, name := range armed {
			fmt.Printf("halting %s to remove breakpoints\n", name)
			if _, herr := sh.threads[name].Halt(); herr != nil {
				fmt.Printf("could not halt %s: %v\n", name, herr)
				continue
			}
			resume(name)
		}
		// Cores held without a halt command (stopped by an all-threads step).
		names, _ := runctl.Threads(sh.s.Plat)
		for _, name := range names {
			t, oerr := runctl.Open(sh.s, name)
			if oerr != nil {
				continue
			}
			if h, ok := t.(interface{ Held() bool }); ok && h.Held() {
				sh.threads[name] = t
				resume(name)
			}
		}
	}()

	if script != "" {
		for _, line := range strings.Split(script, ";") {
			if done, err := sh.command(line); err != nil || done {
				return err
			}
		}
		return nil
	}
	in := bufio.NewScanner(os.Stdin)
	for {
		prompt := "dci"
		if sh.cur != nil {
			prompt = sh.cur.Name()
		}
		fmt.Printf("%s> ", prompt)
		if !in.Scan() {
			fmt.Println()
			return in.Err()
		}
		done, err := sh.command(in.Text())
		if err != nil {
			fmt.Println("error:", err)
		}
		if done {
			return nil
		}
	}
}

func (sh *shell) selectThread(name string) error {
	if t, ok := sh.threads[name]; ok {
		sh.cur = t
		return nil
	}
	t, err := runctl.Open(sh.s, name)
	if err != nil {
		return err
	}
	sh.threads[name] = t
	sh.cur = t
	return nil
}

func num(s string) (uint64, error) {
	v, err := strconv.ParseUint(strings.ReplaceAll(s, "_", ""), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("bad number %q", s)
	}
	return v, nil
}

func (sh *shell) thread() (runctl.Thread, error) {
	if sh.cur == nil {
		return nil, errors.New("no thread selected")
	}
	return sh.cur, nil
}

// command runs one shell command, then releases the platform's transient
// conditions (on Kaby Lake the CPU wake requests and the uncore TAP
// arbitration); the next access acquires them again.
func (sh *shell) command(line string) (bool, error) {
	done, err := sh.exec(line)
	for _, c := range sh.s.Plat.Transient {
		if rerr := sh.s.ReleaseCondition(c); rerr != nil && err == nil {
			err = rerr
		}
	}
	return done, err
}

func (sh *shell) exec(line string) (done bool, err error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return false, nil
	}
	arg := func(i int) (uint64, error) {
		if i >= len(f) {
			return 0, fmt.Errorf("%s: missing argument", f[0])
		}
		return num(f[i])
	}
	optArg := func(i int, def uint64) (uint64, error) {
		if i >= len(f) {
			return def, nil
		}
		return num(f[i])
	}
	switch f[0] {
	case "help", "?":
		fmt.Print(shellHelp)
	case "quit", "exit", "q":
		return true, nil
	case "detach":
		sh.detach = true
		return true, nil
	case "threads":
		names, taps := runctl.Threads(sh.s.Plat)
		for _, n := range names {
			mark := " "
			if sh.cur != nil && sh.cur.Name() == n {
				mark = "*"
			}
			fmt.Printf("%s %-10s on %s\n", mark, n, taps[n].Name)
		}
	case "thread":
		if len(f) < 2 {
			return false, errors.New("thread NAME")
		}
		return false, sh.selectThread(f[1])
	case "idcodes":
		return false, checkIDCodes(sh.s)
	case "csr", "mmio":
		return false, sh.csrCmd(f)
	case "sb":
		return false, sh.sbCmd(f)
	case "reset":
		pd, ok := sh.s.Drv.(pinDriver)
		if !ok {
			return false, errors.New("probe cannot reset the target")
		}
		// Threads lose their state in a reset; forget about them.
		sh.halted, sh.detach = map[string]bool{}, true
		if err := pd.PulseReset(); err != nil {
			return true, err
		}
		fmt.Println("target reset; the DCI link will drop and come back")
		return true, nil
	case "pins", "preq":
		pd, ok := sh.s.Drv.(pinDriver)
		if !ok {
			return false, errors.New("probe has no pin access")
		}
		if f[0] == "preq" {
			if len(f) < 2 || (f[1] != "on" && f[1] != "off") {
				return false, errors.New("preq on|off")
			}
			return false, pd.SetPREQ(f[1] == "on")
		}
		p, err := pd.ReadPins()
		if err != nil {
			return false, err
		}
		fmt.Printf("PREQ %v  raw % 04x\n", p.PREQ, p.Raw)
	case "scan":
		if len(f) < 4 {
			return false, errors.New("scan TAP IR N:VALUE")
		}
		tap, err := sh.s.TAP(f[1])
		if err != nil {
			return false, err
		}
		ir, err := num(f[2])
		if err != nil {
			return false, err
		}
		n, v, _ := strings.Cut(f[3], ":")
		bits, err := strconv.Atoi(n)
		if err != nil {
			return false, fmt.Errorf("bad length in %q", f[3])
		}
		val, ok := new(big.Int).SetString(v, 0)
		if !ok {
			return false, fmt.Errorf("bad value in %q", f[3])
		}
		out, err := sh.s.IRDR(tap, ir, bits, val)
		if err != nil {
			return false, err
		}
		fmt.Println(hexw(out, bits))
	default:
		t, err := sh.thread()
		if err != nil {
			return false, err
		}
		return false, sh.threadCmd(t, f, arg, optArg)
	}
	return false, nil
}

func (sh *shell) threadCmd(t runctl.Thread, f []string, arg func(int) (uint64, error), optArg func(int, uint64) (uint64, error)) error {
	switch f[0] {
	case "status":
		h, err := t.Halted()
		if err != nil {
			return err
		}
		fmt.Println(map[bool]string{true: "halted", false: "running"}[h])
	case "halt":
		si, err := t.Halt()
		if err != nil {
			return err
		}
		sh.halted[t.Name()] = true
		fmt.Printf("%s halted at %#x (%s)\n", t.Name(), si.PC, si.Reason)
	case "attach":
		a, ok := t.(runctl.Attacher)
		if !ok {
			return fmt.Errorf("%s does not support attach", t.Name())
		}
		if err := a.Attach(); err != nil {
			return err
		}
		sh.halted[t.Name()] = true
		fmt.Printf("%s attached (halted)\n", t.Name())
	case "forcego":
		a, ok := t.(runctl.Attacher)
		if !ok {
			return fmt.Errorf("%s does not support forcego", t.Name())
		}
		if err := a.ForceResume(); err != nil {
			return err
		}
		delete(sh.halted, t.Name())
		fmt.Printf("%s running\n", t.Name())
	case "go", "resume", "c":
		if err := t.Resume(); err != nil {
			return err
		}
		delete(sh.halted, t.Name())
		sh.dropRunning()
		fmt.Printf("%s running\n", t.Name())
	case "regs":
		names := t.Registers()
		vals, err := t.ReadRegisters(names)
		if err != nil {
			return err
		}
		for i, n := range names {
			fmt.Printf("%-9s %08x", n, vals[i])
			if i%4 == 3 || i == len(names)-1 {
				fmt.Println()
			} else {
				fmt.Print("   ")
			}
		}
	case "reg":
		if len(f) < 2 {
			return errors.New("reg NAME [VALUE]")
		}
		if len(f) >= 3 {
			v, err := arg(2)
			if err != nil {
				return err
			}
			return t.WriteRegister(f[1], v)
		}
		v, err := t.ReadRegister(f[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s = %#x\n", f[1], v)
	case "md":
		addr, err := arg(1)
		if err != nil {
			return err
		}
		n, err := optArg(2, 64)
		if err != nil {
			return err
		}
		buf := make([]byte, n)
		if err := t.ReadMemory(addr, buf); err != nil {
			return err
		}
		hexdump(os.Stdout, addr, buf)
	case "mw":
		addr, err := arg(1)
		if err != nil {
			return err
		}
		v, err := arg(2)
		if err != nil {
			return err
		}
		size, err := optArg(3, 4)
		if err != nil {
			return err
		}
		if size != 1 && size != 2 && size != 4 {
			return errors.New("size must be 1, 2 or 4")
		}
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte(v >> (8 * i))
		}
		return t.WriteMemory(addr, buf)
	case "step", "cont", "wait", "bp", "bpc", "trace":
		d, ok := t.(runctl.Debugger)
		if !ok {
			return fmt.Errorf("%s has no breakpoint/step support", t.Name())
		}
		switch f[0] {
		case "step":
			si, err := d.Step()
			if err != nil {
				return err
			}
			sh.halted[t.Name()] = true
			fmt.Printf("%s stopped at %#x (%s)\n", t.Name(), si.PC, si.Reason)
		case "cont", "wait":
			ms, err := optArg(1, 10000)
			if err != nil {
				return err
			}
			if f[0] == "cont" {
				if err := t.Resume(); err != nil {
					return err
				}
				delete(sh.halted, t.Name())
			}
			si, stopped, err := d.Wait(time.Duration(ms) * time.Millisecond)
			if err != nil {
				return err
			}
			if !stopped {
				fmt.Printf("%s still running after %d ms\n", t.Name(), ms)
				return nil
			}
			sh.halted[t.Name()] = true
			fmt.Printf("%s stopped at %#x (%s)\n", t.Name(), si.PC, si.Reason)
		case "bp":
			slot, err := arg(1)
			if err != nil {
				return err
			}
			addr, err := arg(2)
			if err != nil {
				return err
			}
			kind := "x"
			if len(f) > 3 {
				kind = f[3]
			}
			length, err := optArg(4, 1)
			if err != nil {
				return err
			}
			return d.SetBreakpoint(int(slot), addr, kind, int(length))
		case "trace":
			n, err := arg(1)
			if err != nil {
				return err
			}
			la, _ := t.(interface{ ReadLinear(uint64, []byte) error })
			for i := uint64(0); i < n; i++ {
				si, err := d.Step()
				if err != nil {
					return fmt.Errorf("step %d: %w", i, err)
				}
				sh.halted[t.Name()] = true
				line := fmt.Sprintf("%5d %08x", i, si.PC)
				if la != nil {
					var code [15]byte
					if la.ReadLinear(si.PC, code[:]) == nil {
						if inst, err := x86asm.Decode(code[:], codeMode(t)); err == nil {
							line += "  " + x86asm.IntelSyntax(inst, si.PC, nil)
						}
					}
				}
				fmt.Println(line)
			}
		case "bpc":
			slot, err := arg(1)
			if err != nil {
				return err
			}
			if err := d.ClearBreakpoint(int(slot)); err != nil {
				return err
			}
			if sh.halted[t.Name()] {
				return nil // takes effect on resume
			}
			// The breakpoint stays programmed while the thread runs: halt
			// it so that resuming applies the change.
			si, err := t.Halt()
			if err != nil {
				return fmt.Errorf("halting %s to clear the breakpoint: %w", t.Name(), err)
			}
			fmt.Printf("%s halted at %#x (%s) to clear the breakpoint; resuming\n", t.Name(), si.PC, si.Reason)
			return t.Resume()
		}
	case "stepmode":
		sm, ok := t.(interface{ SetStepMode(string) error })
		if !ok {
			return fmt.Errorf("%s has no step modes", t.Name())
		}
		if len(f) < 2 {
			return errors.New("stepmode targeted|all")
		}
		return sm.SetStepMode(f[1])
	case "resetbreak":
		rb, ok := t.(interface{ SetResetBreak(bool) error })
		if !ok {
			return fmt.Errorf("%s has no reset break", t.Name())
		}
		if len(f) < 2 || (f[1] != "on" && f[1] != "off") {
			return errors.New("resetbreak on|off")
		}
		return rb.SetResetBreak(f[1] == "on")
	case "rdmsr", "wrmsr":
		m, ok := t.(runctl.MSRAccessor)
		if !ok {
			return fmt.Errorf("%s has no MSRs", t.Name())
		}
		idx, err := arg(1)
		if err != nil {
			return err
		}
		force := f[len(f)-1] == "force"
		if f[0] == "wrmsr" {
			v, err := arg(2)
			if err != nil {
				return err
			}
			if force {
				return m.WriteMSRForce(uint32(idx), v)
			}
			return m.WriteMSR(uint32(idx), v)
		}
		read := m.ReadMSR
		if force {
			read = m.ReadMSRForce
		}
		v, err := read(uint32(idx))
		if err != nil {
			return err
		}
		fmt.Printf("msr %#x = %#018x\n", idx, v)
	case "v2p", "vmd", "vmw", "dis":
		la, ok := t.(interface {
			Translate(uint64) (uint64, error)
			ReadLinear(uint64, []byte) error
			WriteLinear(uint64, []byte) error
		})
		if !ok {
			return fmt.Errorf("%s has no linear address support", t.Name())
		}
		addr, err := arg(1)
		if err != nil {
			return err
		}
		switch f[0] {
		case "v2p":
			pa, err := la.Translate(addr)
			if err != nil {
				return err
			}
			fmt.Printf("%#x -> %#x\n", addr, pa)
		case "vmd", "dis":
			n, err := optArg(2, 64)
			if err != nil {
				return err
			}
			buf := make([]byte, n)
			if err := la.ReadLinear(addr, buf); err != nil {
				return err
			}
			if f[0] == "vmd" {
				hexdump(os.Stdout, addr, buf)
			} else {
				return disassemble(addr, buf, codeMode(t))
			}
		case "vmw":
			v, err := arg(2)
			if err != nil {
				return err
			}
			size, err := optArg(3, 4)
			if err != nil {
				return err
			}
			buf := make([]byte, size)
			for i := range buf {
				buf[i] = byte(v >> (8 * i))
			}
			return la.WriteLinear(addr, buf)
		}
	case "ior":
		port, err := arg(1)
		if err != nil {
			return err
		}
		size, err := optArg(2, 1)
		if err != nil {
			return err
		}
		v, err := t.IORead(uint16(port), int(size))
		if err != nil {
			return err
		}
		fmt.Printf("port %#x = 0x%0*x\n", port, 2*size, v)
	case "iow":
		port, err := arg(1)
		if err != nil {
			return err
		}
		v, err := arg(2)
		if err != nil {
			return err
		}
		size, err := optArg(3, 1)
		if err != nil {
			return err
		}
		return t.IOWrite(uint16(port), int(size), uint32(v))
	default:
		return fmt.Errorf("unknown command %q (try help)", f[0])
	}
	return nil
}

func hexdump(w io.Writer, addr uint64, b []byte) {
	for off := 0; off < len(b); off += 16 {
		line := b[off:min(off+16, len(b))]
		fmt.Fprintf(w, "%08x  ", addr+uint64(off))
		for i := 0; i < 16; i++ {
			if i < len(line) {
				fmt.Fprintf(w, "%02x ", line[i])
			} else {
				fmt.Fprint(w, "   ")
			}
			if i == 7 {
				fmt.Fprint(w, " ")
			}
		}
		fmt.Fprint(w, " |")
		for _, c := range line {
			if c < 0x20 || c > 0x7e {
				c = '.'
			}
			fmt.Fprintf(w, "%c", c)
		}
		fmt.Fprintln(w, "|")
	}
}

// csrCmd: csr DEV FN OFF [SIZE] [VAL] / mmio DEV FN BAR OFF [SIZE] [VAL]
func (sh *shell) csrCmd(f []string) error {
	if sh.plat.CSR == nil {
		return fmt.Errorf("%s: platform %s has no CSR access", f[0], sh.plat.Name)
	}
	n := 3
	if f[0] == "mmio" {
		n = 4
	}
	if len(f) < n+1 {
		return fmt.Errorf("usage: see help")
	}
	var a []uint64
	for _, x := range f[1:] {
		v, err := num(x)
		if err != nil {
			return err
		}
		a = append(a, v)
	}
	size := 4
	if len(a) > n {
		size = int(a[n])
	}
	write := len(a) > n+1
	var val uint64
	if write {
		val = a[n+1]
	}
	var out uint64
	var err error
	if f[0] == "csr" {
		dev, fn, off := int(a[0]), int(a[1]), uint16(a[2])
		if write {
			err = sh.plat.CSR.WriteCSR(sh.s, dev, fn, off, size, val)
		} else {
			out, err = sh.plat.CSR.ReadCSR(sh.s, dev, fn, off, size)
		}
	} else {
		dev, fn, bar, off := int(a[0]), int(a[1]), int(a[2]), uint16(a[3])
		if write {
			err = sh.plat.CSR.WriteMMIO(sh.s, dev, fn, bar, off, size, val)
		} else {
			out, err = sh.plat.CSR.ReadMMIO(sh.s, dev, fn, bar, off, size)
		}
	}
	if err != nil || write {
		return err
	}
	fmt.Printf("%s = 0x%0*x\n", strings.Join(f[1:n+1], " "), 2*size, out)
	return nil
}

// sbCmd: sb SPACE PORT FID BAR OFF [SIZE] [VAL]
func (sh *shell) sbCmd(f []string) error {
	if sh.plat.Sideband == nil {
		return fmt.Errorf("sb: platform %s has no sideband access", sh.plat.Name)
	}
	if len(f) < 6 {
		return fmt.Errorf("usage: see help")
	}
	op, ok := iosfsb.Spaces[f[1]]
	if !ok {
		return fmt.Errorf("sb: unknown space %q", f[1])
	}
	var a []uint64
	for _, x := range f[2:] {
		v, err := num(x)
		if err != nil {
			return err
		}
		a = append(a, v)
	}
	size := uint64(4)
	if len(a) > 4 {
		size = a[4]
	}
	m := iosfsb.Msg{Port: uint8(a[0]), Opcode: op, FID: uint8(a[1]), BAR: uint8(a[2]), Offset: a[3], Bits: int(size) * 8}
	if len(a) > 5 {
		return sh.plat.Sideband.Write(sh.s, m, a[5])
	}
	v, err := sh.plat.Sideband.Read(sh.s, m)
	if err != nil {
		return err
	}
	fmt.Printf("%s = 0x%0*x\n", strings.Join(f[1:6], " "), 2*size, v)
	return nil
}

// disassemble prints x86 code in the given mode (16, 32 or 64).
func disassemble(addr uint64, code []byte, mode int) error {
	for off := 0; off < len(code); {
		inst, err := x86asm.Decode(code[off:], mode)
		n := inst.Len
		text := x86asm.IntelSyntax(inst, addr+uint64(off), nil)
		if err != nil || n == 0 {
			n, text = 1, "(bad)"
		}
		fmt.Printf("%08x  %-24x %s\n", addr+uint64(off), code[off:min(off+n, len(code))], text)
		off += n
	}
	return nil
}

// dropRunning forgets threads that are no longer halted, e.g. siblings
// resumed together with another thread of the same core.
func (sh *shell) dropRunning() {
	for name := range sh.halted {
		if h, err := sh.threads[name].Halted(); err == nil && !h {
			delete(sh.halted, name)
		}
	}
}

// codeMode is the mode to disassemble a thread's code in.
func codeMode(t runctl.Thread) int {
	if m, ok := t.(interface{ CodeMode() int }); ok {
		return m.CodeMode()
	}
	return 32
}
