// Package upnp asks a home router to forward a port.
//
// It exists for one situation: the panel is on somebody's own machine, behind
// a router, and their friends are on the internet. Without a forwarded port
// the only ways in are an overlay network like Hamachi — which every friend
// has to install — or a tunnel through somebody else's service. A router that
// speaks UPnP can simply be asked, and most home routers do.
//
// Nothing here happens on its own. Punching a hole through a router changes a
// device the whole household shares, which is a different matter from adding a
// rule on the machine the panel was installed on, and it is not a decision to
// make on somebody's behalf.
package upnp

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Discovery settings.
const (
	// searchTimeout bounds the wait for routers to answer. They answer in
	// milliseconds when they are there; the wait is for the ones that are not.
	searchTimeout = 3 * time.Second
	// requestTimeout bounds one SOAP call.
	requestTimeout = 10 * time.Second
	// leaseSeconds asks for a permanent mapping. Many routers refuse a lease
	// and accept zero, which means "until something removes it".
	leaseSeconds = 0
)

// Errors callers distinguish.
var (
	// ErrNoRouter means nothing answered the search. Either there is no UPnP
	// router, or it is switched off — a common and deliberate setting.
	ErrNoRouter = errors.New("no router answered the UPnP search")
	// ErrRefused means the router answered and said no.
	ErrRefused = errors.New("the router refused the request")
)

// The services that can forward a port. A router offers one of them; which
// one depends on whether its uplink is PPPoE.
var serviceTypes = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// Router is a discovered gateway.
type Router struct {
	// ControlURL is where the SOAP calls go.
	ControlURL string
	// ServiceType is the urn the router answered with; every call has to
	// repeat it back.
	ServiceType string
	// LocalIP is this machine's address on the router's network, which is
	// what a port has to be forwarded to.
	LocalIP string

	client *http.Client
}

// Discover finds the router on this network.
func Discover(ctx context.Context) (*Router, error) {
	locations, err := search(ctx)
	if err != nil {
		return nil, err
	}
	if len(locations) == 0 {
		return nil, ErrNoRouter
	}

	client := &http.Client{Timeout: requestTimeout}
	for _, location := range locations {
		router, err := describe(ctx, client, location)
		if err != nil {
			continue
		}
		return router, nil
	}
	return nil, ErrNoRouter
}

// search sends an M-SEARCH and collects the addresses of the descriptions
// that answer.
func search(ctx context.Context) ([]string, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("opening a socket for the search: %w", err)
	}
	defer func() { _ = conn.Close() }()

	target := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	request := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"

	// Sent more than once: SSDP is UDP, and a single lost packet would look
	// exactly like a household with no UPnP router in it.
	for i := 0; i < 3; i++ {
		if _, err := conn.WriteTo([]byte(request), target); err != nil {
			return nil, fmt.Errorf("sending the search: %w", err)
		}
	}

	deadline := time.Now().Add(searchTimeout)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var locations []string
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // the deadline, which is how this loop is meant to end
		}
		if location := headerValue(string(buf[:n]), "location"); location != "" && !seen[location] {
			seen[location] = true
			locations = append(locations, location)
		}
	}
	return locations, nil
}

// headerValue reads one header out of an SSDP response.
func headerValue(response, name string) string {
	for _, line := range strings.Split(response, "\r\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// device is the part of a UPnP description this package reads.
type device struct {
	Services []struct {
		Type       string `xml:"serviceType"`
		ControlURL string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []device `xml:"deviceList>device"`
}

type root struct {
	Device device `xml:"device"`
}

// describe fetches a router's description and finds the service that forwards
// ports, which is nested a device or two down.
func describe(ctx context.Context, client *http.Client, location string) (*Router, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var parsed root
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	serviceType, controlURL, ok := findService(parsed.Device)
	if !ok {
		return nil, errors.New("this device does not forward ports")
	}

	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	control, err := base.Parse(controlURL)
	if err != nil {
		return nil, err
	}

	localIP, err := localAddressTowards(base.Hostname())
	if err != nil {
		return nil, err
	}

	return &Router{
		ControlURL:  control.String(),
		ServiceType: serviceType,
		LocalIP:     localIP,
		client:      client,
	}, nil
}

// findService walks the device tree looking for a connection service.
func findService(d device) (serviceType, controlURL string, ok bool) {
	for _, wanted := range serviceTypes {
		for _, service := range d.Services {
			if service.Type == wanted && service.ControlURL != "" {
				return service.Type, service.ControlURL, true
			}
		}
	}
	for _, child := range d.Devices {
		if t, u, found := findService(child); found {
			return t, u, true
		}
	}
	return "", "", false
}

// localAddressTowards finds this machine's address on the router's network.
//
// A port is forwarded to an address, and a machine with a Hamachi adapter and
// two Ethernet cards has several. The one that matters is the one packets to
// the router leave from, which the routing table already knows — asking it
// costs no traffic, because UDP connects nothing.
func localAddressTowards(host string) (string, error) {
	conn, err := net.Dial("udp4", net.JoinHostPort(host, "1900"))
	if err != nil {
		return "", fmt.Errorf("working out this machine's address: %w", err)
	}
	defer func() { _ = conn.Close() }()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", errors.New("could not read this machine's address")
	}
	return addr.IP.String(), nil
}

// ExternalIP asks the router for the address the internet sees.
func (r *Router) ExternalIP(ctx context.Context) (string, error) {
	var reply struct {
		IP string `xml:"Body>GetExternalIPAddressResponse>NewExternalIPAddress"`
	}
	if err := r.call(ctx, "GetExternalIPAddress", nil, &reply); err != nil {
		return "", err
	}
	if reply.IP == "" {
		return "", errors.New("the router did not report an external address")
	}
	return reply.IP, nil
}

// Forward asks the router to send a port here.
func (r *Router) Forward(ctx context.Context, port int, udp bool, description string) error {
	args := []arg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(port)},
		{"NewProtocol", protocol(udp)},
		{"NewInternalPort", strconv.Itoa(port)},
		{"NewInternalClient", r.LocalIP},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", description},
		{"NewLeaseDuration", strconv.Itoa(leaseSeconds)},
	}
	return r.call(ctx, "AddPortMapping", args, nil)
}

// Remove takes the mapping away again.
func (r *Router) Remove(ctx context.Context, port int, udp bool) error {
	args := []arg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(port)},
		{"NewProtocol", protocol(udp)},
	}
	return r.call(ctx, "DeletePortMapping", args, nil)
}

// Mapping is one forwarding rule as the router reports it.
type Mapping struct {
	InternalClient string
	InternalPort   int
	Enabled        bool
}

// Lookup asks whether a port is already forwarded, and to whom.
//
// Worth asking rather than assuming: a mapping made by this panel and one made
// by a games console look the same from here, and overwriting somebody else's
// is how a household loses something that was working.
func (r *Router) Lookup(ctx context.Context, port int, udp bool) (Mapping, bool, error) {
	var reply struct {
		Client  string `xml:"Body>GetSpecificPortMappingEntryResponse>NewInternalClient"`
		Port    string `xml:"Body>GetSpecificPortMappingEntryResponse>NewInternalPort"`
		Enabled string `xml:"Body>GetSpecificPortMappingEntryResponse>NewEnabled"`
	}
	args := []arg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(port)},
		{"NewProtocol", protocol(udp)},
	}
	if err := r.call(ctx, "GetSpecificPortMappingEntry", args, &reply); err != nil {
		// "NoSuchEntryInArray" is the answer to "is this forwarded?" being no.
		return Mapping{}, false, nil //nolint:nilerr // absence is an answer, not a failure
	}

	internal, _ := strconv.Atoi(reply.Port)
	return Mapping{
		InternalClient: reply.Client,
		InternalPort:   internal,
		Enabled:        reply.Enabled == "1",
	}, reply.Client != "", nil
}

func protocol(udp bool) string {
	if udp {
		return "UDP"
	}
	return "TCP"
}

type arg struct{ name, value string }

// call performs one SOAP request.
func (r *Router) call(ctx context.Context, action string, args []arg, reply any) error {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u="%s">`, action, r.ServiceType)
	for _, a := range args {
		fmt.Fprintf(&body, "<%s>%s</%s>", a.name, xmlEscape(a.value), a.name)
	}
	fmt.Fprintf(&body, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ControlURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+r.ServiceType+`#`+action+`"`)

	// A Router built by hand — pointed at a known gateway, or at a stand-in
	// in a test — carries no client of its own.
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s: %s", ErrRefused, action, soapError(payload))
	}
	if reply == nil {
		return nil
	}
	return xml.Unmarshal(payload, reply)
}

// soapError digs the human-readable half out of a SOAP fault, because the
// status code alone says only that something went wrong.
func soapError(payload []byte) string {
	var fault struct {
		Description string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
		Code        string `xml:"Body>Fault>detail>UPnPError>errorCode"`
	}
	if err := xml.Unmarshal(payload, &fault); err == nil && fault.Description != "" {
		return fault.Description + " (" + fault.Code + ")"
	}
	return strings.TrimSpace(string(payload))
}

func xmlEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}
