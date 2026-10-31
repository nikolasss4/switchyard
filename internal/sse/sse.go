// Package sse reads and writes Server-Sent Events.
package sse

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
)

// Event is one SSE event.
type Event struct {
	Name string
	Data []byte
}

// Reader parses an SSE stream.
type Reader struct {
	r *bufio.Reader
}

// NewReader returns a Reader over r. The buffer is small because a gateway
// holds one per open stream; longer lines are still read whole (ReadBytes
// grows past the buffer).
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 4<<10)}
}

// Next returns the next event with data, skipping comments. It returns io.EOF
// at the end of the stream.
func (r *Reader) Next() (Event, error) {
	var ev Event
	var data bytes.Buffer
	hasData := false
	for {
		line, err := r.r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if hasData {
				ev.Data = data.Bytes()
				return ev, nil
			}
			return Event{}, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if hasData {
				ev.Data = data.Bytes()
				return ev, nil
			}
			ev = Event{}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Name = string(value)
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		}
		if err != nil {
			ev.Data = data.Bytes()
			if hasData {
				return ev, nil
			}
			return Event{}, err
		}
	}
}

// Writer writes SSE events and flushes after each one.
type Writer struct {
	w       io.Writer
	flusher http.Flusher
}

// NewWriter sets the SSE response headers and returns a Writer.
func NewWriter(w http.ResponseWriter) *Writer {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	return &Writer{w: w, flusher: f}
}

// Write sends one event. An empty name sends a data-only event.
func (w *Writer) Write(name string, data []byte) error {
	var buf bytes.Buffer
	buf.Grow(len(data) + len(name) + 16)
	if name != "" {
		buf.WriteString("event: ")
		buf.WriteString(name)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if _, err := w.w.Write(buf.Bytes()); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}
