package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type metadataHerdr struct {
	*fakeHerdr
	launchArgs []string
}

func (h *metadataHerdr) AgentStart(ctx context.Context, name, kind, pane string, args []string) (herdr.Pane, error) {
	h.launchArgs = append([]string(nil), args...)
	return h.fakeHerdr.AgentStart(ctx, name, kind, pane, args)
}

func TestWorkerLaunchMetadataPersistsAndAppearsInDoneMessages(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	h := &metadataHerdr{fakeHerdr: x.h}
	x.e.h = h
	args := []string{"--profile=profile-a", "--account", "account-a", "--model", "model-a"}
	var w store.Worker
	x.mustOp(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "synthetic-worker", Args: args}, &w)
	if w.Account != "account-a" || w.Profile != "profile-a" || w.Model != "model-a" || !reflect.DeepEqual(h.launchArgs, args) {
		t.Fatalf("metadata/launch args: %+v %v", w, h.launchArgs)
	}
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "Synthetic task"}, &task)
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: w.PaneID}, nil)
	x.mustOp(w.PaneID, "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: "summary"}, nil)
	check := func(messages []store.Message, kind string) {
		t.Helper()
		if len(messages) != 1 || messages[0].Kind != kind {
			t.Fatalf("coordinator messages: %+v", messages)
		}
		for _, token := range []string{`account="account-a"`, `profile="profile-a"`, `requested model="model-a"`, `agent="codex"`} {
			if !strings.Contains(messages[0].Body, token) {
				t.Fatalf("missing %s in %q", token, messages[0].Body)
			}
		}
	}
	check(x.inbox(coord), store.KindDone)
	// Reusing the pane must not relabel the historical dispatch's report.
	if _, err := x.st.RegisterWorker(store.Worker{PaneID: w.PaneID, RunID: w.RunID, Model: "model-b"}); err != nil {
		t.Fatal(err)
	}
	x.mustOp(w.PaneID, "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: "late summary"}, nil)
	check(x.inbox(coord), store.KindNote)
}

func TestExplicitModelIsPassedToAgentAndMetadataLabelsAreRecorded(t *testing.T) {
	x := newHarness(t)
	setupRun(x)
	h := &metadataHerdr{fakeHerdr: x.h}
	x.e.h = h
	var w store.Worker
	x.mustOp(coord, "worker.start", WorkerStartArgs{Agent: "codex", Name: "synthetic-worker", Account: "account-a", Profile: "profile-a", Model: "model-a"}, &w)
	if !reflect.DeepEqual(h.launchArgs, []string{"--model", "model-a"}) || w.Account != "account-a" || w.Profile != "profile-a" || w.Model != "model-a" {
		t.Fatalf("explicit launch: %+v %v", w, h.launchArgs)
	}
}

func TestLaunchMetadataParsingDoesNotInferAgentDefaultsOrPromptFlags(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		flags []string
		want  string
	}{
		{nil, []string{"--model", "-m"}, ""},
		{[]string{"-m", "model-a"}, []string{"--model", "-m"}, "model-a"},
		{[]string{"--model=model-a", "--model", "model-b"}, []string{"--model", "-m"}, "model-b"},
		{[]string{"--", "--model=model-a"}, []string{"--model", "-m"}, ""},
		{[]string{"-p", "synthetic prompt"}, []string{"--profile"}, ""},
	} {
		if got := launchArg(tc.args, tc.flags...); got != tc.want {
			t.Fatalf("launchArg(%v, %v)=%q want %q", tc.args, tc.flags, got, tc.want)
		}
	}
}
