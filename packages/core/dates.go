package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday, "日": time.Sunday, "天": time.Sunday,
	"mon": time.Monday, "monday": time.Monday, "一": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday, "二": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday, "三": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday, "四": time.Thursday,
	"fri": time.Friday, "friday": time.Friday, "五": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday, "六": time.Saturday,
}

// ParseWhen parses a due/filter time in now's location. dateOnly reports
// that no clock time was given, in which case the result is midnight.
//
// Accepted: today/tomorrow/今天/明天/后天, +3d/+2w/+4h, weekday names
// (fri, 周五: next occurrence after today; next fri, 下周五: that day of next
// week), 2006-01-02, 2006-01-02 15:04, 2006-01-02T15:04, RFC 3339, and a
// relative day followed by a clock time ("明天 18:00").
func ParseWhen(s string, now time.Time) (t time.Time, dateOnly bool, err error) {
	raw := strings.TrimSpace(s)
	v := strings.ToLower(raw)
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	switch v {
	case "today", "今天", "今日":
		return today, true, nil
	case "tomorrow", "明天", "明日":
		return today.AddDate(0, 0, 1), true, nil
	case "后天":
		return today.AddDate(0, 0, 2), true, nil
	}
	if strings.HasPrefix(v, "+") && len(v) > 2 {
		n, convErr := strconv.Atoi(v[1 : len(v)-1])
		if convErr == nil && n >= 0 {
			switch v[len(v)-1] {
			case 'd':
				return today.AddDate(0, 0, n), true, nil
			case 'w':
				return today.AddDate(0, 0, 7*n), true, nil
			case 'h':
				return now.Add(time.Duration(n) * time.Hour).Truncate(time.Minute), false, nil
			}
		}
	}
	if d, ok := parseWeekday(v, today); ok {
		return d, true, nil
	}
	for _, layout := range []string{"2006-01-02", "2006/01/02"} {
		if d, perr := time.ParseInLocation(layout, raw, loc); perr == nil {
			return d, true, nil
		}
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if d, perr := time.ParseInLocation(layout, raw, loc); perr == nil {
			return d, false, nil
		}
	}
	if d, perr := time.Parse(time.RFC3339, raw); perr == nil {
		return d, false, nil
	}
	// A relative day with a clock time: "明天 18:00", "fri 9:30".
	if i := strings.LastIndexByte(raw, ' '); i > 0 {
		if clock, perr := time.Parse("15:04", raw[i+1:]); perr == nil {
			if d, dateOnly, derr := ParseWhen(raw[:i], now); derr == nil && dateOnly {
				return time.Date(d.Year(), d.Month(), d.Day(), clock.Hour(), clock.Minute(), 0, 0, loc), false, nil
			}
		}
	}
	return time.Time{}, false, fmt.Errorf("cannot parse time %q (try today, tomorrow, +3d, fri, 2026-10-07 or \"2026-10-07 18:00\")", s)
}

func parseWeekday(v string, today time.Time) (time.Time, bool) {
	next := false
	for _, p := range []string{"next ", "下周", "下星期", "下礼拜"} {
		if strings.HasPrefix(v, p) {
			v, next = strings.TrimPrefix(v, p), true
			break
		}
	}
	if !next {
		for _, p := range []string{"周", "星期", "礼拜"} {
			v = strings.TrimPrefix(v, p)
		}
	}
	wd, ok := weekdays[v]
	if !ok {
		return time.Time{}, false
	}
	if next {
		// Same weekday in the following Monday-based week.
		offset := (int(today.Weekday()) + 6) % 7 // days since Monday
		monday := today.AddDate(0, 0, -offset+7)
		return monday.AddDate(0, 0, (int(wd)+6)%7), true
	}
	days := (int(wd) - int(today.Weekday()) + 7) % 7
	if days == 0 {
		days = 7
	}
	return today.AddDate(0, 0, days), true
}

func EndOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
}

// ParseDue parses a due date; date-only values mean the end of that day.
func ParseDue(s string, now time.Time) (time.Time, error) {
	t, dateOnly, err := ParseWhen(s, now)
	if err != nil {
		return t, err
	}
	if dateOnly {
		t = EndOfDay(t)
	}
	return t.UTC(), nil
}
