package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseStopReasonNull(t *testing.T) {
	b, _ := json.Marshal(Response{ID: "m", Type: "message", Role: "assistant"})
	if !strings.Contains(string(b), `"stop_reason":null`) || !strings.Contains(string(b), `"content":[]`) || strings.Count(string(b), "stop_reason") != 1 {
		t.Fatalf("%s", b)
	}
	var r Response
	if err := json.Unmarshal([]byte(`{"id":"m","stop_reason":null,"content":[{"type":"text","text":"x"}]}`), &r); err != nil || r.StopReason != "" || r.Content[0].Text != "x" {
		t.Fatalf("%+v %v", r, err)
	}
	_ = json.Unmarshal([]byte(`{"stop_reason":"end_turn"}`), &r)
	if r.StopReason != "end_turn" {
		t.Fatal(r.StopReason)
	}
}

func TestRequestSystemStringOrBlocks(t *testing.T) {
	var a, b Request
	_ = json.Unmarshal([]byte(`{"system":"be brief","messages":[{"role":"user","content":"hi"}]}`), &a)
	_ = json.Unmarshal([]byte(`{"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`), &b)
	if a.SystemText() != "be brief" || b.SystemText() != "be brief" || a.Messages[0].Text() != "hi" || b.Messages[0].Text() != "hi" {
		t.Fatal("system/content parsing")
	}
	out, _ := json.Marshal(b)
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("cache_control lost: %s", out)
	}
}
