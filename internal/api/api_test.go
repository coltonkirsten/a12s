package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventEnvelope(t *testing.T) {
	raw := `{"v":1,"agentComputer":"scout","session":"s_9c","turn":"t_41","seq":7,"ts":"2026-10-05T12:00:00Z","type":"text.delta","data":{"text":"hi"}}`
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.V != EventVersion || ev.Seq != 7 || ev.Type != EventTextDelta || ev.TS != time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) {
		t.Fatalf("event = %+v", ev)
	}
	var d TextDeltaData
	if err := json.Unmarshal(ev.Data, &d); err != nil || d.Text != "hi" {
		t.Fatalf("data = %+v, %v", d, err)
	}
	out, err := json.Marshal(ev)
	if err != nil || string(out) != raw {
		t.Errorf("round trip:\n%s\n%s", out, raw)
	}
}

func TestCredentialsAreBase64(t *testing.T) {
	out, err := json.Marshal(PowerRequest{State: PowerAwake, Credentials: Credentials{"openai": []byte("sk-x")}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"state":"awake","credentials":{"openai":"c2steA=="}}` {
		t.Errorf("got %s", out)
	}
}
