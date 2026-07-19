// Package api is the /api/v1 surface (spec §12.2): the versioned REST
// API client software consumes. It is a PEER of the web apps over the
// same domain layer — handlers authenticate with kernel.APIAuthed
// (bearer keys, scope-gated; no cookies, no CSRF, no HTML) and consume
// pkg/domain directly, never another app's handlers.
//
// Endpoint files grow with the app phases (Drive's land in phase 2,
// Mail's in 3–4, Calendar's in 5, Media's in 6); the response
// conventions every endpoint shares live in respond.go. docs/api.md is
// the reference — response shapes documented there are test-covered.
package api

import (
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

type handlers struct {
	k *kernel.App
	// kickOutbound nudges the mailer's outbound loop after a send so a
	// zero-hold release ships immediately (nil is a no-op).
	kickOutbound func()
}

// Mount registers the /api/v1 routes. Called explicitly from cmd/pcp,
// which passes the mailer's KickOutbound.
func Mount(k *kernel.App, kickOutbound func()) kernel.Mount {
	h := &handlers{k: k, kickOutbound: kickOutbound}
	routes := []kernel.Route{
		{Pattern: "GET /api/v1/profile", Handler: k.APIAuthed(apikeys.ScopeProfileRead, h.profile)},
		{Pattern: "GET /api/v1/scopes", Handler: k.APIAuthed(kernel.ScopeAny, h.scopes)},
	}
	routes = append(routes, h.driveRoutes(k)...)
	routes = append(routes, h.mailRoutes(k)...)
	routes = append(routes, h.calendarRoutes(k)...)
	routes = append(routes, h.contactsRoutes(k)...)
	routes = append(routes, h.mediaRoutes(k)...)
	routes = append(routes, h.messengerRoutes(k)...)
	routes = append(routes, h.gitRoutes(k)...)
	routes = append(routes, h.buildRoutes(k)...)
	routes = append(routes, h.smarthomeRoutes(k)...)
	// Everything unmatched under /api/v1/ answers the JSON envelope —
	// an API client must never have to parse an HTML 404.
	routes = append(routes, kernel.Route{Pattern: "/api/v1/", Handler: http.HandlerFunc(notFound)})
	return kernel.Mount{App: "api", Routes: routes}
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	kernel.APIError(w, http.StatusNotFound, "not_found", "no such endpoint")
}
