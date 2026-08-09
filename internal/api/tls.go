package api

import (
	"net/http"

	"github.com/collybia/mirocraft/internal/certs"
)

// CertStatus reports the certificate the panel is served with.
//
// An interface rather than *certs.Manager so the API can be built without one:
// a panel behind a reverse proxy that terminates TLS serves plain HTTP, which
// is a supported install rather than a broken one.
type CertStatus interface {
	Status() certs.Status
}

// handleTLSStatus serves GET /tls.
//
// The architecture calls for a warning in the panel when the certificate is
// self-signed, and this is what carries it: a browser's own warning explains
// nothing about which of three modes the operator ended up in.
func (a *API) handleTLSStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireScope(w, r, ScopeServersRead); !ok {
		return
	}

	if a.certs == nil {
		writeJSON(w, http.StatusOK, certs.Status{Mode: certs.ModeOff})
		return
	}
	writeJSON(w, http.StatusOK, a.certs.Status())
}
