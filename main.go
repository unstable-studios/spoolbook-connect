// spoolbook-connect, by Unstable Studios. Built on Prusa Connect by Prusa
// Research; licensed under OCL v1.1 + Micro v1 + NoSaaS v1 + SWAtt v1 (LICENSE).
//
// Command spoolbook-connect is a Prusa Connect Local module that keeps
// spoolbook's spools in step with the Materials extension: a linked spool's
// amount left, and the prints that used it.
//
// This file is the SDK glue: the handshake, the settings, the event stream
// and the routes. What happens to an event is in internal/bridge.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/extension"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"
	"github.com/prusa3d/prusa-connect/sdk/stateapi"
	"github.com/prusa3d/prusa-connect/sdk/wire"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
	"github.com/unstable-studios/spoolbook-connect/internal/connect"
	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

const name = "spoolbook"

// version is the release, kept equal to module.yaml's by release-please.
const version = "0.1.1" // x-release-please-version

//go:embed openapi.json
var openAPI []byte

// Settings are module.yaml's options.
type Settings struct {
	SpoolbookURL string `json:"spoolbook_url"`
	Token        string `json:"token"`
	Library      string `json:"library"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// stderr goes to the host's log, which stamps the time; stdout is the wire
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	}))

	line, err := wire.Handshake{
		Protocol: wire.ProtocolMajor,
		MinMinor: wire.MinMinor,
		MaxMinor: wire.MaxMinor,
		Address:  wire.StdioAddress,
	}.Line()
	if err != nil {
		return err
	}

	if _, err = os.Stdout.WriteString(line); err != nil {
		return err
	}

	serves := wirerpc.Methods{}

	conn, err := wirerpc.NewConn(stdio{}, wirerpc.Options{Role: wirerpc.RoleChild, Methods: serves})
	if err != nil {
		return err
	}

	kv := connect.KV{Records: stateapi.NewClient(conn)}
	materials := connect.Materials{Caller: routesapi.NewClient(conn)}
	b := bridge.New(kv, materials, log, time.Now)
	b.Devices = connect.Devices{Fleet: deviceapi.NewClient(conn)}

	wake := make(chan struct{}, 1)
	b.Wake = func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	routes := &connect.Routes{
		Links: b.Links, Outbox: b.Outbox, Jobs: b.Jobs, KV: kv,
		Spools: spoolsFor{materials},
	}

	var once sync.Once

	serves[wirerpc.MethodInfo] = func(context.Context, wirerpc.Call) (any, error) {
		return routesapi.Describe(wirerpc.Info{
			Name:     name,
			Version:  extension.Version(version),
			Contract: wirerpc.Contract,
			Serves:   []wirerpc.Method{wirerpc.MethodConfigure},
		}, openAPI)
	}
	serves[wirerpc.MethodConfigure] = wirerpc.Configure(func(_ context.Context, held Settings) error {
		client, err := spoolbook.New(held.SpoolbookURL, held.Token, held.Library)
		if err != nil {
			return err
		}

		// plugin.configure comes once, before any other call
		once.Do(func() {
			routes.SetHost(client.Host())
			log.Info("sending to spoolbook", "host", client.Host(), "library", held.Library)

			settings := bridge.SettingsHash(held.SpoolbookURL, held.Library, held.Token)

			go follow(ctx, conn, &connect.Dispatcher{Handler: b, KV: kv, Log: log}, log)
			go deliver(ctx, b.Outbox, client, settings, wake, log)
			go settles(ctx, b)
		})

		return nil
	})
	serves[wirerpc.MethodShutdown] = func(context.Context, wirerpc.Call) (any, error) { return nil, nil }
	serves[routesapi.MethodServe] = routesapi.Serve(routes.Handler())

	return conn.Serve(ctx)
}

// follow reads the event stream from where it last stopped, and opens it
// again when it ends.
func follow(ctx context.Context, conn *wirerpc.Conn, d *connect.Dispatcher, log *slog.Logger) {
	wait := time.Second

	for ctx.Err() == nil {
		// without its cursor it would start at the head and skip what was missed
		cursor, err := d.Cursor(ctx)
		if err == nil {
			params := busapi.SubscribeParams{Types: connect.Types, Resume: cursor}
			err = busapi.Subscribe(ctx, conn, params, func(w busapi.Window) error {
				wait = time.Second

				return d.Window(ctx, w)
			})
		}

		if err != nil && ctx.Err() == nil {
			log.Warn("event stream ended", "error", err, "again in", wait.String())
		}

		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}

		wait = min(wait*2, time.Minute)
	}
}

// deliver sends the outbox when something is queued, and when the next retry
// is due.
func deliver(ctx context.Context, outbox *bridge.Outbox, client *spoolbook.Client, settings string, wake <-chan struct{}, log *slog.Logger) {
	// the store answers once the host marks the module ready, just after
	// plugin.configure
	for wait := 100 * time.Millisecond; outbox.Ready(ctx) != nil; wait = min(wait*2, 10*time.Second) {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}

	// a pause made under other settings is lifted; under these it holds
	if err := outbox.UseSettings(ctx, settings); err != nil {
		log.Warn("could not read the pause", "error", err)
	}

	for ctx.Err() == nil {
		next, err := outbox.Flush(ctx, client)
		if err != nil && ctx.Err() == nil {
			log.Warn("could not read the queue", "error", err)
			next = 30 * time.Second
		}

		if next == 0 {
			next = 5 * time.Minute
		}

		timer := time.NewTimer(next)
		select {
		case <-wake:
			// let a burst of events land first
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
}

// settles runs the settle worker once the store answers.
func settles(ctx context.Context, b *bridge.Bridge) {
	for wait := 100 * time.Millisecond; b.Outbox.Ready(ctx) != nil; wait = min(wait*2, 10*time.Second) {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}

	b.RunSettles(ctx)
}

// spoolsFor lists Materials' spools as the person asking.
type spoolsFor struct{ m connect.Materials }

func (s spoolsFor) Spools(ctx context.Context, principal string) ([]bridge.MaterialsSpool, error) {
	return s.m.As(principal).Spools(ctx)
}

// stdio is the channel the host handed this process.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }
