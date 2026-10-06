package httpserver

import (
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

func TestRSSScheduleNext(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	parse := func(raw string) time.Time {
		t.Helper()
		value, err := time.ParseInLocation("2006-01-02 15:04:05", raw, zone)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, test := range []struct{ raw, after, next string }{
		{"15", "2026-10-06 08:03:45", "2026-10-06 08:18:45"},
		{"*/15 * * * *", "2026-10-06 08:00:00", "2026-10-06 08:15:00"},
		{"0 9 * * 0", "2026-10-06 08:00:00", "2026-10-12 09:00:00"},
		{"0 9 * * sun", "2026-10-06 08:00:00", "2026-10-11 09:00:00"},
		{"0 9 1 * mon", "2026-10-06 08:00:00", "2027-02-01 09:00:00"},
		{"5,35 8-10/2 * jan,oct tue-thu", "2026-10-06 08:35:00", "2026-10-06 10:05:00"},
		{"10/20 8 * * *", "2026-10-06 08:31:00", "2026-10-06 08:50:00"},
		{"0 0 29 feb *", "2026-10-06 08:00:00", "2028-02-29 00:00:00"},
		{"0 0 29 feb mon", "2026-10-06 08:00:00", "2044-02-29 00:00:00"},
		{"0 0 last feb *", "2028-02-01 08:00:00", "2028-02-29 00:00:00"},
		{"0 0 last * *", "2026-10-06 08:00:00", "2026-10-31 00:00:00"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			schedule, err := parseRSSSchedule(test.raw, zone)
			if err != nil {
				t.Fatal(err)
			}
			if got := schedule.next(parse(test.after)); !got.Equal(parse(test.next)) {
				t.Fatalf("got %s, want %s", got, test.next)
			}
		})
	}
	for _, raw := range []string{"", "0", "-1", "1.5", "99999999999999999999999", "@daily", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 7", "*/0 * * * *", "*/-1 * * * *", "*/999 * * * *", "1-0 * * * *", "1,,2 * * * *", "* * * * fri-mon", "* * 3rd * *", "0 9 1st mon * *"} {
		if _, err := parseRSSSchedule(raw, zone); err == nil {
			t.Errorf("accepted invalid/unsupported schedule %q", raw)
		}
	}
	schedule, err := parseRSSSchedule("0 0 31 feb *", zone)
	if err != nil || !schedule.next(parse("2026-01-01 00:00:00")).IsZero() {
		t.Fatal("impossible calendar date must not run", err)
	}
}

func TestRSSScheduleDSTAndTimezone(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	schedule, _ := parseRSSSchedule("30 2 * * *", zone)
	after := time.Date(2026, 3, 7, 3, 0, 0, 0, zone)
	next := schedule.next(after)
	if next.Day() != 9 || next.Hour() != 2 || next.Minute() != 30 {
		t.Fatalf("DST gap was normalized instead of skipped: %s", next)
	}
	t.Setenv("TZ", "")
	location, err := rssWorkerLocation()
	if err != nil || location.String() != "Asia/Shanghai" {
		t.Fatal(location, err)
	}
	t.Setenv("TZ", "not/a/zone")
	if _, err := rssWorkerLocation(); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}

func TestRSSWorkerReconcilesLiveSchedules(t *testing.T) {
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	plan := rssWorkerPlan{location: time.UTC}
	tasks := []rsstaskconfig.Task{{ID: 1, State: "Y", Uses: "D", Interval: "2"}, {ID: 2, State: "1", Uses: "D", Interval: "* * * * *"}, {ID: 3, State: "N", Uses: "D", Interval: "1"}, {ID: 4, State: "Y", Uses: "X", Interval: "1"}, {ID: 5, State: "Y", Uses: "D", Interval: "bad"}}
	check := func(now time.Time, want ...int64) {
		t.Helper()
		got := plan.reconcile(now, tasks)
		if len(got) != len(want) {
			t.Fatalf("due %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("due %v, want %v", got, want)
			}
		}
	}
	check(base) // no immediate startup execution
	check(base.Add(time.Minute), 2)
	check(base.Add(2*time.Minute), 1, 2)
	check(base.Add(2 * time.Minute))                    // no repeated delivery of same tick
	check(base.Add(9*time.Minute+20*time.Second), 1, 2) // one, not outage backlog
	if next := plan.tasks[1].next; !next.Equal(base.Add(10 * time.Minute)) {
		t.Fatal("interval phase drifted", next)
	}
	tasks[0].State = "N"
	tasks[1].Interval = "10"
	check(base.Add(10 * time.Minute)) // disable and edit invalidate due entries
	tasks = append(tasks[:0], rsstaskconfig.Task{ID: 1, State: "Y", Uses: "D", Interval: "2"})
	check(base.Add(11 * time.Minute))
	check(base.Add(13*time.Minute), 1)
	if len(plan.tasks) != 1 {
		t.Fatal("deleted tasks retained", plan.tasks)
	}
}
