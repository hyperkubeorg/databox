package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// routeScopes is the authoritative route → required-scope table, kept by
// hand ON PURPOSE: TestRouteScopeTable fails when a route registers with
// a scope this table doesn't expect, or ships without an entry here — so
// changing an endpoint's authorization is always a visible, reviewed
// diff in two places.
var routeScopes = map[string]string{
	"GET /api/v1/profile": apikeys.ScopeProfileRead,
	"GET /api/v1/scopes":  kernel.ScopeAny,

	"GET /api/v1/drive/drives":                  apikeys.ScopeDriveRead,
	"GET /api/v1/drive/list/{drive}/{node}":     apikeys.ScopeDriveRead,
	"GET /api/v1/drive/stat/{drive}/{node}":     apikeys.ScopeDriveRead,
	"POST /api/v1/drive/mkdir":                  apikeys.ScopeDriveWrite,
	"POST /api/v1/drive/rename":                 apikeys.ScopeDriveWrite,
	"POST /api/v1/drive/move":                   apikeys.ScopeDriveWrite,
	"DELETE /api/v1/drive/nodes/{drive}/{node}": apikeys.ScopeDriveWrite,
	"PUT /api/v1/drive/upload/{drive}/{parent}": apikeys.ScopeDriveWrite,
	"POST /api/v1/drive/uploads":                apikeys.ScopeDriveWrite,
	"GET /api/v1/drive/uploads/{id}":            apikeys.ScopeDriveWrite,
	"PUT /api/v1/drive/uploads/{id}":            apikeys.ScopeDriveWrite,
	"POST /api/v1/drive/uploads/{id}/finish":    apikeys.ScopeDriveWrite,
	"GET /api/v1/drive/download/{drive}/{node}": apikeys.ScopeDriveRead,
	"GET /api/v1/drive/versions/{drive}/{node}": apikeys.ScopeDriveRead,
	"POST /api/v1/drive/restore":                apikeys.ScopeDriveWrite,
	"GET /api/v1/drive/shares/{drive}/{node}":   apikeys.ScopeDriveRead,
	"POST /api/v1/drive/shares":                 apikeys.ScopeDriveWrite,
	"DELETE /api/v1/drive/shares/{token}":       apikeys.ScopeDriveWrite,

	"GET /api/v1/mail/mailboxes":                      apikeys.ScopeMailRead,
	"GET /api/v1/mail/folders/{box}":                  apikeys.ScopeMailRead,
	"GET /api/v1/mail/threads/{box}":                  apikeys.ScopeMailRead,
	"GET /api/v1/mail/threads/{box}/{thread}":         apikeys.ScopeMailRead,
	"GET /api/v1/mail/messages/{msg}":                 apikeys.ScopeMailRead,
	"GET /api/v1/mail/messages/{msg}/raw":             apikeys.ScopeMailRead,
	"GET /api/v1/mail/messages/{msg}/attachments/{n}": apikeys.ScopeMailRead,
	"POST /api/v1/mail/threads/{box}/{thread}/read":   apikeys.ScopeMailWrite,
	"POST /api/v1/mail/threads/{box}/{thread}/star":   apikeys.ScopeMailWrite,
	"POST /api/v1/mail/threads/{box}/{thread}/move":   apikeys.ScopeMailWrite,
	"POST /api/v1/mail/threads/{box}/{thread}/labels": apikeys.ScopeMailWrite,
	"POST /api/v1/mail/folders/{box}":                 apikeys.ScopeMailWrite,
	"DELETE /api/v1/mail/folders/{box}/{id}":          apikeys.ScopeMailWrite,
	"POST /api/v1/mail/labels":                        apikeys.ScopeMailWrite,
	"PATCH /api/v1/mail/labels/{id}":                  apikeys.ScopeMailWrite,
	"DELETE /api/v1/mail/labels/{id}":                 apikeys.ScopeMailWrite,
	"GET /api/v1/mail/drafts/{box}":                   apikeys.ScopeMailWrite,
	"GET /api/v1/mail/drafts/{box}/{id}":              apikeys.ScopeMailWrite,
	"POST /api/v1/mail/drafts":                        apikeys.ScopeMailWrite,
	"DELETE /api/v1/mail/drafts/{box}/{id}":           apikeys.ScopeMailWrite,
	"POST /api/v1/mail/drafts/{box}/{id}/attachments": apikeys.ScopeMailWrite,
	"POST /api/v1/mail/send":                          apikeys.ScopeMailSend,
	"POST /api/v1/mail/send/cancel":                   apikeys.ScopeMailSend,

	"GET /api/v1/calendar/calendars":                        apikeys.ScopeCalendarRead,
	"GET /api/v1/calendar/events":                           apikeys.ScopeCalendarRead,
	"POST /api/v1/calendar/events":                          apikeys.ScopeCalendarWrite,
	"PATCH /api/v1/calendar/events/{drive}/{node}/{id}":     apikeys.ScopeCalendarWrite,
	"DELETE /api/v1/calendar/events/{drive}/{node}/{id}":    apikeys.ScopeCalendarWrite,
	"POST /api/v1/calendar/events/{drive}/{node}/{id}/rsvp": apikeys.ScopeCalendarWrite,

	"GET /api/v1/contacts":                   apikeys.ScopeContactsRead,
	"GET /api/v1/contacts/{drive}/{node}":    apikeys.ScopeContactsRead,
	"POST /api/v1/contacts":                  apikeys.ScopeContactsWrite,
	"PUT /api/v1/contacts/{drive}/{node}":    apikeys.ScopeContactsWrite,
	"DELETE /api/v1/contacts/{drive}/{node}": apikeys.ScopeContactsWrite,

	"GET /api/v1/media/folders":                              apikeys.ScopeMediaRead,
	"GET /api/v1/media/catalog/{drive}/{folder}":             apikeys.ScopeMediaRead,
	"GET /api/v1/media/entry/{drive}/{folder}/{kind}/{slug}": apikeys.ScopeMediaRead,
	"GET /api/v1/media/progress":                             apikeys.ScopeMediaRead,
	"PUT /api/v1/media/progress":                             apikeys.ScopeMediaWrite,
	"POST /api/v1/media/watched":                             apikeys.ScopeMediaWrite,
	"GET /api/v1/media/lists/{list}":                         apikeys.ScopeMediaRead,
	"PUT /api/v1/media/lists/{list}":                         apikeys.ScopeMediaWrite,
	"GET /api/v1/media/playlists":                            apikeys.ScopeMediaRead,
	"POST /api/v1/media/playlists":                           apikeys.ScopeMediaWrite,
	"GET /api/v1/media/playlists/{id}":                       apikeys.ScopeMediaRead,
	"PATCH /api/v1/media/playlists/{id}":                     apikeys.ScopeMediaWrite,
	"DELETE /api/v1/media/playlists/{id}":                    apikeys.ScopeMediaWrite,
	"GET /api/v1/media/art/{drive}/{folder}/{slug}":          apikeys.ScopeMediaRead,

	"GET /api/v1/messenger/servers":                   apikeys.ScopeMsgRead,
	"GET /api/v1/messenger/servers/{server}/channels": apikeys.ScopeMsgRead,
	"GET /api/v1/messenger/servers/{server}/members":  apikeys.ScopeMsgRead,
	"GET /api/v1/messenger/channels/{cid}/messages":   apikeys.ScopeMsgRead,
	"POST /api/v1/messenger/channels/{cid}/messages":  apikeys.ScopeMsgWrite,
	"POST /api/v1/messenger/channels/{cid}/read":      apikeys.ScopeMsgWrite,
	"GET /api/v1/messenger/channels/{cid}/typing":     apikeys.ScopeMsgRead,
	"POST /api/v1/messenger/channels/{cid}/typing":    apikeys.ScopeMsgWrite,
	"PATCH /api/v1/messenger/messages/{msg}":          apikeys.ScopeMsgWrite,
	"DELETE /api/v1/messenger/messages/{msg}":         apikeys.ScopeMsgWrite,
	"GET /api/v1/messenger/att/{cid}/{blob}":          apikeys.ScopeMsgRead,
	"GET /api/v1/messenger/dms":                       apikeys.ScopeMsgRead,
	"POST /api/v1/messenger/dms":                      apikeys.ScopeMsgWrite,
	"GET /api/v1/messenger/unread":                    apikeys.ScopeMsgRead,
	"GET /api/v1/messenger/search":                    apikeys.ScopeMsgRead,
	"GET /api/v1/messenger/presence":                  apikeys.ScopeMsgRead,
	"PUT /api/v1/messenger/presence":                  apikeys.ScopeMsgWrite,
	"POST /api/v1/messenger/join/{code}":              apikeys.ScopeMsgWrite,

	"GET /api/v1/git/repos":                                         apikeys.ScopeGitRead,
	"POST /api/v1/git/repos":                                        apikeys.ScopeGitWrite,
	"GET /api/v1/git/repos/{ns}/{name}":                             apikeys.ScopeGitRead,
	"PATCH /api/v1/git/repos/{ns}/{name}":                           apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}":                          apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/fork":                       apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/contents":                   apikeys.ScopeGitWrite,
	"GET /api/v1/git/repos/{ns}/{name}/grants":                      apikeys.ScopeGitRead,
	"PUT /api/v1/git/repos/{ns}/{name}/grants":                      apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}/grants/{subject...}":      apikeys.ScopeGitWrite,
	"GET /api/v1/git/orgs":                                          apikeys.ScopeGitRead,
	"POST /api/v1/git/orgs":                                         apikeys.ScopeGitWrite,
	"GET /api/v1/git/orgs/{org}":                                    apikeys.ScopeGitRead,
	"PATCH /api/v1/git/orgs/{org}":                                  apikeys.ScopeGitWrite,
	"GET /api/v1/git/orgs/{org}/members":                            apikeys.ScopeGitRead,
	"POST /api/v1/git/orgs/{org}/members":                           apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/orgs/{org}/members/{username}":              apikeys.ScopeGitWrite,
	"GET /api/v1/git/orgs/{org}/teams":                              apikeys.ScopeGitRead,
	"POST /api/v1/git/orgs/{org}/teams":                             apikeys.ScopeGitWrite,
	"PATCH /api/v1/git/orgs/{org}/teams/{team}":                     apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/orgs/{org}/teams/{team}":                    apikeys.ScopeGitWrite,
	"POST /api/v1/git/orgs/{org}/teams/{team}/members":              apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/orgs/{org}/teams/{team}/members/{username}": apikeys.ScopeGitWrite,
	"GET /api/v1/git/profile":                                       apikeys.ScopeGitRead,
	"PUT /api/v1/git/profile":                                       apikeys.ScopeGitWrite,

	"GET /api/v1/git/repos/{ns}/{name}/issues":                             apikeys.ScopeGitRead,
	"POST /api/v1/git/repos/{ns}/{name}/issues":                            apikeys.ScopeGitWrite,
	"GET /api/v1/git/repos/{ns}/{name}/issues/{n}":                         apikeys.ScopeGitRead,
	"POST /api/v1/git/repos/{ns}/{name}/issues/{n}/comments":               apikeys.ScopeGitWrite,
	"PATCH /api/v1/git/repos/{ns}/{name}/issues/{n}/comments/{id}":         apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}/issues/{n}/comments/{id}":        apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/issues/{n}/state":                  apikeys.ScopeGitWrite,
	"PUT /api/v1/git/repos/{ns}/{name}/issues/{n}/labels":                  apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/issues/{n}/assignees":              apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}/issues/{n}/assignees/{username}": apikeys.ScopeGitWrite,
	"GET /api/v1/git/repos/{ns}/{name}/labels":                             apikeys.ScopeGitRead,
	"POST /api/v1/git/repos/{ns}/{name}/labels":                            apikeys.ScopeGitWrite,
	"PATCH /api/v1/git/repos/{ns}/{name}/labels/{id}":                      apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}/labels/{id}":                     apikeys.ScopeGitWrite,

	"GET /api/v1/git/repos/{ns}/{name}/merges":                             apikeys.ScopeGitRead,
	"POST /api/v1/git/repos/{ns}/{name}/merges":                            apikeys.ScopeGitWrite,
	"GET /api/v1/git/repos/{ns}/{name}/merges/{n}":                         apikeys.ScopeGitRead,
	"POST /api/v1/git/repos/{ns}/{name}/merges/{n}/comments":               apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/merges/{n}/state":                  apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/merges/{n}/merge":                  apikeys.ScopeGitWrite,
	"POST /api/v1/git/repos/{ns}/{name}/merges/{n}/assignees":              apikeys.ScopeGitWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}/merges/{n}/assignees/{username}": apikeys.ScopeGitWrite,
	"PUT /api/v1/git/repos/{ns}/{name}/merges/{n}/labels":                  apikeys.ScopeGitWrite,

	"GET /api/v1/git/repos/{ns}/{name}/builds":             apikeys.ScopeBuildRead,
	"POST /api/v1/git/repos/{ns}/{name}/builds/trigger":    apikeys.ScopeBuildWrite,
	"GET /api/v1/git/repos/{ns}/{name}/builds/{n}":         apikeys.ScopeBuildRead,
	"POST /api/v1/git/repos/{ns}/{name}/builds/{n}/cancel": apikeys.ScopeBuildWrite,
	"POST /api/v1/git/repos/{ns}/{name}/builds/{n}/retry":  apikeys.ScopeBuildWrite,
	"DELETE /api/v1/git/repos/{ns}/{name}/builds/{n}":      apikeys.ScopeBuildWrite,
	"GET /api/v1/git/repos/{ns}/{name}/releases":           apikeys.ScopeBuildRead,
	"GET /api/v1/git/repos/{ns}/{name}/releases/{id}":      apikeys.ScopeBuildRead,

	"GET /api/v1/smarthome/spaces":                           apikeys.ScopeSmartHomeRead,
	"GET /api/v1/smarthome/spaces/{id}":                      apikeys.ScopeSmartHomeRead,
	"GET /api/v1/smarthome/spaces/{id}/events":               apikeys.ScopeSmartHomeRead,
	"POST /api/v1/smarthome/spaces/{id}/events/ack":          apikeys.ScopeSmartHomeWrite,
	"GET /api/v1/smarthome/spaces/{id}/clips":                apikeys.ScopeSmartHomeRead,
	"GET /api/v1/smarthome/spaces/{id}/cam/{cam}/index":      apikeys.ScopeSmartHomeRead,
	"GET /api/v1/smarthome/spaces/{id}/cam/{cam}/seg/{ts}":   apikeys.ScopeSmartHomeRead,
	"GET /api/v1/smarthome/spaces/{id}/cam/{cam}/thumb/{ts}": apikeys.ScopeSmartHomeRead,
	"POST /api/v1/smarthome/spaces/{id}/cam/{cam}/boost":     apikeys.ScopeSmartHomeWrite,
}

// fillPattern turns a route pattern into a concrete request path.
func fillPattern(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") {
			segs[i] = "testtesttest"
		}
	}
	return method, strings.Join(segs, "/")
}

// TestRouteScopeTable drives EVERY registered /api/v1 route through the
// real mux twice: with no credential (must 401) and with a key holding
// every scope EXCEPT the expected one (must 403 naming exactly that
// scope). A route whose scope drifts from the table — or a new route
// with no table entry — fails loudly.
func TestRouteScopeTable(t *testing.T) {
	h := testHandlers(t)
	k := h.k
	ctx := context.Background()
	if _, err := k.Users.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	mux, err := k.Router(Mount(k, nil))
	if err != nil {
		t.Fatal(err)
	}

	// One key per scope, holding everything BUT that scope.
	allScopes := make([]string, 0, len(apikeys.Scopes))
	for _, s := range apikeys.Scopes {
		allScopes = append(allScopes, s.Name)
	}
	allBut := map[string]string{}
	for _, missing := range allScopes {
		rest := make([]string, 0, len(allScopes)-1)
		for _, s := range allScopes {
			if s != missing {
				rest = append(rest, s)
			}
		}
		token, _, err := k.APIKeys.Mint(ctx, "ada", "no-"+missing, rest, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		allBut[missing] = token
	}

	seen := map[string]bool{}
	for _, route := range Mount(k, nil).Routes {
		if route.Pattern == "/api/v1/" {
			continue // the JSON-404 catch-all carries no scope
		}
		want, listed := routeScopes[route.Pattern]
		if !listed {
			t.Errorf("route %q has no entry in routeScopes — add it with its intended scope", route.Pattern)
			continue
		}
		seen[route.Pattern] = true
		method, path := fillPattern(route.Pattern)

		// No credential: always 401.
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", route.Pattern, w.Code)
		}

		if want == kernel.ScopeAny {
			continue // any valid key is admitted; nothing to deny
		}
		// A key with every OTHER scope: exactly the missing-scope 403.
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+allBut[want])
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "missing scope "+want) {
			t.Errorf("%s with all-but-%s key = %d %q, want 403 missing scope %s",
				route.Pattern, want, w.Code, w.Body.String(), want)
		}
	}
	for pattern := range routeScopes {
		if !seen[pattern] {
			t.Errorf("routeScopes lists %q but no such route is registered", pattern)
		}
	}
}
