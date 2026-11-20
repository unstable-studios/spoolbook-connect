package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/event"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
)

// Types is what the module subscribes to.
var Types = []string{event.TypeDeviceJobStarted, event.TypeDeviceJobSettled, TypeMaterialsSpool}

// Handler is what the events go to (bridge.Bridge).
type Handler interface {
	OnSpool(ctx context.Context, ev bridge.SpoolEvent) error
	OnJobStarted(ctx context.Context, deviceID string, ev bridge.JobStarted) error
	OnJobSettled(ctx context.Context, deviceID string, ev bridge.JobSettled) error
}

const cursorKey = "cursor"

// MaxAttempts is how often one event is handled again before it is skipped.
const MaxAttempts = 5

// Dispatcher hands each window's events to the bridge and keeps the cursor,
// so a restart resumes where it stopped.
type Dispatcher struct {
	Handler Handler
	KV      bridge.KV
	Log     *slog.Logger

	attempts map[string]int
}

// Cursor is where the last subscription stopped, empty for none. An error
// means the store didn't answer: subscribing from the head then would skip
// what was missed.
func (d *Dispatcher) Cursor(ctx context.Context) (string, error) {
	raw, err := d.KV.Get(ctx, cursorKey)
	if errors.Is(err, bridge.ErrNotFound) {
		return "", nil
	}

	return string(raw), err
}

// Window handles one window. An error ends the subscription without moving
// the cursor, so the window is read again; an event that keeps failing is
// skipped after MaxAttempts.
func (d *Dispatcher) Window(ctx context.Context, w busapi.Window) error {
	if d.attempts == nil {
		d.attempts = map[string]int{}
	}

	if w.Resync {
		d.Log.Warn("the event stream lost its place; prints in that gap aren't recorded")
	}

	for _, raw := range w.Events {
		var env event.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			d.Log.Warn("unreadable event; skipped", "error", err)
			continue
		}

		if err := d.handle(ctx, env); err != nil {
			d.attempts[env.ID]++
			if d.attempts[env.ID] < MaxAttempts {
				return fmt.Errorf("%s %s: %w", env.Type, env.ID, err)
			}

			d.Log.Error("an event kept failing; skipped", "type", env.Type, "id", env.ID, "error", err)
		}

		delete(d.attempts, env.ID)
	}

	if w.Cursor != "" {
		if err := d.KV.Put(ctx, cursorKey, []byte(w.Cursor)); err != nil {
			d.Log.Warn("could not keep the event cursor", "error", err)
		}
	}

	return nil
}

var errNotADevice = errors.New("the event names no printer")

func deviceOf(env event.Envelope) (string, error) {
	origin, err := event.ParseOrigin(env.Subject)
	if err != nil {
		if _, o, srcErr := event.ParseSourceURN(env.Source); srcErr == nil {
			origin, err = o, nil
		}
	}

	if err != nil || origin.Kind() != event.KindDevice || origin.ID() == "" {
		return "", errNotADevice
	}

	return origin.ID(), nil
}

func (d *Dispatcher) handle(ctx context.Context, env event.Envelope) error {
	switch env.Type {
	case TypeMaterialsSpool:
		var ev bridge.SpoolEvent
		if err := json.Unmarshal(env.Data, &ev); err != nil {
			d.Log.Warn("unreadable spool event; skipped", "error", err)
			return nil
		}

		return d.Handler.OnSpool(ctx, ev)

	case event.TypeDeviceJobStarted:
		device, err := deviceOf(env)
		if err != nil {
			d.Log.Warn("job.started without a printer; skipped", "id", env.ID)
			return nil
		}

		var ev bridge.JobStarted
		if err := json.Unmarshal(env.Data, &ev); err != nil {
			d.Log.Warn("unreadable job.started; skipped", "error", err)
			return nil
		}

		return d.Handler.OnJobStarted(ctx, device, ev)

	case event.TypeDeviceJobSettled:
		device, err := deviceOf(env)
		if err != nil {
			d.Log.Warn("job.settled without a printer; skipped", "id", env.ID)
			return nil
		}

		var ev bridge.JobSettled
		if err := json.Unmarshal(env.Data, &ev); err != nil {
			d.Log.Warn("unreadable job.settled; skipped", "error", err)
			return nil
		}

		return d.Handler.OnJobSettled(ctx, device, ev)
	}

	return nil
}
