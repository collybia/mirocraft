package netinfo

import (
	"net"
	"testing"
)

// Hamachi hands out 25.x and Radmin 26.x, and both of those ranges belong to
// somebody in the real internet. The adapter's name is what identifies them;
// the range is a coincidence that would otherwise make the panel call a
// genuine public address "Hamachi" and send its owner looking for a VPN they
// do not need.
func TestOverlaysAreIdentifiedByAdapterNotByRange(t *testing.T) {
	cases := []struct {
		ip    string
		iface string
		want  Kind
	}{
		{"25.34.12.9", "Hamachi", KindHamachi},
		{"26.108.4.2", "Radmin VPN", KindRadmin},
		{"10.147.20.5", "ZeroTier One [8056c2e21c]", KindZeroTier},
		{"100.83.12.4", "Tailscale", KindTailscale},

		// The same ranges on an ordinary adapter are what they look like.
		{"25.34.12.9", "Ethernet", KindPublic},
		{"26.108.4.2", "eth0", KindPublic},

		{"192.168.1.5", "Ethernet", KindLAN},
		{"10.0.0.4", "eth0", KindLAN},
		{"172.16.3.1", "Ethernet 2", KindLAN},
		{"127.0.0.1", "lo", KindLoopback},
		{"203.0.113.7", "eth0", KindPublic},

		// Carrier-grade NAT is not the internet, whatever the provider says.
		{"100.71.4.9", "eth0", KindTailscale},
	}

	for _, c := range cases {
		if got := classify(net.ParseIP(c.ip), c.iface); got != c.want {
			t.Errorf("%s on %q = %q, want %q", c.ip, c.iface, got, c.want)
		}
	}
}

// The first address in the list is the one to hand out, so the order is part
// of the answer rather than a detail.
func TestTheMostUsefulAddressComesFirst(t *testing.T) {
	addrs := []Address{
		{IP: "127.0.0.1", Kind: KindLoopback},
		{IP: "192.168.1.5", Kind: KindLAN},
		{IP: "25.34.12.9", Kind: KindHamachi},
		{IP: "203.0.113.7", Kind: KindPublic},
	}
	sortByUsefulness(addrs)

	want := []string{"203.0.113.7", "25.34.12.9", "192.168.1.5", "127.0.0.1"}
	for i, ip := range want {
		if addrs[i].IP != ip {
			t.Fatalf("order = %v, want %v", addrs, want)
		}
	}
}

// A machine with no public address cannot be reached by forwarding a port,
// however long its owner spends in the router's settings.
func TestHasPublic(t *testing.T) {
	behindNAT := []Address{
		{Kind: KindLAN}, {Kind: KindHamachi}, {Kind: KindLoopback},
	}
	if HasPublic(behindNAT) {
		t.Error("a machine behind NAT was reported as reachable from the internet")
	}
	if !HasPublic(append(behindNAT, Address{Kind: KindPublic})) {
		t.Error("a public address was not noticed")
	}
}

// Whatever this machine is, the answer has to be usable: an address a person
// can type into Minecraft.
func TestRealMachineGivesTypeableAddresses(t *testing.T) {
	addrs, err := Addresses()
	if err != nil {
		t.Fatalf("Addresses: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("no addresses at all, and this machine is running a test over some network")
	}

	for _, addr := range addrs {
		ip := net.ParseIP(addr.IP)
		if ip == nil || ip.To4() == nil {
			t.Errorf("%q is not something anyone can type into a game", addr.IP)
		}
	}
}
