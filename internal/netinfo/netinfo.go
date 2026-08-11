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
var adapterNames = map[string]Kind{
	"hamachi":   KindHamachi,
	"radmin":    KindRadmin,
	"zerotier":  KindZeroTier,
	"tailscale": KindTailscale,
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
	KindLoopback:  6,
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
