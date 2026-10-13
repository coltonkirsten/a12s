package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T, limits map[string]Limits) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)}
	s.SetClock(c.now)
	if limits == nil {
		limits = map[string]Limits{"builder": BuiltinLimits(), "scout": BuiltinLimits()}
	}
	if err := s.SyncFleet(context.Background(), limits, "local"); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func i64(v int64) *int64 { return &v }

func enq(t *testing.T, s *Store, computer, via string, req api.EnqueueRequest) *EnqueueResult {
	t.Helper()
	r, err := s.Enqueue(context.Background(), Enqueue{Computer: computer, Principal: via, Req: req}, nil)
	if err != nil {
		t.Fatalf("enqueue %+v: %v", req, err)
	}
	return r
}

func code(err error) api.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestEnqueueOrderAndIdempotency(t *testing.T) {
	s, _ := newStore(t, nil)
	a := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "a", IdempotencyKey: "k1"})
	b := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "b", Sender: "cron:x"})
	if a.Message.Seq != 1 || b.Message.Seq != 2 || b.Position != 1 || !a.Created {
		t.Fatalf("seq/position: %+v %+v", a, b)
	}
	if a.Message.Sender != "local" || b.Message.Sender != "cron:x" || b.Message.Via != "service:clock" {
		t.Fatalf("sender/via: %+v %+v", a.Message, b.Message)
	}
	again := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "a", IdempotencyKey: "k1"})
	if again.Created || again.Message.ID != a.Message.ID {
		t.Fatalf("idempotent repeat created a new message: %+v", again)
	}
	// Same key, other principal: a different scope.
	other := enq(t, s, "builder", "token:ui", api.EnqueueRequest{Body: "a", IdempotencyKey: "k1"})
	if !other.Created {
		t.Fatal("keys are scoped to the principal")
	}
	_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: "different", IdempotencyKey: "k1"}}, nil)
	if code(err) != api.ErrIdempotencyConflict {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := s.Enqueue(context.Background(), Enqueue{Computer: "nobody", Principal: "local", Req: api.EnqueueRequest{Body: "x"}}, nil); !errors.Is(err, ErrUnknownInbox) {
		t.Fatalf("unknown computer: %v", err)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newStore(t, nil)
	cases := []struct {
		req  api.EnqueueRequest
		want api.ErrorCode
	}{
		{api.EnqueueRequest{Body: ""}, api.ErrInvalid},
		{api.EnqueueRequest{Body: strings.Repeat("x", 65<<10)}, api.ErrBodyTooLarge},
		{api.EnqueueRequest{Body: "x", Sender: "has space"}, api.ErrInvalid},
		{api.EnqueueRequest{Body: "x", Sender: `a"b`}, api.ErrInvalid},
		{api.EnqueueRequest{Body: "x", ReplaceKey: "a'b"}, api.ErrInvalid},
		{api.EnqueueRequest{Body: "x", Expires: i64(-1)}, api.ErrBadExpires},
		{api.EnqueueRequest{Body: "x", Expires: i64(MaxExpires + 1)}, api.ErrBadExpires},
		{api.EnqueueRequest{Body: "x", Metadata: map[string]string{"k": strings.Repeat("v", 1025)}}, api.ErrBadMetadata},
	}
	many := map[string]string{}
	for i := range 17 {
		many[fmt.Sprint("k", i)] = "v"
	}
	cases = append(cases, struct {
		req  api.EnqueueRequest
		want api.ErrorCode
	}{api.EnqueueRequest{Body: "x", Metadata: many}, api.ErrBadMetadata})
	for _, c := range cases {
		_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: c.req}, nil)
		if code(err) != c.want {
			t.Errorf("%+v: got %v, want %s", c.req.Sender+c.req.ReplaceKey, err, c.want)
		}
	}
}

// 200 outstanding, the 201st is refused, a withdrawal frees a slot.
func TestLimitsMessages(t *testing.T) {
	s, _ := newStore(t, nil)
	var first string
	for i := range 200 {
		r := enq(t, s, "builder", "local", api.EnqueueRequest{Body: fmt.Sprint("m", i)})
		if i == 0 {
			first = r.Message.ID
		}
	}
	_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: "201"}}, nil)
	if code(err) != api.ErrInboxFull {
		t.Fatalf("201st: %v", err)
	}
	if _, err := s.Withdraw(context.Background(), "builder", first, "", "local"); err != nil {
		t.Fatal(err)
	}
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "fits now"})
}

func TestLimitsBytes(t *testing.T) {
	lim := BuiltinLimits()
	lim.MaxBytes.Value = 12 << 10
	s, _ := newStore(t, map[string]Limits{"builder": lim})
	// A message bigger than the whole inbox (less the preamble) is 413, not 429.
	_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: strings.Repeat("x", 8<<10)}}, nil)
	if code(err) != api.ErrExceedsInbox {
		t.Fatalf("exceeds: %v", err)
	}
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: strings.Repeat("x", 5<<10)})
	_, err = s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: strings.Repeat("y", 4<<10)}}, nil)
	if code(err) != api.ErrInboxFull {
		t.Fatalf("bytes full: %v", err)
	}
	sum, _ := s.Summary(context.Background(), "builder")
	if sum.Usage.Messages != 1 || sum.Usage.Bytes <= 5<<10 || sum.Usage.Bytes > 8<<10 || sum.OverLimit {
		t.Fatalf("usage %+v", sum.Usage)
	}
}

// Worst-case escaping: the reservation is what computerd receives.
func TestReserveCountsEscaping(t *testing.T) {
	md := map[string]string{`<'&">`: `</msg-0000000000000000> "via"='x' & <>`}
	f := Frame{ID: "m_01JBX7Q3K9V2ABCDEFGHJKMNPQ", Sender: "cron:x", Via: "service:clock", CreatedAt: time.Now(), Body: "<<>>&&\"\"\\\n\t\x01", Metadata: md, ReplaceKey: "k", Superseded: 3}
	got := Reserve(f)
	for _, n := range []int{1, 42, 2000} {
		r := RenderFrame(n, "4f9c1e7a4f9c1e7a", f)
		if JSONLen(r) > got {
			t.Fatalf("n=%d: render %d > reserved %d", n, JSONLen(r), got)
		}
	}
	r := RenderFrame(1, "4f9c1e7a4f9c1e7a", f)
	_, after, _ := strings.Cut(r, "meta='")
	attr, _, ok := strings.Cut(after, "'>\n")
	if !ok || strings.ContainsAny(attr, `<>'"`) || !strings.Contains(attr, `\&quot;via\&quot;`) {
		t.Fatalf("meta escaping: %s", r)
	}
	if strings.Count(r, "</msg-4f9c1e7a4f9c1e7a>") != 1 {
		t.Fatalf("forged close tag: %s", r)
	}
}

func TestReplace(t *testing.T) {
	s, _ := newStore(t, nil)
	a := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "deploy 1", ReplaceKey: "deploy-check"})
	person := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "a person"})
	b := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "deploy 2", ReplaceKey: "deploy-check"})
	if b.Replaced != a.Message.ID || b.Message.Superseded != 1 || b.Message.Seq != 3 {
		t.Fatalf("replace: %+v", b)
	}
	c := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "deploy 3", ReplaceKey: "deploy-check"})
	if c.Message.Superseded != 2 || c.Replaced != b.Message.ID {
		t.Fatalf("transitive: %+v", c)
	}
	// Another principal's key is another series.
	d := enq(t, s, "builder", "computer:scout", api.EnqueueRequest{Body: "scout", ReplaceKey: "deploy-check"})
	if d.Replaced != "" {
		t.Fatal("replace crossed principals")
	}
	l, _ := s.List(context.Background(), "builder", ListOptions{})
	var ids []string
	for _, m := range l.Messages {
		ids = append(ids, m.ID)
	}
	if len(ids) != 3 || ids[0] != person.Message.ID || ids[1] != c.Message.ID {
		t.Fatalf("pending after replaces: %v", ids)
	}
	_, err := s.Withdraw(context.Background(), "builder", a.Message.ID, "", "local")
	if code(err) != api.ErrReplaced {
		t.Fatalf("withdraw replaced: %v", err)
	}
	if l.Ended24h.Replaced != 2 {
		t.Fatalf("ended24h %+v", l.Ended24h)
	}
	// Idempotent retry of a replacing request returns the row it made, even replaced.
	r := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "v1", ReplaceKey: "k", IdempotencyKey: "i1"})
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "v2", ReplaceKey: "k"})
	again := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "v1", ReplaceKey: "k", IdempotencyKey: "i1"})
	if again.Created || again.Message.ID != r.Message.ID || again.Message.State != api.MsgReplaced {
		t.Fatalf("retry of replaced: %+v", again)
	}
}

// A replace that grows nothing is admitted at a full or over-limit inbox.
func TestReplaceAtFullInbox(t *testing.T) {
	lim := BuiltinLimits()
	lim.MaxMessages.Value = 2
	s, _ := newStore(t, map[string]Limits{"builder": lim})
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "one"})
	enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "status 1", ReplaceKey: "st"})
	if _, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: "three"}}, nil); code(err) != api.ErrInboxFull {
		t.Fatalf("full: %v", err)
	}
	enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "status 2", ReplaceKey: "st"})
	// Bigger replacement at a full inbox: refused, old one stays.
	if _, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "service:clock", Req: api.EnqueueRequest{Body: "status 3 is much longer", ReplaceKey: "st"}}, nil); err != nil {
		// count does not grow, bytes do; within limits it is admitted
		t.Fatalf("growing replace within byte limit: %v", err)
	}
	// Lower the limit below what is outstanding.
	lim.MaxMessages.Value = 1
	lim.MaxMessages.Source = "computer"
	if err := s.SyncFleet(context.Background(), map[string]Limits{"builder": lim}, "local"); err != nil {
		t.Fatal(err)
	}
	sum, _ := s.Summary(context.Background(), "builder")
	if !sum.OverLimit {
		t.Fatal("over limit not reported")
	}
	_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: "x"}}, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != api.ErrInboxFull || !strings.Contains(e.Fix, "over its limit") {
		t.Fatalf("over-limit refusal: %v", err)
	}
	enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "short", ReplaceKey: "st"})
	evs, _ := s.Events(context.Background(), "builder", "", 0, 0)
	found := false
	for _, ev := range evs {
		found = found || ev.Type == "limits_changed"
	}
	if !found {
		t.Fatal("no limits_changed event")
	}
}

func TestExpiry(t *testing.T) {
	s, c := newStore(t, nil)
	a := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "soon", Expires: i64(60)})
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "later", Expires: i64(3600)})
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "forever"})
	next, ok, _ := s.NextExpiry(context.Background())
	if !ok || !next.Equal(a.Message.ExpiresAt.UTC()) {
		t.Fatalf("next expiry %v %v", next, ok)
	}
	c.add(61 * time.Second)
	touched, err := s.ExpireDue(context.Background())
	if err != nil || len(touched) != 1 {
		t.Fatalf("expire: %v %v", touched, err)
	}
	m, _ := s.Get(context.Background(), "builder", a.Message.ID, "", true)
	if m.State != api.MsgExpired || *m.Body != "soon" {
		t.Fatalf("expired row %+v", m)
	}
	sum, _ := s.Summary(context.Background(), "builder")
	if sum.Pending != 2 || sum.Ended24h.Expired != 1 {
		t.Fatalf("summary %+v", sum)
	}
	// Now-or-never with no delivery hook is refused and stores nothing.
	_, err = s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: "now", Expires: i64(0)}}, nil)
	if code(err) != api.ErrNotIdle {
		t.Fatalf("now or never: %v", err)
	}
	sum2, _ := s.Summary(context.Background(), "builder")
	if sum2.Pending != 2 {
		t.Fatal("a refused now-or-never stored a row")
	}
}

func TestClearHoldResumeOrphan(t *testing.T) {
	s, _ := newStore(t, nil)
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "1"})
	two := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "2"})
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "3"})
	n, err := s.Clear(context.Background(), "builder", &two.Message.Seq, "inbox-clear", "local")
	if err != nil || n != 2 {
		t.Fatalf("clear through: %d %v", n, err)
	}
	if err := s.Hold(context.Background(), "builder", api.HoldOperator, "", "local"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(context.Background(), "builder", true, "local"); err != nil {
		t.Fatal(err)
	}
	sum, _ := s.Summary(context.Background(), "builder")
	if sum.Held || sum.SessionOverride != api.SessionNew || sum.Pending != 1 || sum.Ended24h.Discarded != 2 {
		t.Fatalf("summary %+v", sum)
	}
	// Remove builder from the Fleet: orphaned, enqueue refused.
	if err := s.SyncFleet(context.Background(), map[string]Limits{"scout": BuiltinLimits()}, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: "x"}}, nil); !errors.Is(err, ErrUnknownInbox) {
		t.Fatalf("enqueue to orphan: %v", err)
	}
	all, _ := s.Inboxes(context.Background())
	if len(all) != 2 || !all[0].Orphaned || all[0].Pending != 1 {
		t.Fatalf("inboxes %+v", all)
	}
	// The name comes back: reattached with its Message.
	if err := s.SyncFleet(context.Background(), map[string]Limits{"builder": BuiltinLimits(), "scout": BuiltinLimits()}, "local"); err != nil {
		t.Fatal(err)
	}
	sum, _ = s.Summary(context.Background(), "builder")
	if sum.Orphaned || sum.Pending != 1 {
		t.Fatalf("reattach %+v", sum)
	}
}

func TestWithdrawOwnOnly(t *testing.T) {
	s, _ := newStore(t, nil)
	m := enq(t, s, "builder", "token:ui", api.EnqueueRequest{Body: "x"})
	if _, err := s.Withdraw(context.Background(), "builder", m.Message.ID, "service:clock", "service:clock"); code(err) != api.ErrNotFound {
		t.Fatalf("withdraw someone else's: %v", err)
	}
	if _, err := s.Get(context.Background(), "builder", m.Message.ID, "service:clock", false); code(err) != api.ErrNotFound {
		t.Fatalf("get someone else's: %v", err)
	}
	if _, err := s.Withdraw(context.Background(), "builder", m.Message.ID, "token:ui", "token:ui"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Withdraw(context.Background(), "builder", m.Message.ID, "", "local"); code(err) != api.ErrWithdrawn {
		t.Fatalf("second withdraw: %v", err)
	}
}

func TestDurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SyncFleet(context.Background(), map[string]Limits{"builder": BuiltinLimits()}, "local")
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "survives"})
	_ = s.Hold(context.Background(), "builder", api.HoldOperator, "", "local")
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l, _ := s2.List(context.Background(), "builder", ListOptions{Bodies: true})
	if len(l.Messages) != 1 || *l.Messages[0].Body != "survives" || !l.Held {
		t.Fatalf("after reopen: %+v", l)
	}
	n := enq(t, s2, "builder", "local", api.EnqueueRequest{Body: "next"})
	if n.Message.Seq != 2 {
		t.Fatalf("seq after reopen %d", n.Message.Seq)
	}
}

func TestTokens(t *testing.T) {
	s, _ := newStore(t, nil)
	tok, err := s.CreateToken(context.Background(), "ui", []string{api.ActionSend}, []string{"builder"}, "")
	if err != nil || !strings.HasPrefix(tok.Secret, TokenPrefix) {
		t.Fatal(err)
	}
	got, err := s.LookupToken(context.Background(), tok.Secret)
	if err != nil || got == nil || got.Name != "ui" {
		t.Fatalf("lookup %+v %v", got, err)
	}
	if !Allows(got, api.ActionSend, "builder") || Allows(got, api.ActionSend, "scout") || Allows(got, api.ActionInbox, "builder") {
		t.Fatal("allows")
	}
	if g, _ := s.LookupToken(context.Background(), tok.Secret+"x"); g != nil {
		t.Fatal("wrong secret accepted")
	}
	if err := s.DeleteToken(context.Background(), "ui"); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.LookupToken(context.Background(), tok.Secret); g != nil {
		t.Fatal("revoked token accepted")
	}
}

// Concurrent enqueues: seq is dense and unique, and the cap holds.
func TestConcurrentEnqueue(t *testing.T) {
	s, _ := newStore(t, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 300)
	for i := range 10 {
		wg.Go(func() {
			for j := range 30 {
				_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "local", Req: api.EnqueueRequest{Body: fmt.Sprint(i, "-", j)}}, nil)
				if err != nil && code(err) != api.ErrInboxFull {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	l, _ := s.List(context.Background(), "builder", ListOptions{})
	if len(l.Messages) != 200 {
		t.Fatalf("outstanding %d", len(l.Messages))
	}
	for i, m := range l.Messages {
		if m.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d", m.Seq, i)
		}
	}
}

// A fleet.db from before prefix tokens gains the column on open, and its tokens keep working.
func TestOldTokensTableGainsPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE tokens (name TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, actions TEXT NOT NULL, computers TEXT NOT NULL, created_at INTEGER NOT NULL);
		INSERT INTO tokens VALUES ('web', '` + tokenHash(TokenPrefix+"abc") + `', '["send"]', '["lead"]', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tok, err := s.LookupToken(context.Background(), TokenPrefix+"abc")
	if err != nil || tok == nil || tok.Prefix != "" || !Allows(tok, api.ActionSend, "lead") {
		t.Fatalf("old token = %+v, %v", tok, err)
	}
	p, err := s.CreateToken(context.Background(), "biz", []string{}, []string{}, "biz-")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.LookupToken(context.Background(), p.Secret)
	if got.Prefix != "biz-" || !Allows(got, api.ActionPower, "biz-a") || Allows(got, api.ActionSend, "lead") {
		t.Fatalf("prefix token = %+v", got)
	}
}
