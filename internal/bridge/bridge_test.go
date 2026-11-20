package bridge

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)} }
func id(n int64) *int64                  { return &n }
func f(v float64) *float64               { return &v }
func linkTo(spool string) spoolbook.Spool {
	return spoolbook.Spool{Vendor: "prusament", Filament: "pla", Spool: spool}
}

// fakeMaterials answers tools per printer and spools by id.
type fakeMaterials struct {
	tools  map[string][]Tool
	spools map[int64]MaterialsSpool
}

func (m *fakeMaterials) Tools(_ context.Context, device string) ([]Tool, error) {
	return m.tools[device], nil
}

func (m *fakeMaterials) Spool(_ context.Context, n int64) (MaterialsSpool, error) {
	s, ok := m.spools[n]
	if !ok {
		return MaterialsSpool{}, ErrNotFound
	}

	return s, nil
}

func setup(t *testing.T) (*Bridge, *fakeMaterials, *clock, *MemKV) {
	t.Helper()

	kv := NewMemKV()
	c := newClock()
	m := &fakeMaterials{tools: map[string][]Tool{}, spools: map[int64]MaterialsSpool{}}
	b := New(kv, m, quiet, c.now)

	for n, s := range map[int64]string{1: "s1", 2: "s2"} {
		if err := b.Links.Put(context.Background(), Link{MaterialsSpool: n, Spool: linkTo(s)}); err != nil {
			t.Fatal(err)
		}
	}

	return b, m, c, kv
}

func pending(t *testing.T, b *Bridge) []Entry {
	t.Helper()

	entries, err := b.Outbox.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	return entries
}

func TestPrintID(t *testing.T) {
	t.Parallel()

	a := PrintID("printer-1", "job-1", "0")
	if len(a) != 64 || !spoolbook.PrintIDFormat.MatchString(a) {
		t.Fatalf("PrintID = %q, not 64 hex", a)
	}

	if a != PrintID("printer-1", "job-1", "0") {
		t.Error("not stable")
	}

	for _, other := range []string{
		PrintID("printer-1", "job-1", "1"),
		PrintID("printer-2", "job-1", "0"),
		PrintID("printer-1", "job-2", "0"),
		PrintID("printer-1j", "ob-1", "0"),
	} {
		if other == a {
			t.Errorf("collision: %s", other)
		}
	}

	// sha256 of "printer-1", NUL, "job-1", NUL, "0", checked with Python's hashlib
	if want := "a3a4165b78a706d46b9178775c7fc3efe59075b675893a948708a72ff143267e"; a != want {
		t.Errorf("PrintID = %s, want %s", a, want)
	}
}

func TestGrams(t *testing.T) {
	t.Parallel()

	cases := []struct{ start, now, want float64 }{
		{1000, 980, 20},
		{500.26, 487.11, 13.2},
		{500, 500, 0},
		{500, 510, 0}, // weighed back up: never negative
		{0, 0, 0},
	}

	for _, tc := range cases {
		if got := Grams(tc.start, tc.now); got != tc.want {
			t.Errorf("Grams(%v, %v) = %v, want %v", tc.start, tc.now, got, tc.want)
		}
	}
}

func TestToolNumber(t *testing.T) {
	t.Parallel()

	cases := map[string]*int{"0": new(0), "3": new(3), "0.2": new(2), "1.4": new(4), "x": nil, "": nil, "64": nil, "-1": nil}
	for in, want := range cases {
		got := ToolNumber(in)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("ToolNumber(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFileName(t *testing.T) {
	t.Parallel()

	cases := []struct{ display, path, want string }{
		{"Benchy", "/usb/BENCHY~1.BGC", "Benchy"},
		{"", "/usb/benchy_0.4n.bgcode", "benchy_0.4n.bgcode"},
		{"", "", ""},
		{"two\nlines", "", "two lines"},
		{strings.Repeat("a", 250), "", strings.Repeat("a", 200)},
		{strings.Repeat("😀", 150), "", strings.Repeat("😀", 100)},
	}

	for _, tc := range cases {
		if got := fileName(tc.display, tc.path); got != tc.want {
			t.Errorf("fileName(%q, %q) = %q, want %q", tc.display, tc.path, got, tc.want)
		}
	}
}

func TestOnSpool(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		events []SpoolEvent
		// printing puts spool 1 on a tool of an open print
		printing bool
		want     []float64
	}{
		{"linked", []SpoolEvent{{SpoolID: 1, Remaining: 812.34}}, false, []float64{812.3}},
		{"unlinked", []SpoolEvent{{SpoolID: 9, Remaining: 100}}, false, nil},
		{"deleted", []SpoolEvent{{SpoolID: 1, Remaining: 100, Deleted: true}}, false, nil},
		{"unchanged", []SpoolEvent{{SpoolID: 1, Remaining: 800}, {SpoolID: 1, Remaining: 800.01}}, false, []float64{800}},
		{"newer replaces older", []SpoolEvent{{SpoolID: 1, Remaining: 800}, {SpoolID: 1, Remaining: 799}}, false, []float64{799}},
		{"negative", []SpoolEvent{{SpoolID: 1, Remaining: -3}}, false, []float64{0}},
		{"while printing", []SpoolEvent{{SpoolID: 1, Remaining: 790}}, true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, m, _, _ := setup(t)
			ctx := context.Background()

			if tc.printing {
				m.tools["p1"] = []Tool{{DeviceID: "p1", ToolID: "0", SpoolID: id(1), Remaining: 800}}
				if err := b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"}); err != nil {
					t.Fatal(err)
				}
			}

			for _, ev := range tc.events {
				if err := b.OnSpool(ctx, ev); err != nil {
					t.Fatal(err)
				}
			}

			var got []float64
			for _, e := range pending(t, b) {
				if e.Body.Print != nil || e.Spool != linkTo("s1") {
					t.Errorf("unexpected entry %+v", e)
				}
				got = append(got, *e.Body.Remaining)
			}

			if !equal(got, tc.want) {
				t.Errorf("queued %v, want %v", got, tc.want)
			}
		})
	}
}

func equal(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestPrints(t *testing.T) {
	t.Parallel()

	type queued struct {
		spool     string
		remaining float64
		print     bool
		grams     float64
		tool      string
	}

	cases := []struct {
		name    string
		start   []Tool
		end     []Tool
		spools  map[int64]MaterialsSpool
		outcome string
		want    []queued
	}{
		{
			name:    "finished",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 776.55}},
			outcome: Finished,
			want:    []queued{{"s1", 776.6, true, 23.4, "0"}},
		},
		{
			name:    "failed counts too",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 790}},
			outcome: Failed,
			want:    []queued{{"s1", 790, true, 10, "0"}},
		},
		{
			name:    "no drop",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			outcome: Finished,
			want:    []queued{{"s1", 800, true, 0, "0"}},
		},
		{
			name:    "spool changed mid-print",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0", SpoolID: id(2), Remaining: 950}},
			spools:  map[int64]MaterialsSpool{1: {ID: 1, Remaining: 640}},
			outcome: Finished,
			// spool 2 wasn't on a tool at the start, so its amount goes as usual
			want: []queued{{"s2", 950, false, 0, ""}, {"s1", 640, true, 0, "0"}},
		},
		{
			name:    "spool taken off and gone",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0"}},
			outcome: Finished,
			want:    []queued{{"s1", 800, true, 0, "0"}},
		},
		{
			name:    "two tools, one linked",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}, {ToolID: "1", SpoolID: id(7), Remaining: 500}},
			end:     []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 790}, {ToolID: "1", SpoolID: id(7), Remaining: 480}},
			outcome: Finished,
			want:    []queued{{"s1", 790, true, 10, "0"}},
		},
		{
			name:    "mmu slots",
			start:   []Tool{{ToolID: "0.0", SpoolID: id(1), Remaining: 800}, {ToolID: "0.3", SpoolID: id(2), Remaining: 300}},
			end:     []Tool{{ToolID: "0.0", SpoolID: id(1), Remaining: 795}, {ToolID: "0.3", SpoolID: id(2), Remaining: 290}},
			outcome: Finished,
			want:    []queued{{"s1", 795, true, 5, "0.0"}, {"s2", 290, true, 10, "0.3"}},
		},
		{
			name:    "cancelled sends the amount left only",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 795}},
			outcome: Cancelled,
			want:    []queued{{"s1", 795, false, 0, ""}},
		},
		{
			name:    "unknown outcome sends the amount left only",
			start:   []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}},
			end:     []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 795}},
			outcome: Unknown,
			want:    []queued{{"s1", 795, false, 0, ""}},
		},
		{
			name:    "no spools on the printer",
			start:   []Tool{{ToolID: "0"}},
			end:     []Tool{{ToolID: "0"}},
			outcome: Finished,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, m, c, kv := setup(t)
			ctx := context.Background()

			m.tools["p1"] = tc.start
			if err := b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1", DisplayName: "Benchy"}); err != nil {
				t.Fatal(err)
			}

			// spool events while printing wait for the print
			for _, tool := range tc.end {
				if tool.SpoolID != nil {
					if err := b.OnSpool(ctx, SpoolEvent{SpoolID: *tool.SpoolID, Remaining: tool.Remaining}); err != nil {
						t.Fatal(err)
					}
				}
			}

			// a restart mid-print: a new bridge over the same store
			c.advance(time.Hour)
			b = New(kv, m, quiet, c.now)
			m.tools["p1"], m.spools = tc.end, tc.spools
			ended := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
			if err := b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: tc.outcome, Ended: ended}); err != nil {
				t.Fatal(err)
			}

			var got []queued
			for _, e := range pending(t, b) {
				q := queued{spool: e.Spool.Spool, remaining: *e.Body.Remaining}
				if p := e.Body.Print; p != nil {
					q.print, q.grams = true, p.Grams
					q.tool = map[string]string{
						PrintID("p1", "j1", "0"): "0", PrintID("p1", "j1", "0.0"): "0.0", PrintID("p1", "j1", "0.3"): "0.3",
					}[p.Print]
					if p.File != "Benchy" || p.Date != ended.Local().Format(time.DateOnly) {
						t.Errorf("print = %+v", p)
					}
				}
				got = append(got, q)
			}

			if len(got) != len(tc.want) {
				t.Fatalf("queued %+v, want %+v", got, tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("queued[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}

			if jobs, _ := b.Jobs(ctx); len(jobs) != 0 {
				t.Errorf("the job is still kept: %+v", jobs)
			}

			// the same amount reported again after the print sends nothing new
			before := len(pending(t, b))
			for _, tool := range tc.end {
				if tool.SpoolID != nil {
					_ = b.OnSpool(ctx, SpoolEvent{SpoolID: *tool.SpoolID, Remaining: tool.Remaining})
				}
			}
			if tc.spools == nil && len(pending(t, b)) != before {
				t.Errorf("a repeated amount was queued again")
			}
		})
	}
}

func TestPrintToolNumberAndMMUSlot(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0.3", SpoolID: id(1), Remaining: 100}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1", Path: "/usb/cube.bgcode"})
	m.tools["p1"] = []Tool{{ToolID: "0.3", SpoolID: id(1), Remaining: 90}}
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print.Tool == nil || *e[0].Body.Print.Tool != 3 || e[0].Body.Print.File != "cube.bgcode" {
		t.Fatalf("queued %+v", e)
	}
}

func TestSettledWithoutStartRecordsNothing(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 100}}
	if err := b.OnJobSettled(context.Background(), "p1", JobSettled{JobID: "j1", Outcome: Finished}); err != nil {
		t.Fatal(err)
	}

	if e := pending(t, b); len(e) != 0 {
		t.Errorf("queued %+v", e)
	}
}

func TestStartedTwiceKeepsTheFirstReading(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 700}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 650}}
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print.Grams != 150 {
		t.Fatalf("queued %+v", e)
	}
}

func TestPrintSupersedesAQueuedAmount(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	_ = b.OnSpool(ctx, SpoolEvent{SpoolID: 1, Remaining: 820})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 780}}
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print == nil || *e[0].Body.Remaining != 780 {
		t.Fatalf("queued %+v", e)
	}
}

func TestLinks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	l := NewLinks(NewMemKV())

	bad := []Link{
		{MaterialsSpool: 0, Spool: linkTo("s1")},
		{MaterialsSpool: 1, Spool: spoolbook.Spool{Vendor: "v", Filament: "a/b", Spool: "s"}},
		{MaterialsSpool: 1, Spool: spoolbook.Spool{Vendor: "", Filament: "f", Spool: "s"}},
	}
	for _, link := range bad {
		if err := l.Put(ctx, link); err == nil {
			t.Errorf("Put(%+v) accepted", link)
		}
	}

	_ = l.Put(ctx, Link{MaterialsSpool: 12, Spool: linkTo("a")})
	_ = l.Put(ctx, Link{MaterialsSpool: 3, Spool: linkTo("b")})

	all, _ := l.List(ctx)
	if len(all) != 2 {
		t.Fatalf("List = %+v", all)
	}

	if err := l.Delete(ctx, 3); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Get(ctx, 3); err != ErrNotFound {
		t.Errorf("Get after Delete: %v", err)
	}

	if _, err := ParseSpoolID("12"); err != nil {
		t.Error(err)
	}

	for _, s := range []string{"", "0", "-2", "x"} {
		if _, err := ParseSpoolID(s); err == nil {
			t.Errorf("ParseSpoolID(%q) accepted", s)
		}
	}
}
