package daemon

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	usageANSI         = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b\\[[0-?]*[ -/]*[@-~]")
	usagePhrase       = regexp.MustCompile(`(?i)^(usage limit (?:has been )?reached|session limit reached|you(?:'ve| have) hit (?:your|the) (?:session |usage )?limit|you(?:'ve| have) reached your (?:session |usage )?limit)\b`)
	usageReset        = regexp.MustCompile(`(?i)^(?:(?:your|the|usage|session) )?(?:limit )?resets?\s+(?:(at|on|in)\s+)?(.+)$`)
	usageClock        = regexp.MustCompile(`(?i)^(\d{1,2})(?::(\d{2}))?\s*(am|pm)?$`)
	usageDate         = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})\s+(?:at\s+)?(.+)$`)
	usageZone         = regexp.MustCompile(`\s+(\([A-Za-z0-9_+/-]+\)|[A-Za-z0-9_+-]+(?:/[A-Za-z0-9_+-]+)+|UTC)$`)
	usageDurationUnit = regexp.MustCompile(`(?i)(\d+)\s*(hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
)

// detectUsageCap recognizes a standalone CLI cap notice, never arbitrary mentions
// of one in prose, prompts, or fenced examples. The canonical notice omits reset
// text so a changing countdown cannot create another event every tick. A reset
// is returned only when the notice or its immediately adjacent line supplies an
// unambiguous value; the caller decides what to do when it remains unknown.
func detectUsageCap(screen string, now time.Time) (notice string, reset time.Time, ok bool) {
	screen = usageANSI.ReplaceAllString(screen, "")
	screen = strings.NewReplacer("’", "'", "\r", "").Replace(screen)
	lines := strings.Split(screen, "\n")
	fence := ""
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			marker := line[:3]
			if fence == "" {
				fence = marker
			} else if fence == marker {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		line = usageNoticeLine(line)
		match := usagePhrase.FindString(line)
		if match == "" || strings.ContainsAny(line, "`\"“”") || strings.HasSuffix(line, "'") {
			continue
		}
		tail := strings.TrimSpace(line[len(match):])
		// Only punctuation or a reset clause can follow the phrase. This avoids
		// treating explanatory prose such as "Usage limit reached is ..." as UI.
		if tail != "" && !strings.ContainsRune(".·•—–-|", []rune(tail)[0]) && !usageReset.MatchString(tail) {
			continue
		}
		tail = strings.TrimSpace(strings.TrimLeft(tail, ".·•—–-| "))
		if tail != "" && !usageReset.MatchString(tail) {
			continue
		}
		notice = strings.ToLower(strings.Join(strings.Fields(match), " "))
		notice = strings.ReplaceAll(notice, "you have", "you've")
		candidates := []string{}
		if tail != "" {
			candidates = append(candidates, tail)
		}
		if i+1 < len(lines) {
			next := usageNoticeLine(strings.TrimSpace(lines[i+1]))
			if usageReset.MatchString(next) {
				candidates = append(candidates, next)
			}
		}
		if len(candidates) == 1 {
			reset = parseUsageReset(candidates[0], now)
		}
		return notice, reset, true
	}
	return "", time.Time{}, false
}

// These are output decorations, not input prompt or Markdown quote markers.
func usageNoticeLine(line string) string {
	line = strings.TrimSpace(strings.TrimLeft(line, "⎿●•⚠│┃ "))
	line = strings.Join(strings.Fields(line), " ")
	for _, prefix := range []string{"Error:", "Warning:"} {
		if len(line) >= len(prefix) && strings.EqualFold(line[:len(prefix)], prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return line
}

func parseUsageReset(clause string, now time.Time) time.Time {
	m := usageReset.FindStringSubmatch(clause)
	if m == nil {
		return time.Time{}
	}
	value := strings.TrimSpace(strings.TrimRight(m[2], ". │┃"))
	if strings.EqualFold(m[1], "in") {
		return parseUsageDuration(value, now)
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	loc := now.Location()
	if zone := usageZone.FindStringSubmatch(value); zone != nil {
		name := strings.Trim(zone[1], "()")
		var err error
		loc, err = time.LoadLocation(name)
		if err != nil {
			return time.Time{}
		}
		value = strings.TrimSpace(value[:len(value)-len(zone[0])])
	}
	localNow := now.In(loc)
	year, month, day := localNow.Date()
	explicitDate := false
	if date := usageDate.FindStringSubmatch(value); date != nil {
		year, _ = strconv.Atoi(date[1])
		monthNumber, _ := strconv.Atoi(date[2])
		month = time.Month(monthNumber)
		day, _ = strconv.Atoi(date[3])
		value = date[4]
		explicitDate = true
	}
	clock := usageClock.FindStringSubmatch(value)
	if clock == nil {
		return time.Time{}
	}
	hour, _ := strconv.Atoi(clock[1])
	minute, _ := strconv.Atoi(clock[2])
	if clock[3] != "" {
		if hour < 1 || hour > 12 {
			return time.Time{}
		}
		hour %= 12
		if strings.EqualFold(clock[3], "pm") {
			hour += 12
		}
	} else if clock[2] == "" {
		// A bare "1" supplies neither a 24-hour clock nor a meridiem.
		return time.Time{}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}
	}
	parsed := time.Date(year, month, day, hour, minute, 0, 0, loc)
	if parsed.Year() != year || parsed.Month() != month || parsed.Day() != day || parsed.Hour() != hour || parsed.Minute() != minute {
		return time.Time{} // Invalid calendar date or a nonexistent DST clock.
	}
	if !explicitDate && !parsed.After(now) {
		nextDay := time.Date(year, month, day+1, 12, 0, 0, 0, loc)
		year, month, day = nextDay.Date()
		parsed = time.Date(year, month, day, hour, minute, 0, 0, loc)
		if parsed.Hour() != hour || parsed.Minute() != minute {
			return time.Time{}
		}
	}
	// A local clock in a DST fold has two possible instants. Leave it unknown.
	for _, delta := range []time.Duration{-2 * time.Hour, -time.Hour, time.Hour, 2 * time.Hour} {
		other := parsed.Add(delta)
		if other.Year() == parsed.Year() && other.YearDay() == parsed.YearDay() && other.Hour() == hour && other.Minute() == minute {
			return time.Time{}
		}
	}
	return parsed
}

func parseUsageDuration(value string, now time.Time) time.Time {
	// Parse only an entire positive duration, not alternatives or approximations.
	parts := usageDurationUnit.FindAllStringSubmatchIndex(value, -1)
	if len(parts) == 0 {
		return time.Time{}
	}
	var compact strings.Builder
	end := 0
	for _, part := range parts {
		if strings.TrimSpace(value[end:part[0]]) != "" {
			return time.Time{}
		}
		compact.WriteString(value[part[2]:part[3]])
		unit := strings.ToLower(value[part[4]:part[5]])
		compact.WriteByte(unit[0])
		end = part[1]
	}
	if strings.TrimSpace(value[end:]) != "" {
		return time.Time{}
	}
	duration, err := time.ParseDuration(compact.String())
	if err != nil || duration <= 0 {
		return time.Time{}
	}
	return now.Add(duration)
}
