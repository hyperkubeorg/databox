// query.go is the interactive query scratchpad (§4,
// developer audience): a REPL-in-a-page that runs one KV operation per
// submit — get, set, delete, or list — against the cluster AS THE LOGGED-IN
// USER, rendering the result inline under the form.
//
// Two deliberate departures from the browser pages:
//
//   - Authorization failures render INSIDE the result panel instead of the
//     403 error page. A scratchpad's job includes answering "can I do
//     this?" — a denial is a result, not a dead end.
//
//   - The POST re-renders the page directly (no redirect), so the result —
//     including one-shot facts like the new revision — sits right under
//     the form that produced it, and the form keeps its inputs for the
//     next iteration.
//
// The page also carries a one-shot watch preview: a few lines of vanilla
// JS (same pattern as watch.tpl) that streams the first events from the
// cookie-authenticated /watch/stream endpoint and stops by itself.
package frontend

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/renderer"
)

// queryResult is the outcome of one executed operation.
type queryResult struct {
	Op  string // the operation that ran (echoed in the panel header)
	Key string // the key/prefix it ran against
	// Err renders as the result when the operation failed — including
	// permission denials, which are first-class scratchpad answers.
	Err string
	// Notice is the one-line success summary ("wrote rev 42").
	Notice string
	// Get results.
	Found     bool
	Rev       uint64
	Size      int64
	Blob      bool
	Printable bool
	Text      string
	Hex       string
	// List results.
	Rows []kvRow
	Next string // cursor where the listing stopped ("" = end)
}

// queryData feeds query.tpl. Form fields echo back so the scratchpad
// keeps state across submits without any client-side storage.
type queryData struct {
	Op     string // selected operation (default "get")
	Key    string
	Value  string
	Limit  int
	Result *queryResult // nil until something ran
}

// queryListLimit bounds a scratchpad listing page.
const (
	queryDefaultLimit = 100
	queryMaxLimit     = 500
)

// queryPage renders the empty scratchpad (GET /query).
func (g *gui) queryPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	// Deep links may preload a key (e.g. future "open in scratchpad").
	g.render(w, http.StatusOK, "query.tpl", g.page(r, u, "Query scratchpad", &queryData{
		Op: "get", Key: r.URL.Query().Get("key"), Limit: queryDefaultLimit,
	}))
}

// queryRun executes one operation (POST /query/run) and re-renders the
// page with the result inline. Every operation authorizes the exact
// (key, verb) pair against the session user's grants — the scratchpad can
// never do more than the API would allow the same user.
func (g *gui) queryRun(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "bad form")
		return
	}
	if !g.checkCSRF(w, r, u) {
		return
	}
	d := &queryData{
		Op:    r.FormValue("op"),
		Key:   r.FormValue("key"),
		Value: r.FormValue("value"),
		Limit: queryDefaultLimit,
	}
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > 0 {
		d.Limit = min(n, queryMaxLimit)
	}
	d.Result = g.runQueryOp(r, u, d, r.FormValue("cursor"))
	g.render(w, http.StatusOK, "query.tpl", g.page(r, u, "Query scratchpad", d))
}

// runQueryOp dispatches one operation and folds every failure — bad
// input, denied grant, storage error — into the result panel.
func (g *gui) runQueryOp(r *http.Request, u auth.User, d *queryData, cursor string) *queryResult {
	res := &queryResult{Op: d.Op, Key: d.Key}
	if d.Key == "" {
		res.Err = "enter a key (or prefix for list)"
		return res
	}
	// Each op checks its own verb, mirroring the API's authorization map.
	verbs := map[string]auth.Verb{
		"get": auth.VerbRead, "set": auth.VerbWrite,
		"delete": auth.VerbDelete, "list": auth.VerbList,
	}
	verb, ok := verbs[d.Op]
	if !ok {
		res.Err = "unknown operation " + d.Op
		return res
	}
	if err := g.s.Authorize(u, d.Key, verb); err != nil {
		res.Err = err.Error() // a denial IS the answer here
		return res
	}
	switch d.Op {
	case "get":
		rec, found, err := g.s.KVGet(r.Context(), d.Key)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		res.Found, res.Rev, res.Blob = found, rec.Rev, rec.Blob
		res.Size = int64(len(rec.Value))
		switch {
		case !found:
			res.Notice = "key not found"
		case rec.Blob:
			res.Notice = fmt.Sprintf("blob manifest at rev %d — open in the blob browser to download", rec.Rev)
			res.Printable, res.Text = true, renderer.PrettyJSON(rec.Value)
		case renderer.Printable(rec.Value):
			res.Notice = fmt.Sprintf("found at rev %d, %d bytes", rec.Rev, res.Size)
			res.Printable, res.Text = true, string(rec.Value)
		default:
			res.Notice = fmt.Sprintf("found at rev %d, %d bytes (binary)", rec.Rev, res.Size)
			res.Hex = renderer.HexPreview(rec.Value, 4096)
		}
	case "set":
		rev, err := g.s.KVSet(r.Context(), d.Key, []byte(d.Value), false)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		res.Rev = rev
		res.Notice = fmt.Sprintf("wrote %d bytes at rev %d", len(d.Value), rev)
	case "delete":
		rev, err := g.s.KVDelete(r.Context(), d.Key)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		res.Rev = rev
		res.Notice = fmt.Sprintf("deleted at rev %d", rev)
	case "list":
		entries, next, err := g.s.KVList(d.Key, cursor, d.Limit)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		for _, e := range entries {
			res.Rows = append(res.Rows, kvRow{
				Key: e.Key, Rev: e.Record.Rev,
				Size: int64(len(e.Record.Value)), Blob: e.Record.Blob,
			})
		}
		res.Next = next
		res.Notice = fmt.Sprintf("%d keys under %s", len(res.Rows), d.Key)
	}
	return res
}
