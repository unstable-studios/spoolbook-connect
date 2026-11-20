package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/prusa3d/prusa-connect/sdk/routesapi"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
)

// MaterialsNamespace is the Materials extension's namespace, where its routes
// are mounted and its events are typed.
const MaterialsNamespace = "com.github.aux.prusa3d.materials"

// The Materials events this module reads.
const (
	TypeMaterialsSpool = MaterialsNamespace + ".spool"
	TypeMaterialsTool  = MaterialsNamespace + ".tool"
)

// pageLimit is the most rows a Materials list answers at once.
const pageLimit = 100

// maxPages bounds a walk through a paged list (2000 spools is Materials' own
// default limit).
const maxPages = 30

// Caller makes one routes.call (routesapi.Client).
type Caller interface {
	Call(ctx context.Context, params routesapi.CallParams) (routesapi.Response, error)
}

// Materials reads the Materials extension through the host.
type Materials struct {
	Caller Caller
	// Principal is who a call runs as; empty is this module.
	Principal string
}

// As returns the same client acting for a person.
func (m Materials) As(principal string) Materials {
	return Materials{Caller: m.Caller, Principal: principal}
}

func (m Materials) get(ctx context.Context, path string, query url.Values, into any) error {
	res, err := m.Caller.Call(ctx, routesapi.CallParams{
		Module: MaterialsNamespace, Principal: m.Principal,
		Method: http.MethodGet, Path: path, Query: query.Encode(),
		Headers: map[string][]string{"Accept": {"application/json"}},
	})
	if err != nil {
		return fmt.Errorf("materials %s: %w", path, err)
	}

	if res.Status != 0 && (res.Status < 200 || res.Status > 299) {
		return fmt.Errorf("materials %s answered %d: %.200s", path, res.Status, res.Body)
	}

	if err := json.Unmarshal(res.Body, into); err != nil {
		return fmt.Errorf("materials %s: %w", path, err)
	}

	return nil
}

func page(offset int) url.Values {
	return url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(pageLimit)}}
}

// Tools returns the tools of one printer (GET /printers/tools, every page).
func (m Materials) Tools(ctx context.Context, deviceID string) ([]bridge.Tool, error) {
	var tools []bridge.Tool

	offset := 0
	for range maxPages {
		var rows struct {
			Tools []bridge.Tool `json:"tools"`
			Next  *int          `json:"next"`
		}
		if err := m.get(ctx, "/printers/tools", page(offset), &rows); err != nil {
			return nil, err
		}

		for _, t := range rows.Tools {
			if t.DeviceID == deviceID {
				tools = append(tools, t)
			}
		}

		if rows.Next == nil || *rows.Next <= offset {
			return tools, nil
		}

		offset = *rows.Next
	}

	return tools, nil
}

// Spools returns every spool (GET /spools, every page).
func (m Materials) Spools(ctx context.Context) ([]bridge.MaterialsSpool, error) {
	var spools []bridge.MaterialsSpool

	offset := 0
	for range maxPages {
		var rows struct {
			Spools []bridge.MaterialsSpool `json:"spools"`
			Next   *int                    `json:"next"`
		}
		if err := m.get(ctx, "/spools", page(offset), &rows); err != nil {
			return nil, err
		}

		spools = append(spools, rows.Spools...)

		if rows.Next == nil || *rows.Next <= offset {
			return spools, nil
		}

		offset = *rows.Next
	}

	return spools, nil
}

// Spool returns one spool. Materials has no route for one by id, so it walks
// the list.
func (m Materials) Spool(ctx context.Context, id int64) (bridge.MaterialsSpool, error) {
	spools, err := m.Spools(ctx)
	if err != nil {
		return bridge.MaterialsSpool{}, err
	}

	for _, s := range spools {
		if s.ID == id {
			return s, nil
		}
	}

	return bridge.MaterialsSpool{}, bridge.ErrNotFound
}
