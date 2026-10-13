package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The recording is synthetic. The command fake rejects every unrecorded request,
// including stale job attempts and reads lacking gh's pagination flags.
type snapshotRecording struct {
	t      *testing.T
	data   map[string]json.RawMessage
	errors map[string]error
	seen   map[string]int
	after  json.RawMessage
}

func newSnapshotRecording(t *testing.T) *snapshotRecording {
	t.Helper()
	b, err := os.ReadFile("testdata/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &snapshotRecording{t: t, errors: map[string]error{}, seen: map[string]int{}}
	if err := json.Unmarshal(b, &r.data); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *snapshotRecording) command(_ context.Context, args ...string) ([]byte, error) {
	r.t.Helper()
	key := ""
	if len(args) > 1 && args[0] == "api" && args[1] == "graphql" {
		if !reflect.DeepEqual(args, []string{"api", "graphql", "-f", "query=" + prQuery, "-f", "owner=you", "-f", "name=myapp", "-F", "number=12"}) {
			r.t.Fatalf("unexpected GraphQL arguments: %v", args)
		}
		key = "pull"
	} else {
		paths := map[string]string{
			"repos/you/myapp/branches/main/protection":                                "protection",
			"repos/you/myapp/branches/main":                                           "branch",
			"repos/you/myapp/rules/branches/main?per_page=100":                        "rules",
			"repos/you/myapp/actions/runs?head_sha=current-head&per_page=100":         "runs",
			"repos/you/myapp/commits/current-head/check-runs?filter=all&per_page=100": "checks",
			"repos/you/myapp/actions/runs/102/attempts/2/jobs?per_page=100":           "jobs-102-2",
			"repos/you/myapp/actions/runs/202/attempts/1/jobs?per_page=100":           "jobs-202-1",
			"repos/you/myapp/commits/current-head/statuses?per_page=100":              "statuses",
		}
		if len(args) > 0 {
			key = paths[args[len(args)-1]]
		}
		want := []string{"api", "--paginate", "--slurp", args[len(args)-1]}
		if key == "protection" || key == "branch" {
			want = []string{"api", args[len(args)-1]}
		}
		if key == "" || !reflect.DeepEqual(args, want) {
			r.t.Fatalf("unexpected gh request: %v", args)
		}
	}
	r.seen[key]++
	if err := r.errors[key]; err != nil {
		return nil, err
	}
	if key == "pull" && r.seen[key] > 1 && r.after != nil {
		return r.after, nil
	}
	return r.data[key], nil
}

func (r *snapshotRecording) snapshot(fallback []string) (Snapshot, error) {
	return (GitHub{Repo: "you/myapp", Number: 12, Fallback: fallback, Command: r.command}).Snapshot(context.Background())
}

func TestGitHubSnapshotPagedLatestEvidence(t *testing.T) {
	r := newSnapshotRecording(t)
	s, err := r.snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Fallback || s.PR.Head != "current-head" || s.PR.PushedAt.IsZero() || len(s.PR.Events) != 1 {
		t.Fatalf("PR metadata: %+v", s)
	}
	wantRequired := []Requirement{{Name: "build", AppID: 7}, {Name: "legacy"}, {Name: "security", AppID: 9}}
	if !reflect.DeepEqual(s.Required, wantRequired) {
		t.Fatalf("merged requirements: got %+v, want %+v", s.Required, wantRequired)
	}
	if len(s.Runs) != 2 || s.Runs[0].ID != 102 || s.Runs[0].Attempt != 2 || s.Runs[1].ID != 202 {
		t.Fatalf("selected runs: %+v", s.Runs)
	}
	var ids []int64
	for _, c := range s.Checks {
		ids = append(ids, c.ID)
		if c.Name == "build" && c.AppID != 7 {
			t.Fatalf("job lost app binding: %+v", c)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if want := []int64{2102, 2201, 3103, 3104, 3203, 4102}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("selected checks: got %v, want %v", ids, want)
	}
	if green, failed := Evaluate(s); !green || len(failed) != 0 {
		t.Fatalf("latest required evidence should pass despite superseded/optional failures: green=%v failures=%+v", green, failed)
	}
	for _, key := range []string{"protection", "rules", "runs", "checks", "jobs-102-2", "jobs-202-1", "statuses"} {
		if r.seen[key] != 1 {
			t.Errorf("request %s count=%d", key, r.seen[key])
		}
	}
	if r.seen["pull"] != 2 {
		t.Fatalf("head must be read before and after evidence: %v", r.seen)
	}
}

func TestGitHubSnapshotDiscoveryPermissions(t *testing.T) {
	denied := errors.New("HTTP 403: permission denied")
	for _, source := range []string{"protection", "rules"} {
		t.Run(source, func(t *testing.T) {
			r := newSnapshotRecording(t)
			r.errors[source] = denied
			if _, err := r.snapshot(nil); err == nil || !strings.Contains(err.Error(), "required_checks") {
				t.Fatalf("incomplete discovery must require configured fallback: %v", err)
			}
			if r.seen["runs"] != 0 {
				t.Fatal("queried CI despite unresolved required policy")
			}
			r = newSnapshotRecording(t)
			r.errors[source] = denied
			s, err := r.snapshot([]string{"configured-gate"})
			if err != nil {
				t.Fatal(err)
			}
			if !s.Fallback {
				t.Fatal("configured fallback must be reported")
			}
			found := false
			for _, req := range s.Required {
				found = found || req == (Requirement{Name: "configured-gate"})
			}
			if !found {
				t.Fatalf("fallback missing: %+v", s.Required)
			}
			if green, _ := Evaluate(s); green {
				t.Fatal("missing configured required gate must remain pending")
			}
		})
	}
}

func TestGitHubSnapshotUnprotectedBranch(t *testing.T) {
	r := newSnapshotRecording(t)
	r.errors["protection"] = errors.New("HTTP 404: not found")
	r.data["branch"] = json.RawMessage(`{"protected":false}`)
	r.data["rules"] = json.RawMessage(`[[]]`)
	s, err := r.snapshot(nil)
	if err != nil || s.Fallback || len(s.Required) != 0 {
		t.Fatalf("verified unprotected branch: snapshot=%+v error=%v", s, err)
	}
	if green, _ := Evaluate(s); !green {
		t.Fatal("observed optional failures do not create required gates")
	}
}

func TestGitHubSnapshotHeadOrBaseChanged(t *testing.T) {
	for _, change := range []struct{ before, after string }{
		{`"headRefOid":"current-head"`, `"headRefOid":"new-head"`},
		{`"baseRefName":"main"`, `"baseRefName":"release"`},
	} {
		t.Run(change.after, func(t *testing.T) {
			r := newSnapshotRecording(t)
			r.after = json.RawMessage(strings.Replace(string(r.data["pull"]), change.before, change.after, 1))
			if _, err := r.snapshot(nil); !errors.Is(err, ErrHeadChanged) {
				t.Fatalf("mixed snapshot must be rejected: %v", err)
			}
		})
	}
}

func TestGitHubSnapshotIncompleteRunListing(t *testing.T) {
	r := newSnapshotRecording(t)
	r.data["runs"] = json.RawMessage(strings.ReplaceAll(string(r.data["runs"]), `"total_count":6`, `"total_count":7`))
	if _, err := r.snapshot(nil); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("truncated run listing must fail closed: %v", err)
	}
}

func TestGitHubSnapshotPendingSelectedRun(t *testing.T) {
	for _, jobs := range []string{"materialized", "empty"} {
		t.Run(jobs, func(t *testing.T) {
			r := newSnapshotRecording(t)
			r.data["runs"] = json.RawMessage(strings.Replace(string(r.data["runs"]), `"run_attempt":2,"head_sha":"current-head","status":"completed"`, `"run_attempt":2,"head_sha":"current-head","status":"in_progress"`, 1))
			if jobs == "empty" {
				r.data["jobs-102-2"] = json.RawMessage(`[{"total_count":0,"jobs":[]}]`)
			}
			s, err := r.snapshot(nil)
			if err != nil {
				t.Fatal(err)
			}
			if green, failed := Evaluate(s); green || len(failed) != 0 {
				t.Fatalf("replacement is pending until run completes: green=%v failures=%+v checks=%+v", green, failed, s.Checks)
			}
		})
	}
}

func TestGitHubSnapshotRequiredOutcomeSelection(t *testing.T) {
	for _, tc := range []struct {
		name, key, before, after string
		failed                   bool
	}{
		{
			name:   "required job from second workflow fails",
			key:    "jobs-202-1",
			before: `"conclusion":"success"`,
			after:  `"conclusion":"failure"`,
			failed: true,
		},
		{
			name:   "latest third party is pending despite older failure",
			key:    "checks",
			before: `"id":2102,"name":"security","status":"completed","conclusion":"success"`,
			after:  `"id":2102,"name":"security","status":"in_progress","conclusion":null`,
		},
		{
			name:   "successful check from wrong app cannot satisfy binding",
			key:    "checks",
			before: `"id":2102,"name":"security","status":"completed","conclusion":"success","app":{"id":9}`,
			after:  `"id":2102,"name":"security","status":"completed","conclusion":"success","app":{"id":8}`,
			// The latest check from the required app remains a failure.
			failed: true,
		},
		{
			name:   "latest legacy status pending suppresses older failure",
			key:    "statuses",
			before: `"id":4102,"context":"legacy","state":"success"`,
			after:  `"id":4102,"context":"legacy","state":"pending"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSnapshotRecording(t)
			if !strings.Contains(string(r.data[tc.key]), tc.before) {
				t.Fatal("fixture change did not match")
			}
			r.data[tc.key] = json.RawMessage(strings.Replace(string(r.data[tc.key]), tc.before, tc.after, 1))
			s, err := r.snapshot(nil)
			if err != nil {
				t.Fatal(err)
			}
			if green, failed := Evaluate(s); green || (len(failed) > 0) != tc.failed {
				t.Fatalf("green=%v failures=%+v checks=%+v", green, failed, s.Checks)
			}
		})
	}
}

func TestGitHubSnapshotAmbiguousProtectionNotFound(t *testing.T) {
	for _, branch := range []string{`{"protected":true}`, `{}`} {
		t.Run(branch, func(t *testing.T) {
			r := newSnapshotRecording(t)
			r.errors["protection"] = errors.New("HTTP 404: not found")
			r.data["branch"] = json.RawMessage(branch)
			if _, err := r.snapshot(nil); err == nil {
				t.Fatal("ambiguous permission/not-found response must fail closed")
			}
		})
	}
}

func TestRequiredWorkflowRuleNeedsFallback(t *testing.T) {
	r := newSnapshotRecording(t)
	r.data["rules"] = json.RawMessage(`[[{"type":"workflows","parameters":{"workflows":[{"path":".github/workflows/ci.yml"}]}}]]`)
	if _, err := r.snapshot(nil); err == nil {
		t.Fatal("required workflow rule was silently ignored")
	}
	if s, err := r.snapshot([]string{"workflow-gate"}); err != nil || !s.Fallback {
		t.Fatalf("%+v %v", s, err)
	}
}
