package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLatestWorkflowAttempts(t *testing.T) {
	var runs []Run
	b, err := os.ReadFile("testdata/runs.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &runs); err != nil {
		t.Fatal(err)
	}
	got := LatestRuns(runs, "abc")
	if len(got) != 1 || got[0].ID != 102 || got[0].Attempt != 2 {
		t.Fatalf("latest: %+v", got)
	}
}
func TestRequiredChecks(t *testing.T) {
	base := Snapshot{PR: PR{Head: "abc", State: "OPEN"}, Required: []Requirement{{Name: "test", AppID: 7}}, Checks: []Check{{Name: "test", AppID: 7, State: "completed", Conclusion: "success"}, {Name: "optional", State: "completed", Conclusion: "failure"}}}
	for _, tc := range []struct {
		name, state, conclusion string
		app                     int64
		green, failed           bool
	}{
		{"success", "completed", "success", 7, true, false}, {"neutral", "completed", "neutral", 7, true, false}, {"skipped", "completed", "skipped", 7, true, false},
		{"failure", "completed", "failure", 7, false, true}, {"cancelled", "completed", "cancelled", 7, false, true}, {"queued", "queued", "", 7, false, false}, {"wrong app", "completed", "success", 8, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Checks = append([]Check(nil), base.Checks...)
			s.Checks[0].State = tc.state
			s.Checks[0].Conclusion = tc.conclusion
			s.Checks[0].AppID = tc.app
			green, failed := Evaluate(s)
			if green != tc.green || (len(failed) > 0) != tc.failed {
				t.Fatalf("%v %v", green, failed)
			}
		})
	}
	base.Checks = nil
	if g, _ := Evaluate(base); g {
		t.Fatal("missing check green")
	}
	base.Required = nil
	if g, _ := Evaluate(base); g {
		t.Fatal("empty evidence green")
	}
}
func TestWatchTransitions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr := PR{Head: "abc", State: "OPEN", PushedAt: now, Events: []TimelineEvent{{ID: "ready1", Kind: "ReadyForReviewEvent", At: now.Add(time.Second)}}}
	green := Snapshot{PR: pr, Required: []Requirement{{Name: "test"}}, Checks: []Check{{Name: "test", State: "completed", Conclusion: "success"}}}
	fail := green
	fail.Checks = []Check{{Name: "test", State: "completed", Conclusion: "failure", URL: "https://example.com/job"}}
	left := green
	left.PR.Events = append(append([]TimelineEvent(nil), pr.Events...), TimelineEvent{ID: "queue1", Kind: "RemovedFromMergeQueueEvent", Reason: "check failed", At: now.Add(time.Minute)})
	merged := green
	merged.PR.State = "MERGED"
	snaps := []Snapshot{fail, fail, green, green, left, merged}
	i := 0
	var events []Event
	err := Watch(context.Background(), Options{Until: "merged", Interval: time.Nanosecond, RaceWindow: 5 * time.Second}, func(context.Context) (Snapshot, error) { s := snaps[i]; i++; return s, nil }, func(_ context.Context, e Event) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range events {
		texts = append(texts, e.Message)
	}
	got := strings.Join(texts, "\n")
	for _, want := range []string{"warning: ready-for-review", "required check failed: test https://example.com/job", "ci green", "left merge queue: check failed", "merged"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("%q in %s", want, got)
		}
	}
}
func TestWatchDeliveryFailureAndCancellation(t *testing.T) {
	s := Snapshot{PR: PR{State: "MERGED"}}
	boom := errors.New("inbox unavailable")
	if err := Watch(context.Background(), Options{Until: "green"}, func(context.Context) (Snapshot, error) { return s, nil }, func(context.Context, Event) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Watch(ctx, Options{Until: "green"}, func(ctx context.Context) (Snapshot, error) { return Snapshot{}, ctx.Err() }, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPushReadyInferenceAndStableReplacement(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr := PR{Head: "abc", State: "OPEN", Events: []TimelineEvent{{ID: "ready", Kind: "ReadyForReviewEvent", At: now}}}
	activity := []Run{{ID: 1, WorkflowID: 7, Head: "abc", Event: "pull_request", CreatedAt: now}, {ID: 2, WorkflowID: 7, Head: "abc", Event: "pull_request", CreatedAt: now}}
	s := Snapshot{PR: pr, Activity: activity, Required: []Requirement{{Name: "test"}}, Checks: []Check{{ID: 1, Name: "test", State: "completed", Conclusion: "cancelled"}}}
	i := 0
	var events []Event
	err := Watch(context.Background(), Options{Until: "green", Interval: time.Nanosecond, RaceWindow: 5 * time.Second}, func(context.Context) (Snapshot, error) {
		i++
		if i == 2 {
			return Snapshot{}, ErrHeadChanged
		}
		if i > 2 {
			s.Checks[0].ID = 2
			s.Checks[0].Conclusion = "success"
		}
		return s, nil
	}, func(_ context.Context, e Event) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !strings.Contains(events[0].Message, "possible push/ready race") || events[1].Message != "ci green" {
		t.Fatalf("events: %+v", events)
	}
	if i != 4 {
		t.Fatalf("did not wait for stable replacement: %d", i)
	}
}

func TestUntilGreenFailsOnlyStableRequiredFailure(t *testing.T) {
	s := Snapshot{PR: PR{Head: "abc", State: "OPEN"}, Required: []Requirement{{Name: "test"}}, Checks: []Check{{ID: 1, Name: "test", State: "completed", Conclusion: "failure"}}}
	polls, posts := 0, 0
	err := Watch(context.Background(), Options{Until: "green", Interval: time.Nanosecond}, func(context.Context) (Snapshot, error) { polls++; return s, nil }, func(context.Context, Event) error { posts++; return nil })
	if err == nil || polls != 2 || posts != 1 {
		t.Fatalf("%d %d %v", polls, posts, err)
	}
	s.PR.State = "CLOSED"
	if err := Watch(context.Background(), Options{Until: "merged"}, func(context.Context) (Snapshot, error) { return s, nil }, nil); err == nil {
		t.Fatal("closed PR succeeded")
	}
}

func TestTimelineWarningsAndQueueReentry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		delta              time.Duration
		push               bool
		queue              []TimelineEvent
		wantRace, wantLeft bool
	}{
		{name: "inside", delta: time.Second, push: true, wantRace: true},
		{name: "outside", delta: 6 * time.Second, push: true},
		{name: "before push", delta: -time.Second, push: true},
		{name: "missing push", delta: time.Second},
		{name: "removed without reason", queue: []TimelineEvent{{ID: "q1", Kind: "RemovedFromMergeQueueEvent", At: now}}, wantLeft: true},
		{name: "reentered", queue: []TimelineEvent{{ID: "q1", Kind: "RemovedFromMergeQueueEvent", At: now}, {ID: "q2", Kind: "AddedToMergeQueueEvent", At: now.Add(time.Second)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{PR: PR{Head: "abc", State: "OPEN", Events: append(tc.queue, TimelineEvent{ID: "ready", Kind: "ReadyForReviewEvent", At: now.Add(tc.delta)})}}
			if tc.push {
				s.PR.PushedAt = now
			}
			i := 0
			var messages []string
			err := Watch(context.Background(), Options{Until: "merged", Interval: time.Nanosecond, RaceWindow: 5 * time.Second}, func(context.Context) (Snapshot, error) {
				i++
				if i == 2 {
					s.PR.State = "MERGED"
				}
				return s, nil
			}, func(_ context.Context, e Event) error { messages = append(messages, e.Message); return nil })
			if err != nil {
				t.Fatal(err)
			}
			all := strings.Join(messages, "\n")
			if strings.Contains(all, "ready-for-review") != tc.wantRace || strings.Contains(all, "left merge queue") != tc.wantLeft {
				t.Fatal(all)
			}
			if tc.wantLeft && !strings.Contains(all, "GitHub did not provide a reason") {
				t.Fatal(all)
			}
		})
	}
}

func TestEmptyWorkflowIsNotCheckEvidence(t *testing.T) {
	if green, _ := Evaluate(Snapshot{Checks: []Check{{Workflow: "7", State: "pending"}}}); green {
		t.Fatal("unnamed workflow placeholder is not check evidence")
	}
}
