package llmagent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// piSignals coordinate the pi-RPC feeder goroutine with the stdout reader.
// The reader closes agentEnd once on the first agent_end event and stores
// the captured sessionFile (from a get_state response) into captured.
type piSignals struct {
	captured  atomic.Pointer[string]
	agentEnd  chan struct{}
	endedOnce sync.Once
}

func newPiSignals() *piSignals {
	return &piSignals{agentEnd: make(chan struct{})}
}

func (s *piSignals) signalAgentEnd() {
	s.endedOnce.Do(func() { close(s.agentEnd) })
}

func (s *piSignals) sessionFile() string {
	if p := s.captured.Load(); p != nil {
		return *p
	}
	return ""
}

// piResponseGrace is the budget we give pi to emit the get_state response
// after we send the request. After this, we close stdin regardless and let
// pi exit; the response usually arrives in well under a second. It is a
// var (not const) so tests can shorten the wait.
var piResponseGrace = 3 * time.Second

// feedPi drives the pi --mode rpc protocol on stdin:
//
//  1. Optionally switch to a previously-captured session file.
//  2. Send the user's prompt as a "prompt" command.
//  3. Wait for the reader goroutine to flag the matching agent_end event.
//  4. Issue get_state so the reader can record the session file path.
//  5. Close stdin so pi exits cleanly.
//
// Errors writing to stdin are logged at debug — if pi has exited early the
// stdout side will surface a meaningful failure.
func (a *Agent) feedPi(ctx context.Context, stdin io.WriteCloser, prompt, sessionID string, sigs *piSignals) {
	defer func() {
		if err := stdin.Close(); err != nil {
			a.logger().Debug("close pi stdin", slog.Any("error", err))
		}
	}()
	enc := json.NewEncoder(stdin)

	if sessionID != "" {
		if err := enc.Encode(struct {
			Type        string `json:"type"`
			SessionPath string `json:"sessionPath"`
		}{"switch_session", sessionID}); err != nil {
			a.logger().Debug("pi switch_session", slog.Any("error", err))
			return
		}
	}

	if err := enc.Encode(struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{"prompt", prompt}); err != nil {
		a.logger().Debug("pi prompt", slog.Any("error", err))
		return
	}

	select {
	case <-sigs.agentEnd:
	case <-ctx.Done():
		return
	}

	if err := enc.Encode(struct {
		Type string `json:"type"`
	}{"get_state"}); err != nil {
		a.logger().Debug("pi get_state", slog.Any("error", err))
		return
	}

	// Hold stdin open briefly so pi can emit the get_state response before
	// we close stdin and trigger its shutdown.
	select {
	case <-time.After(piResponseGrace):
	case <-ctx.Done():
	}
}

// piEventFields is the slim shape we need to drive the pi protocol from
// stdout: the event type, plus the response command and sessionFile when a
// get_state response arrives. Other fields are ignored.
type piEventFields struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Data    struct {
		SessionFile string `json:"sessionFile"`
	} `json:"data"`
}

// observePiLine inspects one stdout line for protocol-relevant events.
// Returns true if this line was the agent_end that should release the feeder.
func observePiLine(line string, sigs *piSignals) {
	var ev piEventFields
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "agent_end":
		sigs.signalAgentEnd()
	case "response":
		if ev.Command == "get_state" && ev.Data.SessionFile != "" {
			path := ev.Data.SessionFile
			sigs.captured.Store(&path)
		}
	default:
		// Other event types (turn_*, message_*, tool_execution_*, …) are
		// forwarded to OnEvent by the caller; the protocol driver is
		// only interested in agent_end and get_state responses.
	}
}
