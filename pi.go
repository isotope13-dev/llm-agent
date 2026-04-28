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
//
// The reader watches for the agent_end event, captures the sessionFile
// from a get_state response, acks switch_session so the feeder can advance
// to the prompt, and auto-cancels any extension_ui_request dialogs so pi
// doesn't deadlock waiting for a TUI response we can't supply.
//
// Both feedPi and the stdout reader (via observePiLine) write to pi's
// stdin, so writes are serialised through writeMu.
type piSignals struct {
	captured     atomic.Pointer[string]
	agentEnd     chan struct{}
	switchAck    chan struct{}
	endedOnce    sync.Once
	switchedOnce sync.Once

	writeMu sync.Mutex
	enc     *json.Encoder

	log *slog.Logger
}

func newPiSignals(stdin io.Writer, log *slog.Logger) *piSignals {
	if log == nil {
		log = slog.Default()
	}
	return &piSignals{
		agentEnd:  make(chan struct{}),
		switchAck: make(chan struct{}),
		enc:       json.NewEncoder(stdin),
		log:       log,
	}
}

func (s *piSignals) signalAgentEnd() {
	s.endedOnce.Do(func() { close(s.agentEnd) })
}

func (s *piSignals) signalSwitchAck() {
	s.switchedOnce.Do(func() { close(s.switchAck) })
}

func (s *piSignals) sessionFile() string {
	if p := s.captured.Load(); p != nil {
		return *p
	}
	return ""
}

// send encodes v as a JSON line on pi's stdin, serialising with feedPi.
func (s *piSignals) send(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.enc.Encode(v)
}

// piResponseGrace is the budget we give pi to emit the get_state response
// after we send the request. After this, we close stdin regardless and let
// pi exit; the response usually arrives in well under a second. It is a
// var (not const) so tests can shorten the wait.
var piResponseGrace = 3 * time.Second

// piSwitchAckGrace is how long feedPi waits for the switch_session response
// before sending the prompt anyway. Switching is synchronous on pi's side
// and normally responds in milliseconds; this is a guard against a missing
// or malformed response, not the common path.
var piSwitchAckGrace = 5 * time.Second

// pi command IDs let us correlate response events to the commands that
// produced them. feedPi sends each command at most once per Run, so fixed
// IDs are sufficient.
const (
	piIDSwitchSession = "cyc-switch"
	piIDPrompt        = "cyc-prompt"
	piIDGetState      = "cyc-state"
)

// feedPi drives the pi --mode rpc protocol on stdin:
//
//  1. Optionally switch to a previously-captured session file and wait for
//     pi's response so the prompt lands on the loaded session.
//  2. Send the user's prompt as a "prompt" command.
//  3. Wait for the reader goroutine to flag the matching agent_end event.
//  4. Issue get_state so the reader can record the session file path.
//  5. Close stdin so pi exits cleanly.
//
// observePiLine auto-cancels any extension_ui_request dialog pi emits, so
// this loop never blocks on a TUI prompt we can't show.
func (a *Agent) feedPi(ctx context.Context, stdin io.Closer, prompt, sessionID string, sigs *piSignals) {
	defer func() {
		if err := stdin.Close(); err != nil {
			a.logger().Debug("close pi stdin", slog.Any("error", err))
		}
	}()

	if sessionID != "" {
		if err := sigs.send(struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			SessionPath string `json:"sessionPath"`
		}{"switch_session", piIDSwitchSession, sessionID}); err != nil {
			a.logger().Debug("pi switch_session", slog.Any("error", err))
			return
		}
		select {
		case <-sigs.switchAck:
		case <-time.After(piSwitchAckGrace):
			a.logger().Warn("pi switch_session ack timeout, sending prompt anyway",
				slog.Duration("grace", piSwitchAckGrace))
		case <-ctx.Done():
			return
		}
	}

	if err := sigs.send(struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Message string `json:"message"`
	}{"prompt", piIDPrompt, prompt}); err != nil {
		a.logger().Debug("pi prompt", slog.Any("error", err))
		return
	}

	select {
	case <-sigs.agentEnd:
	case <-ctx.Done():
		return
	}

	if err := sigs.send(struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{"get_state", piIDGetState}); err != nil {
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
// stdout: the event/response type, the response command (so we know which
// command was acknowledged), the request ID for extension UI dialogs, and
// the sessionFile from a get_state response. Other fields are ignored.
type piEventFields struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Method  string `json:"method"`
	Data    struct {
		SessionFile string `json:"sessionFile"`
	} `json:"data"`
}

// piDialogMethods are the extension UI methods that block pi until the
// client returns an extension_ui_response. Fire-and-forget methods
// (notify, setStatus, setWidget, setTitle, set_editor_text) need no reply
// and are ignored here.
//
// See: https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/rpc.md
var piDialogMethods = map[string]struct{}{
	"select":  {},
	"confirm": {},
	"input":   {},
	"editor":  {},
}

// observePiLine inspects one stdout line for protocol-relevant events:
// signals the feeder on agent_end and switch_session ack, captures the
// session file from a get_state response, and auto-cancels any extension
// UI dialog so pi doesn't deadlock waiting for a TUI response.
func observePiLine(line string, sigs *piSignals) {
	var ev piEventFields
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "agent_end":
		sigs.signalAgentEnd()
	case "response":
		switch ev.Command {
		case "switch_session":
			sigs.signalSwitchAck()
		case "get_state":
			if ev.Data.SessionFile != "" {
				path := ev.Data.SessionFile
				sigs.captured.Store(&path)
			}
		}
	case "extension_ui_request":
		if _, isDialog := piDialogMethods[ev.Method]; !isDialog {
			return
		}
		if err := sigs.send(map[string]any{
			"type":      "extension_ui_response",
			"id":        ev.ID,
			"cancelled": true,
		}); err != nil {
			sigs.log.Debug("pi extension_ui_response", slog.Any("error", err))
		}
	}
}
