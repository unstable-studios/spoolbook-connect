// Package bridge turns Connect Local's events into spoolbook writes: the
// links between Materials spools and spoolbook spools, what each tool held
// when a print started, and an outbox that survives restarts. It knows
// nothing of the SDK: main wires it to Connect through small interfaces.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
)

// ErrNotFound is a key with no record.
var ErrNotFound = errors.New("bridge: no record")

// KV is the module's key/value store (stateapi.Records in Connect).
type KV interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	// List returns the keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
}

func getJSON(ctx context.Context, kv KV, key string, into any) error {
	raw, err := kv.Get(ctx, key)
	if err != nil {
		return err
	}

	return json.Unmarshal(raw, into)
}

func putJSON(ctx context.Context, kv KV, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}

	return kv.Put(ctx, key, raw)
}

// MemKV is a KV in memory, for tests.
type MemKV struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemKV returns an empty store.
func NewMemKV() *MemKV { return &MemKV{data: map[string][]byte{}} }

func (m *MemKV) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}

	return slices.Clone(v), nil
}

func (m *MemKV) Put(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.data[key] = slices.Clone(value)

	return nil
}

func (m *MemKV) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.data[key]; !ok {
		return ErrNotFound
	}

	delete(m.data, key)

	return nil
}

func (m *MemKV) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []string
	for _, k := range slices.Sorted(maps.Keys(m.data)) {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}

	return keys, nil
}
