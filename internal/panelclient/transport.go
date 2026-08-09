package panelclient

import (
	"crypto/tls"
	"net/http"
	"net/url"
)

// tlsConfigOf and proxyOf lift the settings from the client's transport so the
// WebSocket dialer behaves like the rest of the client.
//
// Without them a bot configured with a custom certificate pool — which is
// every bot talking to a default install, since that panel serves a
// self-signed certificate — would work for every REST call and fail on the
// console, which is the confusing half-broken state worth avoiding.

func tlsConfigOf(c *http.Client) *tls.Config {
	transport, ok := c.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return nil
	}
	return transport.TLSClientConfig.Clone()
}

func proxyOf(c *http.Client) func(*http.Request) (*url.URL, error) {
	transport, ok := c.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		return http.ProxyFromEnvironment
	}
	return transport.Proxy
}
