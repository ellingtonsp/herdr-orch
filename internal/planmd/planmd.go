// Package planmd reads and writes the day-plan markdown the orchestrate skill publishes
// (<date>.plan.md): the Items, What, Why, Held and Decisions sections become store rows;
// every other section is kept verbatim. Render(Parse(f)) reproduces f, plus an event log.
package planmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// Section kinds.
const (
	Text      = "text"
	Preamble  = "preamble"
	What      = "what"
	Why       = "why"
	Decisions = "decisions"
	Held      = "held"
	Items     = "items"
	Events    = "events" // export only; ignored on import
)

func kindOf(heading string) string {
	h := strings.ToLower(strings.TrimSpace(heading))
	switch {
	case h == "what":
		return What
	case h == "why":
		return Why
	case h == "items":
		return Items
	case strings.HasPrefix(h, "held"):
		return Held
	case strings.HasPrefix(h, "decisions"):
		return Decisions
	case h == "event log":
		return Events
	}
	return Text
}

const none = "—"

var (
	bulletRe = regexp.MustCompile(`^- \*\*(.+?)\*\* — (.*)$`)
	issueRe  = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-\d+\b`)
)

// Parse reads a published plan. Warnings name content that was skipped.
func Parse(md string) (store.PlanSeed, []string, error) {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	sum := sha256.Sum256([]byte(md))
	seed := store.PlanSeed{Hash: hex.EncodeToString(sum[:])}
	var warn []string

	type sec struct {
		heading string
		lines   []string
	}
	secs := []sec{{}}
	for _, l := range strings.Split(md, "\n") {
		if h, ok := strings.CutPrefix(l, "## "); ok {
			secs = append(secs, sec{heading: h})
			continue
		}
		secs[len(secs)-1].lines = append(secs[len(secs)-1].lines, l)
	}

	items := map[string]*store.PlanItem{}
	complete := map[string]bool{}
	var order []string
	get := func(id string, listed bool) *store.PlanItem {
		if it, ok := items[id]; ok {
			return it
		}
		it := &store.PlanItem{ID: id, State: store.ItemPlanned, Issues: []string{}, Listed: listed}
		items[id] = it
		order = append(order, id)
		return it
	}
	var whatRows, whyRows, heldRows [][]string
	for i, s := range secs {
		body := strings.Join(s.lines, "\n")
		if i == 0 {
			if strings.TrimSpace(body) != "" {
				seed.Sections = append(seed.Sections, store.PlanSection{Kind: Preamble, Body: trimBody(body)})
			}
			continue
		}
		k := kindOf(s.heading)
		switch k {
		case Events:
			continue
		case Text:
			seed.Sections = append(seed.Sections, store.PlanSection{Heading: s.heading, Kind: Text, Body: trimBody(body)})
			continue
		}
		seed.Sections = append(seed.Sections, store.PlanSection{Heading: s.heading, Kind: k})
		switch k {
		case Items:
			hdr, rows := table(s.lines)
			col := columns(hdr)
			if _, ok := col["id"]; !ok {
				return seed, warn, fmt.Errorf("section %q: table has no id column", s.heading)
			}
			for _, r := range rows {
				c := cell(r, col)
				id := c("id")
				if id == "" {
					continue
				}
				if _, dup := items[id]; dup {
					return seed, warn, fmt.Errorf("item %s appears twice in %q", id, s.heading)
				}
				it := get(id, true)
				// Exports carry complete canonical rows alongside the readable projection.
				// JSON preserves held-from, unlisted rows, ordering and multiline fields.
				if details := c("details"); details != "" {
					if err := json.Unmarshal([]byte(details), it); err != nil || it.ID != id {
						return seed, warn, fmt.Errorf("item %s: invalid details", id)
					}
					complete[id] = true
					continue
				}
				it.Issues = splitIssues(c("issues"))
				it.Kind, it.Lane, it.Model = c("kind"), c("lane"), c("model")
				it.PR, it.DispatchRef = c("pr"), c("dispatch ref")
				if st := strings.ToLower(c("state")); st != "" {
					it.State = st
				}
			}
		case What:
			hdr, rows := table(s.lines)
			col := columns(hdr)
			for _, r := range rows {
				c := cell(r, col)
				whatRows = append(whatRows, []string{c("#"), c("work"), c("issues"), c("where"), c("model"), c("output")})
			}
		case Why:
			for _, l := range s.lines {
				if m := bulletRe.FindStringSubmatch(l); m != nil {
					whyRows = append(whyRows, []string{m[1], m[2]})
				} else if strings.TrimSpace(l) != "" {
					warn = append(warn, fmt.Sprintf("why: skipped line %q", l))
				}
			}
		case Held:
			hdr, rows := table(s.lines)
			col := columns(hdr)
			for _, r := range rows {
				c := cell(r, col)
				heldRows = append(heldRows, []string{c("item"), c("what"), c("why held")})
			}
		case Decisions:
			for _, l := range s.lines {
				if m := bulletRe.FindStringSubmatch(l); m != nil {
					seed.Decisions = append(seed.Decisions, store.PlanDecision{Title: m[1], Body: m[2]})
				} else if strings.TrimSpace(l) != "" {
					warn = append(warn, fmt.Sprintf("decisions: skipped line %q", l))
				}
			}
		}
	}
	// Items come first so the What table never reorders them.
	for _, r := range whatRows {
		if r[0] == "" {
			continue
		}
		if complete[r[0]] {
			continue
		}
		it := get(r[0], true)
		if !it.Listed {
			it.Listed = true
		}
		it.Title = r[1]
		it.What = store.ItemWhat{Issues: r[2], Where: r[3], Model: r[4], Output: r[5]}
		if len(it.Issues) == 0 {
			it.Issues = issueRe.FindAllString(r[2], -1)
		}
		if r[2] == joinIssues(it.Issues) {
			it.What.Issues = "" // rendered from the item's issues
		}
	}
	for _, r := range whyRows {
		if complete[r[0]] {
			continue
		}
		it, ok := items[r[0]]
		if !ok {
			warn = append(warn, fmt.Sprintf("why: no item %s; kept as an item", r[0]))
			it = get(r[0], true)
		}
		it.Why = r[1]
	}
	for _, r := range heldRows {
		if complete[r[0]] {
			continue
		}
		if r[0] == "" {
			continue
		}
		it, ok := items[r[0]]
		if !ok {
			it = get(r[0], false)
			it.State = store.ItemHeld
			it.Title = r[1]
			it.Issues = issueRe.FindAllString(r[0], -1)
			if it.Issues == nil {
				it.Issues = []string{}
			}
		} else if it.Title == "" {
			it.Title = r[1]
		}
		it.Held, it.HeldReason = true, r[2]
	}
	for _, id := range order {
		seed.Items = append(seed.Items, *items[id])
	}
	return seed, warn, nil
}

func trimBody(s string) string { return strings.TrimRight(s, "\n \t") }

// table returns the header and data rows of the first markdown table in lines.
func table(lines []string) ([]string, [][]string) {
	var hdr []string
	var rows [][]string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "|") {
			if hdr != nil && t != "" {
				break
			}
			continue
		}
		cells := splitRow(t)
		switch {
		case hdr == nil:
			hdr = cells
		case isSep(cells):
		default:
			rows = append(rows, cells)
		}
	}
	return hdr, rows
}

func splitRow(t string) []string {
	t = strings.TrimPrefix(strings.TrimSuffix(t, "|"), "|")
	var out []string
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		switch {
		case t[i] == '\\' && i+1 < len(t) && t[i+1] == '|':
			b.WriteByte('|')
			i++
		case t[i] == '|':
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteByte(t[i])
		}
	}
	return append(out, strings.TrimSpace(b.String()))
}

func isSep(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

func columns(hdr []string) map[string]int {
	m := map[string]int{}
	for i, h := range hdr {
		m[strings.ToLower(h)] = i
	}
	return m
}

func cell(row []string, col map[string]int) func(string) string {
	return func(name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}
}

func splitIssues(s string) []string {
	out := []string{}
	if s == "" || s == none {
		return out
	}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- render ----------

// Render writes the plan back as markdown in the published layout, followed by an
// "Event log" section when events are given.
func Render(v store.PlanView, events []store.PlanEvent) string {
	var parts []string
	have := map[string]bool{}
	for _, s := range v.Plan.Sections {
		switch s.Kind {
		case Preamble:
			parts = append(parts, s.Body)
		case Text:
			parts = append(parts, joinNonEmpty("## "+s.Heading, s.Body))
		default:
			have[s.Kind] = true
			parts = append(parts, joinNonEmpty("## "+s.Heading, renderKind(v, s.Kind, events != nil)))
		}
	}
	if len(v.Plan.Sections) == 0 {
		parts = append(parts, fmt.Sprintf("# Day plan %s", v.Plan.Day))
	}
	for _, k := range []struct{ kind, heading string }{{What, "What"}, {Why, "Why"}, {Decisions, "Decisions"}, {Held, "Held on purpose"}, {Items, "Items"}} {
		if !have[k.kind] {
			if body := renderKind(v, k.kind, events != nil); body != "" {
				parts = append(parts, "## "+k.heading+"\n"+body)
			}
		}
	}
	if len(events) > 0 {
		parts = append(parts, "## Event log\n"+renderEvents(events))
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func joinNonEmpty(head, body string) string {
	if body == "" {
		return head
	}
	return head + "\n" + body
}

func renderKind(v store.PlanView, kind string, complete bool) string {
	var b strings.Builder
	row := func(cells ...string) {
		writeCells(&b, cells)
	}
	switch kind {
	case Items:
		head := []string{"id", "issues", "kind", "lane", "model", "state", "PR", "dispatch ref"}
		if complete {
			head = append(head, "details")
		}
		row(head...)
		b.WriteString("|" + strings.Repeat("---|", len(head)) + "\n")
		for _, it := range v.Items {
			if it.Listed || complete {
				cells := []string{it.ID, joinIssues(it.Issues), it.Kind, it.Lane, it.Model, it.State, it.PR, it.DispatchRef}
				if complete {
					it.Version, it.CreatedAt, it.UpdatedAt = 0, 0, 0
					details, _ := json.Marshal(it)
					cells = append(cells, string(details))
				}
				row(cells...)
			}
		}
	case What:
		row("#", "work", "issues", "where", "model", "output")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, it := range v.Items {
			if it.Listed && it.Title != "" {
				iss := it.What.Issues
				if iss == "" {
					iss = joinIssues(it.Issues)
				}
				row(it.ID, it.Title, iss, it.What.Where, or(it.What.Model, it.Model), it.What.Output)
			}
		}
	case Why:
		for _, it := range v.Items {
			if it.Why != "" {
				why := it.Why
				if complete {
					why = strings.ReplaceAll(strings.ReplaceAll(why, "\r", " "), "\n", " ")
				}
				fmt.Fprintf(&b, "- **%s** — %s\n", it.ID, why)
			}
		}
	case Decisions:
		for _, d := range v.Decisions {
			fmt.Fprintf(&b, "- **%s** — %s\n", d.Title, d.Body)
		}
	case Held:
		n := 0
		for _, it := range v.Items {
			if it.Held {
				if n == 0 {
					row("item", "what", "why held")
					b.WriteString("|---|---|---|\n")
				}
				n++
				row(it.ID, it.Title, it.HeldReason)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func joinIssues(is []string) string {
	if len(is) == 0 {
		return none
	}
	return strings.Join(is, ", ")
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func renderEvents(events []store.PlanEvent) string {
	var b strings.Builder
	b.WriteString("| seq | time (UTC) | actor | kind | approval | op | item | change |\n|---|---|---|---|---|---|---|---|\n")
	for _, e := range events {
		who := e.Actor
		if e.Principal != "" {
			who = e.Principal + " (" + e.Actor + ")"
		}
		appr := ""
		if e.Approval {
			appr = "yes"
		}
		cells := []string{fmt.Sprint(e.Seq), time.UnixMilli(e.TS).UTC().Format("15:04:05"), who, e.ActorKind, appr, e.Op, e.Item, Change(e)}
		writeCells(&b, cells)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeCells(b *strings.Builder, cells []string) {
	b.WriteString("|")
	for _, c := range cells {
		if c == "" {
			b.WriteString(" |")
		} else {
			b.WriteString(" " + esc(c) + " |")
		}
	}
	b.WriteString("\n")
}

// Change summarizes an event: the fields that changed, plus its note.
func Change(e store.PlanEvent) string {
	var before, after map[string]any
	_ = json.Unmarshal(e.Before, &before)
	_ = json.Unmarshal(e.After, &after)
	var parts []string
	switch {
	case e.Op == "item.add":
		parts = append(parts, fmt.Sprintf("added (%v)", after["state"]))
	case e.Op == "item.remove":
		parts = append(parts, "removed")
	case before != nil && after != nil:
		for _, k := range []string{"state", "position", "held", "held_reason", "pr", "dispatch_ref", "lane", "model", "kind", "title", "why", "issues", "status", "ref"} {
			b, a := fmt.Sprint(before[k]), fmt.Sprint(after[k])
			if b != a && (before[k] != nil || after[k] != nil) {
				if k == "position" || k == "held" || k == "held_reason" {
					if e.Op == "item.hold" || e.Op == "item.release" {
						continue
					}
				}
				parts = append(parts, fmt.Sprintf("%s: %s → %s", k, short(before[k]), short(after[k])))
			}
		}
	}
	if e.Note != "" {
		parts = append(parts, e.Note)
	}
	return strings.Join(parts, "; ")
}

func short(v any) string {
	if v == nil {
		return "∅"
	}
	s := fmt.Sprint(v)
	if s == "" {
		return "∅"
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
