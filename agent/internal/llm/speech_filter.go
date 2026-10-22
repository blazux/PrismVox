package llm

import "strings"

// speechFilter keeps partial markers between chunks, including inside reasoning.
// Providers must send reasoning in tagged blocks or structured reasoning fields;
// unmarked reasoning is indistinguishable from a spoken answer.
type speechFilter struct {
	pending  string
	thinking bool
}

func (f *speechFilter) Feed(s string) string {
	f.pending += s
	var out strings.Builder
	for f.pending != "" {
		marker := "<think>"
		if f.thinking {
			marker = "</think>"
		}
		i := strings.Index(f.pending, marker)
		if !f.thinking {
			if end := strings.Index(f.pending, "</think>"); end >= 0 && (i < 0 || end < i) {
				// Some gateways omit the opening tag. Drop unspoken reasoning
				// from this chunk; unmarked text from prior chunks cannot be recalled.
				out.Reset()
				f.pending = f.pending[end+len("</think>"):]
				continue
			}
		}
		if i >= 0 {
			if !f.thinking {
				out.WriteString(f.pending[:i])
			}
			f.pending = f.pending[i+len(marker):]
			f.thinking = !f.thinking
			continue
		}
		keep := partialMarkerTail(f.pending, marker)
		if !f.thinking {
			if n := partialMarkerTail(f.pending, "</think>"); n > keep {
				keep = n
			}
		}
		if !f.thinking {
			out.WriteString(f.pending[:len(f.pending)-keep])
		}
		f.pending = f.pending[len(f.pending)-keep:]
		break
	}
	return out.String()
}
