package bridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/unstable-studios/spoolbook-connect/internal/spoolbook"
)

// Link maps one Materials spool to one spoolbook spool.
type Link struct {
	// MaterialsSpool is the Materials spool id.
	MaterialsSpool int64 `json:"materials_spool"`
	spoolbook.Spool
	// Remaining is the last amount left queued for spoolbook, nil before the
	// first.
	Remaining *float64 `json:"remaining,omitempty"`
}

const linkPrefix = "link/"

func linkKey(id int64) string { return linkPrefix + strconv.FormatInt(id, 10) }

// Links keeps links in the store, one record each.
type Links struct{ kv KV }

// NewLinks returns the links in kv.
func NewLinks(kv KV) *Links { return &Links{kv: kv} }

// Get returns the link of a Materials spool, and ErrNotFound for none.
func (l *Links) Get(ctx context.Context, id int64) (Link, error) {
	var link Link
	err := getJSON(ctx, l.kv, linkKey(id), &link)

	return link, err
}

// Put adds or replaces a link. Replacing one forgets what was last queued, so
// the next weight is sent.
func (l *Links) Put(ctx context.Context, link Link) error {
	if link.MaterialsSpool <= 0 {
		return errors.New("materials_spool must be a Materials spool id")
	}

	if err := link.Spool.Validate(); err != nil {
		return err
	}

	link.Remaining = nil

	return putJSON(ctx, l.kv, linkKey(link.MaterialsSpool), link)
}

// noted records what was last queued for a link.
func (l *Links) noted(ctx context.Context, link Link, remaining float64) error {
	link.Remaining = &remaining

	return putJSON(ctx, l.kv, linkKey(link.MaterialsSpool), link)
}

// Delete removes a link, and ErrNotFound for none.
func (l *Links) Delete(ctx context.Context, id int64) error {
	return l.kv.Delete(ctx, linkKey(id))
}

// List returns every link, by Materials spool id.
func (l *Links) List(ctx context.Context) ([]Link, error) {
	keys, err := l.kv.List(ctx, linkPrefix)
	if err != nil {
		return nil, err
	}

	links := make([]Link, 0, len(keys))
	for _, key := range keys {
		var link Link
		if err := getJSON(ctx, l.kv, key, &link); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}

			return nil, fmt.Errorf("read %s: %w", key, err)
		}

		links = append(links, link)
	}

	return links, nil
}

// ParseSpoolID reads a Materials spool id from a path or form.
func ParseSpoolID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a Materials spool id", s)
	}

	return id, nil
}
