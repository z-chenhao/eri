package scheduler

import (
	"testing"
	"time"
)

func TestDailyScheduleUsesNamedTimezoneAcrossDays(t *testing.T) {
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	next, err := FirstRun(Schedule{Type: "daily", DailyTime: "10:30", Timezone: "Asia/Shanghai"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 17, 2, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestIntervalRejectsRunawayFrequency(t *testing.T) {
	if _, err := FirstRun(Schedule{Type: "interval", IntervalSeconds: 5}, time.Now()); err == nil {
		t.Fatal("unsafe high-frequency commitment accepted")
	}
}

func TestRelativeOneTimeScheduleUsesRuntimeClock(t *testing.T) {
	now := time.Date(2026, 7, 21, 4, 0, 0, 123, time.UTC)
	resolved, err := resolveSchedule(Schedule{Type: "once", AfterSeconds: 60}, now)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AfterSeconds != 0 || !resolved.At.Equal(now.Add(time.Minute)) {
		t.Fatalf("resolved schedule = %+v", resolved)
	}
	if _, err := resolveSchedule(Schedule{Type: "once", At: now.Add(time.Hour), AfterSeconds: 60}, now); err == nil {
		t.Fatal("ambiguous relative and absolute schedule accepted")
	}
}
