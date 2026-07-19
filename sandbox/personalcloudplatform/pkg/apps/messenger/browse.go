// browse.go — the open-server browser (GET /messenger/browse). Lists open
// servers with a search box and a Join button; invite-only servers never
// appear here.
package messenger

import (
	"net/http"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// BrowsePage is /messenger/browse's typed page struct. It renders
// inside the messenger shell — the rail stays put while discovering.
type BrowsePage struct {
	Shell
	Query   string
	Results []dmessenger.BrowseResult
}

// browse renders the open-server browser.
func (h *handlers) browse(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	q := r.URL.Query().Get("q")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	results, err := h.k.Msg.DiscoverServers(cctx, user.Username, q, 50)
	if err != nil {
		h.k.Log.Warn("messenger browse failed", "err", err)
	}
	pg := BrowsePage{
		Shell:   h.shell(r, sess, user, "Discover servers", "browse", ""),
		Query:   q,
		Results: results,
	}
	ui.Render(w, h.views, "messenger_browse", pg)
}
