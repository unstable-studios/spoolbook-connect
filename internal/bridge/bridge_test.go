package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
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

// fakeMaterials answers tools per printer and spools by id. readings, when
// set, are answered first, one per call, before tools.
type fakeMaterials struct {
	tools    map[string][]Tool
	readings [][]Tool
	spools   map[int64]MaterialsSpool
	calls    int
	// fail fails the next calls to Tools
	fail int
	// onTools runs once, at the next call to Tools
	onTools func()
}

func (m *fakeMaterials) Tools(_ context.Context, device string) ([]Tool, error) {
	m.calls++
	if m.onTools != nil {
		m.onTools()
		m.onTools = nil
	}

	if m.fail > 0 {
		m.fail--
		return nil, errors.New("materials down")
	}

	if len(m.readings) > 0 {
		r := m.readings[0]
		m.readings = m.readings[1:]

		return r, nil
	}

	return m.tools[device], nil
}

func (m *fakeMaterials) Spool(_ context.Context, n int64) (MaterialsSpool, error) {
	s, ok := m.spools[n]
	if !ok {
		return MaterialsSpool{}, ErrNotFound
	}

	return s, nil
}

// settled is OnJobSettled followed by the settle worker's work, in line.
func settled(b *Bridge, ctx context.Context, device string, ev JobSettled) error {
	if err := b.OnJobSettled(ctx, device, ev); err != nil {
		return err
	}

	settles, err := b.settles(ctx)
	if err != nil {
		return err
	}

	for _, s := range settles {
		b.Settle(ctx, s)
	}

	return nil
}

// newBridge is New with a sleep that moves the test clock.
func newBridge(kv KV, m Materials, c *clock) *Bridge {
	b := New(kv, m, quiet, c.now)
	b.Sleep = func(_ context.Context, d time.Duration) error {
		c.advance(d)
		return nil
	}

	return b
}

func setup(t *testing.T) (*Bridge, *fakeMaterials, *clock, *MemKV) {
	t.Helper()

	kv := NewMemKV()
	c := newClock()
	m := &fakeMaterials{tools: map[string][]Tool{}, spools: map[int64]MaterialsSpool{}}
	b := newBridge(kv, m, c)

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

	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	a := PrintID("printer-1", "job-1", at, "0")
	if len(a) != 64 || !spoolbook.PrintIDFormat.MatchString(a) {
		t.Fatalf("PrintID = %q, not 64 hex", a)
	}

	if a != PrintID("printer-1", "job-1", at.In(time.FixedZone("CET", 3600)), "0") {
		t.Error("not stable across time zones")
	}

	for _, other := range []string{
		PrintID("printer-1", "job-1", at, "1"),
		PrintID("printer-2", "job-1", at, "0"),
		PrintID("printer-1", "job-2", at, "0"),
		PrintID("printer-1j", "ob-1", at, "0"),
		// the printer reused its job id after a reset
		PrintID("printer-1", "job-1", at.Add(time.Hour), "0"),
	} {
		if other == a {
			t.Errorf("collision: %s", other)
		}
	}

	// sha256 of "printer-1", NUL, "job-1", NUL, "2026-11-19T10:00:00Z", NUL,
	// "0", checked with Python's hashlib
	if want := "3d32348b017d89267a8b35f9ce66642304f1b4422e3d43af130e6b860fa15221"; a != want {
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

	cases := map[string]*int{
		"0": new(0), "3": new(3), "0.0": new(0), "0.2": new(2), "2.0": new(2),
		"1.4": nil, "1.2": nil, "x": nil, "": nil, "64": nil, "-1": nil, "0.x": nil, "0.-1": nil,
	}
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

	started := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

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
			if err := b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1", DisplayName: "Benchy", Started: started}); err != nil {
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
			b = newBridge(kv, m, c)
			m.tools["p1"], m.spools = tc.end, tc.spools
			ended := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
			if err := settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: tc.outcome, Ended: ended}); err != nil {
				t.Fatal(err)
			}

			var got []queued
			for _, e := range pending(t, b) {
				q := queued{spool: e.Spool.Spool, remaining: *e.Body.Remaining}
				if p := e.Body.Print; p != nil {
					q.print, q.grams = true, p.Grams
					q.tool = map[string]string{
						PrintID("p1", "j1", started, "0"): "0", PrintID("p1", "j1", started, "0.0"): "0.0", PrintID("p1", "j1", started, "0.3"): "0.3",
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
	_ = settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print.Tool == nil || *e[0].Body.Print.Tool != 3 || e[0].Body.Print.File != "cube.bgcode" {
		t.Fatalf("queued %+v", e)
	}
}

func TestSettledWithoutStartRecordsNothing(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 100}}
	if err := settled(b, context.Background(), "p1", JobSettled{JobID: "j1", Outcome: Finished}); err != nil {
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
	_ = settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

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
	_ = settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

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

// An open print that never settles must not hold its spool's updates for good.
func TestStaleJobsDoNotBlockASpool(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// then runs after job j1 started with spool 1 on tool 0
		then func(b *Bridge, m *fakeMaterials, c *clock)
	}{
		{"expired after 48 h", func(_ *Bridge, _ *fakeMaterials, c *clock) { c.advance(JobExpiry + time.Minute) }},
		{"the printer started another print", func(b *Bridge, m *fakeMaterials, _ *clock) {
			m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(2), Remaining: 300}}
			_ = b.OnJobStarted(context.Background(), "p1", JobStarted{JobID: "j2"})
		}},
		{"the spool left the tool", func(_ *Bridge, m *fakeMaterials, _ *clock) {
			m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(2), Remaining: 300}}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, m, c, _ := setup(t)
			ctx := context.Background()
			m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
			if err := b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"}); err != nil {
				t.Fatal(err)
			}

			tc.then(b, m, c)

			if err := b.OnSpool(ctx, SpoolEvent{SpoolID: 1, Remaining: 650}); err != nil {
				t.Fatal(err)
			}

			e := pending(t, b)
			if len(e) != 1 || e[0].Spool != linkTo("s1") || *e[0].Body.Remaining != 650 {
				t.Fatalf("queued %+v", e)
			}
		})
	}
}

func TestOlderJobsOfAPrinterGoOnItsNextStart(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	m.tools["p2"] = []Tool{{ToolID: "0", SpoolID: id(2), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	_ = b.OnJobStarted(ctx, "p2", JobStarted{JobID: "k1"})
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j2"})

	jobs, _ := b.Jobs(ctx)
	if len(jobs) != 2 || jobs[0].JobID != "j2" || jobs[1].JobID != "k1" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestSettleWaitsForTheWeightToStop(t *testing.T) {
	t.Parallel()

	reading := func(g float64) []Tool { return []Tool{{ToolID: "0", SpoolID: id(1), Remaining: g}} }

	steps := 1 + int(SettleFor/SettleEvery)

	cases := []struct {
		name     string
		readings [][]Tool
		after    []Tool
		grams    float64
		reads    int
	}{
		// the first read is still the old weight: wait for it to move
		{"stale, then steady", [][]Tool{reading(800), reading(780), reading(780)}, nil, 20, 3},
		{"still counting", [][]Tool{reading(785), reading(781), reading(779), reading(779)}, nil, 21, 4},
		// already moved before the settle: two equal reads aren't enough,
		// it waits out SettleFor
		{"already moved", [][]Tool{reading(780)}, reading(780), 20, steps},
		{"never steady", func() [][]Tool {
			var r [][]Tool
			for i := range 20 {
				r = append(r, reading(790-float64(i)))
			}
			return r
		}(), nil, 22, steps},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, m, _, _ := setup(t)
			ctx := context.Background()
			m.tools["p1"] = reading(800)
			_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})

			m.calls, m.readings = 0, tc.readings
			if tc.after != nil {
				m.tools["p1"] = tc.after
			}

			if err := settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished}); err != nil {
				t.Fatal(err)
			}

			e := pending(t, b)
			if len(e) != 1 || e[0].Body.Print.Grams != tc.grams || m.calls != tc.reads {
				t.Fatalf("queued %+v after %d reads; want %v g after %d", e, m.calls, tc.grams, tc.reads)
			}
		})
	}
}

func TestNotedRereadsTheLink(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	l := NewLinks(NewMemKV())
	_ = l.Put(ctx, Link{MaterialsSpool: 1, Spool: linkTo("s1")})
	_ = l.Put(ctx, Link{MaterialsSpool: 2, Spool: linkTo("s2")})

	// relinked to another spoolbook spool meanwhile: the new link stays fresh
	_ = l.Put(ctx, Link{MaterialsSpool: 1, Spool: linkTo("other")})
	if err := l.noted(ctx, 1, linkTo("s1"), 500); err != nil {
		t.Fatal(err)
	}

	if got, _ := l.Get(ctx, 1); got.Spool != linkTo("other") || got.Remaining != nil {
		t.Errorf("link = %+v", got)
	}

	// unlinked meanwhile: not written back
	_ = l.Delete(ctx, 2)
	if err := l.noted(ctx, 2, linkTo("s2"), 500); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Get(ctx, 2); err != ErrNotFound {
		t.Errorf("an unlinked spool came back: %v", err)
	}

	if err := l.noted(ctx, 1, linkTo("other"), 400); err != nil {
		t.Fatal(err)
	}

	if got, _ := l.Get(ctx, 1); got.Remaining == nil || *got.Remaining != 400 {
		t.Errorf("link = %+v", got)
	}
}

func TestSettleWithoutLinkedToolsDoesNotWait(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(7), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})

	m.calls = 0
	_ = settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})
	if m.calls != 0 {
		t.Errorf("read the tools %d times for a print with no linked spool", m.calls)
	}
}

func TestSettleRetriesMaterials(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 790}}

	m.fail = 5
	_ = settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print.Grams != 10 {
		t.Fatalf("queued %+v", e)
	}

	if s, _ := b.settles(ctx); len(s) != 0 {
		t.Errorf("settle kept: %+v", s)
	}
}

func TestSettleGivesUpAfterADay(t *testing.T) {
	t.Parallel()

	b, m, c, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})

	start := c.now()
	m.fail = 1 << 30
	_ = settled(b, ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	if c.now().Sub(start) < GiveUpAfter {
		t.Errorf("gave up after %v", c.now().Sub(start))
	}

	if s, _ := b.settles(ctx); len(s) != 0 || len(pending(t, b)) != 0 {
		t.Errorf("settles %+v", s)
	}
}

func TestSettleIsQuickAndHoldsItsSpool(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})

	m.calls = 0
	if err := b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished}); err != nil {
		t.Fatal(err)
	}

	if m.calls != 0 {
		t.Errorf("the event path read Materials %d times", m.calls)
	}

	// while it settles, the spool's amount left waits for the print
	_ = b.OnSpool(ctx, SpoolEvent{SpoolID: 1, Remaining: 780})
	if e := pending(t, b); len(e) != 0 {
		t.Fatalf("queued %+v", e)
	}
}

func TestRunSettlesResumesAfterARestart(t *testing.T) {
	t.Parallel()

	b, m, c, kv := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	// a new process: the settle record is picked up
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 770}}
	b = newBridge(kv, m, c)
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.RunSettles(run); close(done) }()

	for range 1000 {
		if s, _ := b.settles(ctx); len(s) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	<-done

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print.Grams != 30 {
		t.Fatalf("queued %+v", e)
	}
}

type fakeDevices struct{ current, last *DeviceJob }

func (f fakeDevices) Jobs(context.Context, string) (*DeviceJob, *DeviceJob, error) {
	return f.current, f.last, nil
}

func TestReconcileWithConnectsRecord(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	ended := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
	earlier := started.Add(-24 * time.Hour)

	cases := []struct {
		name    string
		devices fakeDevices
		// noStart: the start event carried no time
		noStart bool
		// printing: spool 1's updates wait; settled: the print was recorded
		printing, settled bool
	}{
		{"still current", fakeDevices{current: &DeviceJob{ID: "j1", Started: started}}, false, true, false},
		{"another print now", fakeDevices{current: &DeviceJob{ID: "j9", Started: started}}, false, false, false},
		{"idle, last is another", fakeDevices{last: &DeviceJob{ID: "j0", Started: started, Ended: &ended}}, false, false, false},
		{"idle, settle lost", fakeDevices{last: &DeviceJob{ID: "j1", Started: started, Ended: &ended, Outcome: Finished, DisplayName: "Benchy"}}, false, true, true},
		// the same job id, from an older print: not this one
		{"reused id, last", fakeDevices{last: &DeviceJob{ID: "j1", Started: earlier, Ended: &ended, Outcome: Finished}}, false, false, false},
		{"reused id, current", fakeDevices{current: &DeviceJob{ID: "j1", Started: earlier}}, false, false, false},
		// no start time from the event: matched within the window of when
		// this module saw it start
		{"no start time, near", fakeDevices{last: &DeviceJob{ID: "j1", Started: newClock().now().Add(-time.Minute), Ended: &ended, Outcome: Finished}}, true, true, true},
		{"no start time, far", fakeDevices{last: &DeviceJob{ID: "j1", Started: earlier, Ended: &ended, Outcome: Finished}}, true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, m, _, _ := setup(t)
			b.Devices = tc.devices
			ctx := context.Background()
			m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
			ev := JobStarted{JobID: "j1", Started: started}
			if tc.noStart {
				ev.Started = time.Time{}
			}
			_ = b.OnJobStarted(ctx, "p1", ev)

			if got := b.printing(ctx, 1); got != tc.printing {
				t.Errorf("printing = %v, want %v", got, tc.printing)
			}

			settles, _ := b.settles(ctx)
			if (len(settles) == 1) != tc.settled {
				t.Fatalf("settles = %+v", settles)
			}

			if tc.settled {
				if s := settles[0]; s.Outcome != Finished || !s.Ended.Equal(ended) {
					t.Errorf("settle = %+v", s)
				}
			}

			if jobs, _ := b.Jobs(ctx); len(jobs) != map[bool]int{true: 1}[tc.printing && !tc.settled] {
				t.Errorf("jobs = %+v", jobs)
			}
		})
	}
}

func TestStartForgetsCachedJobs(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	started := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	d := &switchDevices{current: &DeviceJob{ID: "j0", Started: started}}
	b.Devices = d
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}

	// cached: the printer's current print is j0
	_, _, _ = b.cachedJobs(ctx, "p1")

	d.current = &DeviceJob{ID: "j1", Started: started}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1", Started: started})

	if !b.printing(ctx, 1) {
		t.Error("a stale cached answer forgot the new print")
	}
}

type switchDevices struct{ current *DeviceJob }

func (s *switchDevices) Jobs(context.Context, string) (*DeviceJob, *DeviceJob, error) {
	return s.current, nil, nil
}

func TestSettleOnlyOnce(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})

	// the event and a reconcile, at once
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished}) })
	}
	wg.Wait()

	settles, _ := b.settles(ctx)
	if len(settles) != 1 {
		t.Fatalf("settles = %+v", settles)
	}

	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 790}}
	b.Settle(ctx, settles[0])

	// a second run from an older listing does nothing
	b.Settle(ctx, settles[0])

	e := pending(t, b)
	if len(e) != 1 || e[0].Body.Print.Grams != 10 {
		t.Fatalf("queued %+v", e)
	}

	if s, _ := b.settles(ctx); len(s) != 0 {
		t.Errorf("settles = %+v", s)
	}
}

func TestSettleRetryDoesNotBringBackAFinishedRecord(t *testing.T) {
	t.Parallel()

	b, m, _, kv := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})
	settles, _ := b.settles(ctx)

	// another run finishes it while this one's read is failing
	m.fail = 1
	m.onTools = func() { _ = kv.Delete(ctx, settleKey("p1", "j1")) }

	b.Settle(ctx, settles[0])

	if s, _ := b.settles(ctx); len(s) != 0 {
		t.Errorf("brought back: %+v", s)
	}

	if e := pending(t, b); len(e) != 0 {
		t.Errorf("queued %+v", e)
	}
}

func TestSettleQueuesAnAmountHeldBack(t *testing.T) {
	t.Parallel()

	b, m, _, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 790}}
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	// reported while it settled: held back
	_ = b.OnSpool(ctx, SpoolEvent{SpoolID: 1, Remaining: 790})
	settles, _ := b.settles(ctx)

	// then an unload took 5 g more before the worker's last read
	m.spools[1] = MaterialsSpool{ID: 1, Remaining: 785}
	b.Settle(ctx, settles[0])

	e := pending(t, b)
	if len(e) != 2 || e[0].Body.Print == nil || e[1].Body.Print != nil || *e[1].Body.Remaining != 785 {
		t.Fatalf("queued %+v", e)
	}
}

func TestSettleAcceptsWhatMaterialsSaid(t *testing.T) {
	t.Parallel()

	b, m, c, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 780}}
	_ = b.OnJobSettled(ctx, "p1", JobSettled{JobID: "j1", Outcome: Finished})

	// Materials told of 780 after the settle: the first read agrees
	_ = b.OnSpool(ctx, SpoolEvent{SpoolID: 1, Remaining: 780})
	settles, _ := b.settles(ctx)

	start := c.now()
	m.calls = 0
	b.Settle(ctx, settles[0])

	if waited := c.now().Sub(start); waited != 0 || m.calls != 1 {
		t.Errorf("waited %v over %d reads", waited, m.calls)
	}

	if e := pending(t, b); len(e) != 1 || e[0].Body.Print.Grams != 20 {
		t.Fatalf("queued %+v", e)
	}
}

func TestPrintingCachesTheToolsRead(t *testing.T) {
	t.Parallel()

	b, m, c, _ := setup(t)
	ctx := context.Background()
	m.tools["p1"] = []Tool{{ToolID: "0", SpoolID: id(1), Remaining: 800}}
	_ = b.OnJobStarted(ctx, "p1", JobStarted{JobID: "j1"})

	m.calls = 0
	for range 5 {
		b.printing(ctx, 1)
	}

	if m.calls != 1 {
		t.Errorf("read the tools %d times in 5 s", m.calls)
	}

	c.advance(cacheFor)
	b.printing(ctx, 1)
	if m.calls != 2 {
		t.Errorf("not read again after %v", cacheFor)
	}
}
