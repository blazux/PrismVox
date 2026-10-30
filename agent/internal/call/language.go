package call

import (
	"context"
	"encoding/json"
	"fmt"
	"phoneagent/internal/config"
	"phoneagent/internal/locale"
	"reflect"
	"strings"
	"sync"
)

type callLocale struct {
	sync.RWMutex
	primary, current, signature        string
	automatic                          bool
	source                             map[string]string
	translations                       map[string]map[string]string
	preparing                          map[string]bool
	callerText, hintText, hintLanguage string
	outputProtected                    bool
	cfg                                *config.Config
	warmed                             map[string]bool
}

func (h *Handler) initLanguage(ctx context.Context, cfg *config.Config, primary string, automatic bool) {
	primary = locale.Normalize(primary)
	if primary == "" {
		primary = "en"
	}
	source := map[string]string{}
	for _, key := range locale.Keys() {
		source[key] = cfg.Get(key)
	}
	state := &callLocale{primary: primary, current: primary, automatic: automatic, source: source, signature: locale.Signature(source), translations: map[string]map[string]string{}, preparing: map[string]bool{}}
	if h.pool != nil {
		rows, err := h.pool.Query(ctx, "SELECT language,phrases FROM voice_phrase_translations WHERE source_hash=$1", state.signature)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var language string
				var data []byte
				if rows.Scan(&language, &data) == nil {
					var phrases map[string]string
					if json.Unmarshal(data, &phrases) == nil && locale.ValidTranslation(source, phrases) {
						state.translations[language] = phrases
					}
				}
			}
		}
	}
	for _, base := range []string{"en", "fr"} {
		if reflect.DeepEqual(source, locale.Defaults(base)) {
			for _, target := range []string{"en", "fr"} {
				state.translations[target] = locale.Defaults(target)
			}
			break
		}
	}
	state.cfg = cfg
	state.warmed = map[string]bool{primary: true}
	h.language = state
	if h.sttClient != nil {
		h.sttClient.OnLanguage = func(code, text string) { h.observeCallerHint(ctx, code, text) }
	}
}

func (h *Handler) currentLanguage() string {
	if h.language == nil {
		return "en"
	}
	h.language.RLock()
	defer h.language.RUnlock()
	return h.language.current
}
func (h *Handler) primaryLanguage() string {
	if h.language == nil {
		return "en"
	}
	return h.language.primary
}
func (h *Handler) setLanguage(ctx context.Context, code string) {
	state := h.language
	if state == nil || !state.automatic {
		return
	}
	code = locale.Normalize(code)
	if code == "" {
		return
	}
	state.Lock()
	state.current = code
	state.Unlock()
	h.prepareLanguage(ctx, code)
}
func (h *Handler) observeSpeech(ctx context.Context, text string) {
	state := h.language
	if state == nil {
		return
	}
	code := locale.Detect(text)
	state.Lock()
	state.outputProtected = true
	if state.automatic && code != "" {
		state.current = code
	}
	state.Unlock()
	if state.automatic && code != "" {
		h.prepareLanguage(ctx, code)
	}
}
func (h *Handler) observeCallerHint(ctx context.Context, code, text string) {
	state := h.language
	if state == nil {
		return
	}
	code = locale.Normalize(code)
	if code == "" {
		return
	}
	state.Lock()
	state.hintText = text
	state.hintLanguage = code
	apply := state.automatic && state.callerText == text && !state.outputProtected
	if apply {
		state.current = code
	}
	state.Unlock()
	if apply {
		h.prepareLanguage(ctx, code)
	}
}
func (h *Handler) acceptCallerText(ctx context.Context, text string) {
	state := h.language
	if state == nil {
		return
	}
	code := locale.Detect(text)
	state.Lock()
	state.callerText = text
	state.outputProtected = false
	if state.hintText == text {
		code = state.hintLanguage
	}
	if state.automatic && code != "" {
		state.current = code
	}
	state.Unlock()
	if state.automatic && code != "" {
		h.prepareLanguage(ctx, code)
	}
}
func (h *Handler) phrase(cfg *config.Config, key string) string {
	return h.phraseIn(cfg, key, h.currentLanguage())
}
func (h *Handler) phraseIn(cfg *config.Config, key, code string) string {
	state := h.language
	if state == nil {
		return cfg.Get(key)
	}
	state.RLock()
	defer state.RUnlock()
	if text := state.translations[code][key]; text != "" {
		return text
	}
	if code == state.primary {
		if !cfg.Configured[key] {
			if text := locale.Default(code, key); text != "" {
				return text
			}
		}
		return cfg.Get(key)
	}
	phrasePreparation.Lock()
	shared := phrasePreparation.ready[state.signature+":"+code][key]
	phrasePreparation.Unlock()
	if shared != "" {
		return shared
	}
	// Reviewed generic phrases remain usable during a model outage. Unsupported
	// cold languages stay silent instead of playing a phrase in another language.
	return locale.Default(code, key)
}
func renderPhrase(text string, values ...string) string {
	for _, value := range values {
		text = strings.Replace(text, "%s", value, 1)
	}
	return text
}
func (h *Handler) languageInstruction() string {
	state := h.language
	if state != nil && !state.automatic {
		return "Use the configured call language: " + state.primary + "."
	}
	return "Use the other person's language and follow explicit requests to switch language. Keep the established language for short acknowledgements or names. Address unknown people politely and formally; keep the assistant's established register for recognized callers."
}

var phrasePreparation = struct {
	sync.Mutex
	active map[string]bool
	ready  map[string]map[string]string
}{active: map[string]bool{}, ready: map[string]map[string]string{}}

func (h *Handler) prepareLanguage(ctx context.Context, code string) {
	state := h.language
	if state == nil {
		return
	}
	state.Lock()
	warm := !state.warmed[code]
	state.warmed[code] = true
	state.Unlock()
	if warm && h.ttsClient != nil {
		go h.prepareFiller(ctx, state.cfg)
	}
	if h.llmClient == nil {
		return
	}
	state.Lock()
	if code == state.primary || state.translations[code] != nil || state.preparing[code] {
		state.Unlock()
		return
	}
	state.preparing[code] = true
	source := state.source
	signature := state.signature
	state.Unlock()
	key := signature + ":" + code
	phrasePreparation.Lock()
	if phrasePreparation.active[key] {
		phrasePreparation.Unlock()
		return
	}
	phrasePreparation.active[key] = true
	phrasePreparation.Unlock()
	go func() {
		defer func() { phrasePreparation.Lock(); delete(phrasePreparation.active, key); phrasePreparation.Unlock() }()
		phrases, err := h.llmClient.TranslatePhrases(ctx, source, code)
		if err != nil || !locale.ValidTranslation(source, phrases) {
			return
		}
		phrasePreparation.Lock()
		if len(phrasePreparation.ready) >= 128 {
			for previous := range phrasePreparation.ready {
				delete(phrasePreparation.ready, previous)
				break
			}
		}
		phrasePreparation.ready[key] = phrases
		phrasePreparation.Unlock()
		if h.pool != nil {
			data, _ := json.Marshal(phrases)
			_, _ = h.pool.Exec(ctx, "INSERT INTO voice_phrase_translations(source_hash,language,phrases) VALUES($1,$2,$3) ON CONFLICT(source_hash,language) DO UPDATE SET phrases=EXCLUDED.phrases", signature, code, data)
		}
		state.Lock()
		state.translations[code] = phrases
		state.Unlock()
		if h.ttsClient != nil && h.currentLanguage() == code {
			go h.prepareFiller(ctx, state.cfg)
		}
	}()
}

func (h *Handler) localWaitingClips() []waitingClip {
	code := h.currentLanguage()
	var clips []waitingClip
	for _, clip := range h.waitCues {
		if clip.Neutral || locale.Normalize(clip.Language) == code {
			clips = append(clips, clip)
		}
	}
	return clips
}
func (h *Handler) languageContext(cfg *config.Config) string {
	return fmt.Sprintf("\n\n## Conversation language\n%s Start in %s when there is no dialogue yet. Language hints are guidance; an explicit request to switch takes priority.", h.languageInstruction(), h.currentLanguage())
}
