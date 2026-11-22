package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func completionEvent(delta map[string]interface{}, finish string) string {
	choice := map[string]interface{}{"delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{choice}})
	return "data: " + string(data) + "\n\n"
}

func completionTool(index int, name, arguments string) map[string]interface{} {
	return map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
		"index": index, "id": fmt.Sprintf("call_%d", index),
		"function": map[string]interface{}{"name": name, "arguments": arguments},
	}}}
}

func runCompletionFixture(t *testing.T, body string) (string, []ToolCall, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client := &Client{URL: server.URL, Model: "fixture", streamHTTP: server.Client()}
	sentences, batches := make(chan string, 16), make(chan []ToolCall, 1)
	err := client.streamInner(context.Background(), "", nil, "", "", nil, "Synthetic request", sentences, batches)
	var text []string
	for len(sentences) > 0 {
		text = append(text, <-sentences)
	}
	var calls []ToolCall
	for len(batches) > 0 {
		calls = append(calls, (<-batches)...)
	}
	return strings.Join(text, " "), calls, err
}

func TestIncompleteStreamsNeverReleaseTools(t *testing.T) {
	tool := completionEvent(completionTool(0, "end_call", "{}"), "")
	for _, tc := range []struct{ name, suffix string }{
		{"eof", ""},
		{"provider_error", "data: {\"error\":{\"message\":\"private-provider-detail\"}}\n\n"},
		{"length", completionEvent(nil, "length") + "data: [DONE]\n\n"},
		{"filtered", completionEvent(nil, "content_filter") + "data: [DONE]\n\n"},
		{"unknown_finish", completionEvent(nil, "unknown") + "data: [DONE]\n\n"},
		{"malformed_event", "data: {broken\n\ndata: [DONE]\n\n"},
		{"error_after_finish", completionEvent(nil, "tool_calls") + "data: {\"error\":\"synthetic\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, calls, err := runCompletionFixture(t, tool+tc.suffix)
			if err == nil || len(calls) != 0 {
				t.Fatalf("incomplete stream released tools: calls=%#v err=%v", calls, err)
			}
			if strings.Contains(err.Error(), "private-provider-detail") {
				t.Fatal("provider error body leaked into diagnostics")
			}
		})
	}
}

func TestStreamFailuresPreservePartialSpeech(t *testing.T) {
	for _, suffix := range []string{"", "data: {\"error\":\"synthetic\"}\n\n", completionEvent(nil, "length")} {
		text, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": "Bonjour. Une réponse partielle"}, "")+suffix)
		if err == nil || text != "Bonjour. Une réponse partielle" || len(calls) != 0 {
			t.Fatalf("partial speech lost: text=%q calls=%#v err=%v", text, calls, err)
		}
	}
	text, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": "Bonjour end_call({"}, ""))
	if err == nil || text != "Bonjour" || len(calls) != 0 {
		t.Fatalf("partial invocation became speech/action: %q %#v %v", text, calls, err)
	}
}

func TestInvalidArgumentsRejectTheWholeToolBatch(t *testing.T) {
	for _, arguments := range []string{"{", "null", "[]", `{} {}`, `{"destination":`} {
		t.Run(arguments, func(t *testing.T) {
			body := completionEvent(completionTool(1, "take_message", `{"recipient":"Fixture","content":"Synthetic"}`), "") +
				completionEvent(completionTool(3, "end_call", arguments), "") + "data: [DONE]\n\n"
			_, calls, err := runCompletionFixture(t, body)
			if err == nil || len(calls) != 0 {
				t.Fatalf("partially validated batch escaped: %#v %v", calls, err)
			}
		})
	}
}

func TestCompleteStreamsRetainValidToolsAndCompatibilityFormats(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"structured", completionEvent(completionTool(2, "transfer_call", `{"destination":`), "") + completionEvent(completionTool(2, "", `"Fixture"}`), "") + completionEvent(nil, "tool_calls") + "data: [DONE]\n\n", "transfer_call"},
		{"done_only_parameterless", completionEvent(completionTool(0, "end_call", ""), "") + "data: [DONE]\n\n", "end_call"},
		{"normal_finish_then_eof", completionEvent(completionTool(0, "end_call", "{}"), "") + completionEvent(nil, "tool_calls"), "end_call"},
		{"raw_json_tag", completionEvent(map[string]interface{}{"content": `<tool_call>{"name":"end_call","arguments":{}}</tool_call>`}, "") + "data: [DONE]\n\n", "end_call"},
		{"raw_function_tag", completionEvent(map[string]interface{}{"content": "<tool_call><function=transfer_call><parameter=destination>Fixture</parameter></function></tool_call>"}, "stop"), "transfer_call"},
		{"inline_kwargs", completionEvent(map[string]interface{}{"content": `transfer_call(destination="Fixture", reason='Synthetic')`}, "") + "data: [DONE]\n\n", "transfer_call"},
		{"inline_sentence_in_argument", completionEvent(map[string]interface{}{"content": `take_message(recipient="Fixture", content="First sentence. `}, "") + completionEvent(map[string]interface{}{"content": `Second sentence.")`}, "stop"), "take_message"},
		{"inline_json", completionEvent(map[string]interface{}{"content": `prefix {"name":"end_call","arguments":{}}`}, "") + "data: [DONE]\n\n", "end_call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, calls, err := runCompletionFixture(t, tc.body)
			if err != nil || len(calls) != 1 || calls[0].Name != tc.want || calls[0].Arguments == nil {
				t.Fatalf("valid tool lost: %#v %v", calls, err)
			}
		})
	}
}

func TestMixedInlineThenIncompleteRawBatchIsRejected(t *testing.T) {
	for _, raw := range []string{"end_call(). <tool_call", `end_call(). {"name":"take_message","arguments":`, "end_call(). take_message("} {
		_, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": raw}, "")+"data: [DONE]\n\n")
		if err == nil || len(calls) > 0 {
			t.Fatalf("mixed incomplete batch escaped: %q %#v %v", raw, calls, err)
		}
	}
}

func TestMalformedFragmentsBetweenValidCompatibilityToolsRejectBatch(t *testing.T) {
	valid := `<tool_call>{"name":"end_call","arguments":{}}</tool_call>`
	for _, raw := range []string{
		valid + `<tool_call broken` + valid,
		valid + ` take_message(`,
		valid + ` take_message(content="unfinished)`,
		`{"name":"take_message","arguments":` + valid,
		valid + ` {"name":"take_message","arguments":`,
		`take_message( {"name":"end_call","arguments":{}}`,
		valid + ` {"name":"end_call","arguments":{}} take_message(`,
	} {
		_, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": raw}, "stop"))
		if err == nil || len(calls) > 0 {
			t.Errorf("malformed fragment escaped: %q calls=%#v err=%v", raw, calls, err)
		}
	}
}

func TestValidMixedCompatibilityToolsRetainWholeBatch(t *testing.T) {
	xml := `<tool_call>{"name":"end_call","arguments":{}}</tool_call>`
	for _, raw := range []string{
		`accept_call() ` + xml,
		xml + ` accept_call()`,
		xml + ` {"name":"accept_call","arguments":{}}`,
		`accept_call() {"name":"end_call","arguments":{}}`,
	} {
		_, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": raw}, "stop"))
		if err != nil || len(calls) != 2 {
			t.Errorf("valid mixed batch lost: %q calls=%#v err=%v", raw, calls, err)
		}
	}
	text, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": "A normal reply without tools."}, "stop"))
	if err != nil || text != "A normal reply without tools." || len(calls) != 0 {
		t.Fatalf("normal speech changed: %q %#v %v", text, calls, err)
	}
}

func TestInvalidCompatibilityToolsCannotBypassValidation(t *testing.T) {
	for _, raw := range []string{
		`end_call({)`, `end_call(null)`, `end_call(message="unfinished)`,
		`{"name":"end_call","arguments":null}`,
		`<tool_call>{"name":"end_call","arguments":null}</tool_call>`,
		`<tool_call>{"name":"end_call","arguments":{}}</tool_call><tool_call>{`,
		`<tool_call>{"name":"end_call","arguments":{}}</tool_call><tool_call`,
		`<tool_call><function=end_call></tool_call>`,
		`<tool_call><function=end_call>invalid argument data</function></tool_call>`,
	} {
		_, calls, err := runCompletionFixture(t, completionEvent(map[string]interface{}{"content": raw}, "")+"data: [DONE]\n\n")
		if err == nil || len(calls) != 0 {
			t.Fatalf("invalid compatibility tool accepted: %q %#v %v", raw, calls, err)
		}
	}
}

func TestToolBatchWaitsForCompletionAndCancellationDropsIt(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_%t", cancelTurn), func(t *testing.T) {
			pending, finish := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, completionEvent(completionTool(0, "end_call", "{}"), ""))
				w.(http.Flusher).Flush()
				close(pending)
				select {
				case <-finish:
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			client := &Client{URL: server.URL, Model: "fixture", streamHTTP: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sentences, calls, errs := client.StreamResponse(ctx, "", nil, "", "", nil, "Synthetic")
			select {
			case <-pending:
			case <-ctx.Done():
				t.Fatal("provider did not receive the synthetic request")
			}
			if len(calls) != 0 {
				t.Fatal("tools released before the provider completed")
			}
			if cancelTurn {
				cancel()
			} else {
				close(finish)
			}
			for range sentences {
			}
			var names []string
			for batch := range calls {
				for _, call := range batch {
					names = append(names, call.Name)
				}
			}
			err := <-errs
			if cancelTurn && (err == nil || len(names) != 0) {
				t.Fatalf("cancelled batch escaped: %v %v", names, err)
			}
			if !cancelTurn && (err != nil || !reflect.DeepEqual(names, []string{"end_call"})) {
				t.Fatalf("completed batch lost: %v %v", names, err)
			}
		})
	}
}

func TestBatchCompletionsAlsoRejectInvalidTools(t *testing.T) {
	for _, tc := range []struct{ name, arguments, finish string }{
		{"malformed_arguments", "{", "tool_calls"},
		{"null_arguments", "null", "tool_calls"},
		{"truncated_completion", "{}", "length"},
		{"valid", "{}", "tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
					"finish_reason": tc.finish,
					"message": map[string]interface{}{"content": "Partial reply", "tool_calls": []interface{}{
						map[string]interface{}{"id": "valid", "function": map[string]string{"name": "take_message", "arguments": `{"recipient":"Fixture","content":"Synthetic"}`}},
						map[string]interface{}{"id": "tested", "function": map[string]string{"name": "end_call", "arguments": tc.arguments}},
					}},
				}}})
			}))
			defer server.Close()
			client := &Client{URL: server.URL, http: server.Client()}
			_, calls, err := client.chatCompletions(context.Background(), map[string]interface{}{"model": "fixture"})
			if tc.name == "valid" {
				if err != nil || len(calls) != 2 {
					t.Fatalf("valid batch lost: %#v %v", calls, err)
				}
			} else if err == nil || len(calls) != 0 {
				t.Fatalf("invalid batch escaped: %#v %v", calls, err)
			}
		})
	}
}
