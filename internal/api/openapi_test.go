package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specDocument is only as much of OpenAPI as the contract test needs.
type specDocument struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Servers []struct {
		URL string `yaml:"url"`
	} `yaml:"servers"`
	Tags []struct {
		Name string `yaml:"name"`
	} `yaml:"tags"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components struct {
		Schemas    map[string]yaml.Node `yaml:"schemas"`
		Responses  map[string]yaml.Node `yaml:"responses"`
		Parameters map[string]yaml.Node `yaml:"parameters"`
	} `yaml:"components"`
}

type operation struct {
	Summary   string               `yaml:"summary"`
	Tags      []string             `yaml:"tags"`
	Responses map[string]yaml.Node `yaml:"responses"`
}

// httpMethods are the keys in a path item that are operations rather than
// metadata such as "parameters" or "summary".
var httpMethods = map[string]string{
	"get": http.MethodGet, "post": http.MethodPost, "put": http.MethodPut,
	"patch": http.MethodPatch, "delete": http.MethodDelete, "head": http.MethodHead,
	"options": http.MethodOptions,
}

func loadSpec(t *testing.T) specDocument {
	t.Helper()

	var doc specDocument
	if err := yaml.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	return doc
}

// specRoutes returns the spec's operations as router-shaped keys.
func specRoutes(t *testing.T, doc specDocument) map[string]string {
	t.Helper()

	const prefix = "/api/v1"
	out := make(map[string]string)

	for path, item := range doc.Paths {
		for verb, op := range item {
			method, ok := httpMethods[verb]
			if !ok {
				t.Errorf("openapi.yaml: %s has an unexpected key %q", path, verb)
				continue
			}
			out[method+" "+prefix+path] = op.Summary
		}
	}
	return out
}

// The point of the spec is that it describes this API. One that nobody checks
// drifts from the code within a release, so the check is a test rather than a
// habit.
func TestOpenAPIMatchesTheRouter(t *testing.T) {
	env := newTestEnv(t)

	doc := loadSpec(t)
	fromSpec := specRoutes(t, doc)

	fromRouter := make(map[string]struct{})
	for _, route := range env.api.AllRoutes() {
		fromRouter[route] = struct{}{}
	}

	var undocumented, invented []string
	for route := range fromRouter {
		if _, ok := fromSpec[route]; !ok {
			undocumented = append(undocumented, route)
		}
	}
	for route := range fromSpec {
		if _, ok := fromRouter[route]; !ok {
			invented = append(invented, route)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(invented)

	for _, route := range undocumented {
		t.Errorf("the router serves %q but openapi.yaml does not describe it", route)
	}
	for _, route := range invented {
		t.Errorf("openapi.yaml describes %q but the router does not serve it", route)
	}

	if len(fromRouter) == 0 {
		t.Fatal("the router reported no routes at all")
	}
	t.Logf("%d routes, all described", len(fromRouter))
}

// A path parameter the router names {sid} and the spec names {scheduleId} is
// a spec that reads correctly and matches nothing.
func TestOpenAPIPathParametersMatchTheRouter(t *testing.T) {
	env := newTestEnv(t)
	doc := loadSpec(t)

	routerPaths := make(map[string]struct{})
	for _, route := range env.api.AllRoutes() {
		_, path, _ := strings.Cut(route, " ")
		routerPaths[path] = struct{}{}
	}

	for path := range doc.Paths {
		full := "/api/v1" + path
		if _, ok := routerPaths[full]; !ok {
			t.Errorf("openapi.yaml path %q does not match any router pattern; "+
				"a renamed path parameter reads correctly and matches nothing", path)
		}
	}
}

// Every operation needs a summary and a tag: Swagger UI groups by tag and
// lists by summary, and an operation missing either is one a reader cannot
// find.
func TestOpenAPIOperationsAreDescribed(t *testing.T) {
	doc := loadSpec(t)

	known := make(map[string]struct{}, len(doc.Tags))
	for _, tag := range doc.Tags {
		known[tag.Name] = struct{}{}
	}

	for path, item := range doc.Paths {
		for verb, op := range item {
			where := strings.ToUpper(verb) + " " + path

			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("%s has no summary", where)
			}
			if len(op.Tags) == 0 {
				t.Errorf("%s has no tag, so it lands in an unnamed group", where)
			}
			for _, tag := range op.Tags {
				if _, ok := known[tag]; !ok {
					t.Errorf("%s uses the tag %q, which is not declared in tags:", where, tag)
				}
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s documents no responses", where)
			}
		}
	}
}

// A $ref pointing at a component that does not exist renders as an error in
// the documentation and is easy to introduce by renaming one.
func TestOpenAPIReferencesResolve(t *testing.T) {
	doc := loadSpec(t)

	present := map[string]map[string]struct{}{
		"schemas":    keySet(doc.Components.Schemas),
		"responses":  keySet(doc.Components.Responses),
		"parameters": keySet(doc.Components.Parameters),
	}

	var tree any
	if err := yaml.Unmarshal(openAPISpec, &tree); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}

	for _, ref := range findRefs(tree) {
		const prefix = "#/components/"
		if !strings.HasPrefix(ref, prefix) {
			t.Errorf("$ref %q is not a local component reference", ref)
			continue
		}
		section, name, ok := strings.Cut(strings.TrimPrefix(ref, prefix), "/")
		if !ok {
			t.Errorf("$ref %q is malformed", ref)
			continue
		}
		if _, ok := present[section][name]; !ok {
			t.Errorf("$ref %q points at a component that does not exist", ref)
		}
	}
}

func keySet(m map[string]yaml.Node) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// findRefs walks the decoded document and collects every $ref value.
//
// Walking rather than scanning the text: the file mixes block and flow style,
// so a line-based reader has to strip quotes and stray closing braces, and
// gets it subtly wrong on exactly the lines that matter.
func findRefs(node any) []string {
	var out []string

	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				if ref, ok := child.(string); ok {
					out = append(out, ref)
				}
				continue
			}
			out = append(out, findRefs(child)...)
		}
	case []any:
		for _, child := range value {
			out = append(out, findRefs(child)...)
		}
	}
	return out
}

func TestOpenAPIHeaderIsSane(t *testing.T) {
	doc := loadSpec(t)

	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi = %q", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		t.Error("info needs a title and a version")
	}
	// The spec is served from the same daemon it describes, so a relative
	// server URL keeps "try it out" pointed at whichever host loaded the page.
	if len(doc.Servers) == 0 || doc.Servers[0].URL != "/api/v1" {
		t.Errorf("servers = %+v, want the relative /api/v1", doc.Servers)
	}
}

// --- the served endpoints ---

func TestSpecAndDocsAreServedWithoutCredentials(t *testing.T) {
	env := newTestEnv(t)

	spec := env.do(http.MethodGet, "/api/v1/openapi.yaml", nil, "")
	defer func() { _ = spec.Body.Close() }()
	if spec.StatusCode != http.StatusOK {
		t.Fatalf("openapi.yaml: status %d", spec.StatusCode)
	}
	if ct := spec.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("openapi.yaml content-type = %q", ct)
	}

	page := env.do(http.MethodGet, "/api/v1/docs", nil, "")
	defer func() { _ = page.Body.Close() }()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("docs: status %d", page.StatusCode)
	}
	if ct := page.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("docs content-type = %q", ct)
	}
}

// The documentation must not need the internet: an operator copies one file
// onto a VPS, and a page that only renders with a CDN reachable is exactly the
// dependency the single-binary rule exists to avoid.
func TestDocsAssetsAreServedFromTheBinary(t *testing.T) {
	env := newTestEnv(t)

	for _, asset := range []struct{ path, contentType string }{
		{"/api/v1/docs/swagger-ui-bundle.js", "javascript"},
		{"/api/v1/docs/swagger-ui.css", "css"},
	} {
		resp := env.do(http.MethodGet, asset.path, nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", asset.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, asset.contentType) {
			t.Errorf("%s: content-type = %q", asset.path, ct)
		}
		_ = resp.Body.Close()
	}

	page := env.do(http.MethodGet, "/api/v1/docs", nil, "")
	defer func() { _ = page.Body.Close() }()

	body := make([]byte, 4096)
	n, _ := page.Body.Read(body)
	html := string(body[:n])

	for _, host := range []string{"//unpkg.com", "//cdn.", "https://cdn", "jsdelivr"} {
		if strings.Contains(html, host) {
			t.Errorf("the docs page loads something from %s", host)
		}
	}
	if !strings.Contains(html, "/api/v1/openapi.yaml") {
		t.Error("the docs page does not point at this daemon's own spec")
	}
}
