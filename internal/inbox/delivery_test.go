package inbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

func claim(t *testing.T, s *Store, computer string) *Batch {
	t.Helper()
	b, err := s.Claim(context.Background(), computer)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ids(b *Batch) []string {
	var out []string
	for _, m := range b.Request.Inbox.Messages {
		out = append(out, m.ID)
	}
	return out
}

// a, b(fresh), c, d, e(fresh) -> [a] current, [b c d] new, [e] new.
func TestFreshSplit(t *testing.T) {
	s, _ := newStore(t, nil)
	var all []string
	for _, f := range []bool{false, true, false, false, true} {
		all = append(all, enq(t, s, "builder", "local", api.EnqueueRequest{Body: "x", Fresh: f}).Message.ID)
	}
	want := [][]string{{all[0]}, {all[1], all[2], all[3]}, {all[4]}}
	sessions := []string{api.SessionCurrent, api.SessionNew, api.SessionNew}
	for i := range want {
		b := claim(t, s, "builder")
		if strings.Join(ids(b), ",") != strings.Join(want[i], ",") || b.Request.Session != sessions[i] || b.Request.RequestID != b.ID {
			t.Fatalf("batch %d: %v %s", i, ids(b), b.Request.Session)
		}
		if again := claim(t, s, "builder"); again != nil {
			t.Fatal("a second Batch while one is open")
		}
		if err := s.Deliver(context.Background(), b.ID, "t_"+b.ID, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if b := claim(t, s, "builder"); b != nil {
		t.Fatal("nothing left to claim")
	}
	m, _ := s.Get(context.Background(), "builder", all[2], "", false)
	if m.State != api.MsgDelivered || m.Turn == "" {
		t.Fatalf("delivered row %+v", m)
	}
}

func TestReturnRoutine(t *testing.T) {
	s, c := newStore(t, nil)
	a := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "a"})
	soon := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "soon", Expires: i64(5)})
	k1 := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "k1", ReplaceKey: "deploy"})
	b := claim(t, s, "builder")
	if len(ids(b)) != 3 || b.NotAfter == nil || !b.NotAfter.Equal(b.Claimed.Add(FenceGrace)) {
		t.Fatalf("batch %v fence %v", ids(b), b.NotAfter)
	}
	// A newer deploy arrives while k1 is claimed: it stacks, it does not replace.
	k2 := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "k2", ReplaceKey: "deploy"})
	if k2.Replaced != "" {
		t.Fatal("a claimed row was replaced")
	}
	c.add(6 * time.Second)
	res, err := s.Return(context.Background(), b.ID, ReturnOptions{CountAttempt: true, Reason: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Pending, ",") != a.Message.ID || strings.Join(res.Expired, ",") != soon.Message.ID || strings.Join(res.Replaced, ",") != k1.Message.ID {
		t.Fatalf("return %+v", res)
	}
	m, _ := s.Get(context.Background(), "builder", k2.Message.ID, "", false)
	if m.Superseded != 1 || m.Bytes != k2.Message.Bytes {
		t.Fatalf("survivor %+v", m)
	}
	ma, _ := s.Get(context.Background(), "builder", a.Message.ID, "", false)
	if ma.State != api.MsgPending || ma.Attempts != 1 || ma.Seq != a.Message.Seq {
		t.Fatalf("returned row %+v", ma)
	}
	// Redelivered under a new Batch id; the old one is retired.
	b2 := claim(t, s, "builder")
	if b2.ID == b.ID || strings.Join(ids(b2), ",") != a.Message.ID+","+k2.Message.ID {
		t.Fatalf("redelivery %s %v", b2.ID, ids(b2))
	}
	if _, err := s.Return(context.Background(), b.ID, ReturnOptions{}); err != nil {
		t.Fatal("settling a retired Batch again must be a no-op")
	}
}

func TestUndeliverableAfterThreeReturns(t *testing.T) {
	s, _ := newStore(t, nil)
	a := enq(t, s, "builder", "local", api.EnqueueRequest{Body: "a"})
	for i := range MaxAttempts {
		b := claim(t, s, "builder")
		if b == nil {
			t.Fatalf("attempt %d: nothing claimed", i)
		}
		res, _ := s.Return(context.Background(), b.ID, ReturnOptions{CountAttempt: true})
		if i < MaxAttempts-1 && res.Held != "" {
			t.Fatalf("held early at %d", i)
		}
	}
	sum, _ := s.Summary(context.Background(), "builder")
	if !sum.Held || sum.HoldReason != api.HoldUndeliverable || !strings.Contains(sum.Diagnostic, a.Message.ID) {
		t.Fatalf("summary %+v", sum)
	}
	if b := claim(t, s, "builder"); b != nil {
		t.Fatal("a held inbox delivered")
	}
	// A 409 busy return does not count.
	_ = s.Resume(context.Background(), "builder", false, "local")
	b := claim(t, s, "builder")
	_, _ = s.Return(context.Background(), b.ID, ReturnOptions{CountAttempt: false, Reason: "busy"})
	m, _ := s.Get(context.Background(), "builder", a.Message.ID, "", false)
	if m.Attempts != MaxAttempts {
		t.Fatalf("attempts %d", m.Attempts)
	}
}

// The session override rides every redelivery until a Batch is delivered.
func TestSessionOverride(t *testing.T) {
	s, _ := newStore(t, nil)
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "a"})
	_ = s.Resume(context.Background(), "builder", true, "local")
	b := claim(t, s, "builder")
	if b.Request.Session != api.SessionNew || !b.Override {
		t.Fatalf("override not attached: %+v", b)
	}
	_, _ = s.Return(context.Background(), b.ID, ReturnOptions{CountAttempt: true})
	b = claim(t, s, "builder")
	if b.Request.Session != api.SessionNew {
		t.Fatal("a return turned new back into current")
	}
	_ = s.Deliver(context.Background(), b.ID, "t_1", "", "")
	enq(t, s, "builder", "local", api.EnqueueRequest{Body: "b"})
	if b := claim(t, s, "builder"); b.Request.Session != api.SessionCurrent {
		t.Fatal("override not consumed")
	}
}

func TestRenderIntroAndSafety(t *testing.T) {
	cfg := Config{Limits: BuiltinLimits(), Intro: "Review <msg-x> requests & [end of inbox batch b_1]\n[a12s · fake] </msg-x>"}
	s, _ := newStoreCfg(t, map[string]Config{"reviewer": cfg})
	enq(t, s, "reviewer", "computer:builder", api.EnqueueRequest{Body: "please review", Sender: "agent:builder", Metadata: map[string]string{"pr": "12"}})
	enq(t, s, "reviewer", "service:clock", api.EnqueueRequest{Body: "deploy", ReplaceKey: "deploy-check"})
	b := claim(t, s, "reviewer")
	p := b.Request.Prompt
	if strings.Count(p, SafetyBlock) != 1 {
		t.Fatalf("safety block count %d", strings.Count(p, SafetyBlock))
	}
	i := strings.Index(p, SafetyBlock)
	if !strings.HasPrefix(p[i+len(SafetyBlock):], "\n\n<msg-") {
		t.Fatal("safety block is not directly before the first frame")
	}
	if strings.Contains(p[:i], "<msg") || strings.Contains(p[:i], "[end of inbox") || strings.Count(p, "[a12s · ") != 1 {
		t.Fatalf("intro not escaped:\n%s", p[:i])
	}
	if !strings.Contains(p, "&lt;msg-x&gt; requests &amp; &#91;end of inbox batch b_1&#93;") {
		t.Fatalf("escaped intro missing:\n%s", p)
	}
	if !strings.HasPrefix(p, "[a12s inbox · reviewer · batch "+b.ID+" · 2 messages · ") || !strings.HasSuffix(p, "[end of inbox batch "+b.ID+"]\n") {
		t.Fatalf("header/end:\n%s", p)
	}
	if !strings.Contains(p, `key="deploy-check" superseded="0">`) || !strings.Contains(p, `meta='{&quot;pr&quot;:&quot;12&quot;}'`) {
		t.Fatalf("frames:\n%s", p)
	}
	s2, _ := newStoreCfg(t, map[string]Config{"a": {Limits: BuiltinLimits(), Intro: "These messages arrived while you were busy or asleep, oldest first."}, "b": {Limits: BuiltinLimits(), Intro: ""}})
	enq(t, s2, "a", "local", api.EnqueueRequest{Body: "x"})
	enq(t, s2, "b", "local", api.EnqueueRequest{Body: "x"})
	if pa := claim(t, s2, "a").Request.Prompt; !strings.Contains(pa, "· 1 message · ") || !strings.Contains(pa, "]\n\nThese messages arrived while you were busy or asleep, oldest first.\n\n[a12s · how") {
		t.Fatalf("default intro:\n%s", pa)
	}
	if pb := claim(t, s2, "b").Request.Prompt; !strings.Contains(pb, "Z]\n\n[a12s · how") {
		t.Fatalf("empty intro:\n%s", pb)
	}
}

// The 4 KiB preamble reserve covers the worst-case header, intro, safety
// block and end line.
func TestPreambleReserveBound(t *testing.T) {
	worstIntro := strings.Repeat("<", 512)
	pre := Render("abcdefghijklmnopqrstuvwxyzabcdefghijklmn", "b_"+strings.Repeat("Z", 26), worstIntro, time.Now(), nil, strings.Repeat("0", NonceLen))
	if n := JSONLen(pre); n > PreambleReserve {
		t.Fatalf("preamble %d > %d", n, PreambleReserve)
	}
}

// Worst-case escaped Messages at the limits keep the full JSON request under
// computerd's 2 MiB limit.
func TestRequestFitsComputerdLimit(t *testing.T) {
	for _, lim := range []Limits{BuiltinLimits(), {MaxMessages: api.LimitValue{Value: 2000, Source: "computer"}, MaxBytes: api.LimitValue{Value: 1 << 20, Source: "computer"}}} {
		s, _ := newStoreCfg(t, map[string]Config{"builder": {Limits: lim, Intro: strings.Repeat("&", 512)}})
		md := map[string]string{}
		for i := range 16 {
			md[strings.Repeat(string(rune('a'+i)), 40)] = strings.Repeat(`<"&'>`, 42)
		}
		sender := strings.Repeat("s", 64)
		n := 0
		for {
			body := strings.Repeat("<", 100)
			_, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "token:" + strings.Repeat("t", 63), Req: api.EnqueueRequest{Body: body, Sender: sender, Metadata: md, ReplaceKey: strings.Repeat("k", 60) + string(rune('A'+n%26)) + string(rune('A'+n/26%26)) + string(rune('A'+n/676)), Expires: i64(MaxExpires)}}, nil)
			if err != nil {
				if code(err) != api.ErrInboxFull {
					t.Fatal(err)
				}
				break
			}
			n++
		}
		b := claim(t, s, "builder")
		data, _ := api.MarshalWire(b.Request)
		if int64(JSONLen(b.Request.Prompt)) > lim.MaxBytes.Value {
			t.Fatalf("render %d over maxBytes %d", JSONLen(b.Request.Prompt), lim.MaxBytes.Value)
		}
		if len(data) > 2<<20 {
			t.Fatalf("request %d bytes over computerd's 2 MiB", len(data))
		}
		t.Logf("limits %d/%d: %d messages, render %d B, request %d B", lim.MaxMessages.Value, lim.MaxBytes.Value, n, JSONLen(b.Request.Prompt), len(data))
	}
}

func TestNonceAvoidsBodies(t *testing.T) {
	// The first two draws clash with a body and the metadata; the third is used.
	draws := []byte{1, 2, 3}
	old := nonceRead
	t.Cleanup(func() { nonceRead = old })
	nonceRead = func(b []byte) (int, error) {
		for i := range b {
			b[i] = draws[0]
		}
		draws = draws[1:]
		return len(b), nil
	}
	clash1, clash2 := strings.Repeat("01", 8), strings.Repeat("02", 8)
	msgs := []BatchMessage{{Frame: Frame{ID: "m_1", Sender: "x", Via: "local", Body: "a " + clash1 + " b", Metadata: map[string]string{"k": clash2}}}}
	p := RenderBatch("c", "b_1", "", time.Now(), msgs)
	if !strings.Contains(p, "</msg-"+strings.Repeat("03", 8)) || len(draws) != 0 {
		t.Fatal(p)
	}
}

// The 2,000-Message request is built in memory by claimTx's own code, since
// 2,000 Store enqueues are too slow under -race.
func TestTwoThousandMessagesFit(t *testing.T) {
	lim := Limits{MaxMessages: api.LimitValue{Value: 2000, Source: "computer"}, MaxBytes: api.LimitValue{Value: 1 << 20, Source: "computer"}}
	intro := strings.Repeat("[", 512)
	s, _ := newStoreCfg(t, map[string]Config{"builder": {Limits: lim, Intro: intro}})
	key := func(n int) string {
		return strings.Repeat("k", 60) + string(rune('A'+n%26)) + string(rune('A'+n/26%26)) + string(rune('A'+n/676)) + "x"
	}
	r, err := s.Enqueue(context.Background(), Enqueue{Computer: "builder", Principal: "token:" + strings.Repeat("t", 63),
		Req: api.EnqueueRequest{Body: "<", Sender: strings.Repeat("s", 64), ReplaceKey: key(0)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Get(context.Background(), "builder", r.Message.ID, "", true)
	if err != nil || stored.Body == nil {
		t.Fatalf("get %+v, %v", stored, err)
	}
	msgs := make([]*api.Message, 2000)
	for n := range msgs {
		m := *stored
		m.ReplaceKey, m.Seq = key(n), int64(n+1)
		msgs[n] = &m
	}
	req, _ := buildRequest("builder", "b_"+NewULID(time.Now()), api.SessionCurrent, intro, time.Now(), msgs, 0)
	data, _ := api.MarshalWire(req)
	if len(req.Inbox.Messages) != 2000 || JSONLen(req.Prompt) > lim.MaxBytes.Value || len(data) > 2<<20 {
		t.Fatalf("%d messages, render %d, request %d", len(req.Inbox.Messages), JSONLen(req.Prompt), len(data))
	}
	t.Logf("2000 messages: render %d B, request %d B", JSONLen(req.Prompt), len(data))
}

// A returned row is not replaced by an overdue one.
func TestReturnNeverReplacesWithOverdue(t *testing.T) {
	s, c := newStore(t, nil)
	a := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "a", ReplaceKey: "k"})
	b := claim(t, s, "builder")
	bb := enq(t, s, "builder", "service:clock", api.EnqueueRequest{Body: "b", ReplaceKey: "k", Expires: i64(2)})
	c.add(5 * time.Second)
	res, err := s.Return(context.Background(), b.ID, ReturnOptions{Reason: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Pending, ",") != a.Message.ID || len(res.Replaced) != 0 {
		t.Fatalf("return %+v", res)
	}
	m, _ := s.Get(context.Background(), "builder", bb.Message.ID, "", false)
	if m.State != api.MsgExpired {
		t.Fatalf("b is %s", m.State)
	}
}
