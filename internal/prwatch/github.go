package prwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Run struct {
	ID         int64     `json:"id"`
	WorkflowID int64     `json:"workflow_id"`
	SuiteID    int64     `json:"check_suite_id"`
	Attempt    int       `json:"run_attempt"`
	Head       string    `json:"head_sha"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Event      string    `json:"event"`
	CreatedAt  time.Time `json:"created_at"`
}

func LatestRuns(runs []Run, head string) []Run {
	latest := map[int64]Run{}
	for _, r := range runs {
		if r.Head != head {
			continue
		}
		old, ok := latest[r.WorkflowID]
		if !ok || r.ID > old.ID || (r.ID == old.ID && r.Attempt > old.Attempt) {
			latest[r.WorkflowID] = r
		}
	}
	out := make([]Run, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkflowID < out[j].WorkflowID })
	return out
}

type Command func(context.Context, ...string) ([]byte, error)

func ExecGH(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

type GitHub struct {
	Repo     string
	Number   int
	Fallback []string
	Command  Command
}

func (g GitHub) command(ctx context.Context, args ...string) ([]byte, error) {
	if g.Command != nil {
		return g.Command(ctx, args...)
	}
	return ExecGH(ctx, args...)
}
func (g GitHub) api(ctx context.Context, path string, out any) error {
	b, err := g.command(ctx, "api", path)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode gh %s: %w", path, err)
	}
	return nil
}
func (g GitHub) pages(ctx context.Context, path string, out any) error {
	b, err := g.command(ctx, "api", "--paginate", "--slurp", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

const prQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){headRefOid baseRefName state url commits(last:1){nodes{commit{pushedDate}}} timelineItems(last:100,itemTypes:[READY_FOR_REVIEW_EVENT,ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT]){nodes{__typename ... on ReadyForReviewEvent{id createdAt} ... on AddedToMergeQueueEvent{id createdAt} ... on RemovedFromMergeQueueEvent{id createdAt reason}}}}}}`

func (g GitHub) pull(ctx context.Context) (PR, error) {
	parts := strings.Split(g.Repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return PR{}, errors.New("repo must be owner/name")
	}
	b, err := g.command(ctx, "api", "graphql", "-f", "query="+prQuery, "-f", "owner="+parts[0], "-f", "name="+parts[1], "-F", "number="+strconv.Itoa(g.Number))
	if err != nil {
		return PR{}, err
	}
	var response struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					PR
					Commits struct {
						Nodes []struct {
							Commit struct {
								PushedAt time.Time `json:"pushedDate"`
							}
						}
					}
					Timeline struct{ Nodes []TimelineEvent } `json:"timelineItems"`
				}
			}
		}
		Errors []json.RawMessage
	}
	if err := json.Unmarshal(b, &response); err != nil {
		return PR{}, err
	}
	p := response.Data.Repository.PullRequest
	if len(response.Errors) > 0 || p == nil || p.Head == "" {
		return PR{}, errors.New("GitHub did not return a complete pull request")
	}
	p.PR.Events = p.Timeline.Nodes
	if len(p.Commits.Nodes) > 0 {
		p.PR.PushedAt = p.Commits.Nodes[0].Commit.PushedAt
	}
	return p.PR, nil
}

type apiRequirement struct {
	Context       string `json:"context"`
	AppID         int64  `json:"app_id"`
	IntegrationID int64  `json:"integration_id"`
}

func (g GitHub) requirements(ctx context.Context, base string) ([]Requirement, bool, error) {
	root := "repos/" + g.Repo
	var protection struct {
		Required *struct {
			Contexts []string
			Checks   []apiRequirement
		} `json:"required_status_checks"`
	}
	classicErr := g.api(ctx, root+"/branches/"+url.PathEscape(base)+"/protection", &protection)
	// An unprotected branch is the only safe way to interpret protection's ambiguous 404.
	if classicErr != nil {
		var branch struct {
			Protected *bool `json:"protected"`
		}
		if err := g.api(ctx, root+"/branches/"+url.PathEscape(base), &branch); err == nil && branch.Protected != nil && !*branch.Protected {
			classicErr = nil
		}
	}
	var pages [][]struct {
		Type       string
		Parameters struct {
			Checks []apiRequirement `json:"required_status_checks"`
		}
	}
	rulesErr := g.pages(ctx, root+"/rules/branches/"+url.PathEscape(base)+"?per_page=100", &pages)
	var out []Requirement
	add := func(name string, id int64) {
		if name == "" {
			return
		}
		if id < 0 {
			id = 0
		}
		for _, r := range out {
			if r.Name == name && r.AppID == id {
				return
			}
		}
		out = append(out, Requirement{Name: name, AppID: id})
	}
	if protection.Required != nil {
		for _, c := range protection.Required.Checks {
			add(c.Context, c.AppID)
		}
		for _, name := range protection.Required.Contexts {
			bound := false
			for _, c := range protection.Required.Checks {
				if c.Context == name {
					bound = true
				}
			}
			if !bound {
				add(name, 0)
			}
		}
	}
	for _, page := range pages {
		for _, rule := range page {
			if rule.Type == "workflows" {
				rulesErr = errors.New("required workflows need a configured check-name fallback")
			}
			if rule.Type == "required_status_checks" {
				for _, c := range rule.Parameters.Checks {
					add(c.Context, c.IntegrationID)
				}
			}
		}
	}
	fallback := classicErr != nil || rulesErr != nil
	if fallback {
		if g.Fallback == nil {
			return nil, false, fmt.Errorf("cannot discover all required checks; configure projects.<name>.required_checks for %s (protection: %v; rules: %v)", g.Repo, classicErr, rulesErr)
		}
		for _, name := range g.Fallback {
			add(name, 0)
		}
	}
	return out, fallback, nil
}

type apiCheck struct {
	ID                       int64
	Name, Status, Conclusion string
	URL                      string `json:"html_url"`
	App                      struct{ ID int64 }
	Suite                    struct{ ID int64 } `json:"check_suite"`
}

func (g GitHub) Snapshot(ctx context.Context) (Snapshot, error) {
	p, err := g.pull(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{PR: p}
	if p.State == "MERGED" || p.State == "CLOSED" {
		return s, nil
	}
	s.Required, s.Fallback, err = g.requirements(ctx, p.Base)
	if err != nil {
		return s, err
	}
	root := "repos/" + g.Repo
	var rp []struct {
		Total int   `json:"total_count"`
		Runs  []Run `json:"workflow_runs"`
	}
	if err = g.pages(ctx, root+"/actions/runs?head_sha="+url.QueryEscape(p.Head)+"&per_page=100", &rp); err != nil {
		return s, err
	}
	var runs []Run
	for _, page := range rp {
		runs = append(runs, page.Runs...)
	}
	if len(rp) > 0 && rp[0].Total > len(runs) {
		return s, errors.New("workflow run listing incomplete; refusing stale CI evidence")
	}
	s.Runs = LatestRuns(runs, p.Head)
	for _, r := range runs {
		if r.Head == p.Head {
			s.Activity = append(s.Activity, r)
		}
	}
	var cp []struct {
		Checks []apiCheck `json:"check_runs"`
	}
	if err = g.pages(ctx, root+"/commits/"+url.PathEscape(p.Head)+"/check-runs?filter=all&per_page=100", &cp); err != nil {
		return s, err
	}
	checks := map[int64]apiCheck{}
	for _, page := range cp {
		for _, c := range page.Checks {
			checks[c.ID] = c
		}
	}
	// All action suites are excluded below: only jobs from each selected run attempt count.
	actionSuites := map[int64]bool{}
	for _, r := range runs {
		actionSuites[r.SuiteID] = true
	}
	for _, r := range s.Runs {
		var jp []struct {
			Jobs []struct {
				ID                       int64
				Name, Status, Conclusion string
				URL                      string `json:"html_url"`
				CheckURL                 string `json:"check_run_url"`
			}
		}
		path := fmt.Sprintf("%s/actions/runs/%d/attempts/%d/jobs?per_page=100", root, r.ID, r.Attempt)
		if err = g.pages(ctx, path, &jp); err != nil {
			return s, err
		}
		count := 0
		latestJobs := map[string]Check{}
		for _, page := range jp {
			for _, j := range page.Jobs {
				checkID, _ := strconv.ParseInt(j.CheckURL[strings.LastIndex(j.CheckURL, "/")+1:], 10, 64)
				if j.ID > latestJobs[j.Name].ID {
					latestJobs[j.Name] = Check{Name: j.Name, Workflow: strconv.FormatInt(r.WorkflowID, 10), State: j.Status, Conclusion: j.Conclusion, URL: j.URL, ID: j.ID, AppID: checks[checkID].App.ID}
				}
				count++
			}
		}
		for _, c := range latestJobs {
			s.Checks = append(s.Checks, c)
		}
		// A new queued run with no jobs must suppress old green/failure until jobs arrive.
		if count == 0 || r.Status != "completed" {
			s.Checks = append(s.Checks, Check{Name: "", Workflow: strconv.FormatInt(r.WorkflowID, 10), State: "pending"})
		}
	}
	latest := map[string]apiCheck{}
	for _, c := range checks {
		if actionSuites[c.Suite.ID] {
			continue
		}
		key := fmt.Sprintf("%d:%s", c.App.ID, c.Name)
		if c.ID > latest[key].ID {
			latest[key] = c
		}
	}
	for key, c := range latest {
		s.Checks = append(s.Checks, Check{Name: c.Name, Workflow: key, AppID: c.App.ID, ID: c.ID, State: c.Status, Conclusion: c.Conclusion, URL: c.URL})
	}
	var statuses [][]struct {
		ID             int64
		Context, State string
		URL            string `json:"target_url"`
	}
	if err = g.pages(ctx, root+"/commits/"+url.PathEscape(p.Head)+"/statuses?per_page=100", &statuses); err != nil {
		return s, err
	}
	legacy := map[string]Check{}
	for _, page := range statuses {
		for _, st := range page {
			if st.ID > legacy[st.Context].ID {
				state := "completed"
				if st.State == "pending" {
					state = "pending"
				}
				legacy[st.Context] = Check{Name: st.Context, Workflow: "status", ID: st.ID, State: state, Conclusion: st.State, URL: st.URL}
			}
		}
	}
	for _, c := range legacy {
		s.Checks = append(s.Checks, c)
	}
	// Re-read the head after the multi-request snapshot. Never announce another head's checks.
	after, err := g.pull(ctx)
	if err != nil {
		return s, err
	}
	if after.Head != p.Head || after.Base != p.Base {
		return s, ErrHeadChanged
	}
	s.PR = after
	return s, nil
}

var ErrHeadChanged = errors.New("PR head or base changed during observation")
