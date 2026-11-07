// SPDX-License-Identifier: GPL-2.0-only

// Package dci talks to an Intel DCI "DbC" (USB Debug Class) target, i.e. the
// ExI bridge in the PCH that shows up as a USB device when DCI is enabled.
// See docs/protocol.md for the wire format.
package dci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/gousb"
	"libredci/pkg/jtag"
)

// Known DCI DbC product IDs (vendor 0x8087). Alder Point-S's DbC is 0x0b23.
var ProductIDs = []gousb.ID{
	0x0a6e, 0x0a6f, 0x0a70, 0x0a71, 0x0a72, 0x0a73, 0x0a74, 0x0a75, 0x0a76,
	0x0a77, 0x0a78, 0x0a79, 0x0ae6, 0x0ae7, 0x0ae8, 0x0ae9, 0x0aea, 0x0aeb,
	0x0aec, 0x0aed, 0x0aee, 0x0aef, 0x0b23,
}

const (
	vendorIntel = gousb.ID(0x8087)
	dfxIface    = 1 // interface carrying ExI DFX packets (bulk EP 3 in/out)
	dfxEP       = 3
	dmaIface    = 3 // DMA interface (bulk EP 2 in/out)
	dmaEP       = 2
	maxXfer     = 1024 // OpenIPC never exceeds one max-size SuperSpeed packet
)

// Register banks / registers as observed (names are best guesses).
const (
	bankBridge = 0
	bankJtag   = 1

	regArbitration = 0x01 // bank 0: 2 = request debug ownership, 0 = release
	regUnlock0     = 0x06 // bank 0: regs 6..9 take a fixed unlock key
	regBridgeA     = 0x0a
	regSession     = 0x0b // bank 0: per-session value chosen by the host

	regJtagCtrl  = 0x00 // bank 1: control; bit 1 selects chain (0=PCH, 1=CPU)
	regJtagClock = 0x01 // bank 1: TCK setting (0x28 for chain 0, 0x20 for chain 1)
	regJtag2     = 0x02
	regJtag3     = 0x03
	regJtagRun   = 0x05 // bank 1: OpenIPC pulses 0 -> 1 after (re)configuring
	regJtag6     = 0x06
)

var unlockKey = [4]uint16{0x49d6, 0xa232, 0x2ddf, 0x2b99}

// Chain settings for bank-1 regs 0 and 1, as programmed by OpenIPC for
// KBL+SPT-LP (MuxSel 1 and 3 in its probe config).
var chainSettings = [2][2]uint16{
	{0x1811, 0x0028}, // chain 0: PCH (SPT) TAP network
	{0x1813, 0x0020}, // chain 1: CPU (KBL uncore) TAP network
}

// Probe is an open connection to a DCI target.
type Probe struct {
	ctx  *gousb.Context
	dev  *gousb.Device
	cfg  *gousb.Config
	intf *gousb.Interface
	out  *gousb.OutEndpoint
	in   *gousb.InEndpoint

	dmaIntf *gousb.Interface
	dmaIn   *gousb.InEndpoint

	Verbose bool
	Timeout time.Duration

	cfgSeq  byte
	jtagSeq byte
	chain   int
	trigger bool

	mu       sync.Mutex
	statusCh chan Status
	tdoCh    chan TDO
	readErr  error
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Open finds the first DCI DbC device and claims its DFX interface.
func Open() (*Probe, error) {
	ctx := gousb.NewContext()
	devs, err := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		if d.Vendor != vendorIntel {
			return false
		}
		for _, p := range ProductIDs {
			if d.Product == p {
				return true
			}
		}
		return false
	})
	if len(devs) == 0 {
		ctx.Close()
		if err != nil {
			return nil, fmt.Errorf("dci: open: %w", err)
		}
		return nil, errors.New("dci: no DCI DbC device found (is DCI enabled and the target connected?)")
	}
	for _, d := range devs[1:] {
		d.Close()
	}
	p := &Probe{ctx: ctx, dev: devs[0], Timeout: 2 * time.Second, chain: -1,
		statusCh: make(chan Status, 64), tdoCh: make(chan TDO, 256)}
	if err := p.claim(); err != nil {
		p.closeUSB()
		return nil, err
	}
	rctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go p.reader(rctx)
	return p, nil
}

func (p *Probe) claim() error {
	p.dev.SetAutoDetach(true)
	cfg, err := p.dev.Config(1)
	if err != nil {
		return fmt.Errorf("dci: set config: %w", err)
	}
	p.cfg = cfg
	intf, err := cfg.Interface(dfxIface, 0)
	if err != nil {
		return fmt.Errorf("dci: claim interface %d: %w", dfxIface, err)
	}
	p.intf = intf
	if p.out, err = intf.OutEndpoint(dfxEP); err != nil {
		return err
	}
	if p.in, err = intf.InEndpoint(dfxEP); err != nil {
		return err
	}
	return nil
}

func (p *Probe) closeUSB() {
	if p.dmaIntf != nil {
		p.dmaIntf.Close()
	}
	if p.intf != nil {
		p.intf.Close()
	}
	if p.cfg != nil {
		p.cfg.Close()
	}
	if p.dev != nil {
		p.dev.Close()
	}
	p.ctx.Close()
}

// DMARead reads n bytes from the DMA interface (interface 3, bulk IN
// endpoint 0x82), claiming it on first use. The target sends the payload
// of a DMA read programmed into its DbC.
func (p *Probe) DMARead(n int, timeout time.Duration) ([]byte, error) {
	if p.dmaIn == nil {
		intf, err := p.cfg.Interface(dmaIface, 0)
		if err != nil {
			return nil, fmt.Errorf("dci: claim DMA interface: %w", err)
		}
		in, err := intf.InEndpoint(dmaEP)
		if err != nil {
			intf.Close()
			return nil, err
		}
		p.dmaIntf, p.dmaIn = intf, in
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	buf := make([]byte, n)
	got := 0
	for got < n {
		k, err := p.dmaIn.ReadContext(ctx, buf[got:])
		got += k
		if err != nil {
			return buf[:got], fmt.Errorf("dci: DMA read: %w", err)
		}
	}
	return buf, nil
}

// reader continuously drains the IN endpoint and routes packets.
func (p *Probe) reader(ctx context.Context) {
	defer p.wg.Done()
	buf := make([]byte, maxXfer)
	for {
		n, err := p.in.ReadContext(ctx, buf)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.mu.Lock()
			p.readErr = err
			p.mu.Unlock()
			return
		}
		for o := 0; o+PacketSize <= n; o += PacketSize {
			var pk Packet
			copy(pk[:], buf[o:o+PacketSize])
			if p.Verbose {
				log.Printf("<- % x", pk[:])
			}
			switch pk.Type() {
			case TypeConfig:
				select {
				case p.statusCh <- parseStatus(pk):
				default: // nobody waiting and queue full: drop oldest
					<-p.statusCh
					p.statusCh <- parseStatus(pk)
				}
			case TypeJtag:
				p.tdoCh <- parseTDO(pk)
			default:
				log.Printf("dci: unknown inbound packet type %#x: % x", pk.Type(), pk[:16])
			}
		}
	}
}

func (p *Probe) send(pkts []Packet) error {
	buf := make([]byte, 0, maxXfer)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
		defer cancel()
		_, err := p.out.WriteContext(ctx, buf)
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("dci: target not accepting packets (bridge busy or stuck)")
		}
		buf = buf[:0]
		return err
	}
	for i := range pkts {
		if p.Verbose {
			log.Printf("-> % x", pkts[i][:])
		}
		if len(buf)+PacketSize > maxXfer {
			if err := flush(); err != nil {
				return err
			}
		}
		buf = append(buf, pkts[i][:]...)
	}
	return flush()
}

func (p *Probe) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readErr
}

// waitStatus waits for a status packet satisfying ok.
func (p *Probe) waitStatus(ok func(Status) bool) (Status, error) {
	t := time.NewTimer(p.Timeout)
	defer t.Stop()
	for {
		select {
		case s := <-p.statusCh:
			if ok(s) {
				return s, nil
			}
		case <-t.C:
			if err := p.err(); err != nil {
				return Status{}, fmt.Errorf("dci: read: %w", err)
			}
			return Status{}, errors.New("dci: timed out waiting for status packet")
		}
	}
}

// WriteRegs sends one config packet. With wait set it requests a status
// packet and waits until the target acknowledges this packet.
func (p *Probe) WriteRegs(wait bool, writes ...RegWrite) (Status, error) {
	if len(writes) > maxWritesPerConfig {
		return Status{}, fmt.Errorf("dci: too many register writes (%d)", len(writes))
	}
	p.cfgSeq++
	seq := p.cfgSeq
	if err := p.send([]Packet{configPacket(seq, wait, writes)}); err != nil {
		return Status{}, err
	}
	if !wait {
		return Status{}, nil
	}
	return p.waitStatus(func(s Status) bool { return s.CfgAck == seq&cfgAckMask })
}

func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Connect performs arbitration, unlocks the ExI bridge and configures the
// JTAG engine, replaying the sequence OpenIPC uses.
func (p *Probe) Connect() error {
	drain(p.statusCh)
	drain(p.tdoCh)
	p.cfgSeq = 0

	st, err := p.WriteRegs(true, RegWrite{regArbitration, bankBridge, 2})
	if err != nil {
		return fmt.Errorf("dci: arbitration: %w", err)
	}
	if !st.ArbitrationGranted() {
		return fmt.Errorf("dci: arbitration not granted (%v)", st)
	}
	// The target keeps its JTAG sequence counter across sessions; continue it.
	p.jtagSeq = st.JtagAck

	steps := [][]RegWrite{
		{{regUnlock0, bankBridge, unlockKey[0]}, {regUnlock0 + 1, bankBridge, unlockKey[1]},
			{regUnlock0 + 2, bankBridge, unlockKey[2]}, {regUnlock0 + 3, bankBridge, unlockKey[3]}},
		{{regBridgeA, bankBridge, 0x3c00}, {regSession, bankBridge, uint16(rand.Intn(0x10000))}},
		{{regJtag6, bankJtag, 0xff0e}},
		{{regJtag2, bankJtag, 0}},
		{{regJtagCtrl, bankJtag, 0x1a11}},
		{{regJtagCtrl, bankJtag, 0x1a11}},
		{{regJtagCtrl, bankJtag, chainSettings[0][0]}, {regJtagClock, bankJtag, chainSettings[0][1]},
			{regJtag2, bankJtag, 0x0098}, {regJtag3, bankJtag, 0x00f0}, {regJtagRun, bankJtag, 1}},
	}
	for i, w := range steps {
		if _, err := p.WriteRegs(true, w...); err != nil {
			return fmt.Errorf("dci: connect step %d: %w", i+1, err)
		}
	}
	if err := p.pulseRun(); err != nil {
		return err
	}
	p.chain = 0
	return nil
}

func (p *Probe) pulseRun() error {
	if _, err := p.WriteRegs(false, RegWrite{regJtagRun, bankJtag, 0}); err != nil {
		return err
	}
	_, err := p.WriteRegs(false, RegWrite{regJtagRun, bankJtag, 1})
	return err
}

// SelectChain routes the JTAG engine to chain 0 (PCH) or 1 (CPU).
func (p *Probe) SelectChain(n int) error {
	if n < 0 || n >= len(chainSettings) {
		return fmt.Errorf("dci: no such chain %d", n)
	}
	if p.chain == n {
		return nil
	}
	if _, err := p.WriteRegs(true,
		RegWrite{regJtagCtrl, bankJtag, p.ctrl(n)},
		RegWrite{regJtagClock, bankJtag, chainSettings[n][1]},
		RegWrite{regJtag2, bankJtag, p.reg2()}); err != nil {
		return fmt.Errorf("dci: select chain %d: %w", n, err)
	}
	if err := p.pulseRun(); err != nil {
		return err
	}
	p.chain = n
	return nil
}

// Trigger mode lets TCK commands with the Wait bit stall until the trigger
// input (the CPU's PRDY) fires: bank-1 reg 0 bit 5 and reg 2 = 0xa8.
const (
	ctrlTrigger = 0x0020
	reg2Normal  = 0x0098
	reg2Trigger = 0x00a8
)

func (p *Probe) ctrl(chain int) uint16 {
	v := chainSettings[chain][0]
	if p.trigger {
		v |= ctrlTrigger
	}
	return v
}

func (p *Probe) reg2() uint16 {
	if p.trigger {
		return reg2Trigger
	}
	return reg2Normal
}

// SetTriggerMode enables or disables trigger waits for JTAG scans.
func (p *Probe) SetTriggerMode(on bool) error {
	if p.trigger == on {
		return nil
	}
	p.trigger = on
	if p.chain < 0 {
		return nil
	}
	_, err := p.WriteRegs(true,
		RegWrite{regJtagCtrl, bankJtag, p.ctrl(p.chain)},
		RegWrite{regJtag2, bankJtag, p.reg2()})
	return err
}

// SetPREQ drives the probe-mode request (PREQ) to the CPU. It is bit 1 of
// the JTAG engine's bank-1 register 5 (bit 0 keeps the engine running).
func (p *Probe) SetPREQ(assert bool) error {
	v := uint16(1)
	if assert {
		v |= 2
	}
	_, err := p.WriteRegs(true, RegWrite{regJtagRun, bankJtag, v})
	return err
}

// PulseReset resets the target platform the way the reference tool does:
// bank-1 reg 5 bit 2 asserted for ~170 ms. The DbC link drops while the
// target resets; the Probe is unusable afterwards and must be reopened.
func (p *Probe) PulseReset() error {
	if _, err := p.WriteRegs(false, RegWrite{regJtagRun, bankJtag, 5}); err != nil {
		return err
	}
	time.Sleep(170 * time.Millisecond)
	_, err := p.WriteRegs(false, RegWrite{regJtagRun, bankJtag, 1})
	p.chain = -1 // nothing on the other side is in a known state any more
	return err
}

// Pins is a snapshot of the debug signals the bridge observes.
type Pins struct {
	PREQ bool
	// Raw holds status words 6..13 and bank-1 reg 7 of the pin-status
	// reply; the meaning of most bits is not known yet.
	Raw [9]uint16
}

// ReadPins asks the bridge for a fresh pin snapshot: writing bank-1 reg 0
// with bit 15 set makes the next status packet carry pin state (the bit
// clears itself).
func (p *Probe) ReadPins() (Pins, error) {
	if p.chain < 0 {
		return Pins{}, errors.New("dci: not connected")
	}
	st, err := p.WriteRegs(true, RegWrite{regJtagCtrl, bankJtag, p.ctrl(p.chain) | 0x8000})
	if err != nil {
		return Pins{}, err
	}
	var pins Pins
	for i := 0; i < 8; i++ {
		pins.Raw[i] = st.Word(6 + i)
	}
	pins.Raw[8] = st.BankReg(7)
	pins.PREQ = st.Word(8)&2 != 0
	return pins, nil
}

// Disconnect returns the bridge to its idle state and releases arbitration,
// with the same register writes as the reference tool.
func (p *Probe) Disconnect() error {
	steps := [][]RegWrite{
		{{regSession, bankBridge, 0}, {regBridgeA, bankBridge, 0x3c00},
			{regJtagCtrl, bankJtag, 0}, {regJtagClock, bankJtag, 0}, {regJtag2, bankJtag, 0}, {regJtag3, bankJtag, 0},
			{regJtagRun, bankJtag, 0}, {regJtag6, bankJtag, 0},
			{0x00, 0x30, 0}, {0x12, bankBridge, 0}, {0x11, bankBridge, 0}}, // [observed; meaning unknown]
		{{regUnlock0, bankBridge, 0}, {regUnlock0 + 1, bankBridge, 0}, {regUnlock0 + 2, bankBridge, 0}, {regUnlock0 + 3, bankBridge, 0}},
		{{regArbitration, bankBridge, 0}},
	}
	for _, w := range steps {
		if _, err := p.WriteRegs(true, w...); err != nil {
			return fmt.Errorf("dci: disconnect: %w", err)
		}
	}
	p.chain = -1
	return nil
}

// Close stops the reader and releases the USB device.
func (p *Probe) Close() error {
	p.cancel()
	p.wg.Wait()
	p.closeUSB()
	return nil
}

// Execute implements jtag.Driver on the currently selected chain.
func (p *Probe) Execute(cmds []jtag.TCK) ([]bool, error) {
	if p.chain < 0 {
		return nil, errors.New("dci: not connected")
	}
	drain(p.tdoCh)
	var pkts []Packet
	nib := make([]uint8, 0, jtagNibblesPerPacket)
	for i := 0; i < len(cmds); i += jtagNibblesPerPacket {
		end := min(i+jtagNibblesPerPacket, len(cmds))
		nib = nib[:0]
		for _, c := range cmds[i:end] {
			nib = append(nib, uint8(c))
		}
		p.jtagSeq++
		pkts = append(pkts, jtagPacket(p.jtagSeq, end == len(cmds), nib))
	}
	if len(pkts) == 0 {
		return nil, nil
	}
	last := p.jtagSeq
	want := 0
	for _, c := range cmds {
		if c&jtag.Capture != 0 {
			want++
		}
	}
	if err := p.send(pkts); err != nil {
		return nil, err
	}
	var got []string
	// TDO comes back in one or more packets; the one echoing our final
	// sequence number terminates the scan.
	var bits []bool
	var prev []byte
	t := time.NewTimer(p.Timeout)
	defer t.Stop()
	for {
		select {
		case d := <-p.tdoCh:
			// The target occasionally sends a packet twice; a repeat has
			// the same transmit counter (byte 1) and contents.
			if prev != nil && bytes.Equal(d.Raw[:], prev) {
				got = append(got, fmt.Sprintf("%02x:dup", d.Echo))
				continue
			}
			prev = append(prev[:0], d.Raw[:]...)
			for i := 0; i < d.Bits; i++ {
				bits = append(bits, d.Bit(i))
			}
			got = append(got, fmt.Sprintf("%02x:%d", d.Echo, d.Bits))
			// TDO is returned in packets of up to 448 bits. Several packets
			// can echo the final sequence number when its captures overflow
			// one packet, so stop only once everything has arrived.
			if d.Echo == last && len(bits) >= want {
				if len(bits) != want {
					return nil, fmt.Errorf("dci: got %d TDO bits for %d sent packets (seq ..%02x), expected %d; TDO packets (echo:bits): %s",
						len(bits), len(pkts), last, want, strings.Join(got, " "))
				}
				return bits, nil
			}
		case <-t.C:
			if err := p.err(); err != nil {
				return nil, fmt.Errorf("dci: read: %w", err)
			}
			return nil, fmt.Errorf("dci: timed out waiting for TDO (seq %#02x)", last)
		}
	}
}
