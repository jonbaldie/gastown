package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestNewProxyInitializesProtocolAndStreams(t *testing.T) {
	p := NewProxy()
	if p.stdin != os.Stdin || p.stdout != os.Stdout {
		t.Fatalf("streams = (%v, %v), want os.Stdin and os.Stdout", p.stdin, p.stdout)
	}
	if p.handshakeState != handshakeInit {
		t.Fatalf("handshake state = %d, want %d", p.handshakeState, handshakeInit)
	}
	if p.uiEncoder == nil || p.lastActivity.Load() == 0 {
		t.Fatal("encoder and last activity must be initialized")
	}
}

func TestMarkPromptBusyRequiresPromptRequest(t *testing.T) {
	p := NewProxy()
	for _, msg := range []any{
		"not a message",
		&JSONRPCMessage{Method: "other", ID: "id"},
		&JSONRPCMessage{Method: "session/prompt"},
	} {
		if p.markPromptBusy(msg) {
			t.Fatalf("markPromptBusy(%#v) = true", msg)
		}
	}
	for _, tc := range []struct {
		id   any
		want string
	}{{"prompt-id", "prompt-id"}, {42, "42"}} {
		if !p.markPromptBusy(&JSONRPCMessage{Method: "session/prompt", ID: tc.id}) {
			t.Fatalf("markPromptBusy(%#v) = false", tc.id)
		}
		if p.activePromptID != tc.want {
			t.Fatalf("active prompt = %q, want %q", p.activePromptID, tc.want)
		}
	}
}

func TestHandleInjectedResponse(t *testing.T) {
	p := NewProxy()
	p.heartbeatSupported.Store(true)
	if handleInjectedResponse(p, &JSONRPCMessage{ID: 1}) ||
		handleInjectedResponse(p, &JSONRPCMessage{ID: "ordinary"}) {
		t.Fatal("ordinary responses must not be treated as injected")
	}
	if !handleInjectedResponse(p, &JSONRPCMessage{ID: "gt-inject-ok"}) {
		t.Fatal("successful injected response was not consumed")
	}
	if !handleInjectedResponse(p, &JSONRPCMessage{
		ID: "gt-inject-keepalive-1", Error: &JSONRPCError{Code: -1, Message: "unsupported"},
	}) {
		t.Fatal("failed keepalive response was not consumed")
	}
	if p.heartbeatSupported.Load() {
		t.Fatal("failed keepalive must disable heartbeat support")
	}
}

func TestKeepAliveMessageContents(t *testing.T) {
	p := NewProxy()
	mode := keepAliveMessage(p, "session-1", "set_mode", "mode-1", time.Minute)
	if mode.JSONRPC != "2.0" || mode.Method != "session/set_mode" ||
		!strings.HasPrefix(mode.ID.(string), "gt-inject-keepalive-") {
		t.Fatalf("mode heartbeat = %#v", mode)
	}
	var params map[string]any
	if err := json.Unmarshal(mode.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params["sessionId"] != "session-1" || params["modeId"] != "mode-1" {
		t.Fatalf("mode params = %#v", params)
	}

	for _, tc := range []struct{ method, mode string }{{"set_mode", ""}, {"_ping", "mode-1"}} {
		ping := keepAliveMessage(p, "session-1", tc.method, tc.mode, time.Minute)
		if ping.JSONRPC != "2.0" || ping.Method != "_ping" || string(ping.Params) != "{}" {
			t.Fatalf("ping heartbeat = %#v", ping)
		}
	}
}

func TestKeepAliveBusyBoundary(t *testing.T) {
	p := NewProxy()
	p.activePromptID = "busy"
	if !keepAliveBusy(p, 60*time.Second) || p.activePromptID != "busy" {
		t.Fatal("busy prompt at the recovery boundary must remain busy")
	}
	if keepAliveBusy(p, 60*time.Second+time.Nanosecond) || p.activePromptID != "" {
		t.Fatal("stuck busy prompt past the boundary must be cleared")
	}
}

func TestNotificationMessageContents(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		sessionID string
		params    any
		want      string
	}{
		{"empty", "event", "", nil, ""},
		{"session", "event", "s1", nil, `{"sessionId":"s1"}`},
		{"map", "event", "s1", map[string]any{"value": "x"}, `{"sessionId":"s1","value":"x"}`},
		{"scalar", "event", "", "x", `{"params":"x"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := notificationMessage(tc.method, tc.sessionID, tc.params)
			if msg.JSONRPC != "2.0" || msg.Method != tc.method || string(msg.Params) != tc.want {
				t.Fatalf("notification = %#v, params %s", msg, msg.Params)
			}
		})
	}
}

func TestHandleRawAgentOutputBoundsAndTriggers(t *testing.T) {
	p := NewProxy()
	handleRawAgentOutput(p, strings.Repeat("x", 2100), &json.SyntaxError{})
	if len(p.propulsionBuffer) != 2000 || p.Propelled.Load() {
		t.Fatalf("buffer length = %d, propelled = %v", len(p.propulsionBuffer), p.Propelled.Load())
	}
	handleRawAgentOutput(p, " AUTONOMOUS WORK MODE ", &json.SyntaxError{})
	if !p.Propelled.Load() || p.propulsionBuffer != "" {
		t.Fatalf("trigger state: propelled=%v buffer=%q", p.Propelled.Load(), p.propulsionBuffer)
	}
}

func TestForwardMessageToUI(t *testing.T) {
	var out bytes.Buffer
	p := NewProxy()
	p.uiEncoder = json.NewEncoder(&out)
	msg := &JSONRPCMessage{JSONRPC: "2.0", Method: "event"}
	if err := forwardMessageToUI(p, msg, false); err != nil || !strings.Contains(out.String(), `"method":"event"`) {
		t.Fatalf("forward result: err=%v output=%q", err, out.String())
	}
	out.Reset()
	if err := forwardMessageToUI(p, msg, true); err != nil || out.Len() != 0 {
		t.Fatalf("injected message forwarded: err=%v output=%q", err, out.String())
	}
	p.Propelled.Store(true)
	if err := forwardMessageToUI(p, msg, false); err != nil || out.Len() != 0 {
		t.Fatalf("propelled message forwarded: err=%v output=%q", err, out.String())
	}
}

func TestProcessAgentLineStopsAfterUIWriteFailure(t *testing.T) {
	p := NewProxy()
	p.uiEncoder = json.NewEncoder(errorWriter{})
	if processAgentLine(p, `{"jsonrpc":"2.0","method":"event"}`) {
		t.Fatal("processAgentLine() = true after UI write failure")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("UI write failure did not mark proxy done")
	}
}
