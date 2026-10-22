// Command agent connects to Asterisk ARI via WebSocket and dispatches call events.
//
// Environment variables:
//
//	ARI_HOST  — Asterisk hostname (default: asterisk)
//	ARI_PORT  — ARI HTTP port (default: 8088)
//	ARI_USER  — ARI username (default: phoneagent)
//	ARI_PASS  — ARI password (default: change_me)
//	ARI_APP   — ARI application name (default: phoneagent)
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"phoneagent/internal/privacy"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"phoneagent/internal/ari"
	"phoneagent/internal/call"
	"phoneagent/internal/config"
	"phoneagent/internal/db"
)

const (
	outboundPollInterval = 5 * time.Second
	outboundAnswerWait   = 35 * time.Second // originate timeout (30s) + margin
	outboundRetryDelay   = 5 * time.Minute
)

// outboundPending tracks an originated call between originate and StasisStart.
type outboundPending struct {
	task     *db.CallTask
	answered chan *call.Handler
}

// coordinator shares call state between the ARI event loop and the outbound
// poller: the poller only dials when no call is active, and the event loop
// hands answered outbound channels back to the poller.
//
// Active calls are tracked as a SET of channel IDs (not a bare counter) so the
// state can be reconciled against Asterisk's reality after an ARI reconnect —
// a missed StasisEnd during a disconnect would otherwise leave the counter
// permanently > 0 and wedge the poller.
type coordinator struct {
	mu      sync.Mutex
	active  map[string]struct{}         // active caller-facing channel IDs
	pending map[string]*outboundPending // outbound channelID → pending state
}

func newCoordinator() *coordinator {
	return &coordinator{
		active:  make(map[string]struct{}),
		pending: make(map[string]*outboundPending),
	}
}

func (c *coordinator) addActive(cid string) {
	c.mu.Lock()
	c.active[cid] = struct{}{}
	c.mu.Unlock()
}

func (c *coordinator) removeActive(cid string) {
	c.mu.Lock()
	delete(c.active, cid)
	c.mu.Unlock()
}

// reconcile drops active channel IDs that Asterisk no longer knows about. Called
// on each ARI (re)connect so a missed StasisEnd can never wedge the poller.
func (c *coordinator) reconcile(live map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cid := range c.active {
		if _, ok := live[cid]; !ok {
			log.Printf("coordinator: dropping stale active channel %s (gone from Asterisk)", cid)
			delete(c.active, cid)
		}
	}
}

func (c *coordinator) activeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active)
}

func (c *coordinator) addPending(cid string, p *outboundPending) {
	c.mu.Lock()
	c.pending[cid] = p
	c.mu.Unlock()
}

func (c *coordinator) removePending(cid string) {
	c.mu.Lock()
	delete(c.pending, cid)
	c.mu.Unlock()
}

func (c *coordinator) getPending(cid string) *outboundPending {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending[cid]
}

func (c *coordinator) ownedTasks() map[int64]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make(map[int64]bool)
	for _, p := range c.pending {
		ids[p.task.ID] = true
	}
	return ids
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Prism Vox starting")

	ctx := context.Background()

	// Database connection (optional — agent degrades gracefully without it)
	var pool *pgxpool.Pool
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		var err error
		pool, err = db.Connect(ctx)
		if err != nil && pool != nil {
			log.Printf("WARNING: PostgreSQL not reachable at startup (%v) — agent will reconnect on demand", err)
		} else if err != nil {
			log.Printf("WARNING: PostgreSQL unavailable: %v — running without DB", err)
		} else {
			log.Println("PostgreSQL connected")
		}
	}

	ariHost := envDefault("ARI_HOST", "asterisk")
	ariPort := envDefault("ARI_PORT", "8088")
	ariUser := envDefault("ARI_USER", "phoneagent")
	ariPass := envDefault("ARI_PASS", "change_me")
	ariAppEnv := envDefault("ARI_APP", "phoneagent")

	baseURL := fmt.Sprintf("http://%s:%s/ari", ariHost, ariPort)
	wsURL := fmt.Sprintf(
		"ws://%s:%s/ari/events?app=%s&api_key=%s:%s&subscribeAll=false",
		ariHost, ariPort, ariAppEnv, ariUser, ariPass,
	)

	ariClient := ari.New(baseURL, ariUser, ariPass)

	coord := newCoordinator()
	if pool != nil {
		go outboundPoller(ctx, ariClient, pool, coord, ariAppEnv)
	}

	run(ctx, wsURL, ariUser, ariPass, ariClient, pool, coord)
}

// ── Outbound call queue ────────────────────────────────────────────────────────

// outboundPoller drains the call_tasks queue: one call at a time, and only
// when no other call (inbound or outbound) is active.
func outboundPoller(
	ctx context.Context,
	ariClient *ari.Client,
	pool *pgxpool.Pool,
	coord *coordinator,
	ariApp string,
) {
	log.Println("Outbound poller started")
	ticker := time.NewTicker(outboundPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		live, err := ariClient.LiveChannelIDs()
		if err != nil {
			log.Printf("outbound: cannot reconcile Asterisk; deferring calls: %v", err)
			continue
		}
		if n, err := db.RecoverInterruptedTasks(ctx, pool, live, coord.ownedTasks()); err != nil {
			log.Printf("outbound: recovery failed: %v", err)
			continue
		} else if n > 0 {
			log.Printf("outbound: recovered %d interrupted task(s); operator review required", n)
		}
		if coord.activeCount() > 0 || len(live) > 0 {
			continue
		}

		task, err := db.ClaimNextCallTask(ctx, pool)
		if err != nil {
			log.Printf("outbound: claim error: %v", err)
			continue
		}
		if task == nil {
			continue
		}
		runOutboundCall(ctx, ariClient, pool, coord, ariApp, task)
	}
}

// runOutboundCall originates one call for the claimed task and blocks until the
// call finishes (or the originate fails / times out), keeping concurrency at 1.
func runOutboundCall(
	ctx context.Context,
	ariClient *ari.Client,
	pool *pgxpool.Pool,
	coord *coordinator,
	ariApp string,
	task *db.CallTask,
) {
	outCID := fmt.Sprintf("outbound-%d-%d", task.ID, time.Now().UnixNano())
	privacy.Logf("outbound: task %d → dialing %s (%q, attempt %d/%d)",
		task.ID, task.PhoneNumber, task.ContactName, task.Attempts, task.MaxAttempts)

	p := &outboundPending{task: task, answered: make(chan *call.Handler, 1)}
	coord.addPending(outCID, p)
	defer coord.removePending(outCID)

	// Caller ID for the outbound From header. Set it on the originated channel
	// (presentation "allowed") so PJSIP doesn't stamp the default "Anonymous" — the
	// endpoint `callerid` alone doesn't win for ARI-originated channels. Empty
	// sip_callerid_name → just the number (no display name).
	originate := map[string]interface{}{
		"endpoint":  "PJSIP/" + task.PhoneNumber + "@phoneagent-endpoint",
		"app":       ariApp,
		"appArgs":   "outbound," + strconv.FormatInt(task.ID, 10),
		"channelId": outCID,
		"timeout":   30,
	}
	if cfg, cErr := config.Load(ctx, pool); cErr == nil && cfg != nil {
		if cid := call.BuildCallerID(cfg); cid != "" {
			originate["callerId"] = cid
		}
	}

	_, status, err := ariClient.Post("/channels", originate)
	if err != nil || status >= 400 {
		log.Printf("outbound: task %d originate failed (status=%d): %v", task.ID, status, err)
		if rErr := db.RetryCallTask(ctx, pool, task.ID, "failed", outboundRetryDelay); rErr != nil {
			log.Printf("outbound: task %d retry update error: %v", task.ID, rErr)
		}
		return
	}

	select {
	case h := <-p.answered:
		// Call is live — the handler owns the task result from here.
		<-h.Done()
	case <-time.After(outboundAnswerWait):
		log.Printf("outbound: task %d no answer", task.ID)
		ariClient.Delete("/channels/" + outCID) //nolint:errcheck
		if rErr := db.RetryCallTask(ctx, pool, task.ID, "no_answer", outboundRetryDelay); rErr != nil {
			log.Printf("outbound: task %d retry update error: %v", task.ID, rErr)
		}
	case <-ctx.Done():
		ariClient.Delete("/channels/" + outCID) //nolint:errcheck
	}
}

// run is the main ARI event loop with automatic reconnection.
func run(
	ctx context.Context,
	wsURL, ariUser, ariPass string,
	ariClient *ari.Client,
	pool *pgxpool.Pool,
	coord *coordinator,
) {
	activeCalls := make(map[string]*call.Handler)
	// transferChans maps transfer_cid → parent_cid
	transferChans := make(map[string]string)

	for {
		log.Println("Connecting to Asterisk ARI…")

		// Reconcile active-call bookkeeping with Asterisk's reality: drop any
		// channels (and their handlers) that no longer exist — e.g. calls that
		// ended while the WebSocket was down, whose StasisEnd we never saw.
		if live, err := ariClient.LiveChannelIDs(); err != nil {
			log.Printf("ARI reconcile: cannot list channels: %v", err)
		} else {
			for cid := range activeCalls {
				if _, ok := live[cid]; !ok {
					activeCalls[cid].Stop()
					delete(activeCalls, cid)
				}
			}
			coord.reconcile(live)
		}

		if err := connectAndServe(ctx, wsURL, ariUser, ariPass, ariClient, pool, coord, activeCalls, transferChans); err != nil {
			log.Printf("ARI error: %v — reconnecting in 5s", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func connectAndServe(
	ctx context.Context,
	wsURL, ariUser, ariPass string,
	ariClient *ari.Client,
	pool *pgxpool.Pool,
	coord *coordinator,
	activeCalls map[string]*call.Handler,
	transferChans map[string]string,
) error {
	// Build Basic Auth header for the WebSocket handshake
	creds := base64.StdEncoding.EncodeToString([]byte(ariUser + ":" + ariPass))
	header := http.Header{
		"Authorization": []string{"Basic " + creds},
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return fmt.Errorf("WebSocket dial: %w", err)
	}
	defer conn.Close()

	log.Println("Connected to Asterisk ARI — waiting for calls")

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("WebSocket read: %w", err)
		}

		var event map[string]interface{}
		if err := json.Unmarshal(msgBytes, &event); err != nil {
			log.Printf("JSON parse error: %v", err)
			continue
		}

		etype, _ := event["type"].(string)

		switch etype {
		case "StasisStart":
			channel, _ := event["channel"].(map[string]interface{})
			if channel == nil {
				continue
			}
			cid, _ := channel["id"].(string)
			name, _ := channel["name"].(string)
			args := extractArgs(event)

			privacy.Logf("StasisStart — id=%s name=%q args=%v", cid, name, args)

			// Ignore ExternalMedia + Snoop channels we created (the handler that
			// spawned them bridges them itself using the IDs from the POST response).
			if strings.HasPrefix(name, "UnicastRTP/") || strings.HasPrefix(name, "Snoop/") {
				continue
			}

			// Attended transfer leg answered
			if len(args) > 0 && args[0] == "attended_transfer" {
				parentID := ""
				if len(args) > 1 {
					parentID = args[1]
				}
				if parentID != "" {
					if handler, ok := activeCalls[parentID]; ok {
						transferChans[cid] = parentID
						handler.OnTransferChannelStart(cid)
					}
				}
				continue
			}

			// Outbound call answered — hand the channel to an outbound handler
			if len(args) > 0 && args[0] == "outbound" {
				p := coord.getPending(cid)
				if p == nil {
					log.Printf("[%s] Outbound StasisStart for unknown task — hanging up", cid)
					ariClient.Delete("/channels/" + cid) //nolint:errcheck
					continue
				}
				handler := call.NewOutboundHandler(cid, ariClient, pool, p.task)
				activeCalls[cid] = handler
				coord.addActive(cid)
				go handler.Run(ctx)
				p.answered <- handler
				continue
			}

			// Normal incoming call
			caller, _ := channel["caller"].(map[string]interface{})
			callerNumber := ""
			if caller != nil {
				callerNumber, _ = caller["number"].(string)
			}

			handler := call.NewHandler(cid, ariClient, pool, callerNumber)
			activeCalls[cid] = handler
			coord.addActive(cid)
			go handler.Run(ctx)

		case "StasisEnd":
			channel, _ := event["channel"].(map[string]interface{})
			if channel == nil {
				continue
			}
			cid, _ := channel["id"].(string)

			if parentID, ok := transferChans[cid]; ok {
				delete(transferChans, cid)
				log.Printf("[%s] Transfer channel StasisEnd", cid)
				if handler, ok := activeCalls[parentID]; ok {
					handler.OnTransferChannelEnd(cid)
				}
			} else if handler, ok := activeCalls[cid]; ok {
				log.Printf("[%s] StasisEnd", cid)
				handler.Stop()
				delete(activeCalls, cid)
				coord.removeActive(cid)
			}

		case "ChannelHangupRequest":
			channel, _ := event["channel"].(map[string]interface{})
			if channel == nil {
				continue
			}
			cid, _ := channel["id"].(string)

			if _, ok := transferChans[cid]; ok {
				// handled on StasisEnd
			} else if handler, ok := activeCalls[cid]; ok {
				log.Printf("[%s] Hangup", cid)
				handler.Stop()
			}

		case "PlaybackFinished", "PlaybackFailed":
			pb, _ := event["playback"].(map[string]interface{})
			if pb == nil {
				continue
			}
			targetURI, _ := pb["target_uri"].(string)
			pbID, _ := pb["id"].(string)

			if strings.HasPrefix(targetURI, "channel:") {
				cid := strings.TrimPrefix(targetURI, "channel:")
				if parentID, ok := transferChans[cid]; ok {
					if handler, ok := activeCalls[parentID]; ok {
						handler.OnPlaybackFinished(pbID)
					}
				} else if handler, ok := activeCalls[cid]; ok {
					handler.OnPlaybackFinished(pbID)
				}
			}
		}
	}
}

// extractArgs extracts the args array from a StasisStart event.
func extractArgs(event map[string]interface{}) []string {
	argsRaw, ok := event["args"]
	if !ok {
		return nil
	}
	argsSlice, ok := argsRaw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(argsSlice))
	for _, a := range argsSlice {
		if s, ok := a.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
