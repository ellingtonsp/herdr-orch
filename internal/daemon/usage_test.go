package daemon

import (
	"testing"
	"time"
)

func TestDetectUsageCap(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 15, 12, 30, 0, 0, loc)
	at := func(day, hour, minute int) time.Time { return time.Date(2030, 1, day, hour, minute, 0, 0, loc) }
	cases := []struct {
		name, screen, notice string
		reset                time.Time
		ok                   bool
	}{
		{name: "unknown reset", screen: "You've hit your session limit", notice: "you've hit your session limit", ok: true},
		{name: "local noon suffix", screen: "Usage limit reached · limit resets 1pm", notice: "usage limit reached", reset: at(15, 13, 0), ok: true},
		{name: "ANSI and curly apostrophe", screen: "\x1b[31m⎿ You’ve hit your limit · resets 1:45 PM\x1b[0m", notice: "you've hit your limit", reset: at(15, 13, 45), ok: true},
		{name: "next day", screen: "Usage limit reached. Resets at 09:00", notice: "usage limit reached", reset: at(16, 9, 0), ok: true},
		{name: "midnight", screen: "Usage limit reached · resets 12am", notice: "usage limit reached", reset: at(16, 0, 0), ok: true},
		{name: "noon", screen: "Usage limit reached · resets 12pm", notice: "usage limit reached", reset: at(16, 12, 0), ok: true},
		{name: "adjacent reset", screen: "● You've hit your session limit\n  Your limit resets in 2 hours 15 minutes", notice: "you've hit your session limit", reset: at(15, 14, 45), ok: true},
		{name: "compact duration", screen: "Usage limit reached · resets in 1h 30m", notice: "usage limit reached", reset: at(15, 14, 0), ok: true},
		{name: "joined duration", screen: "Usage limit reached · resets in 1h30m", notice: "usage limit reached", reset: at(15, 14, 0), ok: true},
		{name: "output warning and spacing", screen: "⚠ Warning: Usage   limit reached · resets at 14:10", notice: "usage limit reached", reset: at(15, 14, 10), ok: true},
		{name: "OSC hyperlink", screen: "\x1b]8;;https://example.invalid\x1b\\Usage limit reached\x1b]8;;\x1b\\", notice: "usage limit reached", ok: true},
		{name: "RFC3339", screen: "Usage limit reached · resets at 2030-01-15T23:00:00Z", notice: "usage limit reached", reset: time.Date(2030, 1, 15, 23, 0, 0, 0, time.UTC), ok: true},
		{name: "IANA zone", screen: "Usage limit reached · resets 4pm (America/New_York)", notice: "usage limit reached", reset: at(15, 13, 0), ok: true},
		{name: "date", screen: "Usage limit reached · resets on 2030-01-17 at 14:00", notice: "usage limit reached", reset: at(17, 14, 0), ok: true},
		{name: "invalid clock", screen: "Usage limit reached · resets 25:99", notice: "usage limit reached", ok: true},
		{name: "invalid bare clock", screen: "Usage limit reached · resets 1", notice: "usage limit reached", ok: true},
		{name: "invalid meridiem clock", screen: "Usage limit reached · resets 13pm", notice: "usage limit reached", ok: true},
		{name: "negative duration", screen: "Usage limit reached · resets in -1 hour", notice: "usage limit reached", ok: true},
		{name: "duration alternatives", screen: "Usage limit reached · resets in 1 hour or 2 hours", notice: "usage limit reached", ok: true},
		{name: "overflow duration", screen: "Usage limit reached · resets in 99999999999999999999 hours", notice: "usage limit reached", ok: true},
		{name: "ambiguous zone", screen: "Usage limit reached · resets 1pm CST", notice: "usage limit reached", ok: true},
		{name: "invalid zone", screen: "Usage limit reached · resets 1pm (Invalid/Zone)", notice: "usage limit reached", ok: true},
		{name: "invalid date", screen: "Usage limit reached · resets on 2030-02-30 at 14:00", notice: "usage limit reached", ok: true},
		{name: "ambiguous multiple resets", screen: "Usage limit reached · resets 1pm or 3pm", notice: "usage limit reached", ok: true},
		{name: "ambiguous adjacent reset", screen: "Usage limit reached · resets 1pm\nLimit resets at 3pm", notice: "usage limit reached", ok: true},
		{name: "unrelated later reset", screen: "Usage limit reached\nOther output\nLimit resets 1pm", notice: "usage limit reached", ok: true},
		{name: "unknown descriptive reset", screen: "Usage limit reached · resets tomorrow morning", notice: "usage limit reached", ok: true},
		{name: "blank", screen: ""},
		{name: "prompt", screen: "❯ Usage limit reached · limit resets 1pm"},
		{name: "quoted", screen: "\"Usage limit reached · limit resets 1pm\""},
		{name: "blockquote", screen: "> You've hit your session limit"},
		{name: "inline example", screen: "For example: Usage limit reached · resets 1pm"},
		{name: "code assignment", screen: "notice := `You've hit your session limit`"},
		{name: "fenced code", screen: "```text\nUsage limit reached · resets 1pm\n```"},
		{name: "tilde code fence", screen: "~~~\nYou've hit your session limit\n~~~"},
		{name: "discussion", screen: "Usage limit reached is an example string used by the parser."},
		{name: "quoted suffix", screen: "Usage limit reached · resets 1pm`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notice, reset, ok := detectUsageCap(tc.screen, now)
			if notice != tc.notice || !reset.Equal(tc.reset) || ok != tc.ok {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, %v)", notice, reset, ok, tc.notice, tc.reset, tc.ok)
			}
		})
	}
}

func TestUsageCapNoticeStableAcrossCountdownTicks(t *testing.T) {
	now := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	first, _, ok := detectUsageCap("Usage limit reached · resets in 30 minutes", now)
	second, _, nextOK := detectUsageCap("\x1b[31mUsage limit reached · resets in 29 minutes\x1b[0m", now.Add(time.Minute))
	if !ok || !nextOK || first == "" || first != second {
		t.Fatalf("notice changed between ticks: %q, %q", first, second)
	}
}
