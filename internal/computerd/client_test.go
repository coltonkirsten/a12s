package computerd

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStream(t *testing.T) {
	body := ": keepalive\n\n" +
		"event: gap\ndata: {\"lastEventId\":\"99\",\"head\":3}\n\n" +
		"id: 4\r\ndata: {\"v\":1,\"computer\":\"scout\",\"seq\":4,\"type\":\"text.delta\",\"ts\":\"2026-10-05T12:00:00Z\",\"data\":{\"text\":\"hi\"}}\r\n\r\n" +
		"data: a\ndata: b\n\n" +
		"id: 5\ndata: tail"
	s := NewStream(io.NopCloser(strings.NewReader(body)))

	m, err := s.Next()
	if err != nil || m.Event != "gap" || m.ID != "" || !strings.Contains(string(m.Data), `"head":3`) {
		t.Fatalf("gap = %+v, %v", m, err)
	}
	m, err = s.Next()
	if err != nil || m.ID != "4" || m.Event != "" {
		t.Fatalf("event = %+v, %v", m, err)
	}
	ev, err := m.Decode()
	if err != nil || ev.Seq != 4 || ev.Type != "text.delta" || ev.Computer != "scout" {
		t.Fatalf("decode = %+v, %v", ev, err)
	}
	if m, err = s.Next(); err != nil || string(m.Data) != "a\nb" {
		t.Fatalf("multi-line data = %+v, %v", m, err)
	}
	if m, err = s.Next(); err != nil || m.ID != "5" || string(m.Data) != "tail" {
		t.Fatalf("unterminated = %+v, %v", m, err)
	}
	if _, err = s.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v", err)
	}
}
