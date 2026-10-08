package inbox

import (
	"context"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

func TestJournalAndPrune(t *testing.T) {
	s, c := newStore(t, nil)
	w := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "withdraw me"})
	d := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "deliver me"})
	if _, err := s.Withdraw(context.Background(), "builder", w.Message.ID, "", "local"); err != nil {
		t.Fatal(err)
	}
	b := claim(t, s, "builder")
	_ = s.Deliver(context.Background(), b.ID, "t_1", "", "")
	recs, err := s.Unjournaled(context.Background(), "builder", 1<<20)
	if err != nil || len(recs) != 1 || recs[0].ID != w.Message.ID || recs[0].Body != "withdraw me" || recs[0].Reason != "withdrawn" || recs[0].By != "local" {
		t.Fatalf("unjournaled %+v %v", recs, err)
	}
	// Not pruned before it is journaled, even past retention.
	c.add(Retention + time.Hour)
	if n, _ := s.Prune(context.Background()); n != 1 { // only the delivered row
		t.Fatalf("pruned %d", n)
	}
	if _, err := s.Get(context.Background(), "builder", w.Message.ID, "", false); err != nil {
		t.Fatal("unjournaled row pruned")
	}
	_ = s.MarkJournaled(context.Background(), []string{w.Message.ID})
	if n, _ := s.Prune(context.Background()); n != 1 {
		t.Fatalf("pruned %d after journaling", n)
	}
	if _, err := s.Get(context.Background(), "builder", d.Message.ID, "", false); code(err) != api.ErrNotFound {
		t.Fatal("delivered row kept past retention")
	}
}

func TestWakeClearIdempotent(t *testing.T) {
	s, _ := newStore(t, nil)
	enq(t, s, "scout", "local", api.EnqueueRequest{Body: "1"})
	enq(t, s, "scout", "local", api.EnqueueRequest{Body: "2"})
	through, n, err := s.WakeClear(context.Background(), "scout", "r1", "local")
	if err != nil || n != 2 || through != 2 {
		t.Fatalf("clear %d %d %v", through, n, err)
	}
	late := enq(t, s, "scout", "local", api.EnqueueRequest{Body: "3"})
	if _, n, _ := s.WakeClear(context.Background(), "scout", "r1", "local"); n != 0 {
		t.Fatal("a retried clear discarded a later message")
	}
	if m, _ := s.Get(context.Background(), "scout", late.Message.ID, "", false); m.State != api.MsgPending {
		t.Fatalf("late %+v", m)
	}
}
