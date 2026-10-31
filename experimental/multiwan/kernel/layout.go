// Package kernel renders and applies the swanix data path: an nftables table,
// policy-routing rules and per-WAN routing tables. It only owns objects it
// created (its own nft table, its priority range, its table numbers and its
// mark bits) and never flushes anything else.
package kernel

import (
	"fmt"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// Mark bit layout inside the 8-bit window selected by kernel.mark_mask:
//
//	bits 0-3  WAN ID (0 unassigned, 1-14 stable IDs, 15 reject)
//	bit 4     connection was initiated from a WAN (inbound)
//	bit 5     connection was initiated by the router itself
//	bit 6     health probe
//	bit 7     reserved
const (
	RejectID = 15

	bitInbound = 1 << 4
	bitLocal   = 1 << 5
	bitProbe   = 1 << 6
)

// Layout is the concrete mark layout derived from kernel.mark_mask.
type Layout struct {
	Mask  uint32 // the whole 8-bit window
	Shift uint
}

func NewLayout(mask string) (Layout, error) {
	m, s, err := config.ParseMarkMask(mask)
	if err != nil {
		return Layout{}, fmt.Errorf("kernel.mark_mask: %w", err)
	}
	return Layout{Mask: m, Shift: s}, nil
}

// IDMask selects only the WAN ID bits.
func (l Layout) IDMask() uint32 { return 0xf << l.Shift }

// Inbound, Local and Probe are the flag bits.
func (l Layout) Inbound() uint32 { return bitInbound << l.Shift }
func (l Layout) Local() uint32   { return bitLocal << l.Shift }
func (l Layout) Probe() uint32   { return bitProbe << l.Shift }

// WANMark is the packet/ct mark value for a WAN ID.
func (l Layout) WANMark(id int) uint32 { return uint32(id) << l.Shift }

// ProbeMark is the SO_MARK value used by health probes for a WAN.
func (l Layout) ProbeMark(id int) uint32 { return l.WANMark(id) | l.Probe() }

// Keep returns the complement of Mask, preserving other components' bits.
func (l Layout) Keep() uint32 { return ^l.Mask }

// IDOf extracts the WAN ID from a mark.
func (l Layout) IDOf(mark uint32) int { return int((mark & l.IDMask()) >> l.Shift) }
