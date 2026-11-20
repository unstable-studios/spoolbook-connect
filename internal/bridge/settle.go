package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

// DeviceJob is a print as Connect's own record holds it (device.jobs).
type DeviceJob struct {
	ID          string     `json:"id"`
	Path        string     `json:"path,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
	Started     time.Time  `json:"started"`
	Ended       *time.Time `json:"ended,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
}

// Devices reads each printer's current and last print from Connect.
type Devices interface {
	Jobs(ctx context.Context, deviceID string) (current, last *DeviceJob, err error)
}

// Settle is a print that settled, waiting for its weights to settle too.
type Settle struct {
	Job     Job       `json:"job"`
	Outcome string    `json:"outcome"`
	File    string    `json:"file,omitempty"`
	Ended   time.Time `json:"ended"`
	// Queued is when the settle arrived; GiveUpAfter counts from it.
	Queued   time.Time `json:"queued"`
	Attempts int       `json:"attempts,omitempty"`
}

const settlePrefix = "settle/"

func settleKey(deviceID, jobID string) string { return settlePrefix + deviceID + "/" + jobID }

// GiveUpAfter is how long a settle keeps retrying a Materials that won't
// answer before the print is dropped.
const GiveUpAfter = 24 * time.Hour

// cacheFor is how long a read of a printer's tools or jobs is reused by
// printing().
const cacheFor = 5 * time.Second

type cached[T any] struct {
	at    time.Time
	value T
}

type readCache struct {
	mu    sync.Mutex
	tools map[string]cached[[]Tool]
	jobs  map[string]cached[[2]*DeviceJob]
}

type settler struct {
	mu       sync.Mutex
	inflight map[string]bool
	wake     chan struct{}
}

func (b *Bridge) cachedTools(ctx context.Context, deviceID string) ([]Tool, error) {
	b.cache.mu.Lock()
	if c, ok := b.cache.tools[deviceID]; ok && b.Now().Sub(c.at) < cacheFor {
		b.cache.mu.Unlock()
		return c.value, nil
	}
	b.cache.mu.Unlock()

	tools, err := b.Materials.Tools(ctx, deviceID)
	if err != nil {
		return nil, err
	}

	b.cache.mu.Lock()
	if b.cache.tools == nil {
		b.cache.tools = map[string]cached[[]Tool]{}
	}
	b.cache.tools[deviceID] = cached[[]Tool]{at: b.Now(), value: tools}
	b.cache.mu.Unlock()

	return tools, nil
}

func (b *Bridge) cachedJobs(ctx context.Context, deviceID string) (current, last *DeviceJob, err error) {
	b.cache.mu.Lock()
	if c, ok := b.cache.jobs[deviceID]; ok && b.Now().Sub(c.at) < cacheFor {
		b.cache.mu.Unlock()
		return c.value[0], c.value[1], nil
	}
	b.cache.mu.Unlock()

	current, last, err = b.Devices.Jobs(ctx, deviceID)
	if err != nil {
		return nil, nil, err
	}

	b.cache.mu.Lock()
	if b.cache.jobs == nil {
		b.cache.jobs = map[string]cached[[2]*DeviceJob]{}
	}
	b.cache.jobs[deviceID] = cached[[2]*DeviceJob]{at: b.Now(), value: [2]*DeviceJob{current, last}}
	b.cache.mu.Unlock()

	return current, last, nil
}

// printing reports whether a spool belongs to a print still open: one whose
// settle is being worked out, or one that started with it on a tool, that
// Connect still has as the printer's current print, and whose tool still
// holds it.
func (b *Bridge) printing(ctx context.Context, spoolID int64) bool {
	settles, err := b.settles(ctx)
	if err != nil {
		b.Log.Warn("could not read the prints settling", "error", err)
		return true
	}

	for _, s := range settles {
		for _, t := range s.Job.Tools {
			if t.SpoolID == spoolID {
				return true
			}
		}
	}

	jobs, err := b.jobs(ctx)
	if err != nil {
		b.Log.Warn("could not read the prints in progress", "error", err)
		return true
	}

	for _, job := range jobs {
		if !holds(job, spoolID) {
			continue
		}

		if open, err := b.reconcile(ctx, job); err != nil {
			b.Log.Warn("could not read the printer's print", "printer", job.DeviceID, "error", err)
		} else if !open {
			// settled from Connect's record just now: the settle sends it
			if _, err := b.KV.Get(ctx, settleKey(job.DeviceID, job.JobID)); err == nil {
				return true
			}

			continue
		}

		tools, err := b.cachedTools(ctx, job.DeviceID)
		if err != nil {
			// the settle sends the amount if it is still printing
			b.Log.Warn("could not read the tools", "printer", job.DeviceID, "error", err)
			return true
		}

		for _, t := range job.Tools {
			if t.SpoolID != spoolID {
				continue
			}

			for _, now := range tools {
				if now.ToolID == t.ToolID && now.SpoolID != nil && *now.SpoolID == spoolID {
					return true
				}
			}
		}
	}

	return false
}

func holds(job Job, spoolID int64) bool {
	for _, t := range job.Tools {
		if t.SpoolID == spoolID {
			return true
		}
	}

	return false
}

// reconcile checks an open print against Connect's record of the printer.
// Still current: open. Connect's last print, ended: its settle was lost, so
// it is settled from that record. Neither: it is forgotten. Without Devices
// it is taken as open.
func (b *Bridge) reconcile(ctx context.Context, job Job) (bool, error) {
	if b.Devices == nil {
		return true, nil
	}

	current, last, err := b.cachedJobs(ctx, job.DeviceID)
	if err != nil {
		return true, err
	}

	if current != nil && current.ID == job.JobID {
		return true, nil
	}

	if last != nil && last.ID == job.JobID && last.Ended != nil {
		b.Log.Info("a print ended without its settle event; settled from Connect's record",
			"printer", job.DeviceID, "job", job.JobID, "outcome", last.Outcome)

		return false, b.OnJobSettled(ctx, job.DeviceID, JobSettled{
			JobID: job.JobID, Path: last.Path, DisplayName: last.DisplayName,
			Started: last.Started, Ended: *last.Ended, Outcome: last.Outcome,
		})
	}

	b.Log.Info("Connect no longer has this print; forgotten", "printer", job.DeviceID, "job", job.JobID)

	if err := b.KV.Delete(ctx, jobKey(job.DeviceID, job.JobID)); err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}

	return false, nil
}

// Reconcile checks every open print against Connect, so a lost settle is
// found even if its spool never reports again.
func (b *Bridge) Reconcile(ctx context.Context) {
	jobs, err := b.jobs(ctx)
	if err != nil {
		b.Log.Warn("could not read the prints in progress", "error", err)
		return
	}

	for _, job := range jobs {
		if _, err := b.reconcile(ctx, job); err != nil {
			b.Log.Warn("could not read the printer's print", "printer", job.DeviceID, "error", err)
		}
	}
}

func (b *Bridge) settles(ctx context.Context) ([]Settle, error) {
	keys, err := b.KV.List(ctx, settlePrefix)
	if err != nil {
		return nil, err
	}

	out := make([]Settle, 0, len(keys))
	for _, key := range keys {
		var s Settle
		if err := getJSON(ctx, b.KV, key, &s); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}

			b.Log.Warn("unreadable settle record; removed", "key", key, "error", err)
			_ = b.KV.Delete(ctx, key)

			continue
		}

		out = append(out, s)
	}

	return out, nil
}

// OnJobSettled keeps the print for the settle worker and returns: working out
// its grams waits for the weights, which mustn't hold up the event stream.
func (b *Bridge) OnJobSettled(ctx context.Context, deviceID string, ev JobSettled) error {
	key := jobKey(deviceID, ev.JobID)

	var job Job
	if err := getJSON(ctx, b.KV, key, &job); err != nil {
		if errors.Is(err, ErrNotFound) {
			b.Log.Info("a print settled that started before this module watched; not recorded",
				"printer", deviceID, "job", ev.JobID)

			return nil
		}

		return err
	}

	if job.JobStarted.IsZero() {
		job.JobStarted = ev.Started
	}

	file := fileName(ev.DisplayName, ev.Path)
	if file == "" {
		file = job.File
	}

	ended := ev.Ended
	if ended.IsZero() {
		ended = b.Now()
	}

	s := Settle{Job: job, Outcome: ev.Outcome, File: file, Ended: ended, Queued: b.Now()}
	if err := putJSON(ctx, b.KV, settleKey(deviceID, ev.JobID), s); err != nil {
		return err
	}

	if err := b.KV.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	b.wakeSettler()

	return nil
}

func (b *Bridge) wakeSettler() {
	b.settler.mu.Lock()
	w := b.settler.wake
	b.settler.mu.Unlock()

	if w != nil {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// RunSettles works out each settled print until ctx ends: those kept from
// before a restart, and each new one. Every few minutes it also reconciles
// the open prints with Connect.
func (b *Bridge) RunSettles(ctx context.Context) {
	wake := make(chan struct{}, 1)

	b.settler.mu.Lock()
	b.settler.wake = wake
	b.settler.inflight = map[string]bool{}
	b.settler.mu.Unlock()

	var wg sync.WaitGroup
	defer wg.Wait()

	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()

	for {
		settles, err := b.settles(ctx)
		if err != nil && ctx.Err() == nil {
			b.Log.Warn("could not read the prints settling", "error", err)
		}

		for _, s := range settles {
			key := settleKey(s.Job.DeviceID, s.Job.JobID)

			b.settler.mu.Lock()
			busy := b.settler.inflight[key]
			b.settler.inflight[key] = true
			b.settler.mu.Unlock()

			if busy {
				continue
			}

			wg.Go(func() {
				b.Settle(ctx, s)

				b.settler.mu.Lock()
				delete(b.settler.inflight, key)
				b.settler.mu.Unlock()
			})
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-tick.C:
			b.Reconcile(ctx)
		}
	}
}

// Settle works out one settled print, retrying Materials with backoff until
// it answers or GiveUpAfter has passed, then queues its updates and removes
// the record.
func (b *Bridge) Settle(ctx context.Context, s Settle) {
	key := settleKey(s.Job.DeviceID, s.Job.JobID)

	for ctx.Err() == nil {
		err := b.settle(ctx, s)
		if err == nil {
			break
		}

		if ctx.Err() != nil {
			return
		}

		s.Attempts++
		if b.Now().Sub(s.Queued) >= GiveUpAfter {
			b.Log.Error("could not read Materials for a day; print not recorded",
				"printer", s.Job.DeviceID, "job", s.Job.JobID, "error", err)

			break
		}

		wait := min(Backoff(s.Attempts)/3, 5*time.Minute)
		if s.Attempts == 1 || s.Attempts%10 == 0 {
			b.Log.Warn("could not settle a print; will retry",
				"printer", s.Job.DeviceID, "job", s.Job.JobID, "attempt", s.Attempts, "error", err)
		}

		_ = putJSON(ctx, b.KV, key, s)

		if b.Sleep(ctx, wait) != nil {
			return
		}
	}

	if err := b.KV.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		b.Log.Warn("could not remove a settled print", "key", key, "error", err)
	}

	b.wake()
}

// linkedTools are the print's tools whose spools are linked.
func (b *Bridge) linkedTools(ctx context.Context, job Job) ([]JobTool, map[int64]Link, error) {
	var tools []JobTool
	links := map[int64]Link{}

	for _, t := range job.Tools {
		link, err := b.Links.Get(ctx, t.SpoolID)
		if errors.Is(err, ErrNotFound) {
			continue
		}

		if err != nil {
			return nil, nil, err
		}

		tools = append(tools, t)
		links[t.SpoolID] = link
	}

	return tools, links, nil
}

func (b *Bridge) settle(ctx context.Context, s Settle) error {
	starts, links, err := b.linkedTools(ctx, s.Job)
	if err != nil {
		return err
	}

	if len(starts) == 0 {
		return nil
	}

	tools, err := b.settledTools(ctx, s.Job.DeviceID, starts)
	if err != nil {
		return err
	}

	now := map[string]Tool{}
	for _, t := range tools {
		now[t.ToolID] = t
	}

	type queued struct {
		link      Link
		remaining float64
		grams     float64
		tool      string
	}

	// read everything first: a Materials error retries the whole settle
	var out []queued
	for _, start := range starts {
		remaining, grams, err := b.used(ctx, start, now[start.ToolID])
		if err != nil {
			return err
		}

		out = append(out, queued{links[start.SpoolID], remaining, grams, start.ToolID})
	}

	record := s.Outcome == Finished || s.Outcome == Failed

	for _, q := range out {
		if record {
			p := spoolbook.Print{
				Print: PrintID(s.Job.DeviceID, s.Job.JobID, s.Job.JobStarted, q.tool),
				Tool:  b.toolNumber(q.tool),
				Date:  s.Ended.Local().Format(time.DateOnly),
				Grams: q.grams,
				File:  s.File,
			}
			if err := b.Outbox.Print(ctx, q.link.Spool, q.remaining, p); err != nil {
				return err
			}
		} else if q.link.Remaining == nil || *q.link.Remaining != q.remaining {
			if err := b.Outbox.Remaining(ctx, q.link.Spool, q.remaining, 0); err != nil {
				return err
			}
		}

		if err := b.Links.noted(ctx, q.link.MaterialsSpool, q.link.Spool, q.remaining); err != nil {
			return err
		}
	}

	return nil
}

// settledTools reads a printer's tools until the weights have settled: they
// changed since the first read, and two reads SettleEvery apart agree. Or
// else SettleFor has passed.
func (b *Bridge) settledTools(ctx context.Context, deviceID string, starts []JobTool) ([]Tool, error) {
	read := func() ([]Tool, string, error) {
		tools, err := b.Materials.Tools(ctx, deviceID)
		if err != nil {
			return nil, "", fmt.Errorf("read the tools of %s: %w", deviceID, err)
		}

		var sig strings.Builder
		for _, start := range starts {
			for _, t := range tools {
				if t.ToolID == start.ToolID {
					id := int64(0)
					if t.SpoolID != nil {
						id = *t.SpoolID
					}

					fmt.Fprintf(&sig, "%s=%d:%.1f;", t.ToolID, id, round(t.Remaining))
				}
			}
		}

		return tools, sig.String(), nil
	}

	tools, first, err := read()
	if err != nil {
		return nil, err
	}

	last := first
	for waited := time.Duration(0); waited < SettleFor; waited += SettleEvery {
		if err := b.Sleep(ctx, SettleEvery); err != nil {
			return nil, err
		}

		var now string
		if tools, now, err = read(); err != nil {
			return nil, err
		}

		if now != first && now == last {
			return tools, nil
		}

		last = now
	}

	return tools, nil
}
