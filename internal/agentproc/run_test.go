package agentproc

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// captureSink records what consumeStream delivered, so a test can
// assert the regression-case message survived the stream reader.
type captureSink struct {
	sessionID string
	messages  []*domain.AgentMessage
}

func (c *captureSink) OnSession(sid string) error {
	c.sessionID = sid
	return nil
}

func (c *captureSink) OnMessage(m *domain.AgentMessage) error {
	c.messages = append(c.messages, m)
	return nil
}

// TestConsumeStream_HandlesOversizedToolResult is the SKY-* regression
// for "Run X failed: stream: bufio.Scanner: token too long". A real
// tool_result line carrying a multi-megabyte file read used to exceed
// our 1 MB scanner ceiling; the run aborted with no terminal Result
// captured even though the subprocess kept emitting valid JSON we
// just couldn't read. Asserts the bigger line flows through and the
// terminal `result` event is still observed.
func TestConsumeStream_HandlesOversizedToolResult(t *testing.T) {
	huge := strings.Repeat("x", 4*1024*1024) // 4 MB — comfortably past the old 1 MB cap.

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-big"}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/big"}}]}}`,
		`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"` + huge + `"}]}}`,
		`{"type":"result","is_error":false,"duration_ms":50,"num_turns":1,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"{\"status\":\"completed\",\"summary\":\"ok\"}"}`,
		"",
	}, "\n")

	sink := &captureSink{}
	result, err := consumeStream(strings.NewReader(stream), sink, NewStreamState(), "trace-big")
	if err != nil {
		t.Fatalf("consumeStream returned error on oversized line: %v", err)
	}
	if result == nil {
		t.Fatal("expected terminal Result, got nil — stream reader bailed before the result event")
	}
	if sink.sessionID != "sess-big" {
		t.Errorf("session id = %q, want sess-big", sink.sessionID)
	}

	var toolMsg *domain.AgentMessage
	for _, m := range sink.messages {
		if m.Role == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("expected a tool message in the sink — the oversized line was dropped")
	}
	if len(toolMsg.Content) != len(huge) {
		t.Errorf("tool message content length = %d, want %d", len(toolMsg.Content), len(huge))
	}
}

// TestConsumeStream_TrailingLineWithoutNewline guards the EOF path:
// if the subprocess exits after writing a final event without a
// trailing newline, that event must still be parsed rather than
// silently swallowed by the EOF return.
func TestConsumeStream_TrailingLineWithoutNewline(t *testing.T) {
	// Final result event has no trailing \n.
	stream := `{"type":"system","subtype":"init","session_id":"sess-eof"}` + "\n" +
		`{"type":"result","is_error":false,"duration_ms":1,"num_turns":0,"total_cost_usd":0,"stop_reason":"end_turn","result":"{\"status\":\"completed\",\"summary\":\"\"}"}`

	sink := &captureSink{}
	result, err := consumeStream(strings.NewReader(stream), sink, NewStreamState(), "trace-eof")
	if err != nil {
		t.Fatalf("consumeStream returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected terminal Result on EOF-terminated final line")
	}
}
