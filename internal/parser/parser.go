// Package parser detects the usage-limit message in agent output and resolves
// the reset time. Detection is output-scraping by necessity: the authoritative
// reset timestamp is held in the agent's memory only and is not exposed via any
// file or API, so the printed strings are the only signal.
package parser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Source records where a reset time came from, in descending reliability.
const (
	SourceUnix     = "unix"     // explicit unix timestamp in the message
	SourceClock    = "clock"    // a clock time like "2pm" or "6:34 AM"
	SourceRelative = "relative" // a relative duration like "in 2h30m" or "in 45 minutes"
	SourceFallback = "fallback" // nothing parseable; assumed window
)

// ResetInfo is the resolved reset time plus provenance.
type ResetInfo struct {
	Time       time.Time
	Source     string
	Confidence string // "high" | "low"
}

// Detect scans text for any of the limit patterns and returns the full matched
// substring plus the named capture groups of the first match. The match string
// lets callers dedupe a single limit event that lingers in the captured
// scrollback across many polls. ok is false when no pattern matches.
func Detect(patterns []*regexp.Regexp, text string) (match string, groups map[string]string, ok bool) {
	for _, re := range patterns {
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		groups = make(map[string]string)
		for i, name := range re.SubexpNames() {
			if name != "" && i < len(m) {
				groups[name] = strings.TrimRight(strings.TrimSpace(m[i]), ".,")
			}
		}
		return strings.TrimSpace(m[0]), groups, true
	}
	return "", nil, false
}

// Resolve turns the captured groups into a concrete reset time, trying the most
// reliable source first: an explicit unix ts, then a clock time, then a
// fallback window. now and the window are injected for testability.
func Resolve(groups map[string]string, now time.Time, fallbackWindow time.Duration) ResetInfo {
	if ts := groups["ts"]; ts != "" {
		if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
			return ResetInfo{Time: unixGuess(n), Source: SourceUnix, Confidence: "high"}
		}
	}
	if clock := groups["time"]; clock != "" {
		if t, err := ParseClock(clock, now); err == nil {
			return ResetInfo{Time: t, Source: SourceClock, Confidence: "high"}
		}
	}
	if dur := groups["dur"]; dur != "" {
		if d, err := ParseDuration(dur); err == nil {
			return ResetInfo{Time: now.Add(d), Source: SourceRelative, Confidence: "high"}
		}
	}
	return ResetInfo{Time: now.Add(fallbackWindow), Source: SourceFallback, Confidence: "low"}
}

// unixGuess accepts either seconds or milliseconds since the epoch.
func unixGuess(n int64) time.Time {
	// Anything past ~year 33658 in seconds is almost certainly milliseconds.
	if n > 1e12 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}

var (
	// 12-hour clock with am/pm, optional minutes: "2pm", "2:30 PM", "6:34am".
	clock12 = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	// 24-hour clock "HH:MM": "14:00", "06:34".
	clock24 = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)\b`)
)

// ParseClock extracts a clock time from s and resolves it to the next future
// occurrence relative to now, in now's location. If the parsed time is at or
// before now, it rolls forward 24h (the limit resets later today or tomorrow).
//
// Timezone tokens in s are ignored: the time is interpreted in now's location.
// This is a known M1 limitation, acceptable because reset_buffer absorbs small
// skew and resets are local-clock in practice.
func ParseClock(s string, now time.Time) (time.Time, error) {
	if m := clock12.FindStringSubmatch(s); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute := 0
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		if hour < 1 || hour > 12 {
			return time.Time{}, fmt.Errorf("invalid 12h hour %d in %q", hour, s)
		}
		switch strings.ToLower(m[3]) {
		case "pm":
			if hour != 12 {
				hour += 12
			}
		case "am":
			if hour == 12 {
				hour = 0
			}
		}
		return rollForward(now, hour, minute), nil
	}
	if m := clock24.FindStringSubmatch(s); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		return rollForward(now, hour, minute), nil
	}
	return time.Time{}, fmt.Errorf("no clock time found in %q", s)
}

// rollForward returns today's date at hour:minute in now's location, advanced
// by a day if that instant is not strictly in the future.
func rollForward(now time.Time, hour, minute int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// IsNextDayRollForward reports whether reset is effectively previous+24h. A
// bare clock reset that has just passed can roll to tomorrow when stale output
// is parsed again; callers use this to reject that already-satisfied banner.
func IsNextDayRollForward(reset, previous time.Time, tolerance time.Duration) bool {
	if reset.IsZero() || previous.IsZero() {
		return false
	}
	if tolerance < 0 {
		tolerance = -tolerance
	}
	delta := reset.Sub(previous.Add(24 * time.Hour))
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}

// durationWord matches one "<number><unit>" token, e.g. "2h", "30 minutes". No
// trailing word boundary, so compact concatenations like "2h30m" parse fully.
var durationWord = regexp.MustCompile(`(?i)(\d+)\s*(hours?|hrs?|minutes?|mins?|seconds?|secs?|h|m|s)`)

// ParseDuration extracts a relative duration from human text and returns it.
// It sums every "<number><unit>" token it finds ("2 hours 30 minutes", "2h30m",
// "in 45 mins"), falling back to Go's compact format ("1h30m"). Relative resets
// are tz-free, so they are treated as high confidence.
func ParseDuration(s string) (time.Duration, error) {
	var total time.Duration
	for _, m := range durationWord.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		switch unit := strings.ToLower(m[2]); unit[0] {
		case 'h':
			total += time.Duration(n) * time.Hour
		case 'm':
			total += time.Duration(n) * time.Minute
		case 's':
			total += time.Duration(n) * time.Second
		}
	}
	if total > 0 {
		return total, nil
	}
	if d, err := time.ParseDuration(strings.ReplaceAll(strings.TrimSpace(s), " ", "")); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("no duration found in %q", s)
}
