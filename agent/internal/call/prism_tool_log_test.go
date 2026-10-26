package call

import (
	"phoneagent/internal/llm"
	"strings"
	"testing"
)

func TestPrismToolLogPairsRepeatedToolsAndKeepsFailures(t *testing.T) {
	p := prismToolLog{session: "vox-fixture"}
	p.observe(llm.ToolTrace{Type: "tool_use", ID: "a", Tool: "calendar", Input: map[string]interface{}{"action": "list"}})
	p.observe(llm.ToolTrace{Type: "tool_use", ID: "b", Tool: "calendar", Input: map[string]interface{}{"action": "add"}})
	p.observe(llm.ToolTrace{Type: "tool_use", ID: "b", Tool: "calendar"}) // replayed event
	// Prism's result events carry only their ID, not their tool name or arguments.
	p.observe(llm.ToolTrace{Type: "tool_result", ID: "b", Output: "Permission denied", IsError: true})
	p.observe(llm.ToolTrace{Type: "tool_result", ID: "a", Output: "No events."})
	p.observe(llm.ToolTrace{Type: "tool_use", ID: "c", Tool: "note"}) // interrupted before result
	p.observe(llm.ToolTrace{Type: "tool_use", ID: "end", Tool: "end_call"})
	p.observe(llm.ToolTrace{Type: "tool_result", ID: "end", Output: "Queued"})
	p.observe(llm.ToolTrace{Type: "tool_result", ID: "orphan", Output: "Unknown"})
	if len(p.entries) != 3 {
		t.Fatal("duplicate or relayed phone tools entered trace", p.entries)
	}
	if p.entries[0]["outcome"] != "returned" || p.entries[0]["result"] != "No events." || p.entries[1]["outcome"] != "failed" || p.entries[2]["outcome"] != "started" {
		t.Fatal("incorrect execution outcomes", p.entries)
	}
	if p.entries[0]["prism_session"] != "vox-fixture" || p.entries[1]["args"].(map[string]interface{})["action"] != "add" {
		t.Fatal("trace lost its arguments/session")
	}
}

func TestPrismToolLogCapsResultsWithoutSplittingUnicode(t *testing.T) {
	p := prismToolLog{}
	p.observe(llm.ToolTrace{Type: "tool_use", ID: "1", Tool: "note"})
	p.observe(llm.ToolTrace{Type: "tool_result", ID: "1", Output: strings.Repeat("é", 2001)})
	if got := p.entries[0]["result"].(string); len([]rune(got)) != 2000 || p.entries[0]["result_truncated"] != true {
		t.Fatal("unbounded or broken result")
	}
}
