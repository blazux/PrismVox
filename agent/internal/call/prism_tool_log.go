package call

import (
	"phoneagent/internal/llm"
	"time"
)

// Written only by the turn's stream reader, then consumed after llmErrCh joins
// that reader. This keeps remote execution traces out of concurrent call state.
type prismToolLog struct {
	session string
	entries []map[string]interface{}
	byID    map[string]int
}

func (p *prismToolLog) observe(event llm.ToolTrace) {
	if event.Type == "tool_use" {
		switch event.Tool {
		case "", "transfer_call", "take_message", "end_call":
			return
		}
		if p.byID == nil {
			p.byID = make(map[string]int)
		}
		if _, exists := p.byID[event.ID]; event.ID != "" && exists {
			return
		}
		p.entries = append(p.entries, map[string]interface{}{
			"tool": event.Tool, "args": event.Input, "outcome": "started", "source": "prism",
			"prism_session": p.session, "id": event.ID, "started_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
		if event.ID != "" {
			p.byID[event.ID] = len(p.entries) - 1
		}
		return
	}
	index, ok := p.byID[event.ID]
	if !ok || event.Type != "tool_result" {
		return
	}
	entry := p.entries[index]
	entry["outcome"] = "returned"
	if event.IsError {
		entry["outcome"] = "failed"
	}
	result := []rune(event.Output)
	if len(result) > 2000 {
		result = result[:2000]
		entry["result_truncated"] = true
	}
	entry["result"] = string(result)
	entry["ended_at"] = time.Now().UTC().Format(time.RFC3339Nano)
}
