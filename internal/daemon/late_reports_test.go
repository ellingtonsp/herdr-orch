package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestLateDonePreservesClosedDispatchAndNotifiesCoordinator(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2")
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "Synthetic task"}, &task)
	var d store.Dispatch
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"}, &d)
	x.mustOp(coord, "dispatch.fail", DispatchActArgs{Task: task.ID, Reason: "manual close"}, nil)
	beforeTask := x.task(task.ID)
	beforeDispatch, _ := x.st.GetDispatch(d.ID)
	x.inbox(coord)
	for _, body := range []string{"late summary", "additional evidence"} {
		var result struct {
			Message store.Message `json:"message"`
			Late    bool          `json:"late"`
		}
		x.mustOp("w1:p2", "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: body}, &result)
		if !result.Late || result.Message.Kind != store.KindNote || result.Message.Body != body {
			t.Fatalf("late report: %+v", result)
		}
		messages := x.inbox(coord)
		if len(messages) != 1 || messages[0].Kind != store.KindNote || !strings.Contains(messages[0].Body, body) || !strings.Contains(messages[0].Body, d.ID) {
			t.Fatalf("late coordinator note: %+v", messages)
		}
	}
	var views []DispatchView
	x.mustOp(coord, "dispatch.show", DispatchShowArgs{ID: d.ID}, &views)
	if len(views) != 1 || len(views[0].Reports) != 2 {
		t.Fatalf("report history: %+v", views)
	}
	afterDispatch, _ := x.st.GetDispatch(d.ID)
	if afterDispatch != beforeDispatch || !reflect.DeepEqual(x.task(task.ID), beforeTask) {
		t.Fatalf("late report altered settlement: before=%+v after=%+v", beforeDispatch, afterDispatch)
	}
}

func TestDoneRequiresExactAssigneeAndLatestDispatch(t *testing.T) {
	x := newHarness(t)
	setupRun(x, "w1:p2", "w1:p3")
	var task store.Task
	x.mustOp(coord, "task.create", TaskCreateArgs{Title: "Synthetic task"}, &task)
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p2"}, nil)
	for _, caller := range []string{coord, "human", "automation"} {
		_, err := x.op(caller, "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: "claim"})
		wantRefusal(t, err, "not_assignee")
	}
	x.mustOp(coord, "dispatch.fail", DispatchActArgs{Task: task.ID, Reason: "manual close"}, nil)
	x.mustOp(coord, "dispatch", DispatchArgs{Task: task.ID, To: "w1:p3"}, nil)
	_, err := x.op("w1:p2", "send", SendArgs{Kind: store.KindDone, Task: task.ID, Body: "old worker report"})
	wantRefusal(t, err, "not_assignee")
}
