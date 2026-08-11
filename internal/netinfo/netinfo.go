// Package netinfo answers the question a panel on a home machine is actually
// asked: what address do I give my friends?
//
// A machine that hosts a server for friends usually has four addresses and no
// way to tell which one is the right one. 127.0.0.1 works only for the person
// sitting at it. 192.168.1.5 works for the flat. 25.34.12.9 works for whoever
// joined the same Hamachi network. Nothing in the panel said which was which,
// so people handed out localhost and wondered why nobody could connect.
//
// The server itself needs no help: server-ip is left empty on purpose, so it
// listens on every interface a virtual adapter can add later. This package is
// only about naming them.
package netinfo

import (
	"net"
	"strings"
)

// Kind is what sort of network an address belongs to.
type Kind string

// The networks worth telling apart.
const (
	// KindPublic is reachable from the internet, if a router lets it through.
	KindPublic Kind = "public"
	// KindHamachi, KindRadmin, KindZeroTier and KindTailscale are overlay
	// networks: whoever joined the same one can connect, and nobody else.
	KindHamachi   Kind = "hamachi"
	KindRadmin    Kind = "radmin"
	KindZeroTier  Kind = "zerotier"
	KindTailscale Kind = "tailscale"
	// KindLAN is the local network — the flat, the office.
	KindLAN Kind = "lan"
	// KindVirtual is an adapter a hypervisor or a container runtime made up:
	// WSL, Hyper-V, VirtualBox, docker0. It looks like a local network and is
	// not one — nobody else in the flat is on it.
	KindVirtual Kind = "virtual"
	// KindReserved is an address from a range that is not routed on the
	// internet at all. VPN clients hand these out, and calling one public
	// would send somebody to an address that can never answer.
	KindReserved Kind = "reserved"
	// KindLoopback is this machine and nothing else.
	KindLoopback Kind = "loopback"
)

// Address is one way to reach this machine.
type Address struct {
	IP   string `json:"ip"`
	Kind Kind   `json:"kind"`
	// Interface is the adapter's name, so an operator with two virtual
	// networks can tell which is which.
	Interface string `json:"interface"`
}

// adapterNames maps a substring of an adapter's name onto what it is.
//
// The name, not the address range, is what identifies these. Hamachi hands out
// addresses in 25.0.0.0/8 and Radmin in 26.0.0.0/8, and both of those are real
// public ranges that somebody genuinely holds — calling a public address
// "Hamachi" because it starts with 25 would be a confident lie.
// Virtual adapters are here for the opposite reason: their addresses are
// ordinary private ones, indistinguishable from the flat's network by range.
// "vEthernet (WSL)" holds 172.28.48.1 and nobody in the flat can reach it.
var adapterNames = map[string]Kind{
	"hamachi":    KindHamachi,
	"radmin":     KindRadmin,
	"zerotier":   KindZeroTier,
	"tailscale":  KindTailscale,
	"vethernet":  KindVirtual,
	"wsl":        KindVirtual,
	"hyper-v":    KindVirtual,
	"vmnet":      KindVirtual,
	"vmware":     KindVirtual,
	"virtualbox": KindVirtual,
	"host-only":  KindVirtual,
	"docker":     KindVirtual,
	"virbr":      KindVirtual,
}

// notRouted lists IPv4 ranges that exist but never travel the public internet.
//
// Without this the fallback below calls everything it does not recognise
// public — and a VPN client's tunnel adapter, which typically sits in
// 198.18.0.0/15, gets announced as "hand this to your friends".
//
// Private, loopback and link-local ranges are not repeated here; the standard
// library already answers for those.
var notRouted = []*net.IPNet{
	mustCIDR("0.0.0.0/8"),       // "this network"
	mustCIDR("192.0.0.0/24"),    // IETF protocol assignments
	mustCIDR("192.0.2.0/24"),    // TEST-NET-1
	mustCIDR("192.88.99.0/24"),  // 6to4 relay anycast, deprecated
	mustCIDR("198.18.0.0/15"),   // benchmarking, RFC 2544
	mustCIDR("198.51.100.0/24"), // TEST-NET-2
	mustCIDR("203.0.113.0/24"),  // TEST-NET-3
	mustCIDR("224.0.0.0/4"),     // multicast
	mustCIDR("240.0.0.0/4"),     // reserved, and 255.255.255.255 with it
}

func mustCIDR(s string) *net.IPNet {
	_, network, err := net.ParseCIDR(s)
	if err != nil {
		panic("netinfo: bad constant CIDR " + s + ": " + err.Error())
	}
	return network
}

// Addresses lists every address this machine answers on, newest question
// first: something reachable from the internet, then overlay networks, then
// the local one.
func Addresses() ([]Address, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var out []Address
	for _, iface := range interfaces {
		// An adapter that is down has no address anyone can use, and a
		// Hamachi that is installed but not connected is exactly that.
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			// IPv6 is left out on purpose: a Minecraft client takes an
			// address typed by a human, and a link-local IPv6 with a zone
			// suffix is not that.
			if ip.To4() == nil || ip.IsLinkLocalUnicast() {
				continue
			}

			out = append(out, Address{
				IP:        ip.String(),
				Kind:      classify(ip, iface.Name),
				Interface: iface.Name,
			})
		}
	}

	sortByUsefulness(out)
	return out, nil
}

// classify decides what a single address is.
func classify(ip net.IP, ifaceName string) Kind {
	if ip.IsLoopback() {
		return KindLoopback
	}

	lower := strings.ToLower(ifaceName)
	for fragment, kind := range adapterNames {
		if strings.Contains(lower, fragment) {
			return kind
		}
	}

	if ip.IsPrivate() {
		return KindLAN
	}
	// Carrier-grade NAT. Tailscale hands these out, and so does an internet
	// provider who has run out of addresses — in which case this machine is
	// not reachable from the internet either, and saying "public" would send
	// the operator hunting for a router setting that cannot help.
	if four := ip.To4(); four != nil && four[0] == 100 && four[1] >= 64 && four[1] <= 127 {
		return KindTailscale
	}
	// Public is decided by exclusion, so everything excluded has to be listed:
	// what is left over is handed to somebody's friends as "works for
	// everyone", and being wrong about that costs them an evening.
	for _, network := range notRouted {
		if network.Contains(ip) {
			return KindReserved
		}
	}
	return KindPublic
}

// order is how useful each kind is to somebody looking for an address to hand
// out.
var order = map[Kind]int{
	KindPublic:    0,
	KindHamachi:   1,
	KindRadmin:    2,
	KindZeroTier:  3,
	KindTailscale: 4,
	KindLAN:       5,
	KindVirtual:   6,
	KindReserved:  7,
	KindLoopback:  8,
}

func sortByUsefulness(addrs []Address) {
	// A short list; insertion sort keeps it stable without pulling in sort
	// for six elements.
	for i := 1; i < len(addrs); i++ {
		for j := i; j > 0 && order[addrs[j].Kind] < order[addrs[j-1].Kind]; j-- {
			addrs[j], addrs[j-1] = addrs[j-1], addrs[j]
		}
	}
}

// HasPublic reports whether any address is reachable from the internet at all.
//
// The distinction matters: a machine with no public address cannot be reached
// by forwarding a port, however long its owner spends in the router's
// settings. That is the moment to say "use Hamachi" rather than to let them
// find out over an evening.
func HasPublic(addrs []Address) bool {
	for _, addr := range addrs {
		if addr.Kind == KindPublic {
			return true
		}
	}
	return false
}
