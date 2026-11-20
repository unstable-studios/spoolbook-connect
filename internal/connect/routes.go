package connect

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
	// KV keeps the key the page's form tokens are made with.
	KV bridge.KV

	host atomic.Pointer[string]

	keyMu sync.Mutex
	key   []byte
}

// SetHost sets spoolbook's host, shown on the page and in the status.
func (rt *Routes) SetHost(host string) { rt.host.Store(&host) }

// Host is spoolbook's host, empty before the settings arrive.
func (rt *Routes) Host() string {
	if h := rt.host.Load(); h != nil {
		return *h
	}

	return ""
}

const formKeyKey = "form-key"

// formKey is a random key kept in the store, made on first use.
func (rt *Routes) formKey(ctx context.Context) ([]byte, error) {
	rt.keyMu.Lock()
	defer rt.keyMu.Unlock()

	if rt.key != nil {
		return rt.key, nil
	}

	key, err := rt.KV.Get(ctx, formKeyKey)
	if errors.Is(err, bridge.ErrNotFound) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			err = rt.KV.Put(ctx, formKeyKey, key)
		}
	}

	if err != nil {
		return nil, err
	}

	rt.key = key

	return key, nil
}

// formToken is what the page's forms carry: bound to the person the host
// says is asking, so another site can't post a form for them. The host passes
// no cookie or Sec-Fetch-Site header to a module, so it can't be bound to
// those.
func (rt *Routes) formToken(ctx context.Context, principal string) (string, error) {
	key, err := rt.formKey(ctx)
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("page-form\x00" + principal))

	return hex.EncodeToString(mac.Sum(nil)), nil
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
	Spoolbook string `json:"spoolbook"`
	// Paused is set while spoolbook refuses the setup (token, role, address):
	// nothing goes until a send works or the settings change.
	Paused   *bridge.Pause  `json:"paused,omitempty"`
	Pending  []bridge.Entry `json:"pending"`
	Printing []bridge.Job   `json:"printing"`
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

	paused, err := rt.Outbox.Paused(r.Context())
	if err != nil {
		refuse(w, http.StatusBadGateway, "store", "Can't read the queue. Try again.")
		return
	}

	answer(w, http.StatusOK, Status{Spoolbook: rt.Host(), Paused: paused, Pending: pending, Printing: jobs})
}

//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

type pageData struct {
	Host    string
	Rows    []SpoolRow
	Pending int
	Paused  *bridge.Pause
	Token   string
	Error   string
	Notice  string
}

// pageCSP allows the page's own inline styles and form posts to itself only.
// The host may drop it (it keeps its own security headers); it's sent anyway.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (rt *Routes) render(w http.ResponseWriter, r *http.Request, status int, notice, failure string) {
	data := pageData{Host: rt.Host(), Notice: notice, Error: failure}

	token, err := rt.formToken(r.Context(), routesapi.PrincipalOf(r))
	if err != nil && data.Error == "" {
		data.Error = "Can't read this module's store. Try again."
	}

	data.Token = token

	if paused, err := rt.Outbox.Paused(r.Context()); err == nil {
		data.Paused = paused
	}

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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", pageCSP)
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

	want, err := rt.formToken(r.Context(), routesapi.PrincipalOf(r))
	if err != nil || !hmac.Equal([]byte(r.PostForm.Get("token")), []byte(want)) {
		rt.render(w, r, http.StatusForbidden, "", "The form expired. Try again.")
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
