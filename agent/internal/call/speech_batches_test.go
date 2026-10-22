package call

import (
	"context"
	"strings"
	"testing"
	"time"
)

func collectSpeech(in <-chan speechBatch) []string {
	var text []string
	for batch := range in {
		text = append(text, batch.text)
	}
	return text
}

func TestShortSpeechReplyKeepsBothSentences(t *testing.T) {
	in := make(chan string, 3)
	in <- " Bonjour. "
	in <- ""
	in <- "Comment puis-je vous aider ?"
	close(in)
	start := time.Now()
	got := collectSpeech(groupSpeech(context.Background(), in, time.Second))
	if len(got) != 1 || got[0] != "Bonjour. Comment puis-je vous aider ?" {
		t.Fatalf("short reply split or changed: %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("closed reply waited for grouping timeout")
	}
}

func TestSpeechStartsBeforeSlowModelFinishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan string, 1)
	in <- "La première phrase est prête."
	out := groupSpeech(ctx, in, 20*time.Millisecond)
	select {
	case batch := <-out:
		if batch.text != "La première phrase est prête." || batch.firstSentenceAt.IsZero() {
			t.Fatal(batch)
		}
	case <-time.After(time.Second):
		t.Fatal("first audio text blocked on an unfinished model turn")
	}
	in <- "La suite arrive ensuite."
	close(in)
	rest := collectSpeech(out)
	if len(rest) != 1 || rest[0] != "La suite arrive ensuite." {
		t.Fatalf("lost remainder: %v", rest)
	}
}

func TestSpeechGroupingBoundsUseCharactersAndPreserveLongReplies(t *testing.T) {
	in := make(chan string, 3)
	first := strings.Repeat("é", 199) + "."
	second := strings.Repeat("à", 190) + "."
	third := "Et voici la dernière phrase."
	in <- first
	in <- second
	in <- third
	close(in)
	got := collectSpeech(groupSpeech(context.Background(), in, time.Second))
	if len(got) != 2 || got[0] != first+" "+second || got[1] != third {
		t.Fatalf("wrong Unicode grouping or lost content: %v", got)
	}
}

func TestLegacySpeechDoesNotWaitOrGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan string, 2)
	in <- "Une phrase."
	out := groupSpeech(ctx, in, 0)
	select {
	case batch := <-out:
		if batch.text != "Une phrase." {
			t.Fatal(batch)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy service now waits for full reply")
	}
	in <- "Une autre."
	close(in)
	got := collectSpeech(out)
	if len(got) != 1 || got[0] != "Une autre." {
		t.Fatal(got)
	}
}

func TestSpeechGroupingStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan string, 1)
	in <- "Ne pas prononcer."
	out := groupSpeech(ctx, in, time.Hour)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("cancelled speech was emitted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled grouping goroutine leaked")
	}
}
