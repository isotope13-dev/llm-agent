package llmagent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func newTestPiSignals() *piSignals {
	return newPiSignals(io.Discard, nil)
}

func TestObservePiLineAgentEnd(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine(`{"type":"agent_end","messages":[]}`, sigs)
	select {
	case <-sigs.agentEnd:
	default:
		t.Fatal("agentEnd not signalled")
	}
}

func TestObservePiLineAgentEndIdempotent(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine(`{"type":"agent_end"}`, sigs)
	// Second call must not panic by re-closing the channel.
	observePiLine(`{"type":"agent_end"}`, sigs)
}

func TestObservePiLineGetStateCapturesSessionFile(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine(`{"type":"response","command":"get_state","success":true,"data":{"sessionFile":"/p/q.jsonl"}}`, sigs)
	if got := sigs.sessionFile(); got != "/p/q.jsonl" {
		t.Errorf("sessionFile = %q", got)
	}
}

func TestObservePiLineSwitchSessionAcks(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine(`{"type":"response","command":"switch_session","success":true,"data":{"cancelled":false}}`, sigs)
	select {
	case <-sigs.switchAck:
	default:
		t.Fatal("switchAck not signalled")
	}
}

func TestObservePiLineSwitchAckIdempotent(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine(`{"type":"response","command":"switch_session","success":true}`, sigs)
	observePiLine(`{"type":"response","command":"switch_session","success":true}`, sigs)
}

func TestObservePiLineIgnoresOtherResponses(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine(`{"type":"response","command":"set_model","success":true}`, sigs)
	if got := sigs.sessionFile(); got != "" {
		t.Errorf("sessionFile = %q, want empty", got)
	}
	select {
	case <-sigs.switchAck:
		t.Fatal("switchAck should not fire on unrelated response")
	default:
	}
}

func TestObservePiLineIgnoresGarbage(t *testing.T) {
	sigs := newTestPiSignals()
	observePiLine("not json", sigs)
	observePiLine("", sigs)
	if got := sigs.sessionFile(); got != "" {
		t.Errorf("sessionFile = %q, want empty", got)
	}
	select {
	case <-sigs.agentEnd:
		t.Fatal("agentEnd should not have fired")
	default:
	}
}

func TestObservePiLineExtensionDialogAutoCancels(t *testing.T) {
	for _, method := range []string{"select", "confirm", "input", "editor"} {
		t.Run(method, func(t *testing.T) {
			var buf bytes.Buffer
			sigs := newPiSignals(&buf, nil)
			line := `{"type":"extension_ui_request","id":"uuid-1","method":"` + method + `","title":"x"}`
			observePiLine(line, sigs)

			out := strings.TrimSpace(buf.String())
			if out == "" {
				t.Fatalf("no extension_ui_response written")
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("response not JSON: %v\n%s", err, out)
			}
			if got["type"] != "extension_ui_response" {
				t.Errorf("type = %v, want extension_ui_response", got["type"])
			}
			if got["id"] != "uuid-1" {
				t.Errorf("id = %v, want uuid-1", got["id"])
			}
			if got["cancelled"] != true {
				t.Errorf("cancelled = %v, want true", got["cancelled"])
			}
		})
	}
}

func TestObservePiLineExtensionFireAndForgetIgnored(t *testing.T) {
	for _, method := range []string{"notify", "setStatus", "setWidget", "setTitle", "set_editor_text"} {
		t.Run(method, func(t *testing.T) {
			var buf bytes.Buffer
			sigs := newPiSignals(&buf, nil)
			line := `{"type":"extension_ui_request","id":"u","method":"` + method + `"}`
			observePiLine(line, sigs)
			if buf.Len() != 0 {
				t.Errorf("unexpected write for %s: %q", method, buf.String())
			}
		})
	}
}
