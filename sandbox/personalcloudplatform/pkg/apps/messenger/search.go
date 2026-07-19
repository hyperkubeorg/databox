// search.go — the message search surface (Messenger §9). A
// full-screen results page with scope chips (this channel / this server /
// all my servers / DMs) over the maintained inverted index. Query operators
// (from: has:file before: after:) are parsed by the domain.
package messenger

import (
	"net/http"
	"strings"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// SearchHitVM is one rendered search result.
type SearchHitVM struct {
	MessageVM
	Where string
	Link  string
}

// SearchPage is /messenger/search. It renders inside the messenger
// shell; searching a server keeps that server's rail tile lit.
type SearchPage struct {
	Shell
	Query    string
	Scope    string
	ServerID string
	CID      string
	Hits     []SearchHitVM
	Ran      bool
}

// search renders the search page and, when a query is present, its results.
func (h *handlers) search(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	q := r.URL.Query()
	railActive := "home"
	if q.Get("server") != "" {
		railActive = q.Get("server")
	}
	pg := SearchPage{
		Shell:    h.shell(r, sess, user, "Search", railActive, ""),
		Query:    q.Get("q"),
		Scope:    q.Get("scope"),
		ServerID: q.Get("server"),
		CID:      q.Get("channel"),
	}
	if pg.Scope == "" {
		pg.Scope = dmessenger.ScopeAll
	}
	if strings.TrimSpace(pg.Query) != "" {
		pg.Ran = true
		pg.Hits = h.runSearch(r, user, pg)
	}
	ui.Render(w, h.views, "messenger_search", pg)
}

// runSearch executes the query and renders hits with author names + links.
func (h *handlers) runSearch(r *http.Request, user users.User, pg SearchPage) []SearchHitVM {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	scope := dmessenger.SearchScope{Kind: pg.Scope, ServerID: pg.ServerID, CID: pg.CID}
	hits, err := h.k.Msg.Search(cctx, user, scope, pg.Query, 60)
	if err != nil {
		h.k.Log.Warn("messenger search failed", "err", err)
		return nil
	}
	names := map[string]string{}
	out := make([]SearchHitVM, 0, len(hits))
	for _, hit := range hits {
		dn, ok := names[hit.Author]
		if !ok {
			dn = hit.Author
			if u, found, err := h.k.Users.Get(cctx, hit.Author); err == nil && found && u.DisplayName != "" {
				dn = u.DisplayName
			}
			names[hit.Author] = dn
		}
		link := "/messenger/dm/" + hit.CID + "#m-" + hit.ID
		if hit.ServerID != "" {
			link = "/messenger/s/" + hit.ServerID + "/" + hit.CID + "#m-" + hit.ID
		}
		out = append(out, SearchHitVM{
			MessageVM: MessageVM{ID: hit.ID, Author: hit.Author, DisplayName: dn, HTML: safeHTML(hit.HTML), When: hit.Ts},
			Where:     hit.Where,
			Link:      link,
		})
	}
	return out
}
