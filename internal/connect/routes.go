package connect

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/prusa3d/prusa-connect/sdk/routesapi"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

// SpoolLister lists Materials' spools for the person asking.
type SpoolLister interface {
	Spools(ctx context.Context, principal string) ([]bridge.MaterialsSpool, error)
}

// Routes is this module's HTTP surface, mounted by the host under
// /api/v1/modules/<namespace>/.
type Routes struct {
	Links  *bridge.Links
	Outbox *bridge.Outbox
	Jobs   func(ctx context.Context) ([]bridge.Job, error)
	Spools SpoolLister
	// Host is spoolbook's host, for the page.
	Host string
}

// Handler serves the routes.
func (rt *Routes) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /links", rt.listLinks)
	mux.HandleFunc("PUT /links/{materials_spool}", rt.putLink)
	mux.HandleFunc("DELETE /links/{materials_spool}", rt.deleteLink)
	mux.HandleFunc("GET /spools", rt.listSpools)
	mux.HandleFunc("GET /status", rt.status)
	mux.HandleFunc("GET /page", rt.page)
	mux.HandleFunc("POST /page", rt.pagePost)

	return mux
}

type problem struct {
	Code  string `json:"code"`
	Title string `json:"title"`
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func refuse(w http.ResponseWriter, status int, code, title string) {
	answer(w, status, problem{Code: "spoolbook-connect/" + code, Title: title})
}

func (rt *Routes) listLinks(w http.ResponseWriter, r *http.Request) {
	links, err := rt.Links.List(r.Context())
	if err != nil {
		refuse(w, http.StatusBadGateway, "store", "Can't read the links. Try again.")
		return
	}

	answer(w, http.StatusOK, map[string]any{"links": links})
}

func (rt *Routes) putLink(w http.ResponseWriter, r *http.Request) {
	id, err := bridge.ParseSpoolID(r.PathValue("materials_spool"))
	if err != nil {
		refuse(w, http.StatusBadRequest, "spool", "Give a Materials spool id.")
		return
	}

	var body spoolbook.Spool
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err := json.Unmarshal(raw, &body); err != nil {
		refuse(w, http.StatusBadRequest, "body", "Send vendor, filament and spool.")
		return
	}

	link := bridge.Link{MaterialsSpool: id, Spool: body}
	if err := rt.Links.Put(r.Context(), link); err != nil {
		refuse(w, http.StatusBadRequest, "link", err.Error())
		return
	}

	answer(w, http.StatusOK, link)
}

func (rt *Routes) deleteLink(w http.ResponseWriter, r *http.Request) {
	id, err := bridge.ParseSpoolID(r.PathValue("materials_spool"))
	if err != nil {
		refuse(w, http.StatusBadRequest, "spool", "Give a Materials spool id.")
		return
	}

	switch err := rt.Links.Delete(r.Context(), id); {
	case errors.Is(err, bridge.ErrNotFound):
		refuse(w, http.StatusNotFound, "not-linked", "That spool isn't linked.")
	case err != nil:
		refuse(w, http.StatusBadGateway, "store", "Can't remove the link. Try again.")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// SpoolRow is a Materials spool with its link, if any.
type SpoolRow struct {
	bridge.MaterialsSpool
	Link *spoolbook.Spool `json:"link,omitempty"`
}

func (rt *Routes) rows(ctx context.Context, principal string) ([]SpoolRow, error) {
	links, err := rt.Links.List(ctx)
	if err != nil {
		return nil, err
	}

	linked := map[int64]spoolbook.Spool{}
	for _, l := range links {
		linked[l.MaterialsSpool] = l.Spool
	}

	spools, err := rt.Spools.Spools(ctx, principal)
	if err != nil {
		return nil, err
	}

	rows := make([]SpoolRow, 0, len(spools))
	seen := map[int64]bool{}
	for _, s := range spools {
		row := SpoolRow{MaterialsSpool: s}
		if l, ok := linked[s.ID]; ok {
			row.Link = &l
		}

		seen[s.ID] = true
		rows = append(rows, row)
	}

	// a link whose spool Materials no longer lists
	for id, l := range linked {
		if !seen[id] {
			rows = append(rows, SpoolRow{MaterialsSpool: bridge.MaterialsSpool{ID: id}, Link: &l})
		}
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	return rows, nil
}

func (rt *Routes) listSpools(w http.ResponseWriter, r *http.Request) {
	rows, err := rt.rows(r.Context(), routesapi.PrincipalOf(r))
	if err != nil {
		refuse(w, http.StatusBadGateway, "materials", "Can't read Materials' spools. Is Materials installed and running?")
		return
	}

	answer(w, http.StatusOK, map[string]any{"spools": rows})
}

// Status is what the module is doing.
type Status struct {
	Spoolbook string         `json:"spoolbook"`
	Pending   []bridge.Entry `json:"pending"`
	Printing  []bridge.Job   `json:"printing"`
}

func (rt *Routes) status(w http.ResponseWriter, r *http.Request) {
	pending, err := rt.Outbox.Pending(r.Context())
	if err != nil {
		refuse(w, http.StatusBadGateway, "store", "Can't read the queue. Try again.")
		return
	}

	jobs, err := rt.Jobs(r.Context())
	if err != nil {
		refuse(w, http.StatusBadGateway, "store", "Can't read the prints. Try again.")
		return
	}

	answer(w, http.StatusOK, Status{Spoolbook: rt.Host, Pending: pending, Printing: jobs})
}

//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

type pageData struct {
	Host    string
	Rows    []SpoolRow
	Pending int
	Error   string
	Notice  string
}

func (rt *Routes) render(w http.ResponseWriter, r *http.Request, status int, notice, failure string) {
	data := pageData{Host: rt.Host, Notice: notice, Error: failure}

	rows, err := rt.rows(r.Context(), routesapi.PrincipalOf(r))
	if err != nil && data.Error == "" {
		data.Error = "Can't read Materials' spools. Is Materials installed and running?"
	}

	data.Rows = rows

	if pending, err := rt.Outbox.Pending(r.Context()); err == nil {
		data.Pending = len(pending)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, data)
}

func (rt *Routes) page(w http.ResponseWriter, r *http.Request) {
	rt.render(w, r, http.StatusOK, "", "")
}

// pagePost takes the page's forms. The host passes no redirect, so it answers
// the page itself.
func (rt *Routes) pagePost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		rt.render(w, r, http.StatusBadRequest, "", "Can't read the form.")
		return
	}

	id, err := bridge.ParseSpoolID(r.PostForm.Get("materials_spool"))
	if err != nil {
		rt.render(w, r, http.StatusBadRequest, "", "Pick a Materials spool.")
		return
	}

	switch r.PostForm.Get("action") {
	case "link":
		spool, err := parseSpoolbookRef(r.PostForm.Get("spoolbook"))
		if err == nil {
			err = rt.Links.Put(r.Context(), bridge.Link{MaterialsSpool: id, Spool: spool})
		}

		if err != nil {
			rt.render(w, r, http.StatusBadRequest, "", "Enter the spoolbook spool as vendor/filament/spool.")
			return
		}

		rt.render(w, r, http.StatusOK, "Linked.", "")
	case "unlink":
		if err := rt.Links.Delete(r.Context(), id); err != nil && !errors.Is(err, bridge.ErrNotFound) {
			rt.render(w, r, http.StatusBadGateway, "", "Can't remove the link. Try again.")
			return
		}

		rt.render(w, r, http.StatusOK, "Unlinked.", "")
	default:
		rt.render(w, r, http.StatusBadRequest, "", "Unknown action.")
	}
}

// parseSpoolbookRef reads vendor/filament/spool, or a spool page's address
// in spoolbook (…/app/spool/<vendor>/<filament>/<spool>).
func parseSpoolbookRef(s string) (spoolbook.Spool, error) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		s = u.Path
	}

	s, _, _ = strings.Cut(s, "?")
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 3 {
		return spoolbook.Spool{}, errors.New("want vendor/filament/spool")
	}

	parts = parts[len(parts)-3:]
	spool := spoolbook.Spool{Vendor: parts[0], Filament: parts[1], Spool: parts[2]}

	return spool, spool.Validate()
}
