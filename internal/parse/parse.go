// Package parse implements Tier 0: the deterministic Go grammar.
//
// Tier 0 exists so that the common cases cost nothing, need no network, and
// return in microseconds. It is the only tier that can extract free text, so
// it runs first and its output short-circuits everything else.
//
// Design rules, straight from the PRD:
//   - Ambiguity is an explicit outcome, never a guess.
//   - A parse carries a confidence and a rationale the user can read.
//   - A below-threshold result must not schedule anything on its own.
package parse

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind is the task type.
type Kind string

// Kinds.
const (
	KindTimer    Kind = "timer"
	KindReminder Kind = "reminder"
)

// TierRules is the tier identifier recorded on every Tier 0 parse.
const TierRules = "T0"

// Result is the outcome of one parse attempt.
type Result struct {
	// OK is true when any time expression was found. A false OK is not a
	// failure: it means the cascade should escalate to a decision model.
	OK bool
	// Kind is timer or reminder.
	Kind Kind
	// Label is the user's own words for the task, case preserved.
	Label string
	// Duration is set for timers.
	Duration time.Duration
	// DueAt is the absolute fire time. Set for both kinds: a duration is
	// resolved to a wall-clock deadline at arm time so it survives a restart.
	DueAt time.Time
	// Matched is the time expression that was consumed, for the preview UI.
	Matched string
	// Rationale is a human-readable explanation of the parse.
	Rationale string
	// Confidence is the Tier 0 belief that this parse matches user intent.
	Confidence float64
	// Ambiguous marks a parse that is plausible but not certain. Ambiguous
	// results are capped below the confirmation threshold.
	Ambiguous bool
	// Ambiguity explains why, and is shown in the confirmation preview.
	Ambiguity string
	// RolledOver is set when a named time already passed and moved to the
	// following day. Surfaced rather than silently applied.
	RolledOver bool
	// Tier is always TierRules here.
	Tier string
}

// span is a half-open byte range into the prepared input.
type span struct {
	start int
	end   int
}

func (s span) text(src string) string {
	if s.start < 0 || s.end > len(src) || s.start > s.end {
		return ""
	}
	return src[s.start:s.end]
}

func submatchSpan(idx []int, group int) span {
	if group*2+1 >= len(idx) || idx[group*2] < 0 {
		return span{}
	}
	return span{idx[group*2], idx[group*2+1]}
}

// Expressions. Every regex is compiled once at package init and anchored with
// \b on both sides so "5 samples" is never read as "5 seconds".
var (
	reCompoundDur = regexp.MustCompile(`(?i)\b(\d+)\s*(?:h|hr|hrs|hour|hours)\s*(\d+)\s*(?:m|min|mins|minute|minutes)\b`)
	// "1h30" with no trailing unit on the minutes.
	reCompoundTight = regexp.MustCompile(`(?i)\b(\d+)\s*(?:h|hr|hrs|hour|hours)(\d+)\b`)
	reSingleDur     = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(h(?:ou?rs?|rs?)?|m(?:in(?:ute)?s?)?|s(?:ec(?:ond)?s?)?|d(?:ays?)?|w(?:eeks?)?)\b`)
	reArticleDur    = regexp.MustCompile(`(?i)\b(?:a|an|one)\s+(minute|hour|day|week)\b`)
	rePhraseDur     = regexp.MustCompile(`(?i)\b(half an hour|half-hour|half hour|hour and a half|an hour and a half|a quarter of an hour|quarter of an hour|a quarter hour|a quarter of a minute)\b`)

	reRelative = regexp.MustCompile(`(?i)\bin\s+(?:about\s+|approximately\s+|around\s+)?`)

	reClockColon = regexp.MustCompile(`(?i)\b(\d{1,2}):(\d{2})\s*(a\.?m\.?|p\.?m\.?)?`)
	reClockMer   = regexp.MustCompile(`(?i)\b(\d{1,2})\s*(a\.?m\.?|p\.?m\.?)\b`)
	reClock24    = regexp.MustCompile(`\b(1[3-9]|2[0-3])h\b`)
	reClockNamed = regexp.MustCompile(`(?i)\b(noon|midday|midnight)\b`)
	reClockAtBar = regexp.MustCompile(`(?i)\bat\s+(\d{1,2})\b`)

	reDay = regexp.MustCompile(`(?i)\b(today|tonight|tomorrow|tmrw|tono|monday|mon|tuesday|tue|tues|wednesday|wed|thursday|thu|thur|thurs|friday|fri|saturday|sat|sunday|sun|next week|eow|eod|end of day|end of week)\b`)

	// Cue phrases. Anchored at the start so a cue word inside the label
	// survives, e.g. "remember the milk" keeps nothing but "the milk".
	reTimerCue = regexp.MustCompile(`(?i)^(?:please\s+)?(?:start\s+)?(?:a\s+|an\s+)?(?:count(?:down)?|timer|time-?box|pomodoro|focus(?:\s+session)?|stopwatch)\b\s*`)
	reRemindCu = regexp.MustCompile(`(?i)^(?:please\s+)?(?:remind me|reminder|remind|remember(?: me)?(?: to)?|alert me|alert|notify me|notify|nudge me|nudge|ping me|ping|wake me)\b[\s:,]*`)

	reLeadTo = regexp.MustCompile(`(?i)^(?:at|to|that|about|for|on)\s+`)
	reTrim   = regexp.MustCompile(`^[\s,.:;!?\-]+|[\s,.:;!?\-]+$`)
)

var unitScale = map[string]time.Duration{
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
}

var articleScale = map[string]time.Duration{
	"minute": time.Minute,
	"hour":   time.Hour,
	"day":    24 * time.Hour,
	"week":   7 * 24 * time.Hour,
}

var phraseScale = map[string]time.Duration{
	"half an hour":          30 * time.Minute,
	"half-hour":             30 * time.Minute,
	"half hour":             30 * time.Minute,
	"hour and a half":       90 * time.Minute,
	"an hour and a half":    90 * time.Minute,
	"a quarter of an hour":  15 * time.Minute,
	"quarter of an hour":    15 * time.Minute,
	"a quarter hour":        15 * time.Minute,
	"a quarter of a minute": 15 * time.Second,
}

// prepare collapses whitespace and folds typographic punctuation while
// preserving case, because the captured label is the user's own text.
func prepare(s string) string {
	folded := strings.Map(func(r rune) rune {
		switch r {
		case '\u2019', '\u2018':
			return '\''
		case '\u201c', '\u201d':
			return '"'
		case '\u2013', '\u2014':
			return '-'
		case '\u00a0':
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(folded), " ")
}

func trimPunct(s string) string {
	for {
		out := reTrim.ReplaceAllString(s, "")
		if out == s {
			return out
		}
		s = out
	}
}

func quote(s string) string { return "\"" + s + "\"" }

// durHit is a matched duration expression.
type durHit struct {
	sp  span
	dur time.Duration
	txt string
}

// findDuration locates the first duration expression. Compound forms are tried
// before single tokens so "1h30" is never read as "1 hour".
func findDuration(s string) (durHit, bool) {
	if idx := reCompoundDur.FindStringSubmatchIndex(s); idx != nil {
		hours := atoiBetween(s, idx, 1)
		mins := atoiBetween(s, idx, 2)
		d := time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute
		sp := span{idx[0], idx[1]}
		return durHit{sp: sp, dur: d, txt: sp.text(s)}, true
	}

	if idx := reCompoundTight.FindStringSubmatchIndex(s); idx != nil {
		hours := atoiBetween(s, idx, 1)
		mins := atoiBetween(s, idx, 2)
		if mins > 59 {
			return durHit{}, false
		}
		d := time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute
		sp := span{idx[0], idx[1]}
		return durHit{sp: sp, dur: d, txt: sp.text(s)}, true
	}

	if idx := rePhraseDur.FindStringSubmatchIndex(s); idx != nil {
		sp := span{idx[0], idx[1]}
		d := phraseScale[strings.ToLower(sp.text(s))]
		return durHit{sp: sp, dur: d, txt: sp.text(s)}, true
	}

	if idx := reArticleDur.FindStringSubmatchIndex(s); idx != nil {
		sp := span{idx[0], idx[1]}
		d := articleScale[strings.ToLower(submatchSpan(idx, 1).text(s))]
		return durHit{sp: sp, dur: d, txt: sp.text(s)}, true
	}

	if idx := reSingleDur.FindStringSubmatchIndex(s); idx != nil {
		n, err := strconv.ParseFloat(submatchSpan(idx, 1).text(s), 64)
		if err != nil {
			return durHit{}, false
		}
		unit := strings.ToLower(submatchSpan(idx, 2).text(s))
		sp := span{idx[0], idx[1]}
		d := time.Duration(n * float64(unitScale[unit]))
		return durHit{sp: sp, dur: d, txt: sp.text(s)}, true
	}

	return durHit{}, false
}

func atoiBetween(s string, idx []int, group int) int {
	n, _ := strconv.Atoi(submatchSpan(idx, group).text(s))
	return n
}

// clockHit is a matched wall-clock time.
type clockHit struct {
	sp        span
	hour      int
	min       int
	txt       string
	style     string // colon | meridiem | h24 | named | atbare
	meridiem  string
	ambiguous bool
	why       string
}

// clockLabel renders a wall-clock time in the user's own 12-hour style.
func clockLabel(h, m int) string {
	suffix := "am"
	if h >= 12 {
		suffix = "pm"
	}
	hh := h % 12
	if hh == 0 {
		hh = 12
	}
	if m == 0 {
		return strconv.Itoa(hh) + " " + suffix
	}
	return strconv.Itoa(hh) + ":" + pad2(m) + " " + suffix
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// applyMeridiem resolves 12-hour input against the 24-hour field.
func applyMeridiem(h int, mer string) int {
	switch mer {
	case "pm":
		if h < 12 {
			return h + 12
		}
	case "am":
		if h == 12 {
			return 0
		}
	}
	return h
}

// findClock locates the first wall-clock time, most specific form first.
func findClock(s string) (clockHit, bool) {
	// Colon form first: "3:30" outranks the bare number inside it.
	if idx := reClockColon.FindStringSubmatchIndex(s); idx != nil {
		h := atoiBetween(s, idx, 1)
		mn := atoiBetween(s, idx, 2)
		if h > 23 || mn > 59 {
			return clockHit{}, false
		}
		hit := clockHit{}
		hit.sp = span{idx[0], idx[1]}
		hit.hour = h
		hit.min = mn
		hit.txt = hit.sp.text(s)
		hit.style = "colon"
		if idx[6] >= 0 {
			hit.meridiem = normaliseMeridiem(submatchSpan(idx, 3).text(s))
		}
		if hit.meridiem == "" && h <= 12 {
			hit.ambiguous = true
			hit.why = quote(hit.txt) + " has no am/pm - reading it as " + clockLabel(h, mn) + "."
		}
		return hit, true
	}

	if idx := reClockNamed.FindStringSubmatchIndex(s); idx != nil {
		hit := clockHit{}
		hit.sp = span{idx[0], idx[1]}
		hit.txt = hit.sp.text(s)
		hit.style = "named"
		hit.hour = 12
		if strings.ToLower(hit.txt) == "midnight" {
			hit.hour = 0
		}
		return hit, true
	}

	if idx := reClockMer.FindStringSubmatchIndex(s); idx != nil {
		h := atoiBetween(s, idx, 1)
		if h < 1 || h > 12 {
			return clockHit{}, false
		}
		hit := clockHit{}
		hit.sp = span{idx[0], idx[1]}
		hit.hour = h
		hit.txt = hit.sp.text(s)
		hit.style = "meridiem"
		hit.meridiem = normaliseMeridiem(submatchSpan(idx, 2).text(s))
		return hit, true
	}

	if idx := reClock24.FindStringSubmatchIndex(s); idx != nil {
		hit := clockHit{}
		hit.sp = span{idx[0], idx[1]}
		hit.hour = atoiBetween(s, idx, 1)
		hit.txt = hit.sp.text(s)
		hit.style = "h24"
		return hit, true
	}

	if idx := reClockAtBar.FindStringSubmatchIndex(s); idx != nil {
		h := atoiBetween(s, idx, 1)
		if h < 1 || h > 23 {
			return clockHit{}, false
		}
		hit := clockHit{}
		hit.sp = span{idx[0], idx[1]}
		hit.hour = h
		hit.txt = hit.sp.text(s)
		hit.style = "atbare"
		if h == 12 {
			hit.ambiguous = true
			hit.why = "\"at 12\" could be 12 am or 12 pm."
		} else if h <= 12 {
			hit.ambiguous = true
			hit.why = quote("at "+strconv.Itoa(h)) + " could be " +
				clockLabel(h, 0) + " or " + clockLabel(h+12, 0) + "."
		}
		return hit, true
	}

	return clockHit{}, false
}

func normaliseMeridiem(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, ".", ""))
}

// dayRef is a resolved calendar day plus how the user named it.
type dayRef struct {
	day      time.Time
	named    bool
	explicit bool // the user named a day other than today
	word     string
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// addDays advances by calendar days, keeping wall-clock semantics correct
// across DST boundaries where 23h and 25h days exist.
func addDays(t time.Time, n int) time.Time {
	loc := t.Location()
	return time.Date(t.Year(), t.Month(), t.Day()+n, 0, 0, 0, 0, loc)
}

func fridayOnOrAfter(today time.Time) time.Time {
	delta := (int(time.Friday) - int(today.Weekday()) + 7) % 7
	return addDays(today, delta)
}

func atClock(day time.Time, h, m int) time.Time {
	loc := day.Location()
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

func resolveDay(s string, now time.Time) dayRef {
	loc := now.Location()
	today := startOfDay(now, loc)
	idx := reDay.FindStringSubmatchIndex(s)
	if idx == nil {
		return dayRef{day: today}
	}

	word := strings.ToLower(submatchSpan(idx, 1).text(s))
	switch word {
	case "today", "tonight", "eod", "end of day":
		return dayRef{day: today, named: true, word: word}
	case "tomorrow", "tmrw", "tono":
		return dayRef{day: addDays(today, 1), named: true, explicit: true, word: word}
	case "eow", "end of week":
		return dayRef{day: fridayOnOrAfter(today), named: true, explicit: true, word: word}
	case "next week":
		return dayRef{day: addDays(today, 7), named: true, explicit: true, word: word}
	}

	if target, ok := weekdayNames[word]; ok {
		delta := (int(target) - int(today.Weekday()) + 7) % 7
		return dayRef{
			day:      addDays(today, delta),
			named:    true,
			explicit: delta > 0,
			word:     word,
		}
	}
	return dayRef{day: today, named: true, word: word}
}

// Parse runs the Tier 0 grammar. now fixes "today" and supplies the reference
// point for relative offsets; its Location is used for all resolution.
func Parse(input string, now time.Time) Result {
	src := prepare(input)
	if src == "" {
		return Result{Tier: TierRules}
	}

	timerCued := reTimerCue.MatchString(src)
	remindCued := reRemindCu.MatchString(src)

	day := resolveDay(src, now)
	clock, hasClock := findClock(src)
	dur, hasDur := findDuration(src)
	rel := reRelative.FindStringIndex(src)

	switch {
	case rel != nil && hasDur:
		sp := span{rel[0], dur.sp.end}
		return finalize(src, []span{sp}, relativeResult(dur, timerCued, remindCued, now, sp.text(src)))

	case hasClock:
		return finalize(src, []span{clock.sp}, reminderFromClock(clock, day, now, remindCued))

	case hasDur:
		res := timerResult(dur.dur, timerCued, remindCued, now)
		res.Matched = dur.txt
		res.Rationale = "Duration " + quote(dur.txt) + " read as " + durationLabel(dur.dur) + "."
		if !timerCued && !remindCued {
			// A bare unit is a time-box by convention, not an appointment.
			res.Kind = KindTimer
			res.Confidence = 0.90
			res.Rationale += " No cue word, so a bare duration is read as a timer."
		}
		return finalize(src, []span{dur.sp}, res)

	case day.named:
		idx := reDay.FindStringIndex(src)
		sp := span{idx[0], idx[1]}
		res := dayOnlyResult(day, now)
		res.Matched = sp.text(src)
		return finalize(src, []span{sp}, res)
	}

	return Result{Tier: TierRules}
}

func relativeResult(d durHit, timerCued, remindCued bool, now time.Time, matched string) Result {
	res := timerResult(d.dur, timerCued, remindCued, now)
	res.Matched = matched
	res.Rationale = "Relative offset " + quote(matched) + " from now."
	if !timerCued && !remindCued {
		res.Kind = KindTimer
		res.Ambiguous = true
		res.Ambiguity = "No " + quote("remind me") + " or " + quote("count") +
			" cue, so this is read as a timer. Say " +
			quote("remind me in "+matched) + " to make it a reminder."
		res.Confidence = 0.70
	}
	return res
}

func timerResult(d time.Duration, timerCued, remindCued bool, now time.Time) Result {
	res := Result{OK: true, Kind: KindTimer, Duration: d, DueAt: now.Add(d), Tier: TierRules}
	switch {
	case timerCued:
		res.Confidence = 0.95
	case remindCued:
		res.Kind = KindReminder
		res.Confidence = 0.95
	}
	return res
}

func dayOnlyResult(day dayRef, now time.Time) Result {
	h, m := 9, 0
	why := "9:00 am"
	switch day.word {
	case "tonight":
		h, why = 20, "8:00 pm"
	case "eod", "end of day", "eow", "end of week":
		h, why = 17, "5:00 pm"
	}

	due := atClock(day.day, h, m)
	rolled := false
	if due.Before(now) {
		due = atClock(addDays(day.day, 1), h, m)
		rolled = true
	}

	return Result{
		OK:         true,
		Kind:       KindReminder,
		DueAt:      due,
		RolledOver: rolled,
		Confidence: 0.75,
		Ambiguous:  true,
		Ambiguity:  "No time of day given, so this is read as " + why + " on " + day.word + ".",
		Rationale:  "Day anchor " + quote(day.word) + " with no time, defaulted to " + why + ".",
		Tier:       TierRules,
	}
}

func reminderFromClock(c clockHit, day dayRef, now time.Time, cued bool) Result {
	h, m := c.hour, c.min
	if c.meridiem != "" {
		h = applyMeridiem(h, c.meridiem)
	}
	if day.word == "tonight" && c.hour < 12 {
		h = 20
	}

	due := atClock(day.day, h, m)
	rolled := false
	if due.Before(now) && !day.explicit {
		due = atClock(addDays(day.day, 1), h, m)
		rolled = true
	}

	res := Result{
		OK:         true,
		Kind:       KindReminder,
		DueAt:      due,
		Matched:    c.txt,
		Tier:       TierRules,
		Confidence: 0.90,
	}
	if cued {
		res.Confidence = 0.95
	}
	res.Rationale = "Clock time " + quote(c.txt) + " on " + dayPhrase(day) + "."
	if rolled {
		res.RolledOver = true
		res.Rationale += " That time already passed today, so it moved to tomorrow."
	}
	if c.ambiguous {
		res.Ambiguous = true
		res.Ambiguity = c.why
		res.Confidence = 0.65
	}
	return res
}

func dayPhrase(d dayRef) string {
	if d.named {
		return strings.ToLower(d.word)
	}
	return "today"
}

func durationLabel(d time.Duration) string {
	switch {
	case d%(7*24*time.Hour) == 0:
		return plural(int(d/(7*24*time.Hour)), "week")
	case d%(24*time.Hour) == 0:
		return plural(int(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	case d%time.Second == 0:
		return plural(int(d/time.Second), "second")
	default:
		return d.String()
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// finalize strips consumed time expressions and cue words to recover the
// user's own description of the task.
func finalize(src string, consume []span, res Result) Result {
	res.Tier = TierRules
	if !res.OK {
		return res
	}

	label := src
	for _, sp := range consume {
		if sp.start < 0 || sp.end > len(label) || sp.end <= sp.start {
			continue
		}
		label = label[:sp.start] + " " + label[sp.end:]
	}
	label = reTimerCue.ReplaceAllString(label, " ")
	label = reRemindCu.ReplaceAllString(label, " ")
	label = reDay.ReplaceAllString(label, " ")
	// "remind me at 3 PM to email" leaves "at to email" behind. Trim the
	// leading whitespace first so the anchored filler pattern can match, then
	// peel one filler word at a time until the label starts with real content.
	for {
		trimmed := reLeadTo.ReplaceAllString(trimPunct(label), "")
		if trimmed == label {
			break
		}
		label = trimmed
	}
	label = trimPunct(label)
	// An empty label is legitimate: a bare "20 min" has nothing left to say
	// once the time expression is removed, and the UI names it by kind.
	res.Label = label
	return res
}
