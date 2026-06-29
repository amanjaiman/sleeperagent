package parser

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/amanjaiman/agentkeeper/internal/config"
)

func claudePatterns() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)limit reached.*resets\s+(?P<time>.+)`),
		regexp.MustCompile(`(?i)Claude AI usage limit reached\|(?P<ts>\d+)`),
	}
}

func TestDetectTUIClockTime(t *testing.T) {
	pat := claudePatterns()
	match, groups, ok := Detect(pat, "... 5-hour limit reached ∙ resets 2pm\n> ")
	if !ok {
		t.Fatal("expected a match")
	}
	if groups["time"] != "2pm" {
		t.Fatalf("time group = %q, want %q", groups["time"], "2pm")
	}
	if match == "" || !contains(match, "resets 2pm") {
		t.Fatalf("match = %q, want it to contain the limit line", match)
	}
}

func TestDetectHeadlessUnix(t *testing.T) {
	pat := claudePatterns()
	_, groups, ok := Detect(pat, "Claude AI usage limit reached|1718900000")
	if !ok {
		t.Fatal("expected a match")
	}
	if groups["ts"] != "1718900000" {
		t.Fatalf("ts group = %q", groups["ts"])
	}
}

func TestDetectNoMatch(t *testing.T) {
	if _, _, ok := Detect(claudePatterns(), "all good, working away"); ok {
		t.Fatal("did not expect a match")
	}
}

func TestClaudeDefaultPatternsAgainstCorpus(t *testing.T) {
	cfg := config.Default()
	ad, err := cfg.Adapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	now := refNow(t, "10:00")
	cases := []string{
		"claude_5_hour.txt",
		"claude_session_limit.txt",
		"claude_usage_sentence.txt",
		"claude_weekly_limit.txt",
		"claude_headless_unix.txt",
		"claude_relative.txt",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			text := readTestdata(t, name)
			_, groups, ok := Detect(ad.LimitPatterns, text)
			if !ok {
				t.Fatalf("default Claude patterns did not match %s: %q", name, text)
			}
			ri := Resolve(groups, now, fallbackWindowTest)
			if ri.Source == SourceFallback {
				t.Fatalf("%s resolved via fallback; groups=%v", name, groups)
			}
			if !ri.Time.After(now) {
				t.Fatalf("%s resolved to %v, want after %v", name, ri.Time, now)
			}
		})
	}
}

func TestClaudeDefaultPatternsDoNotMatchApproachingLimit(t *testing.T) {
	cfg := config.Default()
	ad, err := cfg.Adapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	text := readTestdata(t, "claude_approaching_limit.txt")
	if match, _, ok := Detect(ad.LimitPatterns, text); ok {
		t.Fatalf("approaching warning must not match; got %q", match)
	}
}

func TestDetectTrimsTrailingPunctuationFromGroups(t *testing.T) {
	pat := []*regexp.Regexp{regexp.MustCompile(`(?i)limit reached.*resets\s+(?P<time>[^\r\n]+)`)}
	_, groups, ok := Detect(pat, "limit reached; resets 3pm.")
	if !ok {
		t.Fatal("expected a match")
	}
	if groups["time"] != "3pm" {
		t.Fatalf("time group = %q, want %q", groups["time"], "3pm")
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestResolveUnixSeconds(t *testing.T) {
	now := time.Unix(1718800000, 0)
	ri := Resolve(map[string]string{"ts": "1718900000"}, now, fallbackWindowTest)
	if ri.Source != SourceUnix || ri.Confidence != "high" {
		t.Fatalf("got source=%s conf=%s", ri.Source, ri.Confidence)
	}
	if !ri.Time.Equal(time.Unix(1718900000, 0)) {
		t.Fatalf("time = %v", ri.Time)
	}
}

func TestResolveUnixMillis(t *testing.T) {
	now := time.Unix(1718800000, 0)
	ri := Resolve(map[string]string{"ts": "1718900000000"}, now, fallbackWindowTest)
	if !ri.Time.Equal(time.UnixMilli(1718900000000)) {
		t.Fatalf("time = %v", ri.Time)
	}
}

func TestResolveClock(t *testing.T) {
	ri := Resolve(map[string]string{"time": "2pm"}, refNow(t, "10:00"), fallbackWindowTest)
	if ri.Source != SourceClock {
		t.Fatalf("source = %s", ri.Source)
	}
	if ri.Time.Hour() != 14 || ri.Time.Minute() != 0 {
		t.Fatalf("time = %v", ri.Time)
	}
}

func TestResolveFallback(t *testing.T) {
	now := refNow(t, "10:00")
	ri := Resolve(map[string]string{}, now, fallbackWindowTest)
	if ri.Source != SourceFallback || ri.Confidence != "low" {
		t.Fatalf("got source=%s conf=%s", ri.Source, ri.Confidence)
	}
	if !ri.Time.Equal(now.Add(fallbackWindowTest)) {
		t.Fatalf("time = %v", ri.Time)
	}
}

func TestParseClock(t *testing.T) {
	now := refNow(t, "10:00") // 2026-06-26 10:00 local
	cases := []struct {
		in   string
		hour int
		min  int
		day  int // expected day-of-month; 26 today, 27 tomorrow
	}{
		{"2pm", 14, 0, 26},
		{"2:30 PM", 14, 30, 26},
		{"6:34 AM", 6, 34, 27},            // already past 10:00 -> tomorrow
		{"12am", 0, 0, 27},                // midnight tonight -> tomorrow
		{"12pm", 12, 0, 26},               // noon today
		{"14:00", 14, 0, 26},              // 24h
		{"resets 9:15pm UTC", 21, 15, 26}, // trailing tz ignored
	}
	for _, c := range cases {
		got, err := ParseClock(c.in, now)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got.Hour() != c.hour || got.Minute() != c.min || got.Day() != c.day {
			t.Errorf("%q: got %v, want %02d:%02d on day %d", c.in, got, c.hour, c.min, c.day)
		}
		if !got.After(now) {
			t.Errorf("%q: resolved time %v is not after now %v", c.in, got, now)
		}
	}
}

func TestParseClockNoMatch(t *testing.T) {
	if _, err := ParseClock("sometime soon", refNow(t, "10:00")); err == nil {
		t.Fatal("expected an error for unparseable time")
	}
}

func TestIsNextDayRollForward(t *testing.T) {
	prev := refNow(t, "19:11")
	if !IsNextDayRollForward(prev.Add(24*time.Hour+30*time.Second), prev, time.Minute) {
		t.Fatal("expected previous+24h within tolerance to be recognized")
	}
	if IsNextDayRollForward(prev.Add(25*time.Hour), prev, time.Minute) {
		t.Fatal("different future reset must not be recognized as a roll-forward ghost")
	}
	if IsNextDayRollForward(time.Time{}, prev, time.Minute) {
		t.Fatal("zero reset must not be recognized")
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"2h30m", 2*time.Hour + 30*time.Minute},
		{"45 minutes", 45 * time.Minute},
		{"1 hour", time.Hour},
		{"90 mins", 90 * time.Minute},
		{"30s", 30 * time.Second},
		{"2 hours 15 minutes", 2*time.Hour + 15*time.Minute},
		{"5 sec", 5 * time.Second},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("expected error for unparseable duration")
	}
}

func TestResolveRelativeDuration(t *testing.T) {
	now := refNow(t, "10:00")
	ri := Resolve(map[string]string{"dur": "in 2h30m"}, now, fallbackWindowTest)
	if ri.Source != SourceRelative || ri.Confidence != "high" {
		t.Fatalf("got source=%s conf=%s", ri.Source, ri.Confidence)
	}
	want := now.Add(2*time.Hour + 30*time.Minute)
	if !ri.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", ri.Time, want)
	}
}

// Codex's documented limit messages must detect + resolve through the same
// generic parser the supervisor uses for any agent.
func TestDetectCodexFormats(t *testing.T) {
	pats := []*regexp.Regexp{
		regexp.MustCompile(`(?i)try again at (?P<time>.+)`),
		regexp.MustCompile(`(?i)try again in (?P<dur>.+)`),
	}
	now := refNow(t, "10:00")

	_, g, ok := Detect(pats, "Rate limited. Try again at 6:34 AM.")
	if !ok || Resolve(g, now, fallbackWindowTest).Source != SourceClock {
		t.Fatalf("clock form: ok=%v groups=%v", ok, g)
	}
	_, g, ok = Detect(pats, "Rate limited. Try again in 2h30m.")
	if !ok {
		t.Fatal("relative form not detected")
	}
	if ri := Resolve(g, now, fallbackWindowTest); ri.Source != SourceRelative {
		t.Fatalf("relative form resolved via %s", ri.Source)
	}
}

const fallbackWindowTest = 5 * time.Hour

func refNow(t *testing.T, hhmm string) time.Time {
	t.Helper()
	clock, err := time.Parse("15:04", hhmm)
	if err != nil {
		t.Fatalf("bad test clock %q: %v", hhmm, err)
	}
	return time.Date(2026, 6, 26, clock.Hour(), clock.Minute(), 0, 0, time.Local)
}
