package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestRedisTargetSamplesAndRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	target := store.RedisTarget{Name: "Production cache", Endpoint: []byte("sealed"), Enabled: true, AlertEnabled: true}
	id, err := st.SaveRedisTarget(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRedisTarget(ctx, target); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name error = %v", err)
	}
	statuses, err := st.RedisStatuses(ctx)
	if err != nil || len(statuses) != 1 || statuses[0].Latest != nil || !statuses[0].Target.AlertEnabled {
		t.Fatalf("initial statuses = %+v, %v", statuses, err)
	}
	at := time.Now().UTC().Add(-2 * time.Hour)
	sample := store.RedisSample{TargetID: id, SampledAt: at, UsedMemory: 80, MaxMemory: 100, ConnectedClients: 4, KeyCount: 10, UptimeSeconds: 99}
	if err := st.RecordRedisCheck(ctx, sample, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRedisCheck(ctx, sample, "Redis did not respond in time."); err != nil {
		t.Fatal(err)
	}
	// A failed check for a target that no longer exists must not pass silently.
	if err := st.RecordRedisCheck(ctx, store.RedisSample{TargetID: id + 100}, "Redis did not respond in time."); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record check for missing target error = %v", err)
	}
	latest, err := st.LatestRedisSample(ctx, id)
	if err != nil || latest.UsedMemory != 80 || !latest.SampledAt.Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	statuses, err = st.RedisStatuses(ctx)
	if err != nil || statuses[0].Target.LastError == "" || statuses[0].Latest == nil {
		t.Fatalf("status after failed check = %+v, %v", statuses, err)
	}
	history, err := st.RedisHistory(ctx, id, time.Now().Add(-24*time.Hour))
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	// The retention sweep removes samples with the issue retention period.
	st.SetClock(func() time.Time { return time.Now().Add(24 * time.Hour) })
	if _, err := st.Sweep(ctx, store.Retention{ShortHours: 1, IssueDays: 1}, store.SweepOptions{Pause: -1}); err != nil {
		t.Fatal(err)
	}
	st.SetClock(time.Now)
	if _, err := st.LatestRedisSample(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sample survived retention: %v", err)
	}
	if err := st.DeleteRedisTarget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.RedisTargets(ctx); len(got) != 0 {
		t.Fatalf("targets after delete = %+v", got)
	}
}
