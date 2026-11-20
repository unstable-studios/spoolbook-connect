package bridge

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

// fakeSender answers from a script, per call, and records what it was sent.
type fakeSender struct {
	answers []error
	sent    []spoolbook.Update
	spools  []spoolbook.Spool
	// during runs inside Send, to change the outbox mid-send
	during func()
}

func (s *fakeSender) Send(_ context.Context, spool spoolbook.Spool, u spoolbook.Update) (spoolbook.Result, error) {
	s.sent = append(s.sent, u)
	s.spools = append(s.spools, spool)

	if s.during != nil {
		s.during()
		s.during = nil
	}

	if len(s.answers) == 0 {
		return spoolbook.Result{Changed: true}, nil
	}

	err := s.answers[0]
	s.answers = s.answers[1:]

	return spoolbook.Result{}, err
}

func failed(status int, retryAfter time.Duration) error {
	return &spoolbook.Error{Class: spoolbook.Classify(status), Status: status, RetryAfter: retryAfter}
}

var networkDown = &spoolbook.Error{Class: spoolbook.Retry, Err: errors.New("connection refused")}

func newOutbox() (*Outbox, *MemKV, *clock) {
	kv := NewMemKV()
	c := newClock()

	return NewOutbox(kv, quiet, c.now), kv, c
}

func TestBackoff(t *testing.T) {
	t.Parallel()

	cases := map[int]time.Duration{
		1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute,
		7: 32 * time.Minute, 8: time.Hour, 20: time.Hour,
	}
	for attempt, want := range cases {
		if got := Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestFlushRetryClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		answer    error
		kept      bool
		notBefore time.Duration
	}{
		{"sent", nil, false, 0},
		{"429 honors Retry-After", failed(http.StatusTooManyRequests, 5*time.Minute), true, 5 * time.Minute},
		{"429 short Retry-After takes the backoff", failed(http.StatusTooManyRequests, time.Second), true, 30 * time.Second},
		{"500", failed(500, 0), true, 30 * time.Second},
		{"503", failed(503, 0), true, 30 * time.Second},
		{"409", failed(409, 0), true, 30 * time.Second},
		{"network", networkDown, true, 30 * time.Second},
		{"429 Retry-After capped at an hour", failed(http.StatusTooManyRequests, 5*time.Hour), true, time.Hour},
		{"404 kept, tried slowly", failed(404, 0), true, MissingRetry},
		{"413 dropped", failed(413, 0), false, 0},
		{"422 dropped", failed(422, 0), false, 0},
		{"400 dropped", failed(400, 0), false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o, _, c := newOutbox()
			ctx := context.Background()
			_ = o.Remaining(ctx, linkTo("s1"), 500, 0)

			send := &fakeSender{answers: []error{tc.answer}}
			wait, err := o.Flush(ctx, send)
			if err != nil {
				t.Fatal(err)
			}

			left, _ := o.Pending(ctx)
			if (len(left) == 1) != tc.kept {
				t.Fatalf("pending = %+v, kept want %v", left, tc.kept)
			}

			if tc.kept {
				if got := left[0].NotBefore.Sub(c.now()); got != tc.notBefore {
					t.Errorf("next try in %v, want %v", got, tc.notBefore)
				}

				if wait != tc.notBefore || left[0].Attempts != 1 || left[0].LastError == "" {
					t.Errorf("wait=%v entry=%+v", wait, left[0])
				}
			}
		})
	}
}

func TestFlushBacksOffFurtherEachTime(t *testing.T) {
	t.Parallel()

	o, _, c := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	send := &fakeSender{answers: []error{networkDown, networkDown, networkDown}}

	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		_, _ = o.Flush(ctx, send)
		left, _ := o.Pending(ctx)
		if got := left[0].NotBefore.Sub(c.now()); got != want {
			t.Errorf("wait %v, want %v", got, want)
		}
		c.advance(want)
	}

	_, _ = o.Flush(ctx, send)
	if left, _ := o.Pending(ctx); len(left) != 0 || len(send.sent) != 4 {
		t.Errorf("pending %+v after %d sends", left, len(send.sent))
	}
}

func TestFlushWaitsForTheDelay(t *testing.T) {
	t.Parallel()

	o, _, c := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, RemainingDelay)

	send := &fakeSender{}
	wait, _ := o.Flush(ctx, send)
	if len(send.sent) != 0 || wait != RemainingDelay {
		t.Fatalf("sent %d, wait %v", len(send.sent), wait)
	}

	// a newer amount restarts the wait
	c.advance(time.Minute)
	_ = o.Remaining(ctx, linkTo("s1"), 490, RemainingDelay)
	c.advance(time.Minute)
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 0 {
		t.Fatal("sent before the newer amount's wait")
	}

	c.advance(time.Minute)
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 1 || *send.sent[0].Remaining != 490 {
		t.Fatalf("sent %+v", send.sent)
	}
}

func TestFlushKeepsEachSpoolInOrder(t *testing.T) {
	t.Parallel()

	o, _, _ := newOutbox()
	ctx := context.Background()
	p := spoolbook.Print{Print: PrintID("p", "j", time.Time{}, "0"), Grams: 10}
	_ = o.Print(ctx, linkTo("s1"), 790, p)
	_ = o.Remaining(ctx, linkTo("s1"), 780, 0)
	_ = o.Remaining(ctx, linkTo("s2"), 300, 0)

	// the print fails: s1's newer amount waits behind it, s2 goes
	send := &fakeSender{answers: []error{failed(503, 0)}}
	_, _ = o.Flush(ctx, send)

	if len(send.sent) != 2 || send.spools[0] != linkTo("s1") || send.sent[0].Print == nil || send.spools[1] != linkTo("s2") {
		t.Fatalf("sent %+v to %+v", send.sent, send.spools)
	}

	left, _ := o.Pending(ctx)
	if len(left) != 2 || left[0].Body.Print == nil || *left[1].Body.Remaining != 780 {
		t.Fatalf("pending %+v", left)
	}
}

func TestFlushKeepsAnEntryReplacedWhileSending(t *testing.T) {
	t.Parallel()

	o, _, _ := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)

	send := &fakeSender{during: func() { _ = o.Remaining(ctx, linkTo("s1"), 450, 0) }}
	_, _ = o.Flush(ctx, send)

	left, _ := o.Pending(ctx)
	if len(left) != 1 || *left[0].Body.Remaining != 450 {
		t.Fatalf("pending %+v", left)
	}
}

func TestOutboxSurvivesARestart(t *testing.T) {
	t.Parallel()

	o, kv, c := newOutbox()
	ctx := context.Background()
	_ = o.Print(ctx, linkTo("s1"), 790, spoolbook.Print{Print: PrintID("p", "j", time.Time{}, "0"), Grams: 10})

	// a new process over the same store: entries kept, order still rising
	o = NewOutbox(kv, quiet, c.now)
	_ = o.Remaining(ctx, linkTo("s2"), 100, 0)

	left, _ := o.Pending(ctx)
	if len(left) != 2 || left[0].Body.Print == nil || left[1].Seq <= left[0].Seq {
		t.Fatalf("pending %+v", left)
	}

	send := &fakeSender{}
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 2 {
		t.Fatalf("sent %d", len(send.sent))
	}

	if left, _ := o.Pending(ctx); len(left) != 0 {
		t.Fatalf("pending %+v", left)
	}
}

func TestPrintIsKeptOncePerID(t *testing.T) {
	t.Parallel()

	o, _, _ := newOutbox()
	ctx := context.Background()
	p := spoolbook.Print{Print: PrintID("p", "j", time.Time{}, "0"), Grams: 10}
	_ = o.Print(ctx, linkTo("s1"), 790, p)
	_ = o.Print(ctx, linkTo("s1"), 790, p)

	if left, _ := o.Pending(ctx); len(left) != 1 {
		t.Fatalf("pending %+v", left)
	}
}

func TestPauseHoldsTheQueueUntilASendWorks(t *testing.T) {
	t.Parallel()

	for _, status := range []int{401, 403, 302} {
		o, _, c := newOutbox()
		ctx := context.Background()
		_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
		_ = o.Remaining(ctx, linkTo("s2"), 300, 0)

		send := &fakeSender{answers: []error{failed(status, 0)}}
		wait, err := o.Flush(ctx, send)
		if err != nil {
			t.Fatal(err)
		}

		// the pass stops: s2 wasn't tried, nothing dropped, the pause shows
		if len(send.sent) != 1 || wait != PauseBase {
			t.Fatalf("%d: sent %d, wait %v", status, len(send.sent), wait)
		}

		if left, _ := o.Pending(ctx); len(left) != 2 {
			t.Fatalf("%d: pending %+v", status, left)
		}

		p, _ := o.Paused(ctx)
		if p == nil || p.Status != status || p.Error == "" {
			t.Fatalf("%d: pause %+v", status, p)
		}

		// still paused: nothing goes
		c.advance(PauseBase / 2)
		_, _ = o.Flush(ctx, send)
		if len(send.sent) != 1 {
			t.Fatalf("%d: sent while paused", status)
		}

		// refused again: a longer pause
		c.advance(PauseBase)
		send.answers = []error{failed(status, 0)}
		if wait, _ = o.Flush(ctx, send); wait != 2*PauseBase {
			t.Fatalf("%d: second pause %v", status, wait)
		}

		// then it works: the pause lifts and everything goes
		c.advance(2 * PauseBase)
		_, _ = o.Flush(ctx, send)
		if left, _ := o.Pending(ctx); len(left) != 0 {
			t.Fatalf("%d: pending %+v", status, left)
		}

		if p, _ := o.Paused(ctx); p != nil {
			t.Fatalf("%d: still paused", status)
		}
	}
}

func TestPauseCapsAtSixHours(t *testing.T) {
	t.Parallel()

	if pauseFor(1) != PauseBase || pauseFor(3) != 4*PauseBase || pauseFor(10) != PauseMax {
		t.Errorf("pauseFor: %v %v %v", pauseFor(1), pauseFor(3), pauseFor(10))
	}
}

func TestPauseKeptUnderTheSameSettings(t *testing.T) {
	t.Parallel()

	o, kv, c := newOutbox()
	ctx := context.Background()
	same := SettingsHash("https://s.example", "lib", "tok")
	_ = o.UseSettings(ctx, same)
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	_, _ = o.Flush(ctx, &fakeSender{answers: []error{failed(401, 0)}})

	// a restart with the same settings: still paused, same wait
	o = NewOutbox(kv, quiet, c.now)
	_ = o.UseSettings(ctx, same)
	p, _ := o.Paused(ctx)
	if p == nil || p.Attempts != 1 {
		t.Fatalf("pause = %+v", p)
	}

	send := &fakeSender{}
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 0 {
		t.Fatal("sent while paused")
	}

	// new settings lift it
	_ = o.UseSettings(ctx, SettingsHash("https://s.example", "lib", "new token"))
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 1 {
		t.Fatalf("sent %d after new settings", len(send.sent))
	}
}

func TestActivePause(t *testing.T) {
	t.Parallel()

	o, _, c := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	_, _ = o.Flush(ctx, &fakeSender{answers: []error{failed(403, 0)}})

	if p, _ := o.Active(ctx); p == nil {
		t.Fatal("not active while waiting")
	}

	// the wait is over and the update still waits: still active
	c.advance(PauseBase)
	if p, _ := o.Active(ctx); p == nil {
		t.Fatal("not active with updates waiting")
	}

	// nothing waiting any more: no notice
	_ = o.kv.Delete(ctx, remainingKey(linkTo("s1")))
	if p, _ := o.Active(ctx); p != nil {
		t.Fatalf("active = %+v", p)
	}
}

func TestMissingClockSurvivesANewerUpdate(t *testing.T) {
	t.Parallel()

	o, _, c := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	_, _ = o.Flush(ctx, &fakeSender{answers: []error{failed(404, 0)}})
	first, _ := o.Pending(ctx)

	c.advance(time.Minute)
	_ = o.Remaining(ctx, linkTo("s1"), 490, 0)
	left, _ := o.Pending(ctx)
	if left[0].MissingSince == nil || !left[0].MissingSince.Equal(*first[0].MissingSince) {
		t.Fatalf("missing since %v, was %v", left[0].MissingSince, first[0].MissingSince)
	}

	// a print for the spool keeps it too
	_ = o.Print(ctx, linkTo("s1"), 480, spoolbook.Print{Print: PrintID("p", "j", time.Time{}, "0")})
	left, _ = o.Pending(ctx)
	if len(left) != 1 || left[0].MissingSince == nil {
		t.Fatalf("pending %+v", left)
	}
}

func TestResumeLiftsAPause(t *testing.T) {
	t.Parallel()

	o, _, _ := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	_, _ = o.Flush(ctx, &fakeSender{answers: []error{failed(401, 0)}})

	if err := o.Resume(ctx); err != nil {
		t.Fatal(err)
	}

	send := &fakeSender{}
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 1 {
		t.Fatalf("sent %d after Resume", len(send.sent))
	}
}

func TestMissingSpoolIsDroppedAfterAWeek(t *testing.T) {
	t.Parallel()

	o, _, c := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	_ = o.Remaining(ctx, linkTo("s2"), 300, 0)

	send := &fakeSender{}
	for range 200 {
		send.answers = []error{failed(404, 0)}
		wait, _ := o.Flush(ctx, send)

		left, _ := o.Pending(ctx)
		if len(left) == 0 || left[0].Spool != linkTo("s1") {
			break
		}

		if wait < MissingRetry || wait > MissingMax {
			t.Fatalf("missing retry in %v", wait)
		}

		c.advance(wait)
	}

	left, _ := o.Pending(ctx)
	if len(left) != 0 {
		t.Fatalf("pending %+v", left)
	}

	if c.now().Sub(newClock().now()) < MissingFor {
		t.Errorf("dropped after %v", c.now().Sub(newClock().now()))
	}
}

func TestRateLimitStopsThePass(t *testing.T) {
	t.Parallel()

	o, _, c := newOutbox()
	ctx := context.Background()
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)
	_ = o.Remaining(ctx, linkTo("s2"), 300, 0)

	send := &fakeSender{answers: []error{failed(429, 2*time.Minute)}}
	wait, _ := o.Flush(ctx, send)
	if len(send.sent) != 1 || wait != 2*time.Minute {
		t.Fatalf("sent %d, wait %v", len(send.sent), wait)
	}

	// s2 isn't sent before the limit lifts either
	c.advance(time.Minute)
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 1 {
		t.Fatal("sent during the rate limit")
	}

	c.advance(time.Minute)
	_, _ = o.Flush(ctx, send)
	if len(send.sent) != 3 {
		t.Fatalf("sent %d after the limit", len(send.sent))
	}
}

func TestCorruptRecordIsMovedAside(t *testing.T) {
	t.Parallel()

	o, kv, _ := newOutbox()
	ctx := context.Background()
	_ = kv.Put(ctx, outboxPrefix+"r/garbage", []byte("{not json"))
	_ = o.Remaining(ctx, linkTo("s1"), 500, 0)

	send := &fakeSender{}
	if _, err := o.Flush(ctx, send); err != nil {
		t.Fatal(err)
	}

	if len(send.sent) != 1 {
		t.Fatalf("sent %d", len(send.sent))
	}

	if raw, err := kv.Get(ctx, badPrefix+outboxPrefix+"r/garbage"); err != nil || string(raw) != "{not json" {
		t.Errorf("quarantined %q, %v", raw, err)
	}

	if _, err := kv.Get(ctx, outboxPrefix+"r/garbage"); err != ErrNotFound {
		t.Errorf("still queued: %v", err)
	}
}
