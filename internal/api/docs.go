package api

import (
	_ "embed"
	"net/http"
	"strings"
	"time"
)

// The OpenAPI description of this API, and the interactive documentation that
// renders it.
//
// Both are served from inside the binary. A docs page that only works when the
// machine can reach a CDN is the kind of dependency the single-binary rule
// exists to avoid, and a page that pulls executable JavaScript from a third
// party is one more party able to run code in an authenticated admin's
// browser.
var (
	//go:embed openapi.yaml
	openAPISpec []byte

	//go:embed docsui/swagger-ui-bundle.js
	swaggerBundle []byte

	//go:embed docsui/swagger-ui.css
	swaggerCSS []byte
)

// startedAt gives the embedded assets a modification time, so a browser can
// cache them across a page load rather than refetching 1.7 MB each time.
var startedAt = time.Now()

// handleOpenAPISpec serves GET /openapi.yaml.
func (a *API) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	http.ServeContent(w, r, "openapi.yaml", startedAt, strings.NewReader(string(openAPISpec)))
}

// handleDocs serves GET /docs and the two assets under it.
//
// One handler rather than three routes: Go's mux would need a wildcard pattern
// for the assets anyway, and keeping them together makes it obvious that the
// page and its assets are one thing.
func (a *API) handleDocs(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/swagger-ui-bundle.js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		http.ServeContent(w, r, "swagger-ui-bundle.js", startedAt, strings.NewReader(string(swaggerBundle)))

	case strings.HasSuffix(r.URL.Path, "/swagger-ui.css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		http.ServeContent(w, r, "swagger-ui.css", startedAt, strings.NewReader(string(swaggerCSS)))

	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Nothing here is loaded from anywhere else, so the page can say so.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		_, _ = w.Write([]byte(docsPage))
	}
}

// docsPage is the Swagger UI shell.
//
// Written here rather than vendored because the stock initializer points at
// the petstore example, and because the page has to work under the panel's own
// path prefix.
const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mirocraft API</title>
<link rel="stylesheet" href="/api/v1/docs/swagger-ui.css">
<style>
  body { margin: 0; background: #fafafa; }
  .swagger-ui .topbar { display: none; }
</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="/api/v1/docs/swagger-ui-bundle.js"></script>
<script>
  window.ui = SwaggerUIBundle({
    url: "/api/v1/openapi.yaml",
    dom_id: "#swagger-ui",
    deepLinking: true,
    // Operations grouped by tag, collapsed: sixty-odd endpoints expanded at
    // once is a wall of text nobody reads.
    docExpansion: "none",
    defaultModelsExpandDepth: 0,
    persistAuthorization: true,
    presets: [SwaggerUIBundle.presets.apis],
  });
</script>
</body>
</html>
`
