package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The fixtures are the payload shapes computerd emits, copied from
// computerd/src/computer.ts (turn.started, resume.failed, turn.finished on
// failure, diagnostic) and computerd/src/adapters (turn.finished, text.delta).
func TestComputerdPayloads(t *testing.T) {
	tests := []struct {
		name string
		json string
		into any
		want any
	}{
		{
			name: "turn.started (computer.ts beginTurn)",
			json: `{"harness":{"kind":"codex","version":"0.154.0"},"model":"gpt-6-astra","resumed":true,"prompt":"hello","cwd":"/home/agent"}`,
			into: &TurnStartedData{},
			want: &TurnStartedData{Harness: Harness{Kind: "codex", Version: "0.154.0"}, Model: "gpt-6-astra", Resumed: true, Prompt: "hello", Cwd: "/home/agent"},
		},
		{
			name: "turn.started without a model",
			json: `{"harness":{"kind":"claude-code","version":"2.1.280"},"model":null,"resumed":false,"prompt":"p","cwd":"/home/agent"}`,
			into: &TurnStartedData{},
			want: &TurnStartedData{Harness: Harness{Kind: "claude-code", Version: "2.1.280"}, Prompt: "p", Cwd: "/home/agent"},
		},
		{
			name: "resume.failed (computer.ts wake precheck)",
			json: `{"sessionId":"s_9c","reason":"thread not found","at":"wake"}`,
			into: &ResumeFailedData{},
			want: &ResumeFailedData{SessionID: "s_9c", Reason: "thread not found", At: ResumeAtWake},
		},
		{
			name: "turn.finished failed (computer.ts)",
			json: `{"status":"failed","usage":null,"error":"adapter ended without turn.finished"}`,
			into: &TurnFinishedData{},
			want: &TurnFinishedData{Status: TurnFailed, Usage: json.RawMessage("null"), Error: "adapter ended without turn.finished"},
		},
		{
			name: "turn.finished completed (adapters)",
			json: `{"status":"completed","usage":{"input_tokens":12,"output_tokens":3}}`,
			into: &TurnFinishedData{},
			want: &TurnFinishedData{Status: TurnCompleted, Usage: json.RawMessage(`{"input_tokens":12,"output_tokens":3}`)},
		},
		{
			name: "diagnostic with extra fields (computer.ts credentials)",
			json: `{"message":"credentials not injected","names":["openai"]}`,
			into: &DiagnosticData{},
			want: &DiagnosticData{Message: "credentials not injected"},
		},
		{
			name: "diagnostic from the controller (computer.ts diagnostic())",
			json: `{"message":"egress denied","data":{"host":"evil.example"},"source":"controller"}`,
			into: &DiagnosticData{},
			want: &DiagnosticData{Message: "egress denied", Data: json.RawMessage(`{"host":"evil.example"}`), Source: "controller"},
		},
		{
			name: "text.delta (adapters)",
			json: `{"text":"hi"}`,
			into: &TextDeltaData{},
			want: &TextDeltaData{Text: "hi"},
		},
		{
			name: "status (computer.ts status())",
			json: `{"phase":"ready","session":null,"activeTurn":null,"harness":{"kind":"codex","version":"0.154.0","model":"gpt-6-astra"},"credentialGeneration":3,"resumePending":true}`,
			into: &ComputerdStatus{},
			want: &ComputerdStatus{Phase: "ready", Harness: Harness{Kind: "codex", Version: "0.154.0", Model: "gpt-6-astra"}, CredentialGeneration: 3, ResumePending: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.json), tt.into); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tt.into, tt.want) {
				t.Errorf("got  %+v\nwant %+v", tt.into, tt.want)
			}
		})
	}
}
