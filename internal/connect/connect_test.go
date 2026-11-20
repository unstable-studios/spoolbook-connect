package connect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/event"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type call struct {
	device string
	kind   string
	any    any
}

type recorder struct {
	calls []call
	fail  error
}

func (r *recorder) OnSpool(_ context.Context, ev bridge.SpoolEvent) error {
	r.calls = append(r.calls, call{"", "spool", ev})
	return r.fail
}

func (r *recorder) OnJobStarted(_ context.Context, d string, ev bridge.JobStarted) error {
	r.calls = append(r.calls, call{d, "started", ev})
	return r.fail
}

func (r *recorder) OnJobSettled(_ context.Context, d string, ev bridge.JobSettled) error {
	r.calls = append(r.calls, call{d, "settled", ev})
	return r.fail
}

func envelope(t *testing.T, id, typ, subject string, data any) json.RawMessage {
	t.Helper()

	raw, _ := json.Marshal(data)
	env, err := json.Marshal(event.Envelope{
		SpecVersion: "1.0", ID: id, Type: typ, Subject: subject, Class: event.ClassState,
		Source: "urn:prusa:site:local/" + strings.Replace(subject, ":", "/", 1), Data: raw,
	})
	if err != nil {
		t.Fatal(err)
	}

	return env
}

func TestDispatcherRoutesEachType(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	kv := bridge.NewMemKV()
	d := &Dispatcher{Handler: rec, KV: kv, Log: quiet}

	w := busapi.Window{Cursor: "c1", Events: []json.RawMessage{
		envelope(t, "1", event.TypeDeviceJobStarted, "device:p1", event.JobStarted{JobID: "j1", DisplayName: "Benchy"}),
		envelope(t, "2", TypeMaterialsSpool, "sink:materials", map[string]any{"spool_id": 4, "remaining_weight_g": 812.5}),
		envelope(t, "3", event.TypeDeviceJobSettled, "device:p1", event.JobSettled{JobID: "j1", Outcome: event.JobFinished}),
		envelope(t, "4", TypeMaterialsTool, "sink:materials", map[string]any{}),
		envelope(t, "5", event.TypeDeviceJobStarted, "dashboard:everyone", event.JobStarted{JobID: "j2"}),
		json.RawMessage(`not json`),
	}}

	if err := d.Window(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	if len(rec.calls) != 3 {
		t.Fatalf("calls = %+v", rec.calls)
	}

	if c := rec.calls[0]; c.kind != "started" || c.device != "p1" || c.any.(bridge.JobStarted).DisplayName != "Benchy" {
		t.Errorf("started = %+v", c)
	}

	if c := rec.calls[1]; c.kind != "spool" || c.any.(bridge.SpoolEvent).SpoolID != 4 || c.any.(bridge.SpoolEvent).Remaining != 812.5 {
		t.Errorf("spool = %+v", c)
	}

	if c := rec.calls[2]; c.kind != "settled" || c.device != "p1" || c.any.(bridge.JobSettled).Outcome != bridge.Finished {
		t.Errorf("settled = %+v", c)
	}

	if cursor(d) != "c1" {
		t.Errorf("cursor not kept")
	}
}

func TestDispatcherRetriesThenSkips(t *testing.T) {
	t.Parallel()

	rec := &recorder{fail: errors.New("materials down")}
	d := &Dispatcher{Handler: rec, KV: bridge.NewMemKV(), Log: quiet}
	w := busapi.Window{Cursor: "c2", Events: []json.RawMessage{
		envelope(t, "1", TypeMaterialsSpool, "sink:materials", map[string]any{"spool_id": 4}),
	}}

	for i := 1; i < MaxAttempts; i++ {
		if err := d.Window(context.Background(), w); err == nil {
			t.Fatalf("attempt %d: no error", i)
		}

		if cursor(d) != "" {
			t.Fatal("cursor moved past a failed window")
		}
	}

	if err := d.Window(context.Background(), w); err != nil {
		t.Fatalf("not skipped after %d attempts: %v", MaxAttempts, err)
	}

	if cursor(d) != "c2" {
		t.Error("cursor not kept after skipping")
	}
}

// fakeCaller answers Materials' routes from fixed pages.
type fakeCaller struct {
	tools  [][]bridge.Tool
	spools [][]bridge.MaterialsSpool
	seen   []routesapi.CallParams
	status int
}

func (f *fakeCaller) Call(_ context.Context, p routesapi.CallParams) (routesapi.Response, error) {
	f.seen = append(f.seen, p)
	if f.status != 0 {
		return routesapi.Response{Status: f.status, Body: []byte(`{"code":"materials/x"}`)}, nil
	}

	q, _ := url.ParseQuery(p.Query)
	offset, _ := strconv.Atoi(q.Get("offset"))
	n := offset / pageLimit

	var body any
	switch p.Path {
	case "/printers/tools":
		page := map[string]any{"tools": f.tools[n], "total": 1}
		if n+1 < len(f.tools) {
			page["next"] = offset + pageLimit
		}
		body = page
	case "/spools":
		page := map[string]any{"spools": f.spools[n], "total": 1}
		if n+1 < len(f.spools) {
			page["next"] = offset + pageLimit
		}
		body = page
	}

	raw, _ := json.Marshal(body)

	return routesapi.Response{Status: 200, Body: raw}, nil
}

func ptr(n int64) *int64 { return &n }

func TestMaterialsToolsWalksPagesAndFiltersThePrinter(t *testing.T) {
	t.Parallel()

	f := &fakeCaller{tools: [][]bridge.Tool{
		{{DeviceID: "p0", ToolID: "0"}, {DeviceID: "p1", ToolID: "0", SpoolID: ptr(3), Remaining: 500}},
		{{DeviceID: "p1", ToolID: "1", SpoolID: ptr(4), Remaining: 200}},
	}}
	m := Materials{Caller: f}

	tools, err := m.Tools(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}

	if len(tools) != 2 || *tools[0].SpoolID != 3 || tools[1].ToolID != "1" {
		t.Fatalf("tools = %+v", tools)
	}

	if f.seen[0].Module != MaterialsNamespace || f.seen[0].Method != http.MethodGet || f.seen[0].Principal != "" {
		t.Errorf("call = %+v", f.seen[0])
	}
}

func TestMaterialsSpoolFindsByID(t *testing.T) {
	t.Parallel()

	f := &fakeCaller{spools: [][]bridge.MaterialsSpool{{{ID: 1}}, {{ID: 2, Remaining: 640}}}}
	m := Materials{Caller: f}.As("user:operator")

	s, err := m.Spool(context.Background(), 2)
	if err != nil || s.Remaining != 640 {
		t.Fatalf("spool = %+v, %v", s, err)
	}

	if _, err := m.Spool(context.Background(), 9); !errors.Is(err, bridge.ErrNotFound) {
		t.Errorf("missing spool: %v", err)
	}

	if f.seen[0].Principal != "user:operator" {
		t.Errorf("principal = %q", f.seen[0].Principal)
	}
}

func TestMaterialsRefusalIsAnError(t *testing.T) {
	t.Parallel()

	m := Materials{Caller: &fakeCaller{status: 503}}
	if _, err := m.Tools(context.Background(), "p1"); err == nil {
		t.Fatal("a 503 was taken as an answer")
	}
}

type listSpools []bridge.MaterialsSpool

func (l listSpools) Spools(context.Context, string) ([]bridge.MaterialsSpool, error) { return l, nil }

func newRoutes() (*Routes, http.Handler) {
	kv := bridge.NewMemKV()
	b := bridge.New(kv, nil, quiet, time.Now)
	rt := &Routes{
		Links: b.Links, Outbox: b.Outbox, Jobs: b.Jobs, KV: kv,
		Spools: listSpools{{ID: 1, UUID: "u1", BrandName: "Prusament", MaterialName: "PLA Galaxy Black", ColorRGBA: "#000000ff", Remaining: 812}, {ID: 2, UUID: "u2"}},
	}

	rt.SetHost("spoolbook.example")

	return rt, rt.Handler()
}

var tokenField = regexp.MustCompile(`name="token" value="([0-9a-f]+)"`)

func do(h http.Handler, method, target, ctype, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	return w
}

func TestLinkRoutes(t *testing.T) {
	t.Parallel()

	_, h := newRoutes()

	cases := []struct {
		method, target, body string
		status               int
	}{
		{http.MethodPut, "/links/1", `{"vendor":"prusament","filament":"pla-galaxy-black","spool":"a1"}`, 200},
		{http.MethodPut, "/links/0", `{"vendor":"v","filament":"f","spool":"s"}`, 400},
		{http.MethodPut, "/links/2", `{"vendor":"v","filament":"../f","spool":"s"}`, 400},
		{http.MethodPut, "/links/2", `nope`, 400},
		{http.MethodDelete, "/links/2", ``, 404},
		{http.MethodGet, "/links", ``, 200},
	}

	for _, tc := range cases {
		w := do(h, tc.method, tc.target, "application/json", tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s = %d %s, want %d", tc.method, tc.target, w.Code, w.Body, tc.status)
		}
	}

	var list struct{ Links []bridge.Link }
	_ = json.Unmarshal(do(h, http.MethodGet, "/links", "", "").Body.Bytes(), &list)
	if len(list.Links) != 1 || list.Links[0].Spool != (spoolbook.Spool{Vendor: "prusament", Filament: "pla-galaxy-black", Spool: "a1"}) {
		t.Fatalf("links = %+v", list.Links)
	}

	var spools struct{ Spools []SpoolRow }
	_ = json.Unmarshal(do(h, http.MethodGet, "/spools", "", "").Body.Bytes(), &spools)
	if len(spools.Spools) != 2 || spools.Spools[0].Link == nil || spools.Spools[1].Link != nil {
		t.Fatalf("spools = %+v", spools.Spools)
	}

	if w := do(h, http.MethodDelete, "/links/1", "", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d", w.Code)
	}

	if w := do(h, http.MethodGet, "/status", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"spoolbook":"spoolbook.example"`) {
		t.Errorf("status = %d %s", w.Code, w.Body)
	}
}

func TestPage(t *testing.T) {
	t.Parallel()

	rt, h := newRoutes()
	form := "application/x-www-form-urlencoded"

	w := do(h, http.MethodGet, "/page", "", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "PLA Galaxy Black") {
		t.Fatalf("page = %d %s", w.Code, w.Body)
	}

	if w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("headers = %v", w.Header())
	}

	m := tokenField.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("the page's forms carry no token")
	}

	tok := "&token=" + m[1]

	// another site's form: no token, or a wrong one
	for _, body := range []string{
		"action=link&materials_spool=1&spoolbook=a/b/c",
		"action=link&materials_spool=1&spoolbook=a/b/c&token=00",
	} {
		if w := do(h, http.MethodPost, "/page", form, body); w.Code != http.StatusForbidden {
			t.Errorf("%s = %d", body, w.Code)
		}
	}

	if _, err := rt.Links.Get(context.Background(), 1); err == nil {
		t.Fatal("linked without a token")
	}

	w = do(h, http.MethodPost, "/page", form, "action=link&materials_spool=1&spoolbook="+url.QueryEscape("https://spoolbook.example/app/spool/prusament/pla/a1?lib=x")+tok)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Linked.") || !strings.Contains(w.Body.String(), "prusament/pla/a1") {
		t.Fatalf("link = %d %s", w.Code, w.Body)
	}

	if l, err := rt.Links.Get(context.Background(), 1); err != nil || l.Spool.Spool != "a1" {
		t.Fatalf("link = %+v, %v", l, err)
	}

	for _, body := range []string{
		"action=link&materials_spool=1&spoolbook=only/two",
		"action=link&materials_spool=x&spoolbook=a/b/c",
		"action=other&materials_spool=1",
	} {
		if w := do(h, http.MethodPost, "/page", form, body+tok); w.Code != 400 {
			t.Errorf("%s = %d", body, w.Code)
		}
	}

	w = do(h, http.MethodPost, "/page", form, "action=unlink&materials_spool=1"+tok)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Unlinked.") {
		t.Fatalf("unlink = %d", w.Code)
	}
}

func TestParseSpoolbookRef(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"prusament/pla/a1":    "prusament/pla/a1",
		" /prusament/pla/a1/": "prusament/pla/a1",
		"https://spoolbook.unstablestudios.com/app/spool/prusament/pla-galaxy-black/s-3?x=1": "prusament/pla-galaxy-black/s-3",
		"a/b":     "",
		"a/b/c d": "",
	}

	for in, want := range cases {
		got, err := parseSpoolbookRef(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q accepted as %v", in, got)
			}

			continue
		}

		if err != nil || got.String() != want {
			t.Errorf("%q = %v, %v; want %s", in, got, err, want)
		}
	}
}

func cursor(d *Dispatcher) string {
	c, _ := d.Cursor(context.Background())
	return c
}

func TestStatusShowsAPause(t *testing.T) {
	t.Parallel()

	rt, h := newRoutes()
	ctx := context.Background()
	_ = rt.Outbox.Remaining(ctx, spoolbook.Spool{Vendor: "v", Filament: "f", Spool: "s"}, 1, 0)
	_, _ = rt.Outbox.Flush(ctx, refuser{})

	var st Status
	_ = json.Unmarshal(do(h, http.MethodGet, "/status", "", "").Body.Bytes(), &st)
	if st.Paused == nil || st.Paused.Status != 401 || len(st.Pending) != 1 {
		t.Fatalf("status = %+v", st)
	}

	if w := do(h, http.MethodGet, "/page", "", ""); !strings.Contains(w.Body.String(), "refused this module") {
		t.Error("the page doesn't say so")
	}
}

type refuser struct{}

func (refuser) Send(context.Context, spoolbook.Spool, spoolbook.Update) (spoolbook.Result, error) {
	return spoolbook.Result{}, &spoolbook.Error{Class: spoolbook.Pause, Status: 401, Code: "unauthorized"}
}
