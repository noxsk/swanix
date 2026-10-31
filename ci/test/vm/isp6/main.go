// Command isp6 is the IPv6 side of a test ISP for the OpenWrt VM test (ci/test/vm).
// It sends router advertisements and answers DHCPv6 (one address and one delegated
// prefix per client) on a single interface, Ethernet or PPP alike: ISC dhcpd refuses
// PPP devices and Kea binds the peer's address on them, and the 24.10 feeds have no
// radvd. It is a test fixture, not a general DHCPv6 server.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/net/ipv6"
)

const (
	msgSolicit     = 1
	msgAdvertise   = 2
	msgRequest     = 3
	msgConfirm     = 4
	msgRenew       = 5
	msgRebind      = 6
	msgReply       = 7
	msgRelease     = 8
	msgDecline     = 9
	msgInfoRequest = 11

	optClientID     = 1
	optServerID     = 2
	optIANA         = 3
	optIAAddr       = 5
	optStatusCode   = 13
	optRapidCommit  = 14
	optIAPD         = 25
	optIAPrefix     = 26
	preferredLife   = 300
	validLife       = 600
	renewT1, rebind = 150, 240
)

var (
	iface      = flag.String("i", "", "interface")
	pdFlag     = flag.String("pd", "", "prefix delegated to every client, e.g. 2001:db8:1:100::/56")
	poolFlag   = flag.String("pool", "", "/64 that client addresses (IA_NA) come from")
	raPrefix   = flag.Bool("ra-prefix", false, "advertise -pool in RAs for SLAAC as well")
	raInterval = flag.Duration("ra-interval", 3*time.Second, "router advertisement interval")
)

type option struct {
	code uint16
	data []byte
}

func parseOptions(b []byte) []option {
	var out []option
	for len(b) >= 4 {
		code, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			break
		}
		out = append(out, option{code, b[4 : 4+n]})
		b = b[4+n:]
	}
	return out
}

func put(dst []byte, code uint16, data []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, code)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(data)))
	return append(dst, data...)
}

func u32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }

func status(code uint16) []byte { return binary.BigEndian.AppendUint16(nil, code) }

type server struct {
	ifi    *net.Interface
	duid   []byte
	pd     netip.Prefix
	pool   netip.Prefix
	reload chan netip.Prefix
}

// address picks a stable host in the pool for a client.
func (s *server) address(clientID []byte) netip.Addr {
	h := sha256.Sum256(clientID)
	a := s.pool.Addr().As16()
	a[14], a[15] = 0x01, h[0]
	return netip.AddrFrom16(a)
}

func (s *server) answer(req []byte) []byte {
	if len(req) < 4 {
		return nil
	}
	typ := req[0]
	opts := parseOptions(req[4:])
	var clientID []byte
	rapid := false
	for _, o := range opts {
		switch o.code {
		case optClientID:
			clientID = o.data
		case optRapidCommit:
			rapid = true
		}
	}
	if clientID == nil {
		return nil
	}
	reply := byte(msgReply)
	switch typ {
	case msgSolicit:
		if !rapid {
			reply = msgAdvertise
		}
	case msgRequest, msgRenew, msgRebind, msgConfirm, msgRelease, msgDecline, msgInfoRequest:
	default:
		return nil
	}
	out := append([]byte{reply}, req[1:4]...)
	out = put(out, optClientID, clientID)
	out = put(out, optServerID, s.duid)
	if typ == msgSolicit && rapid {
		out = put(out, optRapidCommit, nil)
	}
	if typ == msgRelease || typ == msgDecline || typ == msgConfirm {
		return put(out, optStatusCode, status(0))
	}
	release := typ == msgInfoRequest
	for _, o := range opts {
		if release || len(o.data) < 12 || (o.code != optIANA && o.code != optIAPD) {
			continue
		}
		ia := append([]byte{}, o.data[:4]...) // IAID
		ia = u32(u32(ia, renewT1), rebind)
		if o.code == optIANA && s.pool.IsValid() {
			cur := s.address(clientID)
			a := cur.As16()
			ia = put(ia, optIAAddr, u32(u32(a[:], preferredLife), validLife))
			// addresses the client still holds but that are no longer ours: lifetime 0
			for _, sub := range parseOptions(o.data[12:]) {
				if sub.code == optIAAddr && len(sub.data) >= 16 {
					if old := netip.AddrFrom16([16]byte(sub.data[:16])); old != cur {
						ia = put(ia, optIAAddr, u32(u32(sub.data[:16:16], 0), 0))
					}
				}
			}
			out = put(out, optIANA, ia)
		}
		if o.code == optIAPD && s.pd.IsValid() {
			p := s.pd.Addr().As16()
			pfx := u32(u32(nil, preferredLife), validLife)
			pfx = append(append(pfx, byte(s.pd.Bits())), p[:]...)
			ia = put(ia, optIAPrefix, pfx)
			for _, sub := range parseOptions(o.data[12:]) {
				if sub.code == optIAPrefix && len(sub.data) >= 25 {
					old := netip.PrefixFrom(netip.AddrFrom16([16]byte(sub.data[9:25])), int(sub.data[8]))
					if old != s.pd {
						gone := u32(u32(nil, 0), 0)
						ia = put(ia, optIAPrefix, append(gone, sub.data[8:25]...))
					}
				}
			}
			out = put(out, optIAPD, ia)
		}
	}
	return out
}

func (s *server) serveDHCP() {
	group := &net.UDPAddr{IP: net.ParseIP("ff02::1:2"), Port: 547}
	conn, err := net.ListenMulticastUDP("udp6", s.ifi, group)
	if err != nil {
		log.Fatalf("dhcpv6 socket: %v", err)
	}
	// the receiving socket is bound to the group address, which cannot be a source
	send, err := net.ListenUDP("udp6", &net.UDPAddr{})
	if err != nil {
		log.Fatalf("dhcpv6 send socket: %v", err)
	}
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Fatalf("dhcpv6 read: %v", err)
		}
		select {
		case p := <-s.reload:
			s.pd = p
		default:
		}
		if resp := s.answer(buf[:n]); resp != nil {
			from.Zone = s.ifi.Name
			if _, err := send.WriteToUDP(resp, from); err != nil {
				log.Printf("dhcpv6 reply to %v: %v", from, err)
			} else {
				log.Printf("dhcpv6 type %d from %v: answered with type %d (pd %v)", buf[0], from.IP, resp[0], s.pd)
			}
		}
	}
}

// sendRAs advertises a default router with the managed and other-config flags.
func (s *server) sendRAs() {
	c, err := net.ListenPacket("ip6:ipv6-icmp", "::")
	if err != nil {
		log.Fatalf("ra socket: %v", err)
	}
	pc := ipv6.NewPacketConn(c)
	if err := pc.SetMulticastHopLimit(255); err != nil {
		log.Fatal(err)
	}
	if err := pc.SetMulticastInterface(s.ifi); err != nil {
		log.Fatal(err)
	}
	ra := []byte{134, 0, 0, 0, 64, 0xc0, 0x07, 0x08, 0, 0, 0, 0, 0, 0, 0, 0} // lifetime 1800 s, M+O
	if *raPrefix && s.pool.IsValid() {
		p := s.pool.Addr().As16()
		pi := []byte{3, 4, byte(s.pool.Bits()), 0xc0}
		pi = u32(u32(pi, validLife), preferredLife)
		pi = append(append(pi, 0, 0, 0, 0), p[:]...)
		ra = append(ra, pi...)
	}
	dst := &net.IPAddr{IP: net.ParseIP("ff02::1"), Zone: s.ifi.Name}
	for {
		if _, err := pc.WriteTo(ra, nil, dst); err != nil {
			log.Printf("ra: %v", err)
		}
		time.Sleep(*raInterval)
	}
}

func main() {
	flag.Parse()
	ifi, err := net.InterfaceByName(*iface)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{ifi: ifi, reload: make(chan netip.Prefix, 1)}
	if *pdFlag != "" {
		s.pd = netip.MustParsePrefix(*pdFlag)
	}
	if *poolFlag != "" {
		s.pool = netip.MustParsePrefix(*poolFlag)
	}
	h := sha256.Sum256([]byte(ifi.Name))
	s.duid = append([]byte{0, 3, 0, 1}, h[:6]...) // DUID-LL, made up but stable
	// SIGHUP re-reads the delegated prefix from $ISP6_PD_FILE (a renumbering ISP)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if b, err := os.ReadFile(os.Getenv("ISP6_PD_FILE")); err == nil {
				if p, err := netip.ParsePrefix(string(trimSpace(b))); err == nil {
					s.reload <- p
					log.Printf("delegated prefix is now %v", p)
				}
			}
		}
	}()
	go s.sendRAs()
	s.serveDHCP()
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
