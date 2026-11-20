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
		{"401 dropped", failed(401, 0), false, 0},
		{"404 dropped", failed(404, 0), false, 0},
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
	p := spoolbook.Print{Print: PrintID("p", "j", "0"), Grams: 10}
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
	_ = o.Print(ctx, linkTo("s1"), 790, spoolbook.Print{Print: PrintID("p", "j", "0"), Grams: 10})

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
	p := spoolbook.Print{Print: PrintID("p", "j", "0"), Grams: 10}
	_ = o.Print(ctx, linkTo("s1"), 790, p)
	_ = o.Print(ctx, linkTo("s1"), 790, p)

	if left, _ := o.Pending(ctx); len(left) != 1 {
		t.Fatalf("pending %+v", left)
	}
}
