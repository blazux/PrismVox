package llm

import "context"

// ToolTrace is an observation of execution in Prism, never a request for Vox to
// run that tool. Results are paired by ID; Prism does not repeat the tool name.
type ToolTrace struct {
	Type    string
	ID      string
	Tool    string
	Input   map[string]interface{}
	Output  string
	IsError bool
}

type toolTraceKey struct{}

func WithToolTrace(ctx context.Context, observe func(ToolTrace)) context.Context {
	return context.WithValue(ctx, toolTraceKey{}, observe)
}

func observeToolTrace(ctx context.Context, trace ToolTrace) {
	if observe, ok := ctx.Value(toolTraceKey{}).(func(ToolTrace)); ok {
		observe(trace)
	}
}
