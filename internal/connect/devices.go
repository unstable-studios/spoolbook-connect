package connect

import (
	"context"

	"github.com/prusa3d/prusa-connect/sdk/deviceapi"

	"github.com/unstable-studios/spoolbook-connect/internal/bridge"
)

// JobsReader reads a printer's current and last print (deviceapi.Client).
type JobsReader interface {
	Jobs(ctx context.Context, params deviceapi.JobsParams) (deviceapi.Jobs, error)
}

// Devices is Connect's record of each printer's print, as bridge.Devices.
type Devices struct{ Fleet JobsReader }

func (d Devices) Jobs(ctx context.Context, deviceID string) (current, last *bridge.DeviceJob, err error) {
	jobs, err := d.Fleet.Jobs(ctx, deviceapi.JobsParams{ID: deviceID})
	if err != nil {
		return nil, nil, err
	}

	return jobOf(jobs.Current), jobOf(jobs.Last), nil
}

func jobOf(j *deviceapi.Job) *bridge.DeviceJob {
	if j == nil {
		return nil
	}

	return &bridge.DeviceJob{
		ID: j.ID, Path: j.Path, DisplayName: j.DisplayName,
		Started: j.Started, Ended: j.Ended, Outcome: j.Outcome,
	}
}
