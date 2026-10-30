package call

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"phoneagent/internal/privacy"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"phoneagent/internal/ari"
	"phoneagent/internal/audio"
	"phoneagent/internal/config"
	"phoneagent/internal/contacts"
	"phoneagent/internal/db"
	"phoneagent/internal/embeddings"
	"phoneagent/internal/llm"
	"phoneagent/internal/locale"
	"phoneagent/internal/mcp"
	"phoneagent/internal/rag"
	"phoneagent/internal/rtp"
	"phoneagent/internal/stt"
	"phoneagent/internal/tts"
	"phoneagent/internal/vad"
	"phoneagent/internal/voice"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxHistory  = 40 // compact older exchanges above this live-window threshold
	playTimeout = 60 * time.Second
	// drainStallTimeout bounds waitCallerDrained: if the outbound RTP buffer stops
	// draining this long (no remote address learned → nothing being sent), give up
	// rather than wedge the call.
	drainStallTimeout = 3 * time.Second
	transferWait      = 30 * time.Second
	recipientWait     = 20 * time.Second
	greetingDelay     = 1 * time.Second

	// silenceTimeout is how long the agent waits for the caller to speak before
	// re-prompting; after maxSilencePrompts unanswered re-prompts it hangs up.
	silenceTimeout    = 15 * time.Second
	maxSilencePrompts = 2

	// repromptTimeout bounds the LLM call that phrases the silence re-prompt.
	// Generous because nobody is speaking, but finite: past this we fall back to
	// the fixed phrase rather than leave the line silent.
	repromptTimeout = 5 * time.Second

	// minSpeechForReprompt: when STT returns nothing, only ask the caller to repeat
	// if the unintelligible segment was at least this long — shorter blips are noise
	// or filtered hallucinations, not missed speech, so we stay silent.
	minSpeechForReprompt = 1.0 // seconds
	maxEmptyReprompts    = 2

	// fillerDelay: if a TTS synthesis takes longer than this (e.g. the service is
	// regenerating a hallucinated take), play a short filler to bridge the silence.
	fillerDelay = 1500 * time.Millisecond

	// recipientTurns bounds the announcement dialog with an attended-transfer
	// recipient so the caller never waits in MoH indefinitely. Default only —
	// overridable per deployment via the transfer_dialog_turns config key.
	recipientTurns = 3

	// announceGenTimeout bounds the LLM generation of the attended-transfer
	// announcement (run during the recipient's ring). announceGrace is how long
	// turn 0 will still wait for it once the recipient answered, before falling
	// back to the configured phrase_announce.
	announceGenTimeout = 8 * time.Second
	announceGrace      = 1500 * time.Millisecond

	// taskRetryDelay is how long an outbound task waits before its next attempt
	// when the call connected but produced no usable exchange (e.g. voicemail).
	taskRetryDelay = 5 * time.Minute
)

var (
	soundsDir     string
	ariApp        string
	appHostIP     string
	bargeInFrames int
	bargeVADMode  int
	ragTopK       int
)

func init() {
	soundsDir = envDefault("SOUNDS_DIR", "/sounds")
	ariApp = envDefault("ARI_APP", "phoneagent")

	// Barge-in sustain threshold: how much *voiced* caller speech (ms) must accrue
	// before interrupting the agent's playback. Set above background-noise bursts so
	// the agent isn't cut off by ambient noise; brief backchannels are filtered too.
	// 20ms per VAD frame. Lower BARGEIN_MS = more eager.
	bargeInMs := 800
	if v := os.Getenv("BARGEIN_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			bargeInMs = n
		}
	}
	bargeInFrames = bargeInMs / 20

	// Aggressiveness of the barge-in VAD (0–3, stricter at 3). Higher rejects more
	// background noise than the normal-capture default so ambient noise doesn't
	// falsely interrupt the agent.
	bargeVADMode = 3
	if v := os.Getenv("BARGEIN_VAD_MODE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 3 {
			bargeVADMode = n
		}
	}

	// Number of knowledge-base chunks injected per turn by semantic RAG search.
	ragTopK = 5
	if v := os.Getenv("RAG_TOP_K"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ragTopK = n
		}
	}

	host := envDefault("APP_HOST", "agent")
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		privacy.Logf("call: cannot resolve APP_HOST=%q, using directly: %v", host, err)
		appHostIP = host
	} else {
		appHostIP = addrs[0]
		privacy.Logf("call: APP_HOST=%q → %s", host, appHostIP)
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// attendedTransferReq holds parameters for a pending attended transfer.
type attendedTransferReq struct {
	Destination string // resolved extension
	ContactName string // directory name, used in spoken messages
	CallerName  string
	Reason      string
}

// Handler manages the full lifecycle of a single phone call.
type Handler struct {
	channelID    string
	ari          *ari.Client
	pool         *pgxpool.Pool
	callerNumber string
	// callerName is the display name Prism returned for that number, empty for a
	// stranger. It greets them; it grants nothing.
	callerName string
	// transferDir is who this call may be transferred to, fetched once from Prism
	// at pickup. Empty when Vox runs standalone — its own contacts table is then
	// the directory, as it always was.
	transferDir []contacts.Contact

	// conversation state
	history          []llm.Message
	historySummary   string
	memoryRetryAfter time.Time
	memoryPending    chan historyCompaction
	turns            []map[string]string // full transcript: [{role,text}, ...]
	toolsUsed        []map[string]interface{}
	ragUsed          bool
	ragEnabled       bool // exposes the search_knowledge_base tool when true
	groupShortSpeech bool // direct synthesis can preserve prosody across short replies

	// fillerClips holds short phrases by language for slow TTS takes.
	fillerClips map[string]playChunk
	waitCueWav  []byte
	waitCues    []waitingClip
	responseCue *waitingCue
	language    *callLocale
	fillerMu    sync.Mutex
	logID       int64
	message     *string

	// transfer state
	transferDest     string
	attendedTransfer *attendedTransferReq

	// endCall is set when the LLM calls the end_call tool: the agent says
	// goodbye and hangs up instead of waiting for the next caller turn.
	endCall bool

	// task is non-nil for agent-originated outbound calls: the conversation is
	// driven by task.Mission and the outcome is written back to call_tasks.
	task *db.CallTask

	// ARI media state
	bridgeID    string
	extChanID   string
	snoopChanID string // Snoop(spy=in) tap: caller mic only, no playback echo
	rtpReceiver *rtp.Receiver

	// playback futures: pbID → signal channel
	pbFutures map[string]chan struct{}
	pbMu      sync.Mutex

	// attended transfer events (buffered so they never block sender)
	transferStart chan string
	transferEnd   chan string

	// lifecycle
	running  bool
	done     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex

	// service clients
	llmClient *llm.Client
	ttsClient *tts.Client
	sttClient *stt.Client
	embClient *embeddings.Client
	mcpMgr    *mcp.Manager
}

// NewHandler creates a Handler for a new incoming call.
func NewHandler(
	channelID string,
	ariClient *ari.Client,
	pool *pgxpool.Pool,
	callerNumber string,
) *Handler {
	h := &Handler{
		channelID:     channelID,
		ari:           ariClient,
		pool:          pool,
		callerNumber:  callerNumber,
		pbFutures:     make(map[string]chan struct{}),
		transferStart: make(chan string, 1),
		transferEnd:   make(chan string, 1),
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
		llmClient:     llm.New(),
		ttsClient:     tts.New(),
		sttClient:     stt.New(),
		embClient:     embeddings.New(),
		mcpMgr:        mcp.NewManager(),
	}
	// Tell Prism who is on the line (dock). Until it can identify the
	// number, it treats the caller as a guest — never as the token's admin identity.
	h.llmClient.Caller = callerNumber
	return h
}

// NewOutboundHandler creates a Handler for an agent-originated outbound call.
// The channel is already answered when it enters the Stasis app.
func NewOutboundHandler(
	channelID string,
	ariClient *ari.Client,
	pool *pgxpool.Pool,
	task *db.CallTask,
) *Handler {
	h := NewHandler(channelID, ariClient, pool, task.PhoneNumber)
	h.task = task
	// Outbound calls are mission-driven (their own prompt, no directory/RAG); they
	// keep the local SGLang brain even when Vox is docked to Prism (inbound-only).
	h.llmClient.PrismURL = ""
	return h
}

// Done is closed when the call lifecycle (Run) has fully finished.
func (h *Handler) Done() <-chan struct{} {
	return h.done
}

// Stop requests termination; Run alone owns teardown and persistence. It is safe
// for duplicate ARI events, including a hangup received before Run starts.
func (h *Handler) Stop() {
	h.stopOnce.Do(func() { close(h.stop) })
}

// ── ARI event callbacks ────────────────────────────────────────────────────────

// OnPlaybackFinished resolves the pending playback future for the given ID.
func (h *Handler) OnPlaybackFinished(pbID string) {
	h.pbMu.Lock()
	ch, ok := h.pbFutures[pbID]
	if ok {
		delete(h.pbFutures, pbID)
	}
	h.pbMu.Unlock()
	if ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// OnTransferChannelStart is called when the outbound attended-transfer leg answers.
func (h *Handler) OnTransferChannelStart(channelID string) {
	select {
	case h.transferStart <- channelID:
	default:
	}
}

// OnTransferChannelEnd is called when the attended-transfer channel hangs up.
func (h *Handler) OnTransferChannelEnd(channelID string) {
	select {
	case h.transferEnd <- channelID:
	default:
	}
	// Unblock any in-flight playback waits
	h.pbMu.Lock()
	for id, ch := range h.pbFutures {
		delete(h.pbFutures, id)
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.pbMu.Unlock()
}

// ── Main lifecycle ─────────────────────────────────────────────────────────────

// Run executes the full call lifecycle (answer → greeting → capture loop → cleanup).
func (h *Handler) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-h.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	h.mu.Lock()
	h.running = true
	h.mu.Unlock()

	defer func() {
		cancel()
		h.mcpMgr.CloseAll()
		// The call is cancelled, but its final record still needs to be saved.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		h.cleanup(cleanupCtx)
		close(h.done)
	}()
	select {
	case <-h.stop:
		return
	default:
	}

	// Insert call log row
	if h.pool != nil {
		direction := "inbound"
		if h.task != nil {
			direction = "outbound"
		}
		id, err := db.InsertCallLog(ctx, h.pool, h.channelID, h.callerNumber, direction)
		if err != nil {
			log.Printf("[%s] InsertCallLog error: %v", h.channelID, err)
		} else {
			h.logID = id
			if h.task != nil {
				if err := db.SetCallAudience(ctx, h.pool, id, h.task.Audience, h.task.PrismInstance, h.task.OwnerID); err != nil {
					log.Printf("[%s] task audience: %v", h.channelID, err)
				}
			}
		}
	}

	cfg, err := config.Load(ctx, h.pool)
	if err != nil {
		log.Printf("[%s] Config load error: %v", h.channelID, err)
		cfg = &config.Config{
			SystemPrompt: "You are a helpful AI assistant.",
			Greeting:     "Hello, how can I help?",
		}
	}

	settings, voiceErr := voice.Load(cfg.Raw)
	if voiceErr != nil {
		log.Printf("[%s] Voice configuration unavailable: %v", h.channelID, voiceErr)
		return
	}
	h.ttsClient.Configure(settings)
	h.initLanguage(ctx, cfg, settings.Language, settings.AutoLanguage)
	h.loadWaitCue()
	h.groupShortSpeech = settings.TTSProvider == "elevenlabs"
	h.sttClient.Configure(settings)
	h.llmClient.SummaryLanguage = settings.Language
	if value := cfg.Get("voice_summary_language"); value != "" {
		h.llmClient.SummaryLanguage = value
	} else if value := os.Getenv("VOICE_SUMMARY_LANGUAGE"); value != "" {
		h.llmClient.SummaryLanguage = value
	}
	h.sttClient.Hotwords = cfg.Get("whisper_hotwords")

	// Apply the DB-managed dictionary (UI → Dictionnaire) to this call's STT/TTS
	// clients. Empty values let each service fall back to its env default.
	h.ttsClient.SpellWords = cfg.Get("tts_spell_words")
	h.ttsClient.SpellExceptions = cfg.Get("tts_spell_exceptions")

	// One Vox model for all local conversations; recognized callers route to Prism.

	if err := h.llmClient.Configure(cfg.Raw); err != nil {
		log.Printf("[%s] Model configuration unavailable: %v", h.channelID, err)
		return
	}

	// RAG is agentic: when enabled, the model gets a search_knowledge_base tool
	// (inbound only — outbound calls are mission-driven, no KB).
	h.ragEnabled = cfg.RAGEnabled && h.task == nil
	h.embClient.Configure(cfg.Raw)

	if h.pool != nil && cfg.MCPEnabled {
		if err := h.mcpMgr.ConnectAll(ctx, h.pool); err != nil {
			log.Printf("[%s] MCP connect error: %v", h.channelID, err)
		}
	}

	if h.task == nil {
		// Answer the inbound call
		if _, status, err := h.ari.Post("/channels/"+h.channelID+"/answer", nil); err != nil || status >= 400 {
			log.Printf("[%s] Answer failed (status=%d): %v", h.channelID, status, err)
			return
		}
		log.Printf("[%s] Call answered", h.channelID)
	} else {
		// Outbound: the channel entered Stasis already answered.
		log.Printf("[%s] Outbound call connected (task %d)", h.channelID, h.task.ID)
	}

	// Brief wait for SIP audio path to establish
	time.Sleep(greetingDelay)

	// Provision the media path (snoop + external media + bridge) before the greeting
	// so its one-time setup doesn't glitch the audio right after the greeting.
	if !h.setupMedia(ctx) {
		return
	}

	// Who is on the line? This has to be settled here, before a word is spoken:
	// the answer picks the greeting AND the brain, and the greeting comes out
	// before any model runs. An unreachable Prism answers "unknown", which is the
	// switchboard — the line still gets picked up.
	if h.task == nil {
		who := h.llmClient.LookupCaller(ctx, h.callerNumber)
		h.llmClient.CallerKnown = who.Known
		h.callerName = who.Name
		if h.pool != nil && h.logID != 0 {
			audience := "unclassified"
			if !h.llmClient.Linked() {
				audience = "standard"
			} else if who.PrismInstance != "" {
				if who.Known && who.OwnerID != nil {
					audience = "personal"
				} else if !who.Known {
					audience = "standard"
				}
			}
			if err := db.SetCallAudience(ctx, h.pool, h.logID, audience, who.PrismInstance, who.OwnerID); err != nil {
				log.Printf("[%s] call audience: %v", h.channelID, err)
			}
		}
		if who.Known {
			log.Printf("[%s] Caller recognised by Prism", h.channelID)
		}
		// Who this call may be transferred to. Fetched once, here, rather than per
		// turn: it is a system-prompt ingredient and a mid-call HTTP round trip is
		// dead air. A failure leaves it empty, and an empty directory transfers
		// nobody — the agent offers to take a message instead, which is honest.
		if h.llmClient.Linked() {
			if dir, err := h.llmClient.TransferDirectory(ctx); err != nil {
				log.Printf("[%s] Transfer directory unavailable (%v) — no transfers this call", h.channelID, err)
			} else {
				h.transferDir = make([]contacts.Contact, 0, len(dir))
				for _, e := range dir {
					h.transferDir = append(h.transferDir, contacts.Contact{
						Name: e.Name, PhoneNumber: e.Phone, TransferType: e.Transfer,
					})
				}
			}
		}
	}

	// Opening line: configured greeting for inbound, mission-driven for outbound,
	// and a greeting by name for someone we recognise.
	opening := h.phrase(cfg, "greeting")
	switch {
	case h.task != nil:
		opening = h.outboundOpening(ctx, cfg)
	case h.llmClient.CallerKnown && h.callerName != "":
		opening = knownGreeting(h.phrase(cfg, "greeting_known"), h.callerName)
	}
	if err := h.playText(ctx, opening); err != nil {
		log.Printf("[%s] Greeting play error: %v", h.channelID, err)
	} else {
		h.rememberOpening(opening)
	}

	h.captureLoop(ctx, cfg)
}

// rememberOpening records what the caller actually heard, including inbound
// greetings spoken before any model runs. Prism owns its own history, so carry
// this fact with the first caller turn using the existing gateway context.
func (h *Handler) rememberOpening(opening string) {
	opening = strings.TrimSpace(opening)
	if opening == "" {
		return
	}
	h.turns = append(h.turns, map[string]string{"role": "agent", "text": opening})
	h.history = append(h.history, llm.Message{Role: "assistant", Content: opening})
	if h.task == nil && h.llmClient != nil && h.llmClient.Docked() {
		h.llmClient.GatewayContext = append(h.llmClient.GatewayContext,
			fmt.Sprintf("Accueil déjà prononcé par l'assistant avant la première réponse de l'appelant : %q.", opening))
	}
}

// outboundOpening asks the LLM for a mission-specific first sentence.
func (h *Handler) outboundOpening(ctx context.Context, cfg *config.Config) string {
	opening, _, err := h.llmClient.GetResponse(
		ctx,
		h.systemPrompt(cfg),
		nil,
		"", "",
		nil,
		"[Système : l'interlocuteur vient de décrocher. Salue-le et présente l'objet de ton appel en une ou deux phrases.]",
	)
	opening = strings.TrimSpace(StripGemmaThinking(opening))
	if err != nil || opening == "" {
		log.Printf("[%s] Outbound opening LLM error (%v) — using fallback", h.channelID, err)
		return h.phrase(cfg, "phrase_outbound_opening")
	}
	return opening
}

// generateAnnouncement asks the LLM for a natural, one-sentence attended-transfer
// announcement to play to the recipient. It is started during the recipient's ring so
// its latency is hidden; the result (if any) is sent on out. On error or empty output
// nothing is sent and the caller falls back to the configured phrase_announce.
func (h *Handler) generateAnnouncement(ctx context.Context, cfg *config.Config, at *attendedTransferReq, out chan<- string) {
	sys := "You represent the company's reception and have called " + at.ContactName +
		" to announce a waiting call. Form a single short spoken announcement in language " + h.primaryLanguage() +
		". Address the recipient politely and formally, and ask whether they wish to take the call. " +
		"Do not introduce yourself or impersonate the caller. Output only the spoken sentence, without quotes or preamble."
	info := fmt.Sprintf("Appelant : %s.", at.CallerName)
	if at.Reason != "" {
		info = fmt.Sprintf("Appelant : %s. Objet de l'appel : %s.", at.CallerName, at.Reason)
	}

	genCtx, cancel := context.WithTimeout(ctx, announceGenTimeout)
	defer cancel()

	text, _, err := h.llmClient.GetResponse(genCtx, sys, nil, "", "", nil, info)
	if err != nil {
		log.Printf("[%s] Announcement generation error: %v", h.channelID, err)
		return
	}
	if text = strings.TrimSpace(StripGemmaThinking(text)); text != "" {
		out <- text
	}
}

// toolRulesInbound and toolRulesOutbound are the authoritative, non-editable
// tool-usage contracts appended by systemPrompt() to whatever prompt the operator
// configures in the UI. They guarantee the built-in tools keep working regardless
// of how the (DB-stored, UI-editable) persona prompt is changed: the operator owns
// tone and role, the code owns tool mechanics. Gemma 4 also needs the explicit
// "tu DOIS APPELER l'outil" directive or it narrates actions instead of calling
// them (see CLAUDE.md) — keeping it here means a prompt edit can never drop it.
const toolRulesInbound = `

# Tool execution
Use transfer_call, take_message and end_call to perform their actions in the same turn; never merely announce them.
- transfer_call: use a unique name from the supplied directory. For [blind] contacts, call immediately without another question or announcement. For [attended] contacts, collect the caller's name and reason in one question, then pass caller_name and reason. If no contact matches, offer to take a message.
- take_message: confirm the message before recording it.
- end_call: only when the caller clearly finishes. Never end an ongoing conversation on your own.
Do not invent contacts, facts or successful outcomes.`

const toolRulesOutbound = `

# Tool execution
Use take_message to record confirmed information and end_call to finish the mission. An action requires its tool call in the same turn. End only when the mission is complete, the recipient clearly wants to stop, or a voicemail has been left.`

// systemPrompt returns the effective system prompt: the receptionist prompt
// for inbound calls, or the outbound prompt plus the task mission. In both cases
// the matching non-editable tool-usage contract is appended (see toolRules*).
func (h *Handler) systemPrompt(cfg *config.Config) string {
	if h.task == nil {
		prompt := cfg.SystemPrompt + toolRulesInbound + h.languageContext(cfg)
		if h.llmClient == nil || !h.llmClient.Docked() {
			prompt += "\nAddress the other person politely and formally, even if they are informal."
		}
		return h.withMemory(prompt)
	}
	sp := cfg.Get("outbound_system_prompt") + h.languageContext(cfg) + "\nAddress the other person politely and formally, even if they are informal." + "\n\n## Mission\n"
	if h.task.ContactName != "" {
		sp += "Interlocuteur à joindre : " + h.task.ContactName + ".\n"
	}
	return h.withMemory(sp + h.task.Mission + toolRulesOutbound)
}

// ── Media setup ────────────────────────────────────────────────────────────────

// setupMedia provisions the RTP transceiver, the ExternalMedia channel and the
// mixing bridge, then starts the media loops. It runs *before* the greeting so this
// one-time setup doesn't glitch the audio right after the greeting. Returns false on
// any failure — the caller must abort the call.
//
// Topology (the structural fix for self-interruption): the caller channel and the
// ExternalMedia channel are the two participants of a mixing bridge. A mixing bridge
// never reflects a participant's own contribution, so:
//   - what we *receive* on ExternalMedia is the caller only — no echo of our own TTS;
//   - what we *send* on ExternalMedia (agent TTS, as RTP) is mixed to the caller only.
//
// We no longer Snoop the caller nor ARI-/play to them: both the capture and the
// playback ride the single bidirectional ExternalMedia RTP session (rtp.Receiver).
// This makes the agent hearing itself impossible by construction, not by filtering.
func (h *Handler) setupMedia(ctx context.Context) bool {
	rtpRx, err := rtp.New()
	if err != nil {
		log.Printf("[%s] RTP receiver init error: %v", h.channelID, err)
		return false
	}
	h.rtpReceiver = rtpRx

	// Create External Media channel — Asterisk streams the caller's RTP to our UDP
	// port and receives our TTS back on the same (symmetric) session.
	extResult, err := h.ari.PostJSON("/channels/externalMedia", map[string]interface{}{
		"app":           ariApp,
		"external_host": fmt.Sprintf("%s:%d", appHostIP, rtpRx.Port),
		"format":        "slin16",
	})
	if err != nil {
		log.Printf("[%s] ExternalMedia create error: %v", h.channelID, err)
		return false
	}
	h.extChanID, _ = extResult["id"].(string)
	log.Printf("[%s] External media channel: %s", h.channelID, h.extChanID)

	// Create mixing bridge
	bridgeResult, err := h.ari.PostJSON("/bridges", map[string]interface{}{
		"type": "mixing",
		"name": "bridge_" + h.channelID,
	})
	if err != nil {
		log.Printf("[%s] Bridge create error: %v", h.channelID, err)
		return false
	}
	h.bridgeID, _ = bridgeResult["id"].(string)

	// Add the caller + ExternalMedia to the mixing bridge. The caller channel stays
	// in Stasis (we still get its DTMF/hangup events); its media flows through the
	// bridge, so it hears our ExternalMedia audio and we hear its mic — each side
	// getting only the *other's* contribution.
	params := url.Values{}
	params.Set("channel", h.channelID+","+h.extChanID)
	data, status, err := h.ari.PostParams("/bridges/"+h.bridgeID+"/addChannel", params)
	if err != nil || status >= 400 {
		log.Printf("[%s] addChannel failed (status=%d): %v %s", h.channelID, status, err, string(data))
		return false
	}

	log.Printf("[%s] Bridge ready — full-duplex RTP on UDP %d", h.channelID, rtpRx.Port)
	rtpRx.Start()
	return true
}

// ── Capture loop ───────────────────────────────────────────────────────────────

func (h *Handler) captureLoop(ctx context.Context, cfg *config.Config) {
	rtpRx := h.rtpReceiver
	if rtpRx == nil {
		log.Printf("[%s] captureLoop: media not initialised", h.channelID)
		return
	}
	// Drop anything captured during the greeting so the first turn starts clean.
	rtpRx.Drain()

	// Phase-0 fluidity metrics for this call, logged as a summary when the loop exits
	// (makes aggregating across calls trivial — one line per call vs grepping each turn).
	var deadAirs []int64
	var bargeIns, emptyTurns int
	defer func() {
		if len(deadAirs) == 0 {
			return
		}
		log.Printf("[%s] ⏱ call summary: turns=%d endpoint_to_audio p50=%dms p90=%dms | barge_ins=%d empty_transcripts=%d",
			h.channelID, len(deadAirs), percentileMs(deadAirs, 50), percentileMs(deadAirs, 90), bargeIns, emptyTurns)
	}()

	vadProc, err := vad.New()
	if err != nil {
		log.Printf("[%s] VAD init error: %v", h.channelID, err)
		return
	}

	// A stricter VAD for barge-in detection so background noise doesn't falsely
	// interrupt the agent (normal capture keeps the lenient default).
	bargeProc, err := vad.NewWithMode(bargeVADMode)
	if err != nil {
		log.Printf("[%s] barge-in VAD init error: %v — using normal VAD", h.channelID, err)
		bargeProc = vadProc
	}

	defer func() {
		if h.rtpReceiver != nil {
			h.rtpReceiver.Stop()
		}
	}()

	// Waiting audio is limited to the initial silence of a response.

	silentPrompts := 0
	emptyReprompts := 0
	// pendingSegment holds a caller utterance captured (and validated as genuine
	// speech, not playback echo) by the barge-in monitor during the previous
	// turn's playback; when set it is consumed instead of a fresh capture.
	var pendingSegment []int16
	for {
		h.mu.Lock()
		running := h.running
		h.mu.Unlock()
		if !running || ctx.Err() != nil {
			break
		}

		var audioSamples []int16
		var liveSTT *stt.Stream
		if pendingSegment != nil {
			// The caller barged in over the previous turn's playback — that utterance
			// was already captured, use it directly instead of listening anew.
			audioSamples = pendingSegment
			pendingSegment = nil
		} else {
			// Block until a speech segment is detected (goroutine to allow cancellation)
			type segResult struct {
				stream   *stt.Stream
				samples  []int16
				timedOut bool
			}
			segCh := make(chan segResult, 1)
			go func() {
				var stream *stt.Stream
				var onAudio func([]int16)
				if h.sttClient.Realtime() {
					onAudio = func(pcm []int16) {
						if stream == nil {
							stream = h.sttClient.Begin(ctx)
						}
						stream.Add(pcm)
					}
				}
				samples, timedOut := vadProc.Capture(ctx, rtpRx, silenceTimeout, onAudio)
				if ctx.Err() != nil {
					if stream != nil {
						stream.Close()
					}
					return
				}
				segCh <- segResult{stream, samples, timedOut}
			}()

			select {
			case res := <-segCh:
				if res.timedOut {
					silentPrompts++
					if silentPrompts > maxSilencePrompts {
						log.Printf("[%s] Caller silent after %d re-prompts — hanging up", h.channelID, maxSilencePrompts)
						h.sayAndHangup(ctx, h.phrase(cfg, "phrase_farewell"))
						return
					}
					log.Printf("[%s] Silence — re-prompting (%d/%d)", h.channelID, silentPrompts, maxSilencePrompts)
					reprompt := h.generateReprompt(ctx, cfg, silentPrompts)
					if err := h.playText(ctx, reprompt); err != nil {
						log.Printf("[%s] Re-prompt play error: %v", h.channelID, err)
					}
					h.history = append(h.history, llm.Message{Role: "assistant", Content: reprompt})
					rtpRx.Drain()
					continue
				}
				audioSamples = res.samples
				liveSTT = res.stream
			case <-ctx.Done():
				return
			}
		}

		if audioSamples == nil {
			if liveSTT != nil {
				liveSTT.Close()
			}
			break
		}
		silentPrompts = 0

		h.mu.Lock()
		running = h.running
		h.mu.Unlock()
		if !running {
			break
		}

		duration := float64(len(audioSamples)) / float64(rtp.SampleRate)
		log.Printf("[%s] Speech detected (%.1fs)", h.channelID, duration)

		// Latency diagnostics: tReady marks speech-end (start of the dead-air the
		// caller perceives); the per-stage timings are summarized after first audio.
		tReady := time.Now()
		var sttMs int64

		// STT: convert captured audio to text. Barge-in segments go through this
		// same path: the monitor validated a *prefix* to decide the interruption
		// was genuine; the full utterance is transcribed here.
		var transcript string
		wavBytes, wavErr := audio.EncodeWAV(audio.Int16ToFloat32(audioSamples), rtp.SampleRate)
		if wavErr != nil {
			log.Printf("[%s] WAV encode error: %v", h.channelID, wavErr)
		} else {
			var sttErr error
			tSTT := time.Now()
			if liveSTT != nil {
				transcript, sttErr = liveSTT.Finish()
			} else {
				transcript, sttErr = h.sttClient.Transcribe(ctx, wavBytes)
			}
			sttMs = time.Since(tSTT).Milliseconds()
			if sttErr != nil {
				log.Printf("[%s] STT error: %v", h.channelID, sttErr)
			}
		}
		transcript = strings.TrimSpace(transcript)
		if transcript == "" {
			emptyTurns++
			// The caller spoke (VAD fired) but STT returned nothing usable. If the
			// segment was long enough to be real speech, we more likely missed it
			// than it being noise — ask them to repeat instead of going silent.
			if duration >= minSpeechForReprompt && emptyReprompts < maxEmptyReprompts {
				emptyReprompts++
				log.Printf("[%s] Empty transcript on %.1fs speech — asking to repeat (%d/%d)",
					h.channelID, duration, emptyReprompts, maxEmptyReprompts)
				if err := h.playText(ctx, h.phrase(cfg, "phrase_not_understood")); err != nil {
					log.Printf("[%s] Reprompt play error: %v", h.channelID, err)
				}
				if h.rtpReceiver != nil {
					h.rtpReceiver.Drain()
				}
			} else {
				log.Printf("[%s] Empty transcript (%.1fs) — skipping turn", h.channelID, duration)
			}
			continue
		}
		emptyReprompts = 0
		h.acceptCallerText(ctx, transcript)
		privacy.Logf("[%s] Transcript: %q", h.channelID, transcript)
		h.turns = append(h.turns, map[string]string{"role": "caller", "text": transcript})

		// RAG is no longer injected every turn — the model pulls knowledge on demand
		// via the search_knowledge_base tool (see handleToolCalls).
		ragCtx := ""

		// The transfer directory, inbound only. Linked to Prism it is Prism's user
		// list; standalone it is Vox's own contacts table. Never both at once — the
		// local table is the OUTBOUND directory once docked, and merging the two
		// would have the switchboard offer to put a caller through to a supplier.
		contactsCtx := ""
		if h.task == nil {
			switch {
			case h.llmClient.Linked():
				contactsCtx = contacts.PromptFor(h.transferDir, nil)
			case h.pool != nil:
				c, err := contacts.GetPrompt(ctx, h.pool)
				if err != nil {
					log.Printf("[%s] Contacts error: %v", h.channelID, err)
				} else {
					contactsCtx = c
				}
			}
		}

		h.compactHistory(ctx)
		tools := h.buildTools()

		// ── 3-stage streaming pipeline ─────────────────────────────────────────
		// Stage 1: LLM streams sentences → sentCh
		// Stage 2: short sentence group → TTS synthesis → wavCh (goroutine)
		// Stage 3: wavCh → ARI playback              (goroutine)
		// Stages 2+3 overlap so playback of sentence N starts while N+1 is being synthesized.

		tLLM := time.Now()
		h.responseCue = nil
		if len(h.waitCueWav) > 0 || len(h.waitCues) > 0 {
			h.responseCue = &waitingCue{wav: h.waitCueWav, clips: h.waitCues, selectClips: h.localWaitingClips, started: tLLM, delay: 3 * time.Second}
		}

		var firstPacket <-chan time.Time
		if h.rtpReceiver != nil {
			firstPacket = h.rtpReceiver.ObserveNextPacket()
		}
		turnCtx, cancelTurn := context.WithCancel(ctx)
		remoteTools := prismToolLog{session: h.llmClient.SessionID}
		cue := h.responseCue
		turnCtx = llm.WithToolTrace(turnCtx, func(event llm.ToolTrace) {
			remoteTools.observe(event)
			cue.observeTool(event)
		})
		h.llmClient.LanguageHint = h.languageContext(cfg)
		sentCh, toolsCh, llmErrCh := h.llmClient.StreamResponse(
			turnCtx,
			h.systemPrompt(cfg),
			h.history,
			ragCtx,
			contactsCtx,
			tools,
			transcript,
		)

		wavCh := make(chan playChunk, 4)

		// Playback and the current model turn can stop without ending the call.
		playCtx, cancelPlay := context.WithCancel(ctx)

		// Stage 2: TTS synthesis goroutine
		// fullResponse is written by Stage 2 and read concurrently by the barge-in
		// monitor (echo comparison) — hence the mutex + snapshot accessor.
		var fullResponse strings.Builder
		var respMu sync.Mutex
		snapshotResponse := func() string {
			respMu.Lock()
			defer respMu.Unlock()
			return fullResponse.String()
		}
		// Latency diagnostics: first-sentence and first-audio timestamps (written
		// only by this goroutine; read after <-playDone, i.e. once it has finished).
		var tFirstSentence, tFirstBatch, tFirstWav time.Time
		go func() {
			defer close(wavCh)
			var groupWait time.Duration
			if h.groupShortSpeech {
				groupWait = speechGroupWait
			}
			for batch := range groupSpeech(turnCtx, sentCh, groupWait) {
				if tFirstSentence.IsZero() {
					tFirstSentence = batch.firstSentenceAt
					tFirstBatch = time.Now()
				}
				sent := batch.text
				// Accumulate even after a barge-in cancel — the LLM's full text
				// belongs in the history, and the monitor compares echo against it.
				respMu.Lock()
				fullResponse.WriteString(sent)
				fullResponse.WriteString(" ")
				respMu.Unlock()
				if playCtx.Err() != nil {
					continue // barge-in: stop synthesizing the rest of the response
				}
				err := h.synthesizeSpeech(playCtx, sent, wavCh, func() {
					if tFirstWav.IsZero() {
						tFirstWav = time.Now()
					}
				})
				if err != nil && playCtx.Err() == nil {
					log.Printf("[%s] TTS synthesis failed: %v", h.channelID, err)
				}
			}
		}()

		// Stage 3: eager ARI playback. Each sentence is queued for playback as soon
		// as it is synthesized — without waiting for the previous one to finish — so
		// Asterisk plays them back-to-back, with no silent gap between sentences.
		playDone := make(chan struct{})
		go func() {
			defer close(playDone)
			h.playQueued(playCtx, h.channelID, h.withWaitCue(playCtx, wavCh))
		}()

		// Drop audio buffered while we were busy (STT + LLM + first synth) so the
		// monitor reacts to real-time speech during playback, not a stale backlog —
		// otherwise a slow turn (e.g. a long TTS) makes barge-in fire on old audio and
		// capture garbage.
		if h.rtpReceiver != nil {
			h.rtpReceiver.Drain()
		}

		// Barge-in monitor: watch the caller stream while the agent speaks.
		// Validate-before-interrupt: the trunk echoes our own playback back on the
		// caller stream (confirmed with the caller's mic muted), so sustained voice
		// during playback is NOT enough to cut the agent off. Each time the sustain
		// threshold accrues, the audio captured so far is transcribed *while the
		// agent keeps talking* and playback is cancelled only if it is genuine
		// caller speech — echo (unintelligible, hallucinated, or matching our own
		// words) never interrupts: no stutter, no replay, the validator just keeps
		// watching for a real interruption on top of it.
		monCtx, cancelMon := context.WithCancel(ctx)
		bargeCh := make(chan []int16, 1)
		go func() {
			bargeCh <- bargeProc.GetSpeechSegmentCallCtx(ctx, monCtx, rtpRx, bargeInFrames, func(partial []int16) bool {
				tr := h.transcribeSegment(monCtx, partial)
				switch {
				case tr == "":
					log.Printf("[%s] playback echo discarded (unintelligible, %.1fs)",
						h.channelID, float64(len(partial))/float64(rtp.SampleRate))
					return false
				case isSTTHallucination(tr):
					privacy.Logf("[%s] playback echo discarded (STT hallucination %q)", h.channelID, tr)
					return false
				case transcriptEchoesText(tr, snapshotResponse()):
					privacy.Logf("[%s] playback echo discarded (our own words: %q)", h.channelID, tr)
					return false
				}
				privacy.Logf("[%s] genuine barge-in (%q) — interrupting playback", h.channelID, tr)
				cancelPlay()
				cancelTurn()
				return true
			})
		}()

		<-playDone
		interrupted := playCtx.Err() != nil
		if interrupted {
			// Caller barged in — wait for the monitor to finish capturing their
			// utterance (the confirmed capture runs to utterance end, then the
			// full segment goes through the normal STT path next iteration).
			if seg := <-bargeCh; seg != nil {
				pendingSegment = seg
				bargeIns++
				log.Printf("[%s] barge-in: caller interrupted (%.1fs captured)",
					h.channelID, float64(len(seg))/float64(rtp.SampleRate))
			}
		} else {
			// Playback finished cleanly — stop the monitor and drain any residual.
			cancelMon()
			<-bargeCh
			if h.rtpReceiver != nil {
				h.rtpReceiver.Drain()
			}
		}
		cancelMon()
		cancelPlay()

		// First successful UDP send, distinct from provider first bytes.
		select {
		case sentAt := <-firstPacket:
			log.Printf("[%s] latency: endpoint_to_first_rtp=%dms", h.channelID, sentAt.Sub(tReady).Milliseconds())
		default:
		}
		// Latency diagnostics: endpoint → first-provider-audio breakdown.
		// stt + (llm→1st sentence) + (tts 1st) ≈ dead_air (the rest is small glue).
		if !tFirstWav.IsZero() {
			deadAir := tFirstWav.Sub(tReady).Milliseconds()
			deadAirs = append(deadAirs, deadAir)
			log.Printf("[%s] ⏱ latency: stt_final=%dms llm→1st_sentence=%dms speech_group_wait=%dms tts_1st=%dms | endpoint_to_audio=%dms (excludes VAD endpoint delay)",
				h.channelID, sttMs,
				tFirstSentence.Sub(tLLM).Milliseconds(),
				tFirstBatch.Sub(tFirstSentence).Milliseconds(),
				tFirstWav.Sub(tFirstBatch).Milliseconds(),
				deadAir,
			)
		}

		var toolCalls []llm.ToolCall
		if tc, ok := <-toolsCh; ok {
			toolCalls = tc
		}
		if err := <-llmErrCh; err != nil {
			log.Printf("[%s] LLM stream error: %v", h.channelID, err)
		}
		cancelTurn()
		h.toolsUsed = append(h.toolsUsed, remoteTools.entries...)
		for _, entry := range remoteTools.entries {
			if entry["tool"] == "rag_search" {
				h.ragUsed = true
			}
		}
		if ctx.Err() != nil {
			return
		}
		if interrupted {
			toolCalls = nil
			h.llmClient.GatewayContext = append(h.llmClient.GatewayContext,
				"L'appelant a interrompu la réponse. Les actions téléphoniques de ce tour n'ont pas été exécutées.")
		}

		// Add caller's turn to history now so the follow-up LLM call has full context
		h.history = append(h.history, llm.Message{Role: "user", Content: transcript})

		// If the model returned tool calls, execute them. handleToolCalls returns
		// only the NEW text to speak — the streamed text has already been played.
		response := strings.TrimSpace(snapshotResponse())
		if len(toolCalls) > 0 {
			log.Printf("[%s] Tool calls: %d", h.channelID, len(toolCalls))
			followUp, toolErr := h.handleToolCalls(ctx, response, toolCalls, cfg, ragCtx, contactsCtx)
			if h.llmClient.Docked() && followUp != "" {
				h.llmClient.GatewayContext = append(h.llmClient.GatewayContext, "Réponse locale de la passerelle : "+followUp)
			}
			if toolErr != nil {
				log.Printf("[%s] Tool call error: %v", h.channelID, toolErr)
			}
			if followUp != "" {
				if err := h.playText(ctx, followUp); err != nil {
					log.Printf("[%s] Tool follow-up play error: %v", h.channelID, err)
				}
				if h.rtpReceiver != nil {
					h.rtpReceiver.Drain()
				}
				response = strings.TrimSpace(response + " " + followUp)
			}
		}

		if response == "" {
			response = fallbackText
			if !interrupted {
				_ = h.playText(ctx, response)
			}
		}
		privacy.Logf("[%s] Response: %q", h.channelID, response)
		h.turns = append(h.turns, map[string]string{"role": "agent", "text": response})
		h.history = append(h.history, llm.Message{Role: "assistant", Content: response})
		h.responseCue = nil

		// LLM decided the conversation is over
		if h.endCall {
			h.hangup()
			break
		}

		// Check for blind transfer
		if h.transferDest != "" {
			if err := h.executeTransfer(ctx, cfg, h.transferDest); err != nil {
				log.Printf("[%s] Transfer error: %v", h.channelID, err)
				h.recoverTransfer(ctx)
				continue
			}
			break
		}

		// Check for attended transfer
		if h.attendedTransfer != nil {
			at := h.attendedTransfer
			h.attendedTransfer = nil

			result := h.executeAttendedTransfer(ctx, cfg, at)
			if h.llmClient.Docked() {
				h.llmClient.GatewayContext = append(h.llmClient.GatewayContext, "Résultat du transfert accompagné vers "+at.ContactName+" : "+result)
			}
			if result == "accepted" {
				h.transferDest = at.Destination
				break
			}

			name := at.ContactName
			if name == "" {
				name = at.Destination
			}
			reasonMap := map[string]string{
				"rejected":    name + " n'est pas disponible.",
				"no_answer":   name + " ne répond pas.",
				"no_response": name + " ne répond pas.",
				"failed":      "Le transfert a échoué.",
			}
			systemNote, ok := reasonMap[result]
			if !ok {
				systemNote = "Le transfert n'a pas abouti."
			}

			toolsNoTransfer := h.buildToolsNoTransfer()
			followUp, followUpCalls, err := h.llmClient.GetResponse(
				ctx,
				h.systemPrompt(cfg),
				h.history,
				ragCtx,
				contactsCtx,
				toolsNoTransfer,
				"[Système: "+systemNote+" En tant que standardiste, informe poliment l'appelant et propose-lui de laisser un message ou de rappeler plus tard.]",
			)
			if err != nil {
				followUp = "Je suis désolée, le transfert n'a pas abouti. Puis-je prendre un message ?"
				followUpCalls = nil
			}
			followUp = StripGemmaThinking(followUp)
			if followUp != "" {
				if err := h.playText(ctx, followUp); err != nil {
					log.Printf("[%s] Follow-up play error: %v", h.channelID, err)
				}
			}
			extra, _ := h.handleToolCalls(ctx, followUp, followUpCalls, cfg, ragCtx, contactsCtx)
			if extra != "" {
				if err := h.playText(ctx, extra); err != nil {
					log.Printf("[%s] Follow-up tool play error: %v", h.channelID, err)
				}
				followUp = strings.TrimSpace(followUp + " " + extra)
			}
			h.history = append(h.history, llm.Message{Role: "assistant", Content: followUp})
			h.turns = append(h.turns, map[string]string{"role": "agent", "text": followUp})
			if h.rtpReceiver != nil {
				h.rtpReceiver.Drain()
			}

			if h.endCall {
				h.hangup()
				break
			}
			if h.transferDest != "" {
				if err := h.executeTransfer(ctx, cfg, h.transferDest); err != nil {
					log.Printf("[%s] Follow-up transfer error: %v", h.channelID, err)
					h.recoverTransfer(ctx)
					continue
				}
				break
			}
		}
		h.compactHistory(ctx)
	}
}

const fallbackText = "Je suis désolée, une erreur est survenue."

// sayAndHangup plays a final phrase to the caller, then hangs up.
func (h *Handler) sayAndHangup(ctx context.Context, text string) {
	if text != "" {
		if err := h.playText(ctx, text); err != nil {
			log.Printf("[%s] Farewell play error: %v", h.channelID, err)
		}
		h.turns = append(h.turns, map[string]string{"role": "agent", "text": text})
	}
	h.hangup()
}

// hangup terminates the caller's channel. StasisEnd then triggers Cleanup.
func (h *Handler) hangup() {
	h.mu.Lock()
	h.running = false
	h.mu.Unlock()
	log.Printf("[%s] Hanging up", h.channelID)
	h.ari.Delete("/channels/" + h.channelID) //nolint:errcheck
}

// ── Tool handling ──────────────────────────────────────────────────────────────

// handleToolCalls executes the LLM's tool calls and returns the NEW text to
// speak. spokenText is what has already been said this turn — it is never
// replayed, only used as context (history, end_call farewell dedup).
func (h *Handler) handleToolCalls(
	ctx context.Context,
	spokenText string,
	toolCalls []llm.ToolCall,
	cfg *config.Config,
	ragCtx, contactsCtx string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(toolCalls) == 0 {
		return "", nil
	}
	eligible := false
	for _, tc := range toolCalls {
		if !tc.Relayed && toolCueEligible(tc.Name) {
			eligible = true
			break
		}
	}
	defer h.localWaitCue(ctx, eligible)()

	// builtinSpoken collects spoken phrases for built-in tools (no LLM needed).
	// mcpResults collects raw outputs for tools whose outcome the LLM must phrase.
	// fallbackSpoken holds the fixed phrases to speak *only* if that follow-up LLM
	// call fails — otherwise the caller would hear nothing at all.
	var builtinSpoken []string
	var mcpResults []string
	var fallbackSpoken []string

	for _, tc := range toolCalls {
		toolName := tc.Name
		args := tc.Arguments
		if args == nil {
			args = make(map[string]interface{})
		}

		switch toolName {
		case "transfer_call":
			dest := strings.TrimSpace(stringFromMap(args, "destination"))
			callerName := strings.TrimSpace(stringFromMap(args, "caller_name"))
			reason := strings.TrimSpace(stringFromMap(args, "reason"))

			if dest == "" {
				builtinSpoken = append(builtinSpoken, "Je suis désolée, je n'ai pas pu effectuer le transfert.")
				continue
			}

			// Resolve against the directory — never transfer to an unknown destination.
			//
			// Three ways a destination arrives, and all three must carry how the
			// recipient wants to be reached. Getting that wrong is not cosmetic: it
			// puts a stranger straight onto someone's mobile with no announcement.
			contactName, ext, transferType := dest, dest, contacts.TransferBlind
			switch {
			// The caller is one of Prism's users, so Prism is the brain: it resolved
			// the name against its own directory and relayed a number AND the
			// recipient's preference. Trust both — it read them from the profile.
			case tc.Relayed && h.llmClient.Docked() && strings.TrimSpace(stringFromMap(args, "dial_number")) != "":
				contactName = dest
				ext = strings.TrimSpace(stringFromMap(args, "dial_number"))
				transferType = contacts.NormalizeTransferType(stringFromMap(args, "transfer_type"))

			// A stranger, handled here, being put through to one of Prism's users.
			case len(h.transferDir) > 0:
				c := contacts.ResolveEntries(h.transferDir, dest)
				if c == nil {
					privacy.Logf("[%s] transfer_call refused: %q not in the Prism directory", h.channelID, dest)
					mcpResults = append(mcpResults, unknownContactResult(dest))
					fallbackSpoken = append(fallbackSpoken, renderPhrase(h.phrase(cfg, "phrase_unknown_contact"), dest))
					continue
				}
				contactName, ext, transferType = c.Name, c.PhoneNumber, contacts.NormalizeTransferType(c.TransferType)

			// Standalone Vox: its own contacts table is the directory.
			case h.pool != nil && !h.llmClient.Linked():
				c := contacts.Resolve(ctx, h.pool, dest)
				if c == nil {
					privacy.Logf("[%s] transfer_call refused: %q not in directory", h.channelID, dest)
					// Hand the failure back to the model instead of speaking a canned
					// line: it holds the directory (contactsCtx) and can offer a close
					// match ("pas de Jean, mais j'ai un Jean-Marc au support") or a
					// message. The fixed phrase stays as the fallback.
					// Wording validated against the live model: a softer "si un contact
					// ressemble, propose-le" was ignored — it answered "Jean n'existe
					// pas". Spelling out the near-match rule yields "Vouliez-vous
					// parler à Jean-Marc Dupont ?", which is the whole point.
					mcpResults = append(mcpResults, unknownContactResult(dest))
					fallbackSpoken = append(fallbackSpoken, renderPhrase(h.phrase(cfg, "phrase_unknown_contact"), dest))
					continue
				}
				contactName, ext, transferType = c.Name, c.PhoneNumber, contacts.NormalizeTransferType(c.TransferType)

			// Linked to Prism but its directory never arrived: transfer nobody
			// rather than dial an unverified string.
			default:
				log.Printf("[%s] transfer_call refused: no directory available", h.channelID)
				mcpResults = append(mcpResults, unknownContactResult(dest))
				fallbackSpoken = append(fallbackSpoken, renderPhrase(h.phrase(cfg, "phrase_unknown_contact"), dest))
				continue
			}

			// The [transfer] dialplan only dials numeric extensions (_X. / _+X.).
			// A non-dialable number (name, empty, text in the directory) would make
			// Asterisk drop the call right after we announced the transfer — refuse
			// gracefully instead, like an unknown contact.
			if !isDialable(ext) {
				privacy.Logf("[%s] transfer_call refused: contact %q has a non-dialable number %q",
					h.channelID, contactName, ext)
				mcpResults = append(mcpResults, fmt.Sprintf(
					"Outil transfer_call : ÉCHEC. %s est dans l'annuaire mais son numéro n'est pas composable, "+
						"le transfert n'a pas eu lieu. Dis-le à l'appelant et propose de prendre un message.", contactName))
				fallbackSpoken = append(fallbackSpoken, renderPhrase(h.phrase(cfg, "phrase_unknown_contact"), contactName))
				continue
			}

			if transferType == contacts.TransferAttended {
				// An attended transfer announces the caller to a colleague, so the name
				// and reason must come from the caller — not from the model. Asked to
				// fill these fields it will happily invent them (observed live:
				// caller="M. Dupont" from a caller who never gave a name, and
				// reason="Demande de transfert"). Discard anything the caller did not
				// actually say, then ask; the model is never the source of truth right
				// before an irreversible action.
				keptName, keptReason := sanitizeAttendedInfo(callerName, reason, userTurns(h.callTranscript()))
				if keptName != callerName && callerName != "" {
					privacy.Logf("[%s] attended transfer: discarding fabricated caller_name %q", h.channelID, callerName)
				}
				if keptReason != reason && reason != "" {
					privacy.Logf("[%s] attended transfer: discarding placeholder reason %q", h.channelID, reason)
				}
				callerName, reason = keptName, keptReason
				if callerName == "" {
					callerName = h.collectCallerInfo(ctx, h.phrase(cfg, "phrase_ask_name"))
				}
				if reason == "" {
					reason = h.collectCallerInfo(ctx, h.phrase(cfg, "phrase_ask_reason"))
				}
				h.attendedTransfer = &attendedTransferReq{
					Destination: ext,
					ContactName: contactName,
					CallerName:  callerName,
					Reason:      reason,
				}
				h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": "transfer_call", "args": args, "outcome": "queued"})
				builtinSpoken = append(builtinSpoken, h.phrase(cfg, "phrase_hold"))
			} else {
				h.transferDest = ext
				h.toolsUsed = append(h.toolsUsed, map[string]interface{}{
					"tool":    "transfer_call",
					"args":    map[string]interface{}{"destination": ext},
					"outcome": "queued",
				})
				builtinSpoken = append(builtinSpoken, h.phrase(cfg, "phrase_transfer_now"))
			}
			privacy.Logf("[%s] Transfer queued → %s/%s (%s) caller=%q reason=%q",
				h.channelID, contactName, ext, transferType, callerName, reason)

		case "end_call":
			h.endCall = true
			h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": "end_call"})
			// Speak the farewell only if the model didn't already say goodbye itself.
			if strings.TrimSpace(spokenText) == "" {
				builtinSpoken = append(builtinSpoken, h.phrase(cfg, "phrase_farewell"))
			}
			log.Printf("[%s] end_call requested by LLM", h.channelID)

		case "take_message":
			recipient := strings.TrimSpace(stringFromMap(args, "recipient"))
			content := strings.TrimSpace(stringFromMap(args, "content"))
			if recipient == "" || content == "" {
				builtinSpoken = append(builtinSpoken, "Je suis désolée, je n'ai pas pu enregistrer le message.")
				continue
			}
			msg := fmt.Sprintf("Pour %s : %s", recipient, content)
			h.message = &msg
			if h.llmClient.Docked() {
				h.llmClient.GatewayContext = append(h.llmClient.GatewayContext, "Message pour "+recipient+" noté dans le journal d'appel Vox ; personne n'est prévenu automatiquement.")
			}
			h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": "take_message", "args": args, "outcome": "recorded_in_call_log_not_delivered"})
			// The message lands in the call log; nobody is paged. Don't promise a handoff.
			builtinSpoken = append(builtinSpoken, fmt.Sprintf("Le message pour %s est bien noté.", recipient))
			log.Printf("[%s] Message saved", h.channelID)

		case "search_knowledge_base":
			query := strings.TrimSpace(stringFromMap(args, "query"))
			h.ragUsed = true
			h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": "search_knowledge_base", "args": args})
			finishLookup := h.responseCue.beginLookup(toolName, args)
			result := h.searchKnowledge(ctx, query)
			finishLookup()
			privacy.Logf("[%s] KB search %q → %d chars", h.channelID, query, len(result))
			if result == "" {
				result = "Aucune information pertinente trouvée dans la base de connaissance."
			}
			// Fed back to the LLM (like an MCP result) so it answers from the passages.
			mcpResults = append(mcpResults, "Résultats de la base de connaissance pour « "+query+" » :\n"+result)

		default:
			// MCP tool — result needs LLM interpretation
			finishLookup := h.responseCue.beginLookup(toolName, args)
			result, err := h.mcpMgr.CallTool(ctx, toolName, args)
			finishLookup()
			if err != nil {
				log.Printf("[%s] MCP tool '%s' error: %v", h.channelID, toolName, err)
				h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": toolName, "args": args, "outcome": "failed"})
				mcpResults = append(mcpResults, fmt.Sprintf("Tool '%s': error: %v", toolName, err))
				continue
			}
			summaryResult := []rune(result)
			if len(summaryResult) > 2000 {
				summaryResult = append(summaryResult[:2000], []rune("… [truncated]")...)
			}
			h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": toolName, "args": args, "outcome": "returned", "result": string(summaryResult)})
			shortResult := result
			if len(shortResult) > 120 {
				shortResult = shortResult[:120]
			}
			privacy.Logf("[%s] Tool %s → %s", h.channelID, toolName, shortResult)
			mcpResults = append(mcpResults, fmt.Sprintf("Tool '%s': %s", toolName, result))
		}
	}

	// Built-in tools: return directly without LLM
	if len(mcpResults) == 0 {
		return strings.Join(builtinSpoken, " "), nil
	}

	// MCP tools: feed results to LLM for a natural spoken response.
	// h.history ends with user:transcript (added by captureLoop before this call).
	// We strip that trailing user message and combine transcript + tool results into
	// a single user message to avoid two consecutive user messages.
	followupHistory := make([]llm.Message, len(h.history))
	copy(followupHistory, h.history)
	var callerTranscript string
	if n := len(followupHistory); n > 0 && followupHistory[n-1].Role == "user" {
		if s, ok := followupHistory[n-1].Content.(string); ok {
			callerTranscript = s
		}
		followupHistory = followupHistory[:n-1]
	}
	if spokenText != "" {
		followupHistory = append(followupHistory,
			llm.Message{Role: "assistant", Content: spokenText},
		)
	}
	userMsg := "Tool results:\n" + strings.Join(mcpResults, "\n") + "\n\nNow give your final spoken response in one or two sentences."
	if callerTranscript != "" {
		userMsg = "Caller said: \"" + callerTranscript + "\"\n" + userMsg
	}

	final, _, err := h.llmClient.GetResponse(
		ctx,
		h.systemPrompt(cfg),
		followupHistory,
		ragCtx,
		contactsCtx,
		nil,
		userMsg,
	)
	if err != nil {
		log.Printf("[%s] Follow-up LLM error: %v", h.channelID, err)
		// The model could not phrase the outcome — speak the fixed phrases so the
		// caller is not left in silence after a failed transfer.
		return strings.TrimSpace(strings.Join(append(builtinSpoken, fallbackSpoken...), " ")), err
	}
	return strings.TrimSpace(strings.Join(builtinSpoken, " ") + " " + final), nil
}

// ── Transfer ───────────────────────────────────────────────────────────────────

// BuildCallerID builds the ARI callerId for an originated channel from the SIP
// config: `"Name" <user>` when sip_callerid_name is set, else just the user (no
// display name → a clean From, never PJSIP's "Anonymous" default). Empty if no
// sip_username is configured.
func BuildCallerID(cfg *config.Config) string {
	user := cfg.Get("sip_username")
	if user == "" {
		return ""
	}
	if name := strings.TrimSpace(cfg.Get("sip_callerid_name")); name != "" {
		return `"` + name + `" <` + user + `>`
	}
	return user
}

func (h *Handler) executeTransfer(ctx context.Context, cfg *config.Config, destination string) (resultErr error) {
	defer func() {
		outcome := "initiated"
		if resultErr != nil {
			outcome = "failed"
		}
		h.setTransferOutcome(outcome)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Media (bridge, ExternalMedia, snoop) is deliberately NOT released here: if
	// ARI refuses the redirect/continue, the caller must still hear us. On success
	// Asterisk moves the channel and cleanup() releases the leftovers.
	method := strings.ToLower(strings.TrimSpace(cfg.Get("sip_transfer_method")))
	log.Printf("[%s] Blind transfer → %s (method=%s)", h.channelID, destination, method)

	if method == "refer" {
		// Centrex/phone-style: send a SIP REFER to the caller's softswitch via ARI
		// redirect. Asterisk drops out of the call and the core re-targets it — no
		// media stays in our path. The Refer-To host MUST be the registrar domain
		// (sip_domain), not the internal endpoint name — otherwise the softswitch
		// gets an unroutable URI like 9078@phoneagent-endpoint.
		domain := strings.TrimSpace(cfg.Get("sip_domain"))
		// chan_pjsip copies this target verbatim into the REFER's Refer-To, so it
		// must be a full SIP URI — a bare user@host yields a malformed header. The
		// ;user=phone param matches the softswitch's phone-number routing (same as
		// the trunk's To header).
		var endpoint string
		if domain != "" {
			endpoint = fmt.Sprintf("PJSIP/sip:%s@%s;user=phone", destination, domain)
		} else {
			log.Printf("[%s] REFER without sip_domain — Refer-To may be unroutable", h.channelID)
			endpoint = "PJSIP/" + destination
		}
		_, status, err := h.ari.Post(
			"/channels/"+h.channelID+"/redirect",
			map[string]interface{}{
				"endpoint": endpoint,
			},
		)
		if err != nil {
			return err
		}
		if status >= 400 {
			return fmt.Errorf("redirect (REFER) failed: status %d", status)
		}
		return nil
	}

	// Default "bridge": Asterisk dials the destination from the [transfer] dialplan
	// context and bridges the two legs, staying in the media path.
	_, status, err := h.ari.Post(
		"/channels/"+h.channelID+"/continue",
		map[string]interface{}{
			"context":   "transfer",
			"extension": destination,
			"priority":  1,
		},
	)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("continueInDialplan failed: status %d", status)
	}
	return nil
}

func (h *Handler) recoverTransfer(ctx context.Context) {
	h.transferDest = ""
	text := "Le transfert n'a pas abouti. Puis-je prendre un message ?"
	if h.llmClient.Docked() {
		h.llmClient.GatewayContext = append(h.llmClient.GatewayContext, "Le transfert a échoué. L'appelant est toujours en ligne.")
	}
	_ = h.playText(ctx, text)
	h.history = append(h.history, llm.Message{Role: "assistant", Content: text})
	h.turns = append(h.turns, map[string]string{"role": "agent", "text": text})
}

func (h *Handler) executeAttendedTransfer(
	ctx context.Context,
	cfg *config.Config,
	at *attendedTransferReq,
) (outcome string) {
	defer func() { h.setTransferOutcome(outcome) }()
	destination := at.Destination
	if ctx.Err() != nil {
		return "failed"
	}
	privacy.Logf("[%s] Attended transfer → %s/%s (from %q: %q)",
		h.channelID, at.ContactName, destination, at.CallerName, at.Reason)

	// Put caller on hold with Music on Hold
	h.ari.Post("/channels/"+h.channelID+"/moh", nil) //nolint:errcheck

	transferCID := "transfer-" + randHex(12)

	// Clear any stale events
	drainStringChan(h.transferStart)
	drainStringChan(h.transferEnd)

	// Originate outbound call to destination. Set callerId (presentation "allowed")
	// so the recipient's leg doesn't show "Anonymous" — same as the outbound poller.
	originate := map[string]interface{}{
		"endpoint":  fmt.Sprintf("PJSIP/%s@phoneagent-endpoint", destination),
		"app":       ariApp,
		"appArgs":   fmt.Sprintf("attended_transfer,%s", h.channelID),
		"channelId": transferCID,
	}
	if cid := BuildCallerID(cfg); cid != "" {
		originate["callerId"] = cid
	}
	_, status, err := h.ari.Post("/channels", originate)
	if err != nil || status >= 400 {
		log.Printf("[%s] Originate failed (status=%d): %v", h.channelID, status, err)
		h.ari.Delete("/channels/" + h.channelID + "/moh") //nolint:errcheck
		return "failed"
	}

	// Generate a natural announcement while the recipient's phone rings — that dead
	// time hides the LLM latency, so the recipient hears a fresh phrasing the instant
	// they answer. Falls back to phrase_announce if it errors or isn't ready in time.
	announceCh := make(chan string, 1)
	if at.CallerName != "" {
		go h.generateAnnouncement(ctx, cfg, at, announceCh)
	}

	// Wait for transfer channel to answer
	select {
	case <-h.transferStart:
		// answered — continue
	case <-time.After(transferWait):
		log.Printf("[%s] No answer from %s (timeout)", h.channelID, destination)
		h.ari.Delete("/channels/" + transferCID)          //nolint:errcheck
		h.ari.Delete("/channels/" + h.channelID + "/moh") //nolint:errcheck
		return "no_answer"
	case <-ctx.Done():
		h.ari.Delete("/channels/" + transferCID)          //nolint:errcheck
		h.ari.Delete("/channels/" + h.channelID + "/moh") //nolint:errcheck
		return "no_answer"
	}

	// Check if transfer channel hung up immediately after answer
	select {
	case <-h.transferEnd:
		h.ari.Delete("/channels/" + h.channelID + "/moh") //nolint:errcheck
		return "no_answer"
	default:
	}

	// Run the announcement + response phase in a sub-function.
	// All media resources are managed inside to keep defer scope clean.
	decision := h.attendedTransferInner(ctx, cfg, transferCID, at, announceCh)

	if decision == "accepted" {
		log.Printf("[%s] Transfer accepted — bridging with %s", h.channelID, transferCID)

		// Take the caller off hold and announce the connection before bridging.
		h.ari.Delete("/channels/" + h.channelID + "/moh") //nolint:errcheck
		name := at.ContactName
		if name == "" {
			name = destination
		}
		if err := h.playText(ctx, renderPhrase(h.phrase(cfg, "phrase_connecting"), name)); err != nil {
			log.Printf("[%s] Connecting announce error: %v", h.channelID, err)
		}
		if err := h.bridgeTransfer(transferCID); err != nil {
			log.Printf("[%s] Final bridge error: %v", h.channelID, err)
			h.ari.Delete("/channels/" + transferCID) //nolint:errcheck
			return "failed"
		}

		h.mu.Lock()
		h.running = false
		h.mu.Unlock()

		if h.rtpReceiver != nil {
			h.rtpReceiver.Stop()
			h.rtpReceiver = nil
		}
		if h.bridgeID != "" {
			h.ari.Delete("/bridges/" + h.bridgeID) //nolint:errcheck
			h.bridgeID = ""
		}
		if h.extChanID != "" {
			h.ari.Delete("/channels/" + h.extChanID) //nolint:errcheck
			h.extChanID = ""
		}
		if h.snoopChanID != "" {
			h.ari.Delete("/channels/" + h.snoopChanID) //nolint:errcheck
			h.snoopChanID = ""
		}

		return "accepted"
	}

	log.Printf("[%s] Transfer %s — hanging up %s", h.channelID, decision, transferCID)
	h.ari.Delete("/channels/" + transferCID)          //nolint:errcheck
	h.ari.Delete("/channels/" + h.channelID + "/moh") //nolint:errcheck
	return decision
}

// bridgeTransfer confirms both legs were bridged before caller media is released.
// A partially failed add restores the caller to its original media bridge.
func (h *Handler) bridgeTransfer(recipient string) error {
	result, err := h.ari.PostJSON("/bridges", map[string]interface{}{
		"type": "mixing", "name": "final_" + safePrefix(h.channelID, 8),
	})
	if err != nil {
		return err
	}
	id, _ := result["id"].(string)
	if id == "" {
		return fmt.Errorf("ARI returned no bridge id")
	}
	_, status, err := h.ari.PostParams("/bridges/"+id+"/addChannel", url.Values{"channel": {h.channelID + "," + recipient}})
	if err != nil || status >= 400 {
		h.ari.Delete("/bridges/" + id) //nolint:errcheck
		if h.bridgeID != "" {
			_, restoreStatus, restoreErr := h.ari.PostParams("/bridges/"+h.bridgeID+"/addChannel", url.Values{"channel": {h.channelID}})
			if restoreErr != nil || restoreStatus >= 400 {
				log.Printf("[%s] Restore media failed: status=%d err=%v", h.channelID, restoreStatus, restoreErr)
			}
		}
		return fmt.Errorf("bridge add failed: status=%d err=%v", status, err)
	}
	return nil
}

// collectCallerInfo plays a question, waits for the caller's spoken reply, and returns the transcript.
func (h *Handler) collectCallerInfo(ctx context.Context, question string) string {
	if err := h.playText(ctx, question); err != nil {
		log.Printf("[%s] collectCallerInfo play error: %v", h.channelID, err)
		return ""
	}
	h.turns = append(h.turns, map[string]string{"role": "agent", "text": question})
	if h.rtpReceiver == nil {
		return ""
	}
	// Drain echo of the question we just played before listening for the answer.
	h.rtpReceiver.Drain()
	vadProc, err := vad.New()
	if err != nil {
		log.Printf("[%s] collectCallerInfo VAD error: %v", h.channelID, err)
		return ""
	}
	seg, _ := vadProc.GetSpeechSegmentTimeoutContext(ctx, h.rtpReceiver, silenceTimeout)
	if seg == nil {
		return ""
	}
	wav, err := audio.EncodeWAV(audio.Int16ToFloat32(seg), rtp.SampleRate)
	if err != nil {
		return ""
	}
	text, err := h.sttClient.Transcribe(ctx, wav)
	if err != nil {
		log.Printf("[%s] collectCallerInfo STT error: %v", h.channelID, err)
		return ""
	}
	text = strings.TrimSpace(text)
	if text != "" {
		h.turns = append(h.turns, map[string]string{"role": "caller", "text": text})
		h.history = append(h.history, llm.Message{Role: "user", Content: text})
	}
	return text
}

// attendedTransferInner sets up ExternalMedia for the transfer recipient,
// plays the announcement, listens for their response, and returns the decision
// ("accepted" | "rejected" | "no_response"). The recipient's yes/no is captured
// via the accept_call / decline_call tools — far more reliable than parsing
// decision tags out of spoken text. All resources created here are cleaned up
// before returning.
func (h *Handler) attendedTransferInner(
	ctx context.Context,
	cfg *config.Config,
	transferCID string,
	at *attendedTransferReq,
	announceCh <-chan string,
) string {
	decision := "no_response"

	recipientRTP, err := rtp.New()
	if err != nil {
		log.Printf("[%s] recipient RTP init error: %v", h.channelID, err)
		return decision
	}
	defer recipientRTP.Stop()
	recipientRTP.Start()

	extResult, err := h.ari.PostJSON("/channels/externalMedia", map[string]interface{}{
		"app":           ariApp,
		"external_host": fmt.Sprintf("%s:%d", appHostIP, recipientRTP.Port),
		"format":        "slin16",
	})
	if err != nil {
		log.Printf("[%s] recipient ExternalMedia error: %v", h.channelID, err)
		return decision
	}
	recipientExtID, _ := extResult["id"].(string)
	defer h.ari.Delete("/channels/" + recipientExtID) //nolint:errcheck

	bridgeResult, err := h.ari.PostJSON("/bridges", map[string]interface{}{
		"type": "mixing",
		"name": "atbr_" + transferCID[:8],
	})
	if err != nil {
		log.Printf("[%s] recipient bridge error: %v", h.channelID, err)
		return decision
	}
	recipientBridgeID, _ := bridgeResult["id"].(string)
	defer h.ari.Delete("/bridges/" + recipientBridgeID) //nolint:errcheck

	addParams := url.Values{}
	addParams.Set("channel", transferCID+","+recipientExtID)
	h.ari.PostParams("/bridges/"+recipientBridgeID+"/addChannel", addParams) //nolint:errcheck

	// Build system prompt for the conversation with the transfer recipient.
	// Caller info is framed in the third person so the LLM never confuses itself with the caller.
	var callerInfo string
	switch {
	case at.CallerName != "" && at.Reason != "":
		callerInfo = fmt.Sprintf(
			"L'appelant qui attend en ligne se nomme %s. Il souhaite parler à propos de : %s.",
			at.CallerName, at.Reason,
		)
	case at.CallerName != "":
		callerInfo = fmt.Sprintf("L'appelant qui attend en ligne se nomme %s.", at.CallerName)
	default:
		callerInfo = "Un appelant est en attente."
	}
	recipientPrompt := "You represent reception. " + callerInfo + " You have reached " + at.ContactName + " to ask whether they will take the waiting call. Use one short sentence, follow the recipient's language, and start in " + h.primaryLanguage() + ". Do not impersonate the caller or repeat an introduction. Use only supplied facts. Speech recognition may be imperfect: interpret context and phonetic similarity without inventing consent. Call accept_call for a clear acceptance, decline_call for a clear refusal. If unclear, ask again without deciding."
	recipientSTT := h.sttClient.Clone()
	recipientLanguage := h.primaryLanguage()
	recipientTools := []map[string]interface{}{AcceptCallTool, DeclineCallTool}

	// This dialog is colleague-facing — the transfer recipient tolerates a slower
	// pace, and misreading a garbled "yes" loses the call — so it runs on the
	// on-call tier when configured (same capability-first reasoning as
	// agent-originated missions). A copy keeps the caller-facing client untouched
	// (the announcement generator may still be using it concurrently).
	recipientClient := *h.llmClient
	if err := recipientClient.Configure(cfg.Raw); err != nil {
		log.Printf("[%s] Recipient model configuration unavailable: %v", h.channelID, err)
		return "no_response"
	}
	recipientLLM := &recipientClient

	// Conversation loop with the transfer recipient.
	// Turn 0: play the announcement directly (no LLM — minimal latency).
	// Turn 1+: listen for the recipient's reply, run LLM, read the tool decision.
	// The bound is configurable (UI → Configuration → Agent, key
	// transfer_dialog_turns): the classifier re-asks on ambiguous STT, so a noisy
	// line may want more headroom — at the price of the caller waiting in MoH.
	maxTurns := recipientTurns
	if v := strings.TrimSpace(cfg.Get("transfer_dialog_turns")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 10 {
			maxTurns = n
		} else {
			privacy.Logf("[%s] invalid transfer_dialog_turns %q — using %d", h.channelID, v, recipientTurns)
		}
	}
	var recipientHistory []llm.Message
	for turn := 0; turn < maxTurns; turn++ {
		var say string
		var turnDecision string

		if turn == 0 {
			switch {
			case at.CallerName != "" && at.Reason != "":
				// Nominal case is configurable (cfg key phrase_announce, two %s: name, reason).
				say = renderPhrase(h.phraseIn(cfg, "phrase_announce", recipientLanguage), at.CallerName, at.Reason)
			case at.CallerName != "":
				say = renderPhrase(h.phraseIn(cfg, "phrase_announce_name", recipientLanguage), at.CallerName)
			default:
				say = h.phraseIn(cfg, "phrase_announce_unknown", recipientLanguage)
			}
			// Prefer the LLM-generated announcement if it finished during the ring
			// (generation was started in executeAttendedTransfer only when there's a
			// caller name). The switch above is the fallback if it errored or isn't
			// ready within announceGrace.
			if at.CallerName != "" {
				select {
				case gen := <-announceCh:
					if gen != "" {
						say = gen
						log.Printf("[%s] Using LLM-generated announcement", h.channelID)
					}
				case <-time.After(announceGrace):
					log.Printf("[%s] Announcement not ready — using phrase_announce fallback", h.channelID)
				}
			}
			recipientHistory = append(recipientHistory,
				llm.Message{Role: "user", Content: "Annonce l'appel."},
				llm.Message{Role: "assistant", Content: say},
			)
		} else {
			// Listen for recipient's reply
			vadRecipient, vadErr := vad.New()
			if vadErr != nil {
				log.Printf("[%s] recipient VAD error: %v", h.channelID, vadErr)
				return decision
			}
			audioCh := make(chan []int16, 1)
			go func() { audioCh <- vadRecipient.GetSpeechSegment(recipientRTP) }()

			var seg []int16
			select {
			case seg = <-audioCh:
			case <-time.After(recipientWait):
				log.Printf("[%s] recipient no response (timeout)", h.channelID)
				return decision
			case <-h.transferEnd:
				return decision
			case <-ctx.Done():
				return decision
			}
			if seg == nil {
				return decision
			}
			wav, _ := audio.EncodeWAV(audio.Int16ToFloat32(seg), rtp.SampleRate)
			text, sttErr := recipientSTT.Transcribe(ctx, wav)
			if sttErr != nil {
				log.Printf("[%s] recipient STT error: %v", h.channelID, sttErr)
				return decision
			}
			userMsg := strings.TrimSpace(text)
			if code := locale.Detect(userMsg); code != "" {
				recipientLanguage = code
			}
			if userMsg == "" {
				return decision
			}
			privacy.Logf("[%s] recipient said: %q", h.channelID, userMsg)

			response, recipientCalls, llmErr := recipientLLM.GetResponse(
				ctx, recipientPrompt, recipientHistory, "", "", recipientTools, userMsg,
			)
			if llmErr != nil {
				log.Printf("[%s] recipient LLM error: %v", h.channelID, llmErr)
				return decision
			}
			say = StripGemmaThinking(response)

			for _, tc := range recipientCalls {
				switch tc.Name {
				case "accept_call":
					turnDecision = "accepted"
				case "decline_call":
					turnDecision = "rejected"
				}
			}
			// Closing phrase when the model called a tool without speaking
			if say == "" {
				switch turnDecision {
				case "accepted":
					say = h.phraseIn(cfg, "phrase_recipient_accept", recipientLanguage)
				case "rejected":
					say = h.phraseIn(cfg, "phrase_recipient_decline", recipientLanguage)
				}
			}

			recipientHistory = append(recipientHistory,
				llm.Message{Role: "user", Content: userMsg},
				llm.Message{Role: "assistant", Content: say},
			)
		}

		if say != "" {
			if playErr := h.playTo(ctx, transferCID, say); playErr != nil {
				log.Printf("[%s] recipient play error: %v", h.channelID, playErr)
			}
		}
		// Drain RTP echo before listening for the recipient's next reply
		recipientRTP.Drain()

		if turnDecision != "" {
			log.Printf("[%s] recipient decision: %s", h.channelID, turnDecision)
			return turnDecision
		}

		// Check if channel ended after playback
		select {
		case <-h.transferEnd:
			return "no_response"
		default:
		}
	}
	return decision
}

// ── TTS + ARI playback ─────────────────────────────────────────────────────────

// prepareFiller synthesizes the configured filler phrase once (called async at the
// start of the capture loop) so it's ready to bridge the silence of a slow take.
func (h *Handler) prepareFiller(ctx context.Context, cfg *config.Config) {
	code := h.currentLanguage()
	txt := strings.TrimSpace(h.phrase(cfg, "phrase_filler"))
	if txt == "" {
		return
	}
	w, err := h.ttsClient.Fork().SynthesizeContext(ctx, txt)
	if err != nil {
		log.Printf("[%s] filler synth error: %v", h.channelID, err)
		return
	}
	h.fillerMu.Lock()
	if h.fillerClips == nil {
		h.fillerClips = map[string]playChunk{}
	}
	h.fillerClips[code] = playChunk{wav: w, text: txt}
	h.fillerMu.Unlock()
}

// synthesizeFiller synthesizes text; if it takes longer than fillerDelay (the TTS
// service regenerating a hallucinated take), it queues the pre-cached filler so the
// caller isn't left in silence. Returns the real WAV, or nil on error.
func (h *Handler) synthesizeFiller(ctx context.Context, text string, wavCh chan<- playChunk) []byte {
	res := make(chan []byte, 1)
	go func() {
		w, err := h.ttsClient.SynthesizeContext(ctx, text)
		if err != nil {
			log.Printf("[%s] TTS error: %v", h.channelID, err)
			res <- nil
			return
		}
		res <- w
	}()

	select {
	case <-ctx.Done():
		return nil
	case w := <-res:
		return w
	case <-time.After(fillerDelay):
	}

	h.fillerMu.Lock()
	filler := h.fillerClips[h.currentLanguage()]
	h.fillerMu.Unlock()
	if filler.wav != nil && ctx.Err() == nil {
		log.Printf("[%s] slow TTS — playing filler", h.channelID)
		select {
		case wavCh <- filler:
		case <-ctx.Done():
		}
	}
	select {
	case w := <-res:
		return w
	case <-ctx.Done():
		return nil
	}
}

// Re-prompts use the recent dialogue, not the switchboard persona: a recognised
// caller may be speaking to their personal Prism agent. Keep that agent's register.
const repromptPrompt = "Write only a short spoken nudge after a silence. Briefly recall the current topic and ask whether the person is still on the line. Use the language of the recent conversation. Do not answer the request, restart information gathering or claim to place a new call."

// generateReprompt falls back to the configured phrase on an empty history,
// timeout, failed request or empty output. It never executes tools.
func (h *Handler) generateReprompt(ctx context.Context, cfg *config.Config, attempt int) string {
	fallback := h.phrase(cfg, "phrase_still_there")
	if len(h.history) == 0 {
		return fallback
	}

	prompt := repromptPrompt + " Use the language of the recent conversation. " + h.languageInstruction()
	if h.llmClient.Docked() && h.task == nil {
		prompt += " The caller is recognized. Preserve the assistant's established register; recognition alone does not imply familiarity. Default to polite, formal language when unclear."
	} else {
		prompt += " The caller is unknown. Always use a polite, formal register, even if the caller is informal."
	}
	instruction := "The person has gone silent. Say the nudge now, in the conversation language."
	if attempt > 1 {
		instruction += " This is the second nudge: make it shorter and word it differently."
	}

	rctx, cancel := context.WithTimeout(ctx, repromptTimeout)
	defer cancel()

	text, _, err := h.llmClient.GetResponse(rctx, prompt, h.history, "", "", nil, instruction)
	if err != nil {
		log.Printf("[%s] Re-prompt generation failed (%v) — using phrase_still_there", h.channelID, err)
		return fallback
	}
	text = strings.TrimSpace(StripGemmaThinking(text))
	if text == "" {
		return fallback
	}
	// A model that ignores "one sentence" would monologue at someone who is not
	// even listening; keep only the first sentence.
	if sentences := SplitSentences(text); len(sentences) > 0 {
		text = sentences[0]
	}
	privacy.Logf("[%s] Re-prompt generated: %q", h.channelID, text)
	return text
}

// transcribeSegment runs a captured utterance through STT, returning the trimmed
// transcript ("" on any error). Used by the barge-in monitor to validate an
// interruption while the agent is still speaking.
func (h *Handler) transcribeSegment(ctx context.Context, samples []int16) string {
	wavBytes, err := audio.EncodeWAV(audio.Int16ToFloat32(samples), rtp.SampleRate)
	if err != nil {
		log.Printf("[%s] barge-in WAV encode error: %v", h.channelID, err)
		return ""
	}
	transcript, err := h.sttClient.Clone().Transcribe(ctx, wavBytes)
	if err != nil {
		log.Printf("[%s] barge-in STT error: %v", h.channelID, err)
		return ""
	}
	return strings.TrimSpace(transcript)
}

// playText gives direct synthesis a complete short utterance, including greetings
// and tool follow-ups. Long text and legacy services retain sentence streaming.
func (h *Handler) playText(ctx context.Context, text string) error {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chunks := make(chan playChunk, 4)
	result := make(chan error, 1)
	go func() {
		defer close(chunks)
		text = strings.TrimSpace(text)
		sentences := SplitSentences(text)
		if h.groupShortSpeech && text != "" && utf8.RuneCountInString(text) <= shortSpeechChars {
			sentences = []string{text}
		}
		for _, sentence := range sentences {
			if err := h.synthesizeSpeech(pctx, sentence, chunks, nil); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()
	h.playQueued(pctx, h.channelID, h.withWaitCue(pctx, chunks))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return <-result
}

// synthesizeSpeech preserves the legacy WAV path for ARI and streams PCM to RTP.
func (h *Handler) synthesizeSpeech(ctx context.Context, text string, out chan<- playChunk, onAudio func()) error {
	h.observeSpeech(ctx, text)
	emit := func(chunk playChunk) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if onAudio != nil {
			onAudio()
		}
		select {
		case out <- chunk:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if h.rtpReceiver != nil {
		return h.ttsClient.Stream(ctx, text, func(pcm []int16) error { return emit(playChunk{pcm: pcm, text: text}) })
	}
	wav, err := h.ttsClient.SynthesizeContext(ctx, text)
	if err != nil {
		return err
	}
	if len(wav) == 0 {
		return nil
	}
	return emit(playChunk{wav: wav, text: text})
}

// playTo synthesizes a single piece of text and plays it to the named channel.
func (h *Handler) playTo(ctx context.Context, channelID, text string) error {
	privacy.Logf("[%s] TTS to %s (%d chars): %q", h.channelID, channelID, len(text), text)
	wavBytes, err := h.ttsClient.SynthesizeContext(ctx, text)
	if err != nil || len(wavBytes) == 0 {
		return err
	}
	return h.playWAV(ctx, wavBytes, channelID)
}

// playChunk is one synthesized audio segment plus its source text (for logging).
type playChunk struct {
	pcm  []int16
	wav  []byte
	text string
}

// prepareWAVFile resamples a TTS WAV to 16kHz mono PCM_16 and writes it to the
// sounds directory, returning the on-disk path and the bare filename Asterisk
// references via "sound:custom/<filename>". The caller owns removing the file.
// 16kHz is wideband-ready: Asterisk feeds it as G.722 when the call negotiates it,
// and downsamples to 8kHz for G.711 otherwise (no regression).
func (h *Handler) prepareWAVFile(wavBytes []byte, channelID string) (fpath, filename string, err error) {
	const playbackRate = 16000
	samples, srcRate, err := audio.DecodeWAV(wavBytes)
	if err != nil {
		return "", "", fmt.Errorf("prepareWAVFile: decodeWAV: %w", err)
	}
	if srcRate != playbackRate {
		samples = audio.Resample(samples, srcRate, playbackRate)
	}
	outWAV, err := audio.EncodeWAV(samples, playbackRate)
	if err != nil {
		return "", "", fmt.Errorf("prepareWAVFile: encodeWAV: %w", err)
	}
	safeID := strings.NewReplacer(".", "_", "-", "_").Replace(channelID)
	filename = fmt.Sprintf("agent_%s_%s", safeID, randHex(8))
	// .wav16 = Asterisk's 16kHz WAV format. Plain ".wav" is assumed to be 8kHz, so a
	// 16kHz file under ".wav" fails to open ("frequency mismatch") → no audio at all.
	// The sound: reference stays extension-less; Asterisk resolves the .wav16 file.
	fpath = filepath.Join(soundsDir, filename+".wav16")
	if err := os.WriteFile(fpath, outWAV, 0644); err != nil {
		return "", "", fmt.Errorf("prepareWAVFile: write: %w", err)
	}
	log.Printf("[%s] WAV written: %s (%d B)", h.channelID, fpath, len(outWAV))
	return fpath, filename, nil
}

// startPlayback asks Asterisk (via ARI) to play a prepared sound file on the
// channel and returns the playback ID. It does not wait for completion;
// successive calls on the same channel are queued and played in order.
func (h *Handler) startPlayback(channelID, filename string) (string, error) {
	result, err := h.ari.PostJSON("/channels/"+channelID+"/play", map[string]interface{}{
		"media": "sound:custom/" + filename,
	})
	if err != nil {
		return "", fmt.Errorf("startPlayback: ARI play: %w", err)
	}
	pbID, _ := result["id"].(string)
	return pbID, nil
}

// wavToEgressPCM decodes a TTS WAV to mono int16 at the RTP egress rate, ready to be
// framed for the caller (mirror of prepareWAVFile, minus the file). The rate is fixed
// by the static payload type we must send with — see rtp.OutSampleRate.
func wavToEgressPCM(wavBytes []byte) ([]int16, error) {
	samples, srcRate, err := audio.DecodeWAV(wavBytes)
	if err != nil {
		return nil, fmt.Errorf("wavToEgressPCM: decodeWAV: %w", err)
	}
	if srcRate != rtp.OutSampleRate {
		samples = audio.Resample(samples, srcRate, rtp.OutSampleRate)
	}
	return audio.Float32ToInt16(samples), nil
}

// waitCallerDrained blocks until the outbound RTP buffer has been fully sent to the
// caller, or ctx is cancelled (barge-in) — in which case it flushes the buffer so
// the agent goes silent immediately. Polling is cheap (one mutex read / 10ms).
//
// A stall guard bails out if the buffer stops draining for drainStallTimeout: during
// real playback the sender pops a frame every 20ms so it never stalls, but if the
// remote RTP address was never learned (Asterisk sent us nothing) the buffer would
// otherwise never drain and wedge the call — so we give up and flush instead.
func (h *Handler) waitCallerDrained(ctx context.Context) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	lastPending := h.rtpReceiver.PendingSamples()
	lastProgress := time.Now()
	for {
		select {
		case <-ctx.Done():
			h.rtpReceiver.FlushOut()
			return ctx.Err()
		case <-t.C:
			p := h.rtpReceiver.PendingSamples()
			if p == 0 {
				return nil
			}
			if p < lastPending {
				lastPending = p
				lastProgress = time.Now()
			} else if time.Since(lastProgress) > drainStallTimeout {
				log.Printf("[%s] caller RTP drain stalled (%d samples pending) — flushing", h.channelID, p)
				h.rtpReceiver.FlushOut()
				return nil
			}
		}
	}
}

// playWAVToCaller streams one TTS WAV to the caller over the ExternalMedia RTP leg
// and blocks until it has been sent (or barge-in cancels ctx). The RTP replacement
// for the ARI /play path on the caller channel.
func (h *Handler) playWAVToCaller(ctx context.Context, wavBytes []byte) error {
	pcm, err := wavToEgressPCM(wavBytes)
	if err != nil {
		return err
	}
	h.rtpReceiver.Enqueue(pcm)
	return h.waitCallerDrained(ctx)
}

// playWAV plays a single TTS WAV on the channel and blocks until PlaybackFinished
// or timeout. Used for one-shot playback (greeting, follow-ups, announcements).
// Caller-facing audio (channelID == h.channelID) rides the ExternalMedia RTP leg;
// other channels (e.g. an attended-transfer announcement to the recipient) still
// use ARI /play.
func (h *Handler) playWAV(ctx context.Context, wavBytes []byte, channelID string) error {
	if channelID == h.channelID && h.rtpReceiver != nil {
		return h.playWAVToCaller(ctx, wavBytes)
	}
	fpath, filename, err := h.prepareWAVFile(wavBytes, channelID)
	if err != nil {
		return err
	}
	defer os.Remove(fpath) //nolint:errcheck

	pbID, err := h.startPlayback(channelID, filename)
	if err != nil {
		return err
	}
	if pbID == "" {
		return nil
	}

	ch := make(chan struct{}, 1)
	h.pbMu.Lock()
	h.pbFutures[pbID] = ch
	h.pbMu.Unlock()
	defer func() {
		h.pbMu.Lock()
		delete(h.pbFutures, pbID)
		h.pbMu.Unlock()
	}()

	select {
	case <-ch:
	case <-time.After(playTimeout):
		log.Printf("[%s] PlaybackFinished timeout for %s", h.channelID, pbID)
	case <-ctx.Done():
		// Interrupted (barge-in): stop the Asterisk playback so the agent goes
		// silent immediately instead of finishing the sentence.
		h.ari.Delete("/playbacks/" + pbID) //nolint:errcheck
		return ctx.Err()
	}
	return nil
}

// playQueued plays a stream of synthesized segments back-to-back with no gap.
//
// Instead of the serial "play sentence N → wait PlaybackFinished → play N+1"
// cycle (whose per-sentence ARI round-trip is heard as silence between
// sentences), it posts an ARI playback for each chunk as soon as it arrives —
// without waiting for the previous one — so Asterisk queues them and plays them
// continuously. The first sentence still starts as early as before (preserving
// streaming latency); later sentences are queued while earlier ones play.
//
// It blocks until every queued playback has finished. If ctx is cancelled
// (barge-in), all queued and playing playbacks are stopped immediately. Each
// WAV file is removed once its own playback finishes.
func (h *Handler) playQueued(ctx context.Context, channelID string, in <-chan playChunk) {
	if channelID == h.channelID && h.rtpReceiver != nil {
		h.playQueuedToCaller(ctx, in)
		return
	}
	type qItem struct {
		pbID  string
		fpath string
		done  chan struct{}
	}

	var mu sync.Mutex
	var queue []qItem
	stopped := false

	// Barge-in killer: the instant ctx is cancelled, stop every queued/playing
	// playback so the agent goes silent immediately. (After a clean finish ctx is
	// also cancelled by the caller; deleting already-finished playbacks is a
	// harmless no-op.)
	go func() {
		<-ctx.Done()
		mu.Lock()
		stopped = true
		ids := make([]string, 0, len(queue))
		for _, q := range queue {
			ids = append(ids, q.pbID)
		}
		mu.Unlock()
		for _, id := range ids {
			h.ari.Delete("/playbacks/" + id) //nolint:errcheck
		}
	}()

	// Post phase: queue one playback per chunk as soon as it is synthesized.
	for chunk := range in {
		mu.Lock()
		s := stopped
		mu.Unlock()
		if s {
			continue // barge-in: drain remaining chunks without playing
		}
		fpath, filename, err := h.prepareWAVFile(chunk.wav, channelID)
		if err != nil {
			privacy.Logf("[%s] Playback prepare error (%q): %v", h.channelID, chunk.text, err)
			continue
		}
		pbID, err := h.startPlayback(channelID, filename)
		if err != nil {
			privacy.Logf("[%s] Playback start error (%q): %v", h.channelID, chunk.text, err)
			os.Remove(fpath) //nolint:errcheck
			continue
		}
		if pbID == "" {
			os.Remove(fpath) //nolint:errcheck
			continue
		}
		done := make(chan struct{}, 1)
		h.pbMu.Lock()
		h.pbFutures[pbID] = done
		h.pbMu.Unlock()

		mu.Lock()
		if stopped {
			// Barge-in fired between the check and now: cancel this one too.
			mu.Unlock()
			h.ari.Delete("/playbacks/" + pbID) //nolint:errcheck
			h.pbMu.Lock()
			delete(h.pbFutures, pbID)
			h.pbMu.Unlock()
			os.Remove(fpath) //nolint:errcheck
			continue
		}
		queue = append(queue, qItem{pbID: pbID, fpath: fpath, done: done})
		mu.Unlock()
	}

	// Wait phase: every chunk has been posted (in is closed). Wait for each
	// playback to finish in order, cleaning up as we go. A finish that already
	// happened is buffered in done (cap 1), so none is missed.
	for _, q := range queue {
		select {
		case <-q.done:
		case <-time.After(playTimeout):
			log.Printf("[%s] PlaybackFinished timeout for %s", h.channelID, q.pbID)
		case <-ctx.Done():
			// Barge-in: the killer goroutine stops the playbacks; stop waiting.
		}
		h.pbMu.Lock()
		delete(h.pbFutures, q.pbID)
		h.pbMu.Unlock()
		os.Remove(q.fpath) //nolint:errcheck
	}
}

// playQueuedToCaller is the RTP equivalent of playQueued for the caller channel:
// each synthesized chunk is decoded and appended to the outbound RTP buffer as it
// arrives, so the sender paces them out back-to-back with no gap. It blocks until
// every queued chunk has been sent, or until ctx is cancelled (barge-in), which
// flushes the buffer and silences the agent within one 20ms frame.
func (h *Handler) playQueuedToCaller(ctx context.Context, in <-chan playChunk) {
	rtpRx := h.rtpReceiver
	// Barge-in kill switch: drop all pending audio the instant ctx is cancelled.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			rtpRx.FlushOut()
		case <-watchDone:
		}
	}()

	stalled := false
	for chunk := range in {
		if ctx.Err() != nil || stalled {
			continue // barge-in: drain remaining chunks without playing
		}
		pcm := chunk.pcm
		var err error
		if pcm == nil {
			pcm, err = wavToEgressPCM(chunk.wav)
		}
		if err != nil {
			privacy.Logf("[%s] caller PCM decode error (%q): %v", h.channelID, chunk.text, err)
			continue
		}
		if err := h.rtpReceiver.EnqueueContext(ctx, pcm); err != nil {
			stalled = true
			rtpRx.FlushOut()
			if ctx.Err() == nil {
				log.Printf("[%s] Audio playback failed: %v", h.channelID, err)
			}
			continue
		}
	}
	// Block until the sender has drained everything queued (or barge-in cancels).
	h.waitCallerDrained(ctx) //nolint:errcheck
}

// ── Call log ───────────────────────────────────────────────────────────────────

func (h *Handler) finalizeLog(ctx context.Context) {
	if h.pool == nil || h.logID == 0 {
		// Still resolve the outbound task so it never stays stuck in 'calling'.
		h.finalizeTask(ctx, nil, nil)
		return
	}
	lid := h.logID
	h.logID = 0 // prevent double update

	endReason := "hung_up"
	switch {
	case h.transferDest != "":
		endReason = "transferred"
	case h.endCall:
		endReason = "agent_hung_up"
	}

	var summary *string
	if len(h.turns) > 0 {
		// Leave time for the durable log/task writes if summarization stalls.
		summaryCtx, summaryCancel := context.WithTimeout(ctx, 8*time.Second)
		s, err := h.llmClient.GetCallSummary(summaryCtx, h.callTranscript(), h.callerNumber, h.toolsUsed, h.ragUsed, endReason)
		summaryCancel()
		if err != nil {
			log.Printf("[%s] Summary error: %v", h.channelID, err)
		} else if s != "" {
			summary = &s
			privacy.Logf("[%s] Summary: %q", h.channelID, s)
		}
	}

	if err := db.FinalizeCallLog(ctx, h.pool, lid, endReason, h.toolsUsed, h.ragUsed, summary, h.message, h.turns); err != nil {
		log.Printf("[%s] FinalizeCallLog error: %v", h.channelID, err)
	}

	h.finalizeTask(ctx, summary, &lid)
}

// finalizeTask writes the outcome of an outbound call back to call_tasks.
// 'done' requires an actual exchange (or an LLM-driven end_call); a call that
// connected but produced no usable turn — typically voicemail — is retried.
func (h *Handler) finalizeTask(ctx context.Context, summary *string, callLogID *int64) {
	if h.task == nil || h.pool == nil {
		return
	}
	task := h.task
	h.task = nil // prevent double update

	userTurns := 0
	for _, t := range h.turns {
		if t["role"] == "caller" {
			userTurns++
		}
	}

	if h.endCall || userTurns > 0 {
		if err := db.FinishCallTask(ctx, h.pool, task.ID, "done", summary, callLogID); err != nil {
			log.Printf("[%s] FinishCallTask error: %v", h.channelID, err)
		}
		log.Printf("[%s] Outbound task %d done (%d caller turns)", h.channelID, task.ID, userTurns)
		return
	}
	if err := db.RetryCallTask(ctx, h.pool, task.ID, "no_answer", taskRetryDelay); err != nil {
		log.Printf("[%s] RetryCallTask error: %v", h.channelID, err)
	}
	log.Printf("[%s] Outbound task %d: no usable exchange — scheduled for retry", h.channelID, task.ID)
}

// ── Cleanup ────────────────────────────────────────────────────────────────────

// cleanup runs once, after the conversation has stopped. ARI callbacks only Stop.
func (h *Handler) cleanup(ctx context.Context) {
	h.mu.Lock()
	h.running = false
	h.mu.Unlock()
	if h.rtpReceiver != nil {
		h.rtpReceiver.Stop()
	}

	h.finalizeLog(ctx)

	// Unblock any pending playback waits
	h.pbMu.Lock()
	for id, ch := range h.pbFutures {
		delete(h.pbFutures, id)
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.pbMu.Unlock()

	if h.bridgeID != "" {
		h.ari.Delete("/bridges/" + h.bridgeID) //nolint:errcheck
		h.bridgeID = ""
	}
	if h.extChanID != "" {
		h.ari.Delete("/channels/" + h.extChanID) //nolint:errcheck
		h.extChanID = ""
	}
	if h.snoopChanID != "" {
		h.ari.Delete("/channels/" + h.snoopChanID) //nolint:errcheck
		h.snoopChanID = ""
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────────

func (h *Handler) buildTools() []map[string]interface{} {
	mcpTools := h.mcpMgr.Tools()
	result := make([]map[string]interface{}, 0, len(mcpTools)+4)
	result = append(result, mcpTools...)
	if h.task == nil { // transfers only make sense on inbound calls
		result = append(result, TransferTool)
	}
	if h.ragEnabled && h.pool != nil {
		result = append(result, SearchKnowledgeTool)
	}
	result = append(result, TakeMessageTool, EndCallTool)
	return result
}

// buildToolsNoTransfer is used right after a failed transfer so the LLM cannot
// immediately retry the same transfer; it can still search, take a message or end the call.
func (h *Handler) buildToolsNoTransfer() []map[string]interface{} {
	mcpTools := h.mcpMgr.Tools()
	result := make([]map[string]interface{}, 0, len(mcpTools)+3)
	result = append(result, mcpTools...)
	if h.ragEnabled && h.pool != nil {
		result = append(result, SearchKnowledgeTool)
	}
	result = append(result, TakeMessageTool, EndCallTool)
	return result
}

// percentileMs returns the p-th percentile (0–100) of vals (durations in ms).
func percentileMs(vals []int64, p int) int64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p * (len(s) - 1)) / 100
	return s[idx]
}

// userTurns extracts what the caller actually said, for checking claims the model
// makes about them.
func userTurns(history []llm.Message) []string {
	var out []string
	for _, m := range history {
		if m.Role != "user" {
			continue
		}
		if s, ok := m.Content.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringFromMap(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n/2+1)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)[:n]
}

// dialableRe matches destinations the [transfer] dialplan can dial: digits with an
// optional leading "+" (the _X. / _+X. patterns in asterisk/etc/extensions.conf).
var dialableRe = regexp.MustCompile(`^\+?[0-9]+$`)

func isDialable(s string) bool {
	return dialableRe.MatchString(strings.TrimSpace(s))
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func drainStringChan(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// knownGreeting renders the greeting for a recognised caller, whose name is known
// to be non-empty. One literal %s is replaced by the name; a template without one
// is spoken as it was written.
//
// Deliberately NOT fmt.Sprintf: this template is admin-authored prose that a TTS
// reads aloud, and a perfectly reasonable "ouvert 100% du temps" would come out as
// "100%!d(MISSING)u temps". A literal replacement touches the placeholder and
// nothing else.
func knownGreeting(template, name string) string {
	return strings.Replace(template, "%s", name, 1)
}

// unknownContactResult is what the model is told when a transfer target cannot be
// resolved. It hands the failure back rather than speaking a canned line: the
// model holds the directory and can offer a near match ("pas de Jean, mais j'ai un
// Jean-Marc au support") or take a message. Wording validated against the live
// model — a softer "si un contact ressemble, propose-le" was ignored, while
// spelling out the near-match rule produces "Vouliez-vous parler à Jean-Marc
// Dupont ?", which is the whole point.
func unknownContactResult(dest string) string {
	return fmt.Sprintf(
		"Outil transfer_call : ÉCHEC — aucune correspondance unique pour %q (nom absent ou ambigu). Aucun transfert. Demande le nom complet si plusieurs personnes correspondent.\n"+
			"Avant de répondre, relis l'annuaire : si un contact porte un nom proche (prénom identique, "+
			"nom composé, orthographe voisine), NOMME-LE explicitement et demande si c'est bien lui. "+
			"Sinon seulement, propose de prendre un message. Ne rappelle pas transfer_call sans l'accord de l'appelant.", dest)
}

// searchKnowledge answers a knowledge lookup from wherever this deployment keeps
// the switchboard's knowledge.
//
// Linked to a Prism, that is Prism: the base is configured there, once, and an
// admin should not have to know which stack stores what. Standalone, it is Vox's
// own pgvector, exactly as before.
//
// Either way a failure returns the empty string, which the caller turns into "je
// n'ai pas cette information". On a phone line that is the right failure: the
// agent says something true and offers to take a message, instead of leaving a
// silence while something retries.
func (h *Handler) searchKnowledge(ctx context.Context, query string) string {
	if h.llmClient.Linked() {
		hits, err := h.llmClient.SearchKnowledge(ctx, query)
		if err != nil {
			log.Printf("[%s] KB search via Prism failed: %v", h.channelID, err)
			return ""
		}
		var b strings.Builder
		for _, hit := range hits {
			if hit.Source != "" {
				fmt.Fprintf(&b, "[%s] ", hit.Source)
			}
			b.WriteString(hit.Content)
			b.WriteString("\n\n")
		}
		return strings.TrimSpace(b.String())
	}

	if h.pool == nil {
		return ""
	}
	var qvec []float32
	key := ""
	if v, err := h.embClient.EmbedOne(ctx, query); err != nil {
		log.Printf("[%s] KB search embed error: %v", h.channelID, err)
	} else {
		qvec = v
		key = h.embClient.Key(len(v))
	}
	result, err := rag.GetContext(ctx, h.pool, qvec, key, ragTopK)
	if err != nil {
		log.Printf("[%s] KB search error: %v", h.channelID, err)
		return ""
	}
	return result
}
