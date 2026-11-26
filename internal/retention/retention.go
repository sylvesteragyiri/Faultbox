// Package retention runs the background sweep that deletes data older than
// the retention settings (store.Retention): successful job runs after the
// short-lived period, events, failures, runs, samples and rollups after the
// issue retention period.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Interval is the time between sweeps. FirstDelay lets startup finish (and
// migrations settle) before the first one.
const (
	Interval   = 10 * time.Minute
	FirstDelay = time.Minute
)

// Sweeper deletes expired rows on a schedule. The policy is read from the
// store before every sweep, so a change on the Settings page applies from
// the next one.
type Sweeper struct {
	store        *store.Store
	logger       *slog.Logger
	defaultShort int // FAULTBOX_SHORT_RETENTION_HOURS
	defaultDays  int // FAULTBOX_RETENTION_DAYS
	opts         store.SweepOptions
}

// New returns a sweeper whose environment defaults are shortHours and days.
func New(st *store.Store, logger *slog.Logger, shortHours, days int) *Sweeper {
	return &Sweeper{store: st, logger: logger, defaultShort: shortHours, defaultDays: days}
}

// Policy returns the retention in force.
func (s *Sweeper) Policy(ctx context.Context) (store.Retention, error) {
	return s.store.Retention(ctx, s.defaultShort, s.defaultDays)
}

// Sweep runs one sweep now and returns how many rows each table lost.
func (s *Sweeper) Sweep(ctx context.Context) (map[string]int64, error) {
	r, err := s.Policy(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.Sweep(ctx, r, s.opts)
}

// Run sweeps after FirstDelay and then every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	timer := time.NewTimer(FirstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		start := time.Now()
		deleted, err := s.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.Error("retention sweep", "error", err)
		}
		var total int64
		for _, n := range deleted {
			total += n
		}
		if total > 0 {
			s.logger.Info("retention sweep", "deleted", total, "took", time.Since(start).Round(time.Millisecond))
		}
		timer.Reset(Interval)
	}
}
