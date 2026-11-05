// SPDX-License-Identifier: GPL-2.0-only

// Package runctl is architecture-independent run control: halting and
// resuming hardware threads and accessing their registers and memory.
// Implementations register themselves per model.Thread.Arch.
package runctl

import (
	"fmt"
	"time"

	"libredci/pkg/model"
	"libredci/pkg/tapnet"
)

// Thread is one debuggable hardware thread.
type Thread interface {
	Name() string
	// Halted queries the hardware.
	Halted() (bool, error)
	// Halt stops the thread and prepares it for debug accesses.
	Halt() (StopInfo, error)
	// Resume restores the state Halt changed (plus any register writes made
	// while halted) and lets the thread run.
	Resume() error
	// Registers lists register names in display order.
	Registers() []string
	ReadRegister(name string) (uint64, error)
	ReadRegisters(names []string) ([]uint64, error)
	WriteRegister(name string, v uint64) error
	// ReadMemory/WriteMemory access physical memory.
	ReadMemory(addr uint64, buf []byte) error
	WriteMemory(addr uint64, buf []byte) error
	// IORead/IOWrite access I/O ports; size is 1, 2 or 4.
	IORead(port uint16, size int) (uint32, error)
	IOWrite(port uint16, size int, v uint32) error
}

// Attacher is implemented by threads that can adopt an already halted
// thread without modifying it.
type Attacher interface {
	Attach() error
	// Owned reports whether Resume knows the pre-halt state to restore.
	Owned() bool
	// ForceResume resumes even if the pre-halt state is unknown.
	ForceResume() error
}

// Debugger is implemented by threads with hardware breakpoints and
// single-stepping.
type Debugger interface {
	Step() (StopInfo, error)
	// Wait blocks until the thread stops by itself or timeout expires.
	Wait(timeout time.Duration) (si StopInfo, stopped bool, err error)
	// SetBreakpoint arms slot at a linear address; kind is "x" (execute),
	// "w" (write), "rw" (access) or "io"; length 1/2/4/8 for data.
	SetBreakpoint(slot int, addr uint64, kind string, length int) error
	ClearBreakpoint(slot int) error
}

// MSRAccessor is implemented by x86 threads with model-specific registers.
type MSRAccessor interface {
	// ReadMSR/WriteMSR only allow MSRs known to exist on the core.
	ReadMSR(index uint32) (uint64, error)
	WriteMSR(index uint32, v uint64) error
	// The Force variants skip that check; accessing an unimplemented MSR
	// can be fatal for the platform.
	ReadMSRForce(index uint32) (uint64, error)
	WriteMSRForce(index uint32, v uint64) error
}

// StopInfo describes where and why a thread stopped.
type StopInfo struct {
	PC     uint64 // linear address of the next instruction
	Reason string
}

// Factory creates a Thread for a model thread on a TAP.
type Factory func(s *tapnet.Session, tap *model.TAP, th model.Thread) (Thread, error)

var factories = map[string]Factory{}

// Register makes an architecture available; called from init functions.
func Register(arch string, f Factory) { factories[arch] = f }

// Threads lists all threads in a platform model, with the TAP they live on.
func Threads(p *model.Platform) (names []string, taps map[string]*model.TAP) {
	taps = map[string]*model.TAP{}
	p.Walk(func(t *model.TAP, _ int) {
		for _, th := range t.Threads {
			names = append(names, th.Name)
			taps[th.Name] = t
		}
	})
	return names, taps
}

// Open returns a Thread by name.
func Open(s *tapnet.Session, name string) (Thread, error) {
	_, taps := Threads(s.Plat)
	tap, ok := taps[name]
	if !ok {
		return nil, fmt.Errorf("runctl: no thread %q", name)
	}
	for _, th := range tap.Threads {
		if th.Name != name {
			continue
		}
		f, ok := factories[th.Arch]
		if !ok {
			return nil, fmt.Errorf("runctl: thread %s: architecture %q not supported yet", name, th.Arch)
		}
		return f(s, tap, th)
	}
	panic("unreachable")
}
