package llmagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestUsesAgy(t *testing.T) {
	for _, tt := range []struct {
		provider string
		want     bool
	}{
		{"agy", true},
		{"agy:gemini-3.1-pro@high", true},
		{"gemini", true},
		{"gemini:gemini-3.1-pro", true},
		{"claude", false},
		{"agyx", false},
	} {
		if got := usesAgy(tt.provider); got != tt.want {
			t.Errorf("usesAgy(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

// TestAgyStreamInput pins the wire shape agy's print mode accepts: an "event"
// discriminator plus a message whose content is a list of typed blocks. A bare
// string message, or a message without "event", is rejected by the CLI.
func TestAgyStreamInput(t *testing.T) {
	line := agyStreamInput("do the thing\nwith \"quotes\"")
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("payload must be exactly one NDJSON line: %q", line)
	}
	var got agyInput
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Event != "user" || got.Message.Role != "user" {
		t.Errorf("event/role = %q/%q, want user/user", got.Event, got.Message.Role)
	}
	if len(got.Message.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(got.Message.Content))
	}
	if b := got.Message.Content[0]; b.Type != "text" || b.Text != "do the thing\nwith \"quotes\"" {
		t.Errorf("block = %+v", b)
	}
}

// TestRunAgyFramesPrompt asserts the prompt reaches agy's stdin as a
// stream-json message rather than raw text.
func TestRunAgyFramesPrompt(t *testing.T) {
	a := preProbed("agy", `cat`)
	res, err := a.Run(context.Background(), "hello", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, `"event":"user"`) || !strings.Contains(res.Output, `"text":"hello"`) {
		t.Errorf("stdin payload not framed for agy: %q", res.Output)
	}
}

func TestRunAgyCapturesConversationID(t *testing.T) {
	a := preProbed("agy", `cat >/dev/null; echo '{"event":"init","conversation_id":"c-1","init":{}}'; echo '{"event":"result","result":{"conversation_id":"ignored-nested"}}'`)
	res, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != "c-1" {
		t.Errorf("SessionID = %q, want c-1", res.SessionID)
	}
}
