package parse

import (
	"testing"
	"time"
)

// ref is a fixed Friday 10:00 so every expectation is deterministic.
var ref = time.Date(2026, time.October, 2, 10, 0, 0, 0, time.UTC)

func TestParseDurations(t *testing.T) {
	cases := []struct {
		in   string
		dur  time.Duration
		kind Kind
	}{
		{"count 20 min", 20 * time.Minute, KindTimer},
		{"20 min timer", 20 * time.Minute, KindTimer},
		{"time-box 45m", 45 * time.Minute, KindTimer},
		{"countdown for 1h30", 90 * time.Minute, KindTimer},
		{"half an hour", 30 * time.Minute, KindTimer},
		{"an hour and a half", 90 * time.Minute, KindTimer},
		{"a quarter of an hour", 15 * time.Minute, KindTimer},
		{"90 seconds", 90 * time.Second, KindTimer},
		{"focus 25 minutes", 25 * time.Minute, KindTimer},
		{"set a timer for 2 hours", 2 * time.Hour, KindTimer},
		{"remind me in 20 minutes to stretch", 20 * time.Minute, KindReminder},
		{"1 hour 15 minutes", 75 * time.Minute, KindTimer},
		{"an hour", time.Hour, KindTimer},
	}
	for _, tc := range cases {
		got := Parse(tc.in, ref)
		if !got.OK {
			t.Errorf("Parse(%q) did not match anything", tc.in)
			continue
		}
		if got.Duration != tc.dur {
			t.Errorf("Parse(%q).Duration = %v, want %v", tc.in, got.Duration, tc.dur)
		}
		if got.Kind != tc.kind {
			t.Errorf("Parse(%q).Kind = %v, want %v", tc.in, got.Kind, tc.kind)
		}
	}
}

func TestParseClockReminders(t *testing.T) {
	cases := []struct {
		in   string
		due  time.Time
		kind Kind
	}{
		{"remind me at 3 PM to email the invoice", time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC), KindReminder},
		{"remind me today at 3 PM to email", time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC), KindReminder},
		{"tomorrow 9am", time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), KindReminder},
		{"at 15:00 to deploy", time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC), KindReminder},
		{"friday 17:30", time.Date(2026, 10, 2, 17, 30, 0, 0, time.UTC), KindReminder},
		{"notify me at noon to eat", time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), KindReminder},
		{"monday 8am", time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), KindReminder},
	}
	for _, tc := range cases {
		got := Parse(tc.in, ref)
		if !got.OK {
			t.Errorf("Parse(%q) did not match anything", tc.in)
			continue
		}
		if !got.DueAt.Equal(tc.due) {
			t.Errorf("Parse(%q).DueAt = %v, want %v", tc.in, got.DueAt, tc.due)
		}
		if got.Kind != tc.kind {
			t.Errorf("Parse(%q).Kind = %v, want %v", tc.in, got.Kind, tc.kind)
		}
	}
}

// A time that already passed today must roll forward and say so, per US-002.
func TestParseRolloverSurfacesItself(t *testing.T) {
	got := Parse("remind me at 9 AM to stretch", ref)
	want := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	if !got.DueAt.Equal(want) {
		t.Fatalf("DueAt = %v, want %v", got.DueAt, want)
	}
	if !got.RolledOver {
		t.Error("RolledOver = false, want true: the user must be told the time moved")
	}
	if got.Rationale == "" {
		t.Error("Rationale is empty; the rollover must be explained")
	}
}

// An explicit future day must never roll forward.
func TestParseExplicitDayDoesNotRollOver(t *testing.T) {
	got := Parse("tomorrow 9am", ref)
	if got.RolledOver {
		t.Error("RolledOver = true for an explicitly named future day")
	}
}

// The confidence gate is the core safety property: anything genuinely
// ambiguous must land below the confirmation threshold.
func TestAmbiguousParsesStayBelowThreshold(t *testing.T) {
	const threshold = 0.85
	cases := []string{
		"in 2 hours",    // timer or reminder, no cue word
		"in 20 minutes", // same: a bare offset does not say which it is
		"at 9 to call",  // am or pm
		"tomorrow",      // no time of day at all
		"at 3:30 to sync",
	}
	for _, in := range cases {
		got := Parse(in, ref)
		if !got.OK {
			t.Errorf("Parse(%q) did not match anything", in)
			continue
		}
		if !got.Ambiguous {
			t.Errorf("Parse(%q).Ambiguous = false, want true", in)
		}
		if got.Ambiguity == "" {
			t.Errorf("Parse(%q).Ambiguity is empty; the user must be told why", in)
		}
		if got.Confidence >= threshold {
			t.Errorf("Parse(%q).Confidence = %v, must be below the %v gate",
				in, got.Confidence, threshold)
		}
	}
}

func TestUnambiguousParsesClearTheThreshold(t *testing.T) {
	const threshold = 0.85
	cases := []string{
		"count 20 min",
		"remind me at 3 PM to email the invoice",
		"tomorrow 9am",
		"remind me in 20 minutes to stretch",
	}
	for _, in := range cases {
		got := Parse(in, ref)
		if got.Ambiguous {
			t.Errorf("Parse(%q).Ambiguous = true, want false", in)
		}
		if got.Confidence < threshold {
			t.Errorf("Parse(%q).Confidence = %v, want >= %v", in, got.Confidence, threshold)
		}
	}
}

func TestLabelCapturesUserWords(t *testing.T) {
	cases := []struct{ in, want string }{
		{"remind me at 3 PM to email the invoice", "email the invoice"},
		{"remind me in 20 min to call Bob", "call Bob"},
		{"at 15:00 to deploy the api", "deploy the api"},
		{"remind me today at 3 PM to send the contract", "send the contract"},
	}
	for _, tc := range cases {
		got := Parse(tc.in, ref)
		if got.Label != tc.want {
			t.Errorf("Parse(%q).Label = %q, want %q", tc.in, got.Label, tc.want)
		}
	}
}

// A bare duration has no label left once the time expression is consumed.
// That is fine: the UI names it by kind rather than echoing the input back.
func TestBareTimerHasEmptyLabel(t *testing.T) {
	got := Parse("count 20 min", ref)
	if got.Label != "" {
		t.Errorf("Label = %q, want empty", got.Label)
	}
}

// Guards against the unit regex eating the first letter of a longer word.
func TestDoesNotParseSubstringsOfWords(t *testing.T) {
	cases := []string{
		"remind me in 5 samples to review",
		"hello world",
		"buy milk",
		"remind me to check 3 dashboards",
	}
	for _, in := range cases {
		if got := Parse(in, ref); got.OK {
			t.Errorf("Parse(%q).OK = true, want false (false positives are worse than misses)", in)
		}
	}
}

// Every parse must explain itself; a blank rationale is a bug.
func TestRationaleAlwaysPresent(t *testing.T) {
	inputs := []string{
		"count 20 min", "remind me at 3 PM to email", "tomorrow 9am",
		"in 2 hours", "at 9 to call", "tomorrow", "half an hour",
	}
	for _, in := range inputs {
		got := Parse(in, ref)
		if got.OK && got.Rationale == "" {
			t.Errorf("Parse(%q).Rationale is empty", in)
		}
		if got.OK && got.Tier != TierRules {
			t.Errorf("Parse(%q).Tier = %q, want %q", in, got.Tier, TierRules)
		}
	}
}

// Day arithmetic must stay on the calendar, not on 24h arithmetic, so a
// reminder keeps its wall-clock time across a DST transition.
func TestDayAnchorSurvivesDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// The Brazilian DST clock change falls on the first Sunday of November.
	before := time.Date(2026, 10, 30, 22, 0, 0, 0, loc)
	got := Parse("tomorrow 9am", before)
	want := time.Date(2026, 10, 31, 9, 0, 0, 0, loc)
	if !got.DueAt.Equal(want) {
		t.Fatalf("DueAt = %v, want %v", got.DueAt, want)
	}
	if got.DueAt.Hour() != 9 {
		t.Errorf("hour = %d, want 9: the wall-clock time must not drift over DST", got.DueAt.Hour())
	}
}

func BenchmarkParse(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Parse("remind me at 3 PM to email the invoice about invoice 42", ref)
	}
}
