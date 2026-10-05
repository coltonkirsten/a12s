// Package sse reads Server-Sent Events frames. It only frames; reconnect
// policy belongs to each client.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Frame is one dispatched SSE message.
type Frame struct {
	ID    string // the id: field ("" if absent)
	Event string // the event: field ("" for the default "message")
	Data  []byte // data: lines joined with "\n"
}

// Reader reads frames from an SSE body.
type Reader struct {
	r *bufio.Reader
}

// NewReader returns a Reader on r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReaderSize(r, 64<<10)} }

// Next returns the next frame. A frame is dispatched only when its blank
// terminating line has been read; comment lines and empty frames are
// skipped. When the stream ends between frames Next returns io.EOF; when it
// ends inside a frame, that frame is discarded (never returned) and Next
// returns io.ErrUnexpectedEOF. Other read errors are returned as they are.
func (r *Reader) Next() (Frame, error) {
	var f Frame
	var data [][]byte
	partial := false // a line of the current frame has been read
	fields := false  // ... and it was not only comments
	for {
		line, err := r.r.ReadBytes('\n')
		if err != nil {
			// A line without its newline is incomplete too: discard it.
			if errors.Is(err, io.EOF) {
				if partial || len(line) > 0 {
					return Frame{}, io.ErrUnexpectedEOF
				}
				return Frame{}, io.EOF
			}
			return Frame{}, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if !fields {
				partial = false
				continue
			}
			f.Data = bytes.Join(data, []byte("\n"))
			return f, nil
		}
		partial = true
		if line[0] == ':' {
			continue
		}
		fields = true
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "id":
			f.ID = string(value)
		case "event":
			f.Event = string(value)
		case "data":
			data = append(data, append([]byte(nil), value...))
		}
	}
}
