//go:build live

package upnp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Against whatever router this machine is behind.
//
// Tagged `live` and not part of the ordinary suite: it needs a home network,
// and on a datacentre machine or a CI runner there is no gateway to find. The
// protocol parsing is covered by the tests beside this one; what this covers
// is the half that only a real router can answer — that the search reaches it,
// that its description parses, and that it will say what its external address
// is.
//
//	go test -tags live -run TestLive -v ./internal/upnp/
func TestLiveRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	router, err := Discover(ctx)
	if errors.Is(err, ErrNoRouter) {
		t.Skip("no UPnP router on this network, which is a normal thing to be")
	}
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	t.Logf("service %s at %s, this machine is %s",
		router.ServiceType, router.ControlURL, router.LocalIP)

	external, err := router.ExternalIP(ctx)
	if err != nil {
		t.Fatalf("ExternalIP: %v", err)
	}
	t.Logf("the internet sees %s", external)

	// A port nobody uses, forwarded and taken away again, so a live run leaves
	// the router exactly as it found it.
	const port = 25599
	if err := router.Forward(ctx, port, false, "Mirocraft live test"); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	mapping, found, err := router.Lookup(ctx, port, false)
	if err != nil || !found {
		t.Errorf("the mapping just made was not found: %v, found=%v", err, found)
	} else if mapping.InternalClient != router.LocalIP {
		t.Errorf("forwarded to %s, want %s", mapping.InternalClient, router.LocalIP)
	}

	if err := router.Remove(ctx, port, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, found, _ := router.Lookup(ctx, port, false); found {
		t.Error("the mapping survived being removed")
	}
}
