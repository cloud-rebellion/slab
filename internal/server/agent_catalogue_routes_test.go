package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/agent"
)

// TestAdvertisedAgentEndpointsAreRegistered asserts that every endpoint the
// agent context catalogue advertises can actually be routed by the REAL router.
//
// Both sides are enumerated rather than restated: the catalogue is the exact
// value served at GET /api/agent/context (agent.DefaultEndpoints()), and
// routability is decided by chi's own matcher against the router New() builds.
// Asking chi to match, instead of string-comparing route patterns, is what
// makes the test honest about parameterised routes: the catalogue documents
// concrete example paths like /api/agent/settings/seo, which a registered
// /api/agent/settings/{category} route serves correctly.
//
// The defect this exists to catch: GET /api/agent/pages and
// GET /api/agent/pages/{slug}/blocks were advertised from the day the agent API
// shipped and never registered. The SPA catch-all served index.html for
// unmatched paths, so both returned 200 text/html, which an API client cannot
// distinguish from success.
func TestAdvertisedAgentEndpointsAreRegistered(t *testing.T) {
	mux, ok := newTestRouter(t).(*chi.Mux)
	if !ok {
		t.Fatal("router is not a *chi.Mux")
	}

	catalogue := agent.DefaultEndpoints()
	v := reflect.ValueOf(catalogue)
	var checked int
	for i := 0; i < v.NumField(); i++ {
		group := v.Field(i)
		if group.Kind() != reflect.Slice {
			continue
		}
		for j := 0; j < group.Len(); j++ {
			ep := group.Index(j).Interface().(agent.EndpointInfo)
			path := concreteSamplePath(ep.Path)
			checked++
			if !mux.Match(chi.NewRouteContext(), ep.Method, path) {
				t.Errorf("catalogue advertises %s %s (group %s) but the router cannot route %s %s; "+
					"an agent calling it falls through to the SPA handler and gets 200 text/html",
					ep.Method, ep.Path, v.Type().Field(i).Name, ep.Method, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("walked zero catalogue endpoints; the reflection above stopped matching the struct")
	}
}

// TestUnknownAPIPathReturnsJSON404 pins the behaviour that made the missing
// routes invisible. An unmatched /api/* path must be a JSON 404, never the SPA
// index, so a client can tell "no such endpoint" from "success".
//
// The SPA must actually be mounted for this to mean anything: production embeds
// a frontend, and it is mountFrontend's catch-all that produced the 200 text/html.
// With FrontendFS nil (the default in tests) chi's own NotFound answers instead
// and the test would pass without ever reaching the code under test.
func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	prev := FrontendFS
	FrontendFS = fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>app shell</title>")},
	}
	t.Cleanup(func() { FrontendFS = prev })

	h := newTestRouter(t)
	for _, path := range []string{
		"/api/agent/definitely-not-a-route",
		"/api/nope",
		"/api/sites/abc/not-a-route",
	} {
		rec := getNoCookie(t, h, path)
		if rec.Code != 404 {
			t.Errorf("GET %s: status = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type = %q, want application/json", path, ct)
		}
	}

	// Control: a genuine SPA deep link must still get the app shell, otherwise
	// the guard above would "pass" by breaking client-side routing.
	rec := getNoCookie(t, h, "/sites/abc/settings")
	if rec.Code != 200 {
		t.Errorf("SPA deep link: status = %d, want 200", rec.Code)
	}
}

// getNoCookie sends an unauthenticated GET, which is what any client hitting a
// wrong URL sends.
func getNoCookie(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var routeParamRe = regexp.MustCompile(`\{[^}]+\}`)

// concreteSamplePath replaces {param} placeholders with a routable sample
// value so chi can match the pattern. The value never reaches a handler; only
// the routing decision is under test.
func concreteSamplePath(path string) string {
	return routeParamRe.ReplaceAllString(path, "sample")
}
