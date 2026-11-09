package store

import (
	"testing"
	"time"
)

func TestLocalDate(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		name     string
		at       time.Time
		loc      *time.Location
		dayStart time.Duration
		want     time.Time
	}{
		{"上海自然日", time.Date(2026, 11, 3, 17, 0, 0, 0, time.UTC), shanghai, 0, day(4)},
		{"UTC自然日", time.Date(2026, 11, 3, 17, 0, 0, 0, time.UTC), time.UTC, 0, day(3)},
		{"日界前一分钟归前一天", time.Date(2026, 11, 3, 19, 59, 0, 0, time.UTC), shanghai, 4 * time.Hour, day(3)},
		{"到达日界归当天", time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC), shanghai, 4 * time.Hour, day(4)},
	} {
		if got := LocalDate(c.at, c.loc, c.dayStart); !got.Equal(c.want) || got.Location() != time.UTC {
			t.Errorf("%s: LocalDate = %v, want %v", c.name, got, c.want)
		}
	}
}
