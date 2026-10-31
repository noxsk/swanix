package main

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func iaPD(iaid uint32, prefixes ...netip.Prefix) []byte {
	ia := u32(u32(u32(nil, iaid), 0), 0)
	for _, p := range prefixes {
		a := p.Addr().As16()
		pfx := append(u32(u32(nil, 100), 200), byte(p.Bits()))
		ia = put(ia, optIAPrefix, append(pfx, a[:]...))
	}
	return ia
}

func message(typ byte, opts ...option) []byte {
	b := []byte{typ, 0xaa, 0xbb, 0xcc}
	for _, o := range opts {
		b = put(b, o.code, o.data)
	}
	return b
}

// delegated returns prefix -> valid lifetime from the IA_PD of a reply.
func delegated(t *testing.T, resp []byte) map[netip.Prefix]uint32 {
	t.Helper()
	out := map[netip.Prefix]uint32{}
	for _, o := range parseOptions(resp[4:]) {
		if o.code != optIAPD {
			continue
		}
		for _, sub := range parseOptions(o.data[12:]) {
			if sub.code == optIAPrefix {
				p := netip.PrefixFrom(netip.AddrFrom16([16]byte(sub.data[9:25])), int(sub.data[8]))
				out[p] = binary.BigEndian.Uint32(sub.data[4:8])
			}
		}
	}
	return out
}

func TestAnswers(t *testing.T) {
	s := &server{duid: []byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6},
		pd: netip.MustParsePrefix("2001:db8:1:100::/56"), pool: netip.MustParsePrefix("2001:db8:a::/64")}
	cid := option{optClientID, []byte{0, 3, 0, 1, 9, 9, 9, 9, 9, 9}}

	adv := s.answer(message(msgSolicit, cid, option{optIAPD, iaPD(7)}, option{optIANA, u32(u32(u32(nil, 1), 0), 0)}))
	if adv[0] != msgAdvertise || [3]byte(adv[1:4]) != [3]byte{0xaa, 0xbb, 0xcc} {
		t.Fatalf("solicit: type %d, xid %x", adv[0], adv[1:4])
	}
	if got := delegated(t, adv); len(got) != 1 || got[s.pd] != validLife {
		t.Fatalf("solicit: delegated %v", got)
	}
	if r := s.answer(message(msgSolicit, cid, option{optRapidCommit, nil}, option{optIAPD, iaPD(7)})); r[0] != msgReply {
		t.Fatalf("rapid commit must be answered with a reply, got %d", r[0])
	}

	// the ISP renumbers: a renewal of the old prefix gets the new one and the old one with lifetime 0
	old := s.pd
	s.pd = netip.MustParsePrefix("2001:db8:1:300::/56")
	got := delegated(t, s.answer(message(msgRenew, cid, option{optIAPD, iaPD(7, old)})))
	if len(got) != 2 || got[s.pd] != validLife || got[old] != 0 {
		t.Fatalf("renew after renumbering: %v", got)
	}

	if s.answer(message(msgSolicit, option{optIAPD, iaPD(7)})) != nil {
		t.Fatal("a message without client id must be ignored")
	}
}
