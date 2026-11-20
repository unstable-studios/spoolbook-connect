// Package connect adapts the Prusa Connect SDK to the bridge: the record
// store, the Materials routes, the event stream and this module's own routes.
package connect

import (
	"context"
	"errors"

	"github.com/prusa3d/prusa-connect/sdk/stateapi"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
)

// KV is the host's record store as a bridge.KV.
type KV struct{ Records stateapi.Records }

func (k KV) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := k.Records.Get(ctx, stateapi.Key(key))
	if errors.Is(err, stateapi.ErrNoRecord) {
		return nil, bridge.ErrNotFound
	}

	return v.Bytes, err
}

func (k KV) Put(ctx context.Context, key string, value []byte) error {
	_, err := k.Records.Put(ctx, stateapi.Key(key), value)

	return err
}

func (k KV) Delete(ctx context.Context, key string) error {
	err := k.Records.Delete(ctx, stateapi.Key(key))
	if errors.Is(err, stateapi.ErrNoRecord) {
		return bridge.ErrNotFound
	}

	return err
}

func (k KV) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := k.Records.List(ctx, stateapi.Key(prefix))
	if err != nil {
		return nil, err
	}

	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = string(key)
	}

	return out, nil
}
