package call

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"phoneagent/internal/llm"
	"phoneagent/internal/rtp"
)

type waitingClip struct {
	File       string `json:"file"`
	Text       string `json:"text"`
	LookupOnly bool   `json:"lookup_only,omitempty"`
	Language   string `json:"language,omitempty"`
	Neutral    bool   `json:"neutral,omitempty"`
	wav        []byte
	key        string
}

func loadWaitingClips(dir string) ([]waitingClip, error) {
	f, err := os.Open(filepath.Join(dir, "clips.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	var clips []waitingClip
	if err := json.Unmarshal(data, &clips); err != nil {
		return nil, err
	}
	if len(clips) == 0 || len(clips) > 32 {
		return nil, fmt.Errorf("waiting clips must contain 1 to 32 entries")
	}
	neutral := false
	seen := make(map[string]bool)
	for i := range clips {
		clip := &clips[i]
		// Legacy packs were produced in French. Only language-free vocalizations
		// can be shared across languages without a declared pack language.
		if clip.Language == "" && !clip.Neutral {
			switch strings.Trim(strings.ToLower(clip.Text), " .…!?") {
			case "hmm", "hmmm", "mmh", "mmmh":
				clip.Neutral = true
			default:
				clip.Language = "fr"
			}
		}
		if !filepath.IsLocal(clip.File) || filepath.Base(clip.File) != clip.File || strings.TrimSpace(clip.Text) == "" || seen[clip.File] {
			return nil, fmt.Errorf("invalid or duplicate waiting clip")
		}
		seen[clip.File] = true
		clip.key = filepath.Join(dir, clip.File)
		info, err := os.Stat(clip.key)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
			return nil, fmt.Errorf("waiting clip unavailable or too large: %s", clip.File)
		}
		clip.wav, err = os.ReadFile(clip.key)
		if err != nil {
			return nil, err
		}
		pcm, err := wavToEgressPCM(clip.wav)
		if err != nil || len(pcm) == 0 || len(pcm) > 2*rtp.OutSampleRate {
			return nil, fmt.Errorf("waiting clip must be valid audio of at most 2 seconds: %s", clip.File)
		}
		neutral = neutral || !clip.LookupOnly
	}
	if !neutral {
		return nil, fmt.Errorf("waiting clips require a neutral alternative")
	}
	return clips, nil
}

// History contains only clip identifiers, shared across calls, never caller data.
// Cached clips may play concurrently; selection holds this lock only briefly.
type waitingRotation struct {
	sync.Mutex
	recent []string
}

var sharedWaitingRotation waitingRotation

func (r *waitingRotation) choose(clips []waitingClip, lookup bool) waitingClip {
	r.Lock()
	defer r.Unlock()
	var candidates []waitingClip
	for window := len(r.recent); window >= 0; window-- {
		for _, clip := range clips {
			if clip.LookupOnly && !lookup {
				continue
			}
			repeated := false
			for _, key := range r.recent[len(r.recent)-window:] {
				repeated = repeated || key == clip.key
			}
			if !repeated {
				candidates = append(candidates, clip)
			}
		}
		if len(candidates) > 0 {
			break
		}
	}
	if len(candidates) == 0 {
		return waitingClip{}
	}
	clip := candidates[rand.Intn(len(candidates))]
	r.recent = append(r.recent, clip.key)
	if len(r.recent) > 3 {
		r.recent = r.recent[len(r.recent)-3:]
	}
	return clip
}

func (c *waitingCue) clip() playChunk {
	clips := c.clips
	if c.selectClips != nil {
		clips = c.selectClips()
	}
	if len(clips) > 0 {
		clip := sharedWaitingRotation.choose(clips, c.lookups.Load() > 0)
		if len(clip.wav) > 0 {
			return playChunk{wav: clip.wav, text: clip.Text}
		}
	}
	return playChunk{wav: c.wav, text: "Hmm…"}
}

// Be conservative about consultation wording: writes and unknown operations use
// neutral clips. These are observations of tool execution, not model decisions.
func lookupCueEligible(name string, args map[string]interface{}) bool {
	if !toolCueEligible(name) {
		return false
	}
	if action, _ := args["action"].(string); action != "" {
		switch action {
		case "list", "get", "read", "search", "find", "fetch", "query", "status", "history":
			return true
		default:
			return false
		}
	}
	for _, prefix := range []string{"search_", "read_", "list_", "get_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return strings.HasSuffix(name, "_search")
}

func (c *waitingCue) beginLookup(name string, args map[string]interface{}) func() {
	if c == nil || !lookupCueEligible(name, args) {
		return func() {}
	}
	c.lookups.Add(1)
	return func() { c.lookups.Add(-1) }
}

func (c *waitingCue) observeTool(event llm.ToolTrace) {
	if c == nil || event.ID == "" {
		return
	}
	c.lookupMu.Lock()
	defer c.lookupMu.Unlock()
	if event.Type == "tool_use" && lookupCueEligible(event.Tool, event.Input) {
		if c.remoteLookups == nil {
			c.remoteLookups = make(map[string]bool)
		}
		if _, seen := c.remoteLookups[event.ID]; !seen {
			c.remoteLookups[event.ID] = true
			c.lookups.Add(1)
		}
	} else if event.Type == "tool_result" && c.remoteLookups[event.ID] {
		c.remoteLookups[event.ID] = false
		c.lookups.Add(-1)
	}
}
