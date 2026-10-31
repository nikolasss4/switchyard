package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderParsesNamedAndDataOnlyEvents(t *testing.T) {
	in := ": comment\n" +
		"event: message_start\ndata: {\"a\":1}\n\n" +
		"data: line1\ndata: line2\n\n" +
		"data: [DONE]"
	r := NewReader(strings.NewReader(in))
	want := []Event{
		{Name: "message_start", Data: []byte(`{"a":1}`)},
		{Data: []byte("line1\nline2")},
		{Data: []byte("[DONE]")},
	}
	for i, w := range want {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if got.Name != w.Name || string(got.Data) != string(w.Data) {
			t.Fatalf("event %d = %q %q, want %q %q", i, got.Name, got.Data, w.Name, w.Data)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReaderHandlesCRLF(t *testing.T) {
	r := NewReader(strings.NewReader("event: ping\r\ndata: {}\r\n\r\n"))
	ev, err := r.Next()
	if err != nil || ev.Name != "ping" || string(ev.Data) != "{}" {
		t.Fatalf("got %+v, %v", ev, err)
	}
}

func TestReaderLongLine(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	r := NewReader(strings.NewReader("data: " + long + "\n\n"))
	ev, err := r.Next()
	if err != nil || string(ev.Data) != long {
		t.Fatalf("len %d err %v", len(ev.Data), err)
	}
}
