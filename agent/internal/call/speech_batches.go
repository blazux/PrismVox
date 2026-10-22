package call

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	shortSpeechChars = 400
	// Wait only after the first complete sentence, not before model generation.
	// Fast short replies finish in this window; slow replies can still start.
	speechGroupWait = 200 * time.Millisecond
)

type speechBatch struct {
	text            string
	firstSentenceAt time.Time
}

// groupSpeech combines complete sentences without waiting indefinitely for the
// end of a turn. A zero wait preserves sentence-by-sentence HTTP service behavior.
func groupSpeech(ctx context.Context, in <-chan string, wait time.Duration) <-chan speechBatch {
	out := make(chan speechBatch, 1)
	go func() {
		defer close(out)
		var text string
		var first time.Time
		var timer *time.Timer
		var deadline <-chan time.Time
		stopTimer := func() {
			if timer != nil {
				timer.Stop()
			}
			deadline = nil
		}
		defer func() { stopTimer() }()
		flush := func() bool {
			if text == "" {
				return true
			}
			if ctx.Err() != nil {
				return false
			}
			select {
			case out <- speechBatch{text: text, firstSentenceAt: first}:
				text = ""
				stopTimer()
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline:
				if !flush() {
					return
				}
			case raw, ok := <-in:
				if !ok {
					flush()
					return
				}
				sent := strings.TrimSpace(StripGemmaThinking(raw))
				if sent == "" {
					continue
				}
				received := time.Now()
				if text != "" && utf8.RuneCountInString(text)+1+utf8.RuneCountInString(sent) > shortSpeechChars {
					if !flush() {
						return
					}
				}
				if text == "" {
					first = received
					if wait > 0 {
						timer = time.NewTimer(wait)
						deadline = timer.C
					}
				} else {
					text += " "
				}
				text += sent
				if wait <= 0 || utf8.RuneCountInString(text) >= shortSpeechChars {
					if !flush() {
						return
					}
				}
			}
		}
	}()
	return out
}
