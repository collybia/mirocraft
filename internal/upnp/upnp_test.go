package upnp

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeaderValueIsCaseInsensitive(t *testing.T) {
	response := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\n" +
		"Location: http://192.168.1.1:5000/rootDesc.xml\r\nST: upnp:rootdevice\r\n\r\n"

	if got := headerValue(response, "location"); got != "http://192.168.1.1:5000/rootDesc.xml" {
		t.Errorf("location = %q", got)
	}
	// Routers differ on capitalisation, and a case-sensitive read finds
	// nothing on half of them.
	if got := headerValue(response, "LOCATION"); got == "" {
		t.Error("LOCATION was not found, so the read is case-sensitive")
	}
	if got := headerValue(response, "server"); got != "" {
		t.Errorf("a header that is not there returned %q", got)
	}
}

// The service that forwards ports is nested a device or two down, and which
// urn it carries depends on whether the uplink is PPPoE.
func TestTheConnectionServiceIsFoundInNestedDevices(t *testing.T) {
	const description = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
        <controlURL>/ctl/L3F</controlURL>
      </service>
    </serviceList>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
                <controlURL>/ctl/IPConn</controlURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`

	var parsed root
	if err := xml.Unmarshal([]byte(description), &parsed); err != nil {
		t.Fatalf("parsing: %v", err)
	}

	serviceType, controlURL, ok := findService(parsed.Device)
	if !ok {
		t.Fatal("the connection service was not found")
	}
	if serviceType != "urn:schemas-upnp-org:service:WANIPConnection:1" || controlURL != "/ctl/IPConn" {
		t.Errorf("found %q at %q", serviceType, controlURL)
	}
}

// A router that answers a search but cannot forward anything is not the router
// we are looking for.
func TestADeviceThatCannotForwardIsRejected(t *testing.T) {
	const printer = `<?xml version="1.0"?><root><device>
	  <serviceList><service>
	    <serviceType>urn:schemas-upnp-org:service:PrintBasic:1</serviceType>
	    <controlURL>/print</controlURL>
	  </service></serviceList>
	</device></root>`

	var parsed root
	if err := xml.Unmarshal([]byte(printer), &parsed); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, _, ok := findService(parsed.Device); ok {
		t.Error("a printer was accepted as a gateway")
	}
}

// What the router is actually asked, and in what shape. A malformed request is
// answered with a fault that says only that something was wrong.
func TestForwardSendsTheExpectedSoap(t *testing.T) {
	var body, action string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		body, action = string(raw), r.Header.Get("SOAPAction")
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body>
		  <u:AddPortMappingResponse/></s:Body></s:Envelope>`))
	}))
	defer server.Close()

	router := &Router{
		ControlURL:  server.URL,
		ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1",
		LocalIP:     "192.168.1.5",
		client:      server.Client(),
	}

	if err := router.Forward(context.Background(), 25565, false, "Mirocraft"); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if !strings.Contains(action, "#AddPortMapping") {
		t.Errorf("SOAPAction = %q", action)
	}
	for _, want := range []string{
		"<NewExternalPort>25565</NewExternalPort>",
		"<NewInternalPort>25565</NewInternalPort>",
		"<NewProtocol>TCP</NewProtocol>",
		"<NewInternalClient>192.168.1.5</NewInternalClient>",
		"<NewEnabled>1</NewEnabled>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the request has no %s:\n%s", want, body)
		}
	}
}

// A Bedrock port is UDP, and forwarding TCP instead would leave the panel
// reporting success and nobody able to join.
func TestUDPIsSentAsUDP(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		body = string(raw)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body/></s:Envelope>`))
	}))
	defer server.Close()

	router := &Router{ControlURL: server.URL, ServiceType: "x", LocalIP: "10.0.0.2", client: server.Client()}
	if err := router.Forward(context.Background(), 19132, true, "Mirocraft"); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if !strings.Contains(body, "<NewProtocol>UDP</NewProtocol>") {
		t.Errorf("protocol was not UDP:\n%s", body)
	}
}

// The router's refusal has a reason in it, and passing on the status code
// alone would throw away the only useful part.
func TestARefusalCarriesTheRoutersReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body><s:Fault>
		  <detail><UPnPError><errorCode>718</errorCode>
		  <errorDescription>ConflictInMappingEntry</errorDescription>
		  </UPnPError></detail></s:Fault></s:Body></s:Envelope>`))
	}))
	defer server.Close()

	router := &Router{ControlURL: server.URL, ServiceType: "x", LocalIP: "10.0.0.2", client: server.Client()}
	err := router.Forward(context.Background(), 25565, false, "Mirocraft")

	if err == nil {
		t.Fatal("a refusal was reported as success")
	}
	if !strings.Contains(err.Error(), "ConflictInMappingEntry") || !strings.Contains(err.Error(), "718") {
		t.Errorf("error = %v, and the router's reason is missing from it", err)
	}
}

// "Is this port forwarded?" answered with no is an answer, not a failure: the
// router says so with a fault, and treating that as an error would make every
// unforwarded port look like a broken router.
func TestAnAbsentMappingIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body><s:Fault>
		  <detail><UPnPError><errorCode>714</errorCode>
		  <errorDescription>NoSuchEntryInArray</errorDescription>
		  </UPnPError></detail></s:Fault></s:Body></s:Envelope>`))
	}))
	defer server.Close()

	router := &Router{ControlURL: server.URL, ServiceType: "x", LocalIP: "10.0.0.2", client: server.Client()}
	_, found, err := router.Lookup(context.Background(), 25565, false)

	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found {
		t.Error("a port that is not forwarded was reported as forwarded")
	}
}

// A mapping made by something else must be visible as such, so the panel does
// not quietly take a port a games console was using.
func TestAnExistingMappingIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><s:Envelope><s:Body>
		  <u:GetSpecificPortMappingEntryResponse>
		    <NewInternalPort>25565</NewInternalPort>
		    <NewInternalClient>192.168.1.9</NewInternalClient>
		    <NewEnabled>1</NewEnabled>
		  </u:GetSpecificPortMappingEntryResponse></s:Body></s:Envelope>`))
	}))
	defer server.Close()

	router := &Router{ControlURL: server.URL, ServiceType: "x", LocalIP: "192.168.1.5", client: server.Client()}
	mapping, found, err := router.Lookup(context.Background(), 25565, false)

	if err != nil || !found {
		t.Fatalf("Lookup: %v, found=%v", err, found)
	}
	if mapping.InternalClient != "192.168.1.9" || mapping.InternalPort != 25565 || !mapping.Enabled {
		t.Errorf("mapping = %+v", mapping)
	}
}
