package bridge

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

// Sender makes one spoolbook write (spoolbook.Client).
type Sender interface {
	Send(ctx context.Context, spool spoolbook.Spool, update spoolbook.Update) (spoolbook.Result, error)
}

// Entry is one queued write.
type Entry struct {
	// Key is where it is kept: one per spool for an amount left (a newer one
	// replaces it), one per print id for a print.
	Key   string           `json:"key"`
	Spool spoolbook.Spool  `json:"spool"`
	Body  spoolbook.Update `json:"body"`
	// Seq orders entries: older first.
	Seq       int64     `json:"seq"`
	Attempts  int       `json:"attempts"`
	NotBefore time.Time `json:"not_before"`
	LastError string    `json:"last_error,omitempty"`
}

const (
	outboxPrefix = "outbox/"
	seqKey       = "outbox-seq"
)

// Backoff for a retry: Base doubled per attempt, at most Max.
const (
	BackoffBase = 30 * time.Second
	BackoffMax  = time.Hour
)

// Backoff is the wait before the given attempt (1 is the first retry).
func Backoff(attempt int) time.Duration {
	d := BackoffBase
	for i := 1; i < attempt && d < BackoffMax; i++ {
		d *= 2
	}

	return min(d, BackoffMax)
}

// Outbox keeps writes until spoolbook takes them.
type Outbox struct {
	kv  KV
	log *slog.Logger
	now func() time.Time

	mu  sync.Mutex
	seq int64
}

// NewOutbox returns the outbox in kv.
func NewOutbox(kv KV, log *slog.Logger, now func() time.Time) *Outbox {
	return &Outbox{kv: kv, log: log, now: now}
}

// remainingKey is per spool. Spool ids can be long, and a key is at most 150
// bytes, so it is a hash.
func remainingKey(spool spoolbook.Spool) string {
	sum := sha256.Sum256([]byte(spool.String()))

	return outboxPrefix + "r/" + hex.EncodeToString(sum[:16])
}

func printKey(id string) string { return outboxPrefix + "p/" + id }

// nextSeq hands out increasing numbers, kept in the store so they keep rising
// across a restart. Call it with mu held.
func (o *Outbox) nextSeq(ctx context.Context) (int64, error) {
	if o.seq == 0 {
		raw, err := o.kv.Get(ctx, seqKey)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return 0, err
		default:
			o.seq, _ = strconv.ParseInt(string(raw), 10, 64)
		}
	}

	o.seq++

	return o.seq, o.kv.Put(ctx, seqKey, []byte(strconv.FormatInt(o.seq, 10)))
}

// Remaining queues a spool's amount left, after delay. A newer one for the
// same spool replaces an older one that hasn't gone yet.
func (o *Outbox) Remaining(ctx context.Context, spool spoolbook.Spool, grams float64, delay time.Duration) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	seq, err := o.nextSeq(ctx)
	if err != nil {
		return err
	}

	g := grams

	return putJSON(ctx, o.kv, remainingKey(spool), Entry{
		Key: remainingKey(spool), Spool: spool, Body: spoolbook.Update{Remaining: &g},
		Seq: seq, NotBefore: o.now().Add(delay),
	})
}

// Print queues a print with the amount left after it. It supersedes a queued
// amount left for the spool, which is older.
func (o *Outbox) Print(ctx context.Context, spool spoolbook.Spool, remaining float64, print spoolbook.Print) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	seq, err := o.nextSeq(ctx)
	if err != nil {
		return err
	}

	if err := o.kv.Delete(ctx, remainingKey(spool)); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	r, p := remaining, print

	return putJSON(ctx, o.kv, printKey(print.Print), Entry{
		Key: printKey(print.Print), Spool: spool, Body: spoolbook.Update{Remaining: &r, Print: &p},
		Seq: seq, NotBefore: o.now(),
	})
}

// Pending returns every queued entry, oldest first.
func (o *Outbox) Pending(ctx context.Context) ([]Entry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.pending(ctx)
}

func (o *Outbox) pending(ctx context.Context) ([]Entry, error) {
	keys, err := o.kv.List(ctx, outboxPrefix)
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(keys))
	for _, key := range keys {
		var e Entry
		if err := getJSON(ctx, o.kv, key, &e); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}

			return nil, fmt.Errorf("read %s: %w", key, err)
		}

		entries = append(entries, e)
	}

	sortEntries(entries)

	return entries, nil
}

func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Seq, b.Seq) })
}

// Flush sends what is due, oldest first. A spool whose older entry isn't sent
// yet holds its newer ones back, so an amount left never goes out of order.
// It answers when the next entry is due, zero for none.
func (o *Outbox) Flush(ctx context.Context, send Sender) (time.Duration, error) {
	entries, err := o.Pending(ctx)
	if err != nil {
		return 0, err
	}

	held := map[spoolbook.Spool]bool{}
	var next time.Time

	for _, e := range entries {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		if held[e.Spool] {
			continue
		}

		if now := o.now(); e.NotBefore.After(now) {
			held[e.Spool] = true
			if next.IsZero() || e.NotBefore.Before(next) {
				next = e.NotBefore
			}

			continue
		}

		_, sendErr := send.Send(ctx, e.Spool, e.Body)
		if sendErr == nil {
			o.done(ctx, e)
			continue
		}

		var failure *spoolbook.Error
		if errors.As(sendErr, &failure) && failure.Class == spoolbook.Permanent {
			// sent again, it would be refused again: say so once and drop it
			o.log.Error("spoolbook refused an update; dropped",
				"spool", e.Spool.String(), "status", failure.Status, "code", failure.Code, "error", failure.Error())
			o.done(ctx, e)

			continue
		}

		held[e.Spool] = true
		wait := Backoff(e.Attempts + 1)
		if failure != nil && failure.RetryAfter > wait {
			wait = failure.RetryAfter
		}

		at := o.retry(ctx, e, wait, sendErr)
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}

	if next.IsZero() {
		return 0, nil
	}

	return max(next.Sub(o.now()), time.Second), nil
}

// done removes a sent entry, unless a newer one replaced it meanwhile.
func (o *Outbox) done(ctx context.Context, e Entry) {
	o.mu.Lock()
	defer o.mu.Unlock()

	var held Entry
	if err := getJSON(ctx, o.kv, e.Key, &held); err != nil || held.Seq != e.Seq {
		return
	}

	if err := o.kv.Delete(ctx, e.Key); err != nil && !errors.Is(err, ErrNotFound) {
		o.log.Warn("could not remove a sent update", "key", e.Key, "error", err)
	}
}

// retry puts an entry back for later, unless a newer one replaced it.
func (o *Outbox) retry(ctx context.Context, e Entry, wait time.Duration, cause error) time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()

	var held Entry
	if err := getJSON(ctx, o.kv, e.Key, &held); err != nil || held.Seq != e.Seq {
		return time.Time{}
	}

	e.Attempts++
	e.NotBefore = o.now().Add(wait)
	e.LastError = cause.Error()

	if e.Attempts == 1 || e.Attempts%10 == 0 {
		o.log.Warn("spoolbook update failed; will retry",
			"spool", e.Spool.String(), "attempt", e.Attempts, "in", wait.String(), "error", cause.Error())
	}

	if err := putJSON(ctx, o.kv, e.Key, e); err != nil {
		o.log.Warn("could not keep a failed update", "key", e.Key, "error", err)
	}

	return e.NotBefore
}
