package controller

import (
	"encoding/json"
	"fmt"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

const diskWarnAt = 0.9

// reportDiskHigh tells the agent, through its inbox, that its home drive crossed 90%: the agent is the one who knows
// what on it is still in use. computerd journals one disk.usage per crossing, and the event's seq keys the Message, so
// a replayed event enqueues it once.
func (c *Controller) reportDiskHigh(name string, ev api.Event) {
	var d api.DiskUsageData
	if c.inbox == nil || json.Unmarshal(ev.Data, &d) != nil || d.Level != api.DiskHigh || d.TotalBytes <= 0 {
		return
	}
	body := fmt.Sprintf("Your home disk is %s full. Clean up now, before it fills and you stop working. "+
		"Keep what you are still using. Remove what you no longer need: duplicate clones of a repository and worktrees of finished work, "+
		"finished scratch files and downloads, and build output and package caches that can be regenerated (node_modules in checkouts you are not working in). "+
		"Do not delete work in progress. Find the largest directories with `du -xsh ~/* ~/.[!.]* | sort -h`.", diskText(d.DiskUsage))
	req := api.EnqueueRequest{
		Body:           body,
		Sender:         "a12s",
		IdempotencyKey: fmt.Sprintf("disk-high-%d", ev.Seq),
		Metadata:       map[string]string{"event": "disk.high"},
	}
	if _, err := c.inbox.Enqueue(c.ctx, inbox.Enqueue{Computer: name, Principal: PrincipalA12s, Req: req}, nil); err != nil {
		c.logf("%s: disk-high message: %v", name, err)
	}
}

func diskText(u api.DiskUsage) string {
	return fmt.Sprintf("%d%% (%s of %s)", u.UsedBytes*100/u.TotalBytes, gibText(u.UsedBytes), gibText(u.TotalBytes))
}

func gibText(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

func diskFullReason(u *api.DiskUsage) string {
	s := "home disk full: computerd can't write its journal and runs no turns until there is space"
	if u != nil && u.TotalBytes > 0 {
		s += " (" + diskText(*u) + ")"
	}
	return s + "; free space through the terminal or files API, or raise resources.disk"
}

// diskWarning is the status warning for a home drive over 90%, from computerd's last status.
func diskWarning(u *api.DiskUsage) string {
	if u == nil || u.TotalBytes <= 0 || float64(u.UsedBytes) < diskWarnAt*float64(u.TotalBytes) {
		return ""
	}
	if u.UsedBytes >= u.TotalBytes {
		return "home disk full (" + diskText(*u) + "); raise resources.disk or free space"
	}
	return "home disk " + diskText(*u) + " full; raise resources.disk or free space"
}
