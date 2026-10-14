// Package prwatch observes GitHub PRs without changing their lifecycle or checks.
package prwatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Requirement struct {
	Name  string
	AppID int64
}
type Check struct {
	Name, Workflow, State, Conclusion, URL string
	AppID, ID                              int64
}
type TimelineEvent struct {
	ID     string    `json:"id"`
	Kind   string    `json:"__typename"`
	At     time.Time `json:"createdAt"`
	Reason string    `json:"reason"`
}
type PR struct {
	Head     string `json:"headRefOid"`
	Base     string `json:"baseRefName"`
	State    string `json:"state"`
	URL      string `json:"url"`
	PushedAt time.Time
	Events   []TimelineEvent
}
type Snapshot struct {
	PR       PR
	Required []Requirement
	Checks   []Check
	Runs     []Run
	Activity []Run
	Fallback bool
}
type Event struct {
	Message    string `json:"message"`
	Head       string `json:"head"`
	URL        string `json:"url"`
	Escalation bool   `json:"escalation"`
}
type Options struct {
	Until                string
	Interval, RaceWindow time.Duration
}

// Evaluate considers only required checks. No checks is pending, even with no rules.
// When multiple workflows emit the same required context, all must pass.
func Evaluate(s Snapshot) (bool, []Check) {
	if len(s.Required) == 0 {
		for _, c := range s.Checks {
			if c.Name != "" {
				return true, nil
			}
		}
		return false, nil
	}
	green := true
	// A run can expose successful jobs before the workflow itself settles.
	for _, marker := range s.Checks {
		if marker.Name != "" || marker.State != "pending" {
			continue
		}
		materialized, required := false, false
		for _, c := range s.Checks {
			if c.Workflow != marker.Workflow || c.Name == "" {
				continue
			}
			materialized = true
			for _, r := range s.Required {
				if c.Name == r.Name && (r.AppID <= 0 || r.AppID == c.AppID) {
					required = true
				}
			}
		}
		if !materialized || required {
			green = false
		}
	}
	var failed []Check
	for _, r := range s.Required {
		found := false
		for _, c := range s.Checks {
			if c.Name != r.Name || (r.AppID > 0 && r.AppID != c.AppID) {
				continue
			}
			found = true
			if !passed(c) {
				green = false
				if c.State == "completed" {
					failed = append(failed, c)
				}
			}
		}
		if !found {
			green = false
		}
	}
	return green, failed
}
func passed(c Check) bool {
	return c.State == "completed" && (c.Conclusion == "success" || c.Conclusion == "neutral" || c.Conclusion == "skipped")
}

// Watch emits transitions once per observation session. Terminal CI decisions need two
// identical polls so a newly queued replacement can become visible before an old result
// is reported. API and delivery errors are returned, never translated into CI failures.
func Watch(ctx context.Context, o Options, fetch func(context.Context) (Snapshot, error), post func(context.Context, Event) error) error {
	if o.Until != "green" && o.Until != "merged" {
		return errors.New("until must be green or merged")
	}
	seen := map[string]bool{}
	previous := ""
	lastCI := ""
	send := func(s Snapshot, key, msg string, escalation bool) error {
		if seen[key] {
			return nil
		}
		if err := post(ctx, Event{Message: msg, Head: s.PR.Head, URL: s.PR.URL, Escalation: escalation}); err != nil {
			return err
		}
		seen[key] = true
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, err := fetch(ctx)
		if errors.Is(err, ErrHeadChanged) {
			previous = ""
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(o.Interval):
			}
			continue
		}
		if err != nil {
			return err
		}
		if s.PR.State == "MERGED" {
			return send(s, "merged", "merged", false)
		}
		if s.PR.State == "CLOSED" {
			return errors.New("pull request closed without merging")
		}
		if s.Fallback {
			if err := send(s, "fallback", "warning: required checks from configured fallback; GitHub policy discovery unavailable", true); err != nil {
				return err
			}
		}
		for _, ev := range s.PR.Events {
			if ev.Kind != "ReadyForReviewEvent" {
				continue
			}
			pushed, approximate := s.PR.PushedAt, false
			if pushed.IsZero() {
				// A synchronize-triggered PR run also represents a push. GitHub
				// does not expose its action, so require a newer same-workflow run
				// near ready and label this timing inference explicitly.
				for _, r := range s.Activity {
					candidate := r.Event == "push"
					if r.Event == "pull_request" {
						for _, newer := range s.Activity {
							if newer.WorkflowID == r.WorkflowID && newer.ID > r.ID && !newer.CreatedAt.Before(ev.At) && newer.CreatedAt.Sub(ev.At) <= o.RaceWindow {
								candidate = true
							}
						}
					}
					if candidate && r.Head == s.PR.Head && !r.CreatedAt.After(ev.At) && r.CreatedAt.After(pushed) {
						pushed, approximate = r.CreatedAt, true
					}
				}
			}
			d := ev.At.Sub(pushed)
			if !pushed.IsZero() && d >= 0 && d <= o.RaceWindow {
				suffix := ""
				if approximate {
					suffix = " (workflow creation time; possible push/ready race)"
				}
				if err := send(s, "race:"+ev.ID+":"+s.PR.Head, fmt.Sprintf("warning: ready-for-review within %s of push%s", o.RaceWindow, suffix), true); err != nil {
					return err
				}
			}
		}

		// Only the most recent queue transition describes the current queue state.
		var queue *TimelineEvent
		for i := range s.PR.Events {
			ev := &s.PR.Events[i]
			if ev.Kind == "RemovedFromMergeQueueEvent" || ev.Kind == "AddedToMergeQueueEvent" {
				if queue == nil || !ev.At.Before(queue.At) {
					queue = ev
				}
			}
		}
		if queue != nil && queue.Kind == "RemovedFromMergeQueueEvent" {
			reason := queue.Reason
			if reason == "" {
				reason = "GitHub did not provide a reason"
			}
			if err := send(s, "queue:"+queue.ID, "left merge queue: "+reason, true); err != nil {
				return err
			}
		}
		green, failed := Evaluate(s)
		fingerprint := s.PR.Head + "/" + s.PR.Base
		for _, r := range s.Runs {
			fingerprint += fmt.Sprintf("/%d:%d:%s", r.ID, r.Attempt, r.Status)
		}
		var outcomes []string
		for _, c := range s.Checks {
			outcomes = append(outcomes, fmt.Sprintf("%s:%s:%d:%d:%s:%s", c.Workflow, c.Name, c.AppID, c.ID, c.State, c.Conclusion))
		}
		for _, r := range s.Required {
			outcomes = append(outcomes, fmt.Sprintf("required:%s:%d", r.Name, r.AppID))
		}
		sort.Strings(outcomes)
		fingerprint += "/" + strings.Join(outcomes, "/")
		if fingerprint == previous {
			if green {
				if lastCI != s.PR.Head+":green" {
					if err := post(ctx, Event{Message: "ci green", Head: s.PR.Head, URL: s.PR.URL}); err != nil {
						return err
					}
					lastCI = s.PR.Head + ":green"
				}
				if o.Until == "green" {
					return nil
				}
			} else if len(failed) > 0 {
				for _, c := range failed {
					if err := send(s, fmt.Sprintf("failed:%s:%s:%s:%d:%s", s.PR.Head, c.Workflow, c.Name, c.ID, c.Conclusion), "required check failed: "+c.Name+" "+c.URL, true); err != nil {
						return err
					}
				}
				lastCI = "failed"
				if o.Until == "green" {
					return errors.New("required checks failed")
				}
			} else {
				lastCI = "pending"
			}
		} else if !green {
			lastCI = "pending"
		}
		previous = fingerprint
		timer := time.NewTimer(o.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
