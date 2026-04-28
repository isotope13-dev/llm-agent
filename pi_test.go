package llmagent

import "testing"

func TestObservePiLineAgentEnd(t *testing.T) {
	sigs := newPiSignals()
	observePiLine(`{"type":"agent_end","messages":[]}`, sigs)
	select {
	case <-sigs.agentEnd:
	default:
		t.Fatal("agentEnd not signalled")
	}
}

func TestObservePiLineAgentEndIdempotent(t *testing.T) {
	sigs := newPiSignals()
	observePiLine(`{"type":"agent_end"}`, sigs)
	// Second call must not panic by re-closing the channel.
	observePiLine(`{"type":"agent_end"}`, sigs)
}

func TestObservePiLineGetStateCapturesSessionFile(t *testing.T) {
	sigs := newPiSignals()
	observePiLine(`{"type":"response","command":"get_state","success":true,"data":{"sessionFile":"/p/q.jsonl"}}`, sigs)
	if got := sigs.sessionFile(); got != "/p/q.jsonl" {
		t.Errorf("sessionFile = %q", got)
	}
}

func TestObservePiLineIgnoresOtherResponses(t *testing.T) {
	sigs := newPiSignals()
	observePiLine(`{"type":"response","command":"set_model","success":true}`, sigs)
	if got := sigs.sessionFile(); got != "" {
		t.Errorf("sessionFile = %q, want empty", got)
	}
}

func TestObservePiLineIgnoresGarbage(t *testing.T) {
	sigs := newPiSignals()
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
