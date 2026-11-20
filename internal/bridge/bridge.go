package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

// Tool is one printer tool as Materials answers it (GET /printers/tools).
type Tool struct {
	DeviceID string `json:"device_id"`
	// ToolID is the tool, or an MMU slot such as "0.1".
	ToolID string `json:"tool_id"`
	// SpoolID is nil when no spool is on the tool.
	SpoolID   *int64  `json:"spool_id,omitempty"`
	Remaining float64 `json:"remaining_weight_g"`
}

// MaterialsSpool is a spool as Materials answers it (GET /spools).
type MaterialsSpool struct {
	ID           int64   `json:"id"`
	UUID         string  `json:"uuid"`
	MaterialName string  `json:"material_name"`
	BrandName    string  `json:"brand_name"`
	Type         string  `json:"type"`
	ColorRGBA    string  `json:"color_rgba"`
	Remaining    float64 `json:"remaining_weight_g"`
	Archived     *string `json:"archived_at"`
}

// Materials reads the Materials extension.
type Materials interface {
	// Tools returns the tools of one printer.
	Tools(ctx context.Context, deviceID string) ([]Tool, error)
	// Spool returns one spool, and ErrNotFound for none.
	Spool(ctx context.Context, id int64) (MaterialsSpool, error)
}

// SpoolEvent is com.github.aux.prusa3d.materials.spool.
type SpoolEvent struct {
	SpoolID      int64   `json:"spool_id"`
	SpoolUUID    string  `json:"spool_uuid"`
	MaterialUUID string  `json:"material_uuid"`
	Remaining    float64 `json:"remaining_weight_g"`
	Deleted      bool    `json:"deleted,omitempty"`
}

// JobStarted is the part of com.prusa.connect.device.job.started used here.
type JobStarted struct {
	JobID       string    `json:"job_id"`
	Path        string    `json:"path,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Started     time.Time `json:"started"`
}

// JobSettled is the part of com.prusa.connect.device.job.settled used here.
type JobSettled struct {
	JobID       string    `json:"job_id"`
	Path        string    `json:"path,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Started     time.Time `json:"started"`
	Ended       time.Time `json:"ended"`
	Outcome     string    `json:"outcome"`
}

// The outcomes of a print, as event.JobFinished and the rest.
const (
	Finished  = "finished"
	Failed    = "failed"
	Cancelled = "cancelled"
	Unknown   = "unknown"
)

// Job is what each tool held when a print started.
type Job struct {
	DeviceID string    `json:"device_id"`
	JobID    string    `json:"job_id"`
	File     string    `json:"file,omitempty"`
	Tools    []JobTool `json:"tools"`
	// Started is when this module saw the start, which expiry counts from.
	Started time.Time `json:"started"`
	// JobStarted is the event's own start time, part of the print id.
	JobStarted time.Time `json:"job_started"`
}

// JobTool is one tool's spool at the start.
type JobTool struct {
	ToolID    string  `json:"tool_id"`
	SpoolID   int64   `json:"spool_id"`
	Remaining float64 `json:"remaining_weight_g"`
}

const jobPrefix = "job/"

func jobKey(deviceID, jobID string) string { return jobPrefix + deviceID + "/" + jobID }

// RemainingDelay is how long an amount left waits before it goes. A newer one
// restarts the wait, so a spool counting down sends once it settles.
const RemainingDelay = 2 * time.Minute

// JobExpiry is how long a print's start is kept without its settle.
const JobExpiry = 48 * time.Hour

// After a print, the weights are read until two reads SettleEvery apart agree,
// or SettleFor has passed: Materials may still be counting the last grams.
const (
	SettleEvery = 10 * time.Second
	SettleFor   = 2 * time.Minute
)

// Bridge handles the events.
type Bridge struct {
	KV        KV
	Links     *Links
	Outbox    *Outbox
	Materials Materials
	Log       *slog.Logger
	Now       func() time.Time
	// Sleep waits, for the weights to settle. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Wake is poked when something is queued. Optional.
	Wake func()

	toolsLogged sync.Map
}

// New wires a bridge over one store.
func New(kv KV, materials Materials, log *slog.Logger, now func() time.Time) *Bridge {
	return &Bridge{
		KV: kv, Links: NewLinks(kv), Outbox: NewOutbox(kv, log, now),
		Materials: materials, Log: log, Now: now, Sleep: sleep,
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bridge) wake() {
	if b.Wake != nil {
		b.Wake()
	}
}

// round keeps a tenth of a gram, as spoolbook does.
func round(g float64) float64 { return math.Round(g*10) / 10 }

// OnSpool queues a linked spool's new amount left. A spool on a tool that is
// printing waits for the print to settle, which sends the amount with it.
func (b *Bridge) OnSpool(ctx context.Context, ev SpoolEvent) error {
	if ev.Deleted || ev.SpoolID <= 0 {
		return nil
	}

	link, err := b.Links.Get(ctx, ev.SpoolID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	remaining := round(max(ev.Remaining, 0))
	if link.Remaining != nil && *link.Remaining == remaining {
		return nil
	}

	if b.printing(ctx, ev.SpoolID) {
		return nil
	}

	if err := b.Outbox.Remaining(ctx, link.Spool, remaining, RemainingDelay); err != nil {
		return err
	}

	b.wake()

	return b.Links.noted(ctx, link.MaterialsSpool, link.Spool, remaining)
}

// printing reports whether a spool is on a tool of a print still open: one
// that started with it there, and whose tool still holds it.
func (b *Bridge) printing(ctx context.Context, spoolID int64) bool {
	jobs, err := b.jobs(ctx)
	if err != nil {
		b.Log.Warn("could not read the prints in progress", "error", err)

		return true
	}

	for _, job := range jobs {
		for _, t := range job.Tools {
			if t.SpoolID != spoolID {
				continue
			}

			tools, err := b.Materials.Tools(ctx, job.DeviceID)
			if err != nil {
				// the settle sends the amount if it is still printing
				b.Log.Warn("could not read the tools", "printer", job.DeviceID, "error", err)

				return true
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

// jobs returns the prints in progress, removing any kept past JobExpiry.
func (b *Bridge) jobs(ctx context.Context) ([]Job, error) {
	keys, err := b.KV.List(ctx, jobPrefix)
	if err != nil {
		return nil, err
	}

	jobs := make([]Job, 0, len(keys))
	for _, key := range keys {
		var job Job
		if err := getJSON(ctx, b.KV, key, &job); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}

			b.Log.Warn("unreadable print record; removed", "key", key, "error", err)
			_ = b.KV.Delete(ctx, key)

			continue
		}

		if b.Now().Sub(job.Started) > JobExpiry {
			b.Log.Info("a print never settled; forgotten", "printer", job.DeviceID, "job", job.JobID)
			_ = b.KV.Delete(ctx, key)

			continue
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

// Jobs returns the prints in progress, as recorded at their start.
func (b *Bridge) Jobs(ctx context.Context) ([]Job, error) { return b.jobs(ctx) }

// OnJobStarted keeps each tool's spool and its amount left. A printer prints
// one job at a time, so an older one still kept for it never settled: it goes.
func (b *Bridge) OnJobStarted(ctx context.Context, deviceID string, ev JobStarted) error {
	if deviceID == "" || ev.JobID == "" {
		return errors.New("job.started names no printer or job")
	}

	key := jobKey(deviceID, ev.JobID)
	if _, err := b.KV.Get(ctx, key); err == nil {
		// a replay after a restart: the first reading is the one to keep
		return nil
	}

	tools, err := b.Materials.Tools(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("read the tools of %s: %w", deviceID, err)
	}

	older, err := b.KV.List(ctx, jobPrefix+deviceID+"/")
	if err != nil {
		return err
	}

	for _, k := range older {
		b.Log.Info("a print never settled; forgotten", "key", k)
		if err := b.KV.Delete(ctx, k); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}

	job := Job{
		DeviceID: deviceID, JobID: ev.JobID, File: fileName(ev.DisplayName, ev.Path),
		Started: b.Now(), JobStarted: ev.Started,
	}
	for _, t := range tools {
		if t.SpoolID != nil && *t.SpoolID > 0 {
			job.Tools = append(job.Tools, JobTool{ToolID: t.ToolID, SpoolID: *t.SpoolID, Remaining: t.Remaining})
		}
	}

	return putJSON(ctx, b.KV, key, job)
}

// settledTools reads a printer's tools until the job's spools read the same
// twice in a row, SettleEvery apart, or SettleFor has passed.
func (b *Bridge) settledTools(ctx context.Context, job Job) ([]Tool, error) {
	read := func() ([]Tool, string, error) {
		tools, err := b.Materials.Tools(ctx, job.DeviceID)
		if err != nil {
			return nil, "", fmt.Errorf("read the tools of %s: %w", job.DeviceID, err)
		}

		var sig strings.Builder
		for _, start := range job.Tools {
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

	tools, last, err := read()
	if err != nil || len(job.Tools) == 0 {
		return tools, err
	}

	for waited := time.Duration(0); waited < SettleFor; waited += SettleEvery {
		if err := b.Sleep(ctx, SettleEvery); err != nil {
			return nil, err
		}

		var now string
		if tools, now, err = read(); err != nil {
			return nil, err
		}

		if now == last {
			return tools, nil
		}

		last = now
	}

	return tools, nil
}

// OnJobSettled records a finished or failed print on each linked spool it
// used, with the drop in its amount left as grams.
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

	tools, err := b.settledTools(ctx, job)
	if err != nil {
		return err
	}

	now := map[string]Tool{}
	for _, t := range tools {
		now[t.ToolID] = t
	}

	record := ev.Outcome == Finished || ev.Outcome == Failed
	file := fileName(ev.DisplayName, ev.Path)
	if file == "" {
		file = job.File
	}

	ended := ev.Ended
	if ended.IsZero() {
		ended = b.Now()
	}

	started := job.JobStarted
	if started.IsZero() {
		started = ev.Started
	}

	for _, start := range job.Tools {
		link, err := b.Links.Get(ctx, start.SpoolID)
		if errors.Is(err, ErrNotFound) {
			continue
		}

		if err != nil {
			return err
		}

		remaining, grams, err := b.used(ctx, start, now[start.ToolID])
		if err != nil {
			return err
		}

		if record {
			p := spoolbook.Print{
				Print: PrintID(deviceID, ev.JobID, started, start.ToolID),
				Tool:  b.toolNumber(start.ToolID),
				Date:  ended.Local().Format(time.DateOnly),
				Grams: grams,
				File:  file,
			}
			if err := b.Outbox.Print(ctx, link.Spool, remaining, p); err != nil {
				return err
			}
		} else if link.Remaining == nil || *link.Remaining != remaining {
			if err := b.Outbox.Remaining(ctx, link.Spool, remaining, 0); err != nil {
				return err
			}
		}

		if err := b.Links.noted(ctx, link.MaterialsSpool, link.Spool, remaining); err != nil {
			return err
		}
	}

	b.wake()

	if err := b.KV.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	return nil
}

// used is a spool's amount left now and what the print took off it. A spool
// no longer on its tool took nothing that can be told apart.
func (b *Bridge) used(ctx context.Context, start JobTool, now Tool) (remaining, grams float64, err error) {
	if now.SpoolID != nil && *now.SpoolID == start.SpoolID {
		remaining = round(max(now.Remaining, 0))

		return remaining, Grams(start.Remaining, now.Remaining), nil
	}

	spool, err := b.Materials.Spool(ctx, start.SpoolID)
	if errors.Is(err, ErrNotFound) {
		// gone from Materials: keep the amount it started with
		return round(max(start.Remaining, 0)), 0, nil
	}

	if err != nil {
		return 0, 0, fmt.Errorf("read spool %d: %w", start.SpoolID, err)
	}

	return round(max(spool.Remaining, 0)), 0, nil
}

// Grams is what a print took off a spool: the drop in its amount left, never
// below zero. Both are rounded first, so it matches the amounts spoolbook sees.
func Grams(start, now float64) float64 { return round(max(round(start)-round(now), 0)) }

// PrintID is the print's id in spoolbook: one per printer, job, start and
// tool, so a retry or a replay is the same print, and a job id a printer
// reuses after a reset is not.
func PrintID(deviceID, jobID string, started time.Time, toolID string) string {
	stamp := ""
	if !started.IsZero() {
		stamp = started.UTC().Format(time.RFC3339Nano)
	}

	sum := sha256.Sum256([]byte(deviceID + "\x00" + jobID + "\x00" + stamp + "\x00" + toolID))

	return hex.EncodeToString(sum[:])
}

// ToolNumber is spoolbook's tool number, as PrusaSlicer numbers filaments:
// "e" or "e.0" is extruder e, and "0.s" is MMU slot s on the one extruder.
// Anything else (an MMU on a tool past the first) has none: nil.
func ToolNumber(toolID string) *int {
	head, slot, mmu := strings.Cut(toolID, ".")

	e, err := strconv.Atoi(head)
	if err != nil || e < 0 {
		return nil
	}

	n := e
	if mmu {
		s, err := strconv.Atoi(slot)
		switch {
		case err != nil || s < 0:
			return nil
		case s == 0:
			n = e
		case e == 0:
			n = s
		default:
			return nil
		}
	}

	if n > 63 {
		return nil
	}

	return &n
}

// toolNumber is ToolNumber, logging once per tool id it can't number.
func (b *Bridge) toolNumber(toolID string) *int {
	n := ToolNumber(toolID)
	if n == nil {
		if _, seen := b.toolsLogged.LoadOrStore(toolID, true); !seen {
			b.Log.Info("no spoolbook tool number for this tool; prints from it carry none", "tool", toolID)
		}
	}

	return n
}

// fileName is what spoolbook shows for a print: the name the printer shows,
// else the file's own name. At most 200 characters, on one line.
func fileName(display, file string) string {
	name := strings.TrimSpace(display)
	if name == "" && file != "" {
		name = path.Base(file)
	}

	name = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}

		return r
	}, name)

	// spoolbook counts UTF-16 units, as JavaScript does
	units := 0
	for i, r := range name {
		units += max(utf16.RuneLen(r), 1)
		if units > 200 {
			return name[:i]
		}
	}

	return name
}
