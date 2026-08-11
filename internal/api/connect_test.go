package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/netinfo"
	"github.com/collybia/mirocraft/internal/upnp"
)

// withRouter gives the environment a gateway that answers however the test
// wants, without a router being on the network the suite runs on.
func withRouter(t *testing.T, env *testEnv, discover func(context.Context) (*upnp.Router, error)) {
	t.Helper()
	env.api.routers = &routerCache{now: time.Now, discover: discover}
}

// behindNAT makes the environment look like a machine in somebody's flat,
// whatever the machine running the suite actually is.
func behindNAT(t *testing.T, env *testEnv) {
	t.Helper()
	env.api.addresses = func() ([]netinfo.Address, error) {
		return []netinfo.Address{
			{IP: "192.168.1.5", Kind: netinfo.KindLAN, Interface: "Ethernet"},
			{IP: "25.34.12.9", Kind: netinfo.KindHamachi, Interface: "Hamachi"},
			{IP: "127.0.0.1", Kind: netinfo.KindLoopback, Interface: "lo"},
		}, nil
	}
}

// The question this endpoint exists for. A machine hosting for friends has
// several addresses and nothing to say which is which.
func TestConnectListsAddressesMostUsefulFirst(t *testing.T) {
	env := newTestEnv(t)

	body := decodeJSON[connectResponse](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/connect", nil, env.token))

	if body.Port != 25565 {
		t.Errorf("port = %d", body.Port)
	}
	if len(body.Addresses) == 0 {
		t.Fatal("no addresses at all, and this machine is on some network")
	}

	// Every line has to be something a person can type into the game.
	for _, addr := range body.Addresses {
		if addr.Address == "" || addr.IP == "" || addr.Kind == "" {
			t.Errorf("incomplete address: %+v", addr)
		}
	}

	// Loopback is the least useful answer, so it must never be the first one.
	if body.Addresses[0].Kind == netinfo.KindLoopback && len(body.Addresses) > 1 {
		t.Errorf("loopback came first: %+v", body.Addresses)
	}
}

// Behind a router that has never been asked, the panel should say it can ask —
// that is the difference between a dead end and a button.
func TestConnectReportsARouterItCanAsk(t *testing.T) {
	env := newTestEnv(t)
	behindNAT(t, env)
	router, calls := fakeRouter(t, "203.0.113.7", nil)
	withRouter(t, env, func(context.Context) (*upnp.Router, error) { return router, nil })

	body := decodeJSON[connectResponse](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/connect", nil, env.token))

	if body.Internet.State != stateCanForward {
		t.Fatalf("state = %q, want can_forward (calls: %v)", body.Internet.State, *calls)
	}
	if body.Internet.ExternalIP != "203.0.113.7" {
		t.Errorf("external ip = %q", body.Internet.ExternalIP)
	}
}

// The most valuable thing the panel can say to somebody on a shared provider
// address: no router setting will help you, use an overlay network.
func TestCarrierNATIsNamed(t *testing.T) {
	env := newTestEnv(t)
	behindNAT(t, env)
	router, _ := fakeRouter(t, "100.71.4.9", nil)
	withRouter(t, env, func(context.Context) (*upnp.Router, error) { return router, nil })

	body := decodeJSON[connectResponse](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/connect", nil, env.token))

	if body.Internet.State != stateCarrierNAT {
		t.Fatalf("state = %q, want carrier_nat", body.Internet.State)
	}
}

// Nothing answered the search, and that is a normal thing to be — a router
// with UPnP switched off is a deliberate setting, not a fault.
func TestNoRouterIsNotAnError(t *testing.T) {
	env := newTestEnv(t)
	behindNAT(t, env)
	withRouter(t, env, func(context.Context) (*upnp.Router, error) { return nil, upnp.ErrNoRouter })

	resp := env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/connect", nil, env.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON[connectResponse](t, resp)
	if body.Internet.State != stateNoRouter {
		t.Errorf("state = %q", body.Internet.State)
	}
}

// Forwarding needs servers:write, because it changes a device outside this
// machine.
func TestForwardingNeedsWriteAccess(t *testing.T) {
	env := newTestEnv(t)
	withRouter(t, env, func(context.Context) (*upnp.Router, error) { return nil, upnp.ErrNoRouter })

	readOnly := env.mintToken(env.user.ID, []string{ScopeServersRead})
	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/connect/forward", nil, readOnly)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// A port the router already sends somewhere else belongs to whatever is
// there — a console, another PC — and taking it would break that silently.
func TestAPortHeldByAnotherMachineIsNotTaken(t *testing.T) {
	env := newTestEnv(t)
	behindNAT(t, env)
	router, _ := fakeRouter(t, "203.0.113.7", &upnp.Mapping{
		InternalClient: "192.168.1.9", InternalPort: 25565, Enabled: true,
	})
	withRouter(t, env, func(context.Context) (*upnp.Router, error) { return router, nil })

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/connect/forward", nil, token)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "port_in_use" {
		t.Errorf("code = %q", code)
	}
}

// With forwarding switched off the endpoints say so rather than searching the
// network on every look.
func TestForwardingCanBeSwitchedOff(t *testing.T) {
	env := newTestEnv(t)
	env.api.routers = nil

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/connect/forward", nil, token)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestSharedAddressesAreRecognised(t *testing.T) {
	for _, ip := range []string{"192.168.1.1", "10.0.0.1", "172.20.0.1", "100.71.4.9", "127.0.0.1"} {
		if !isSharedAddress(ip) {
			t.Errorf("%s was treated as reachable from the internet", ip)
		}
	}
	for _, ip := range []string{"203.0.113.7", "8.8.8.8", "25.34.12.9"} {
		if isSharedAddress(ip) {
			t.Errorf("%s was treated as shared", ip)
		}
	}
}

// fakeRouter is a gateway that answers SOAP however the test needs, so the
// endpoint can be exercised on a machine with no UPnP router — which is most
// machines, including every CI runner.
func fakeRouter(t *testing.T, externalIP string, existing *upnp.Mapping) (*upnp.Router, *[]string) {
	t.Helper()

	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Header.Get("SOAPAction")
		actions = append(actions, action)
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)

		switch {
		case strings.Contains(action, "GetExternalIPAddress"):
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope><s:Body>
			  <u:GetExternalIPAddressResponse><NewExternalIPAddress>%s</NewExternalIPAddress>
			  </u:GetExternalIPAddressResponse></s:Body></s:Envelope>`, externalIP)

		case strings.Contains(action, "GetSpecificPortMappingEntry"):
			if existing == nil {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body><s:Fault><detail>
				  <UPnPError><errorCode>714</errorCode>
				  <errorDescription>NoSuchEntryInArray</errorDescription></UPnPError>
				  </detail></s:Fault></s:Body></s:Envelope>`))
				return
			}
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope><s:Body>
			  <u:GetSpecificPortMappingEntryResponse>
			    <NewInternalPort>%d</NewInternalPort>
			    <NewInternalClient>%s</NewInternalClient>
			    <NewEnabled>1</NewEnabled>
			  </u:GetSpecificPortMappingEntryResponse></s:Body></s:Envelope>`,
				existing.InternalPort, existing.InternalClient)

		default:
			_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body/></s:Envelope>`))
		}
	}))
	t.Cleanup(server.Close)

	return &upnp.Router{
		ControlURL:  server.URL,
		ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1",
		LocalIP:     "192.168.1.5",
	}, &actions
}
