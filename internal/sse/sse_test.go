package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func frames(t *testing.T, body string) ([]Frame, error) {
	t.Helper()
	r := NewReader(strings.NewReader(body))
	var out []Frame
	for {
		f, err := r.Next()
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

func TestFrames(t *testing.T) {
	got, err := frames(t, ": keepalive\n\nid: 1\ndata: {\"a\":1}\n\n\n\r\nid: 2\r\ndata: x\r\ndata:y\r\n\r\nevent: head\ndata: {\"seq\":2}\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	want := []Frame{
		{ID: "1", Data: []byte(`{"a":1}`)},
		{ID: "2", Data: []byte("x\ny")},
		{Event: "head", Data: []byte(`{"seq":2}`)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames: %+v", len(got), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Event != want[i].Event || string(got[i].Data) != string(want[i].Data) {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// An incomplete frame at EOF is never published, whether it ends after a
// complete line, inside a line, or as a lone comment.
func TestIncompleteFrameDiscarded(t *testing.T) {
	for _, tail := range []string{
		"id: 2\ndata: {\"seq\":2}\n",
		"id: 2\ndata: {\"se",
		"id: 2",
		": keepal",
	} {
		got, err := frames(t, "id: 1\ndata: one\n\n"+tail)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("tail %q: err = %v, want io.ErrUnexpectedEOF", tail, err)
		}
		if len(got) != 1 || got[0].ID != "1" {
			t.Errorf("tail %q: frames %+v, want only frame 1", tail, got)
		}
	}
}
