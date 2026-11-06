package controller

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// PrincipalA12s is the via of a Message a12s itself writes to an inbox.
const PrincipalA12s = "a12s"

// reportTimeLimit tells the agent, through its inbox, that computerd stopped a
// background task at its time limit: the harness itself reports nothing for a
// task stopped from outside. The event's seq keys the Message, so an event read
// again after a reconnect or a Controller restart enqueues it once.
func (c *Controller) reportTimeLimit(name string, ev api.Event) {
	var t api.TaskData
	if c.inbox == nil || json.Unmarshal(ev.Data, &t) != nil || t.TimeLimit <= 0 {
		return
	}
	label := t.Description
	if label == "" {
		label = t.ID
	}
	dur := api.ShortDuration(time.Duration(t.TimeLimit) * time.Second)
	body := fmt.Sprintf("Your background task %q (%s, id %s) was stopped after %s, its time limit. It did not finish. "+
		"If you still need it, start it again; to give it longer, call the a12s background_tasks tool with its new id and hours (up to 24) right after starting it.",
		label, t.Kind, t.ID, dur)
	req := api.EnqueueRequest{
		Body:           body,
		Sender:         "a12s",
		IdempotencyKey: fmt.Sprintf("time-limit-%d-%s", ev.Seq, t.ID),
		Metadata:       map[string]string{"event": "task.time-limit", "task": t.ID},
	}
	if _, err := c.inbox.Enqueue(c.ctx, inbox.Enqueue{Computer: name, Principal: PrincipalA12s, Req: req}, nil); err != nil {
		c.logf("%s: time-limit message for task %s: %v", name, t.ID, err)
	}
}
