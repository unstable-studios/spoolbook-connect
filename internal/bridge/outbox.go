package bridge

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	// MissingSince is when spoolbook first answered 404 for it.
	MissingSince *time.Time `json:"missing_since,omitempty"`
}

// Pause holds the whole queue while spoolbook refuses the setup itself (a
// revoked token, a role or limit, a moved address).
type Pause struct {
	Until    time.Time `json:"until"`
	Status   int       `json:"status"`
	Error    string    `json:"error"`
	Attempts int       `json:"attempts"`
	// Settings is a hash of the settings it was paused under: new settings
	// lift it.
	Settings string `json:"settings,omitempty"`
}

const (
	outboxPrefix = "outbox/"
	badPrefix    = "bad/"
	seqKey       = "outbox-seq"
	pauseKey     = "outbox-pause"
)

// A paused queue tries again after PauseBase, doubled each time, at most
// PauseMax.
const (
	PauseBase = 15 * time.Minute
	PauseMax  = 6 * time.Hour
)

// A spool spoolbook doesn't know is tried at least every MissingRetry, and
// dropped after MissingFor.
const (
	MissingRetry = time.Hour
	MissingMax   = 6 * time.Hour
	MissingFor   = 7 * 24 * time.Hour
)

func pauseFor(attempt int) time.Duration {
	d := PauseBase
	for i := 1; i < attempt && d < PauseMax; i++ {
		d *= 2
	}

	return min(d, PauseMax)
}

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
	// rateUntil holds every send after a 429: the limit is per token.
	rateUntil time.Time
	// settings is the hash of the settings in force.
	settings string
}

// SettingsHash is what a pause remembers of the settings: a hash, so the
// token isn't kept in the clear a second time.
func SettingsHash(url, library, token string) string {
	sum := sha256.Sum256([]byte(url + "\x00" + library + "\x00" + token))

	return hex.EncodeToString(sum[:])
}

// UseSettings says which settings are in force. A pause made under other
// settings is lifted; one made under these is kept, with its attempts and
// its wait.
func (o *Outbox) UseSettings(ctx context.Context, hash string) error {
	o.mu.Lock()
	o.settings = hash
	o.mu.Unlock()

	p, err := o.Paused(ctx)
	if err != nil || p == nil || p.Settings == hash {
		return err
	}

	o.log.Info("the settings changed; trying spoolbook again")

	return o.Resume(ctx)
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
	e := Entry{
		Key: remainingKey(spool), Spool: spool, Body: spoolbook.Update{Remaining: &g},
		Seq: seq, NotBefore: o.now().Add(delay),
	}

	// a 404's clock runs on for the spool, not for one update
	var older Entry
	if getJSON(ctx, o.kv, e.Key, &older) == nil && older.MissingSince != nil {
		e.MissingSince = older.MissingSince
		e.NotBefore = later(e.NotBefore, older.NotBefore)
	}

	return putJSON(ctx, o.kv, e.Key, e)
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

	r, p := remaining, print
	e := Entry{
		Key: printKey(print.Print), Spool: spool, Body: spoolbook.Update{Remaining: &r, Print: &p},
		Seq: seq, NotBefore: o.now(),
	}

	var older Entry
	if getJSON(ctx, o.kv, remainingKey(spool), &older) == nil && older.MissingSince != nil {
		e.MissingSince = older.MissingSince
		e.NotBefore = older.NotBefore
	}

	if err := o.kv.Delete(ctx, remainingKey(spool)); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	return putJSON(ctx, o.kv, e.Key, e)
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
		raw, err := o.kv.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", key, err)
		}

		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil || e.Key != key {
			o.quarantine(ctx, key, raw, err)
			continue
		}

		entries = append(entries, e)
	}

	sortEntries(entries)

	return entries, nil
}

// quarantine moves a record that can't be read out of the queue, to bad/, so
// it can't hold up the rest.
func (o *Outbox) quarantine(ctx context.Context, key string, raw []byte, cause error) {
	o.log.Error("unreadable queued update; moved aside", "key", key, "to", badPrefix+key, "error", cause)

	if err := o.kv.Put(ctx, badPrefix+key, raw); err != nil {
		o.log.Warn("could not keep the unreadable update", "key", key, "error", err)
	}

	if err := o.kv.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		o.log.Warn("could not remove the unreadable update", "key", key, "error", err)
	}
}

// Paused returns the pause in force, nil for none.
func (o *Outbox) Paused(ctx context.Context) (*Pause, error) {
	var p Pause
	err := getJSON(ctx, o.kv, pauseKey, &p)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return &p, nil
}

// Active is the pause while it holds the queue: its wait not over, or
// updates still waiting for the send that will tell. Nil otherwise.
func (o *Outbox) Active(ctx context.Context) (*Pause, error) {
	p, err := o.Paused(ctx)
	if err != nil || p == nil {
		return nil, err
	}

	if p.Until.After(o.now()) {
		return p, nil
	}

	pending, err := o.Pending(ctx)
	if err != nil || len(pending) == 0 {
		return nil, err
	}

	return p, nil
}

// Resume lifts a pause, as new settings or a successful send do.
func (o *Outbox) Resume(ctx context.Context) error {
	if err := o.kv.Delete(ctx, pauseKey); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	return nil
}

func (o *Outbox) pause(ctx context.Context, held *Pause, failure *spoolbook.Error) time.Time {
	o.mu.Lock()
	p := Pause{Status: failure.Status, Error: failure.Error(), Attempts: 1, Settings: o.settings}
	o.mu.Unlock()

	if held != nil {
		p.Attempts = held.Attempts + 1
	}

	p.Until = o.now().Add(pauseFor(p.Attempts))

	o.log.Error("spoolbook refused this module's setup; holding every update",
		"status", failure.Status, "code", failure.Code, "error", failure.Error(), "until", p.Until.Format(time.RFC3339))

	if err := putJSON(ctx, o.kv, pauseKey, p); err != nil {
		o.log.Warn("could not keep the pause", "error", err)
	}

	return p.Until
}

// Ready reports whether the store answers.
func (o *Outbox) Ready(ctx context.Context) error {
	_, err := o.kv.List(ctx, outboxPrefix)

	return err
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Seq, b.Seq) })
}

// Flush sends what is due, oldest first. A spool whose older entry isn't sent
// yet holds its newer ones back, so an amount left never goes out of order.
// A 429 or a paused queue stops the pass. It answers when to flush again,
// zero for nothing queued.
func (o *Outbox) Flush(ctx context.Context, send Sender) (time.Duration, error) {
	until := func(t time.Time) (time.Duration, error) { return max(t.Sub(o.now()), time.Second), nil }

	o.mu.Lock()
	rate := o.rateUntil
	o.mu.Unlock()

	if rate.After(o.now()) {
		return until(rate)
	}

	paused, err := o.Paused(ctx)
	if err != nil {
		return 0, err
	}

	if paused != nil && paused.Until.After(o.now()) {
		return until(paused.Until)
	}

	entries, err := o.Pending(ctx)
	if err != nil {
		return 0, err
	}

	held := map[spoolbook.Spool]bool{}
	var next time.Time
	soonest := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}

	for _, e := range entries {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		if held[e.Spool] {
			continue
		}

		if e.NotBefore.After(o.now()) {
			held[e.Spool] = true
			soonest(e.NotBefore)

			continue
		}

		_, sendErr := send.Send(ctx, e.Spool, e.Body)
		if sendErr == nil {
			o.done(ctx, e)

			if paused != nil {
				o.log.Info("spoolbook takes updates again")
				_ = o.Resume(ctx)
				paused = nil
			}

			continue
		}

		var failure *spoolbook.Error
		if !errors.As(sendErr, &failure) {
			failure = &spoolbook.Error{Class: spoolbook.Retry, Err: sendErr}
		}

		switch failure.Class {
		case spoolbook.Permanent:
			// sent again, it would be refused again: say so once and drop it
			o.log.Error("spoolbook refused an update; dropped",
				"spool", e.Spool.String(), "status", failure.Status, "code", failure.Code, "error", failure.Error())
			o.done(ctx, e)

			continue

		case spoolbook.Pause:
			return until(o.pause(ctx, paused, failure))

		case spoolbook.Missing:
			held[e.Spool] = true
			soonest(o.missing(ctx, e, failure))

			continue
		}

		held[e.Spool] = true
		wait := Backoff(e.Attempts + 1)
		if failure.Status == http.StatusTooManyRequests {
			wait = max(wait, min(failure.RetryAfter, spoolbook.MaxRetryAfter))
		}

		at := o.retry(ctx, e, wait, sendErr)
		soonest(at)

		if failure.Status == http.StatusTooManyRequests {
			// the limit is the token's: nothing else goes either
			o.mu.Lock()
			o.rateUntil = o.now().Add(wait)
			o.mu.Unlock()

			return until(o.now().Add(wait))
		}
	}

	if next.IsZero() {
		return 0, nil
	}

	return until(next)
}

// missing keeps an update for a spool spoolbook doesn't know, tried slowly,
// and drops it after MissingFor.
func (o *Outbox) missing(ctx context.Context, e Entry, failure *spoolbook.Error) time.Time {
	since := o.now()
	if e.MissingSince != nil {
		since = *e.MissingSince
	}

	if o.now().Sub(since) >= MissingFor {
		o.log.Error("spoolbook has no such spool; update dropped after a week",
			"spool", e.Spool.String(), "since", since.Format(time.RFC3339))
		o.done(ctx, e)

		return time.Time{}
	}

	e.MissingSince = &since
	wait := min(max(Backoff(e.Attempts+1), MissingRetry), MissingMax)

	return o.retry(ctx, e, wait, failure)
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
	e.MissingSince = cmp.Or(e.MissingSince, held.MissingSince)

	if e.Attempts == 1 || e.Attempts%10 == 0 {
		o.log.Warn("spoolbook update failed; will retry",
			"spool", e.Spool.String(), "attempt", e.Attempts, "in", wait.String(), "error", cause.Error())
	}

	if err := putJSON(ctx, o.kv, e.Key, e); err != nil {
		o.log.Warn("could not keep a failed update", "key", e.Key, "error", err)
	}

	return e.NotBefore
}
