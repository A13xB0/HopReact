package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// A bot shows as online in Discord for exactly as long as it holds a live
// Gateway (WebSocket) connection with a completed IDENTIFY. Nothing else
// makes it appear online — a bot that only calls the REST API, however
// healthy, is shown offline forever. That is why this file exists: HopReact
// otherwise never needs the Gateway at all.
//
// So presence here is really a liveness indicator. If the bot is green in
// the member list, the process is up and talking to Discord. If it greys
// out, something is wrong before anyone's repeater has to prove it.
//
// Everything in here is best-effort and must stay that way. Presence is
// cosmetic; alerts are not. A Gateway that cannot connect, or drops, or
// gets rate-limited, must never interfere with polling or with sending —
// which is why it runs in its own goroutine, owns no shared state, and
// only ever logs.

// Gateway opcodes (the subset that matters here).
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opPresenceUpdate = 3
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// activityWatching renders as "Watching <name>" in the member list, which
// is the honest verb for what this service does.
const activityWatching = 3

// gatewayVersion pins the protocol; v10 matches the REST base above.
const gatewayVersion = 10

// presenceRefresh is how often the status text is refreshed, so a count in
// it doesn't go stale. Cheap: one small frame.
const presenceRefresh = 10 * time.Minute

// Gateway maintains the connection that makes the bot appear online.
type Gateway struct {
	BotToken string
	Log      *slog.Logger
	// Status returns the text to display. Called on connect and on every
	// refresh, so it can carry live numbers. Optional.
	Status func() string
	// HTTP is used only for the initial /gateway/bot lookup.
	HTTP *http.Client

	mu        sync.Mutex
	sessionID string
	resumeURL string
	seq       int64
}

// Run keeps a Gateway connection up until ctx is cancelled, reconnecting
// with backoff. It never returns an error: there is no failure here worth
// stopping the service for.
func (g *Gateway) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := g.connectOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			g.Log.Warn("discord gateway disconnected", "err", err, "retry_in", backoff)
		default:
			g.Log.Info("discord gateway closed, reconnecting", "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// Exponential with a ceiling. Discord rate-limits IDENTIFY hard, and
		// a tight reconnect loop against it is how a bot gets its token
		// temporarily banned.
		if backoff < 5*time.Minute {
			backoff *= 2
		}
		if err == nil {
			backoff = time.Second // a clean close is not a fault; reset
		}
	}
}

// connectOnce runs one connection to completion. Every piece of per-
// connection state is local to this call, so a previous connection's
// goroutines can never interfere with a newer one — the failure mode that
// bit HopReach's own broker.
func (g *Gateway) connectOnce(ctx context.Context) error {
	url, err := g.gatewayURL(ctx)
	if err != nil {
		return fmt.Errorf("looking up gateway: %w", err)
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dialling %s: %w", url, err)
	}
	defer conn.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Cancelling the context does NOT unblock a goroutine already sitting in
	// ReadJSON — it stays there until the read deadline expires, which is
	// tens of seconds away. Without this, a zombie connection would take
	// that long to be replaced, and shutdown would hang for the same
	// period. Closing the socket is what actually interrupts the read.
	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	// One writer goroutine owns the socket's write side: gorilla forbids
	// concurrent writes, and the heartbeat and presence updates would
	// otherwise race.
	writes := make(chan any, 8)
	var writeWG sync.WaitGroup
	writeWG.Add(1)
	go func() {
		defer writeWG.Done()
		for {
			select {
			case <-connCtx.Done():
				return
			case msg := <-writes:
				if err := conn.WriteJSON(msg); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer writeWG.Wait()

	send := func(v any) {
		select {
		case writes <- v:
		case <-connCtx.Done():
		}
	}

	// HELLO must arrive first and carries the heartbeat interval.
	var hello struct {
		Op int `json:"op"`
		D  struct {
			HeartbeatInterval int `json:"heartbeat_interval"`
		} `json:"d"`
	}
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("reading hello: %w", err)
	}
	if hello.Op != opHello || hello.D.HeartbeatInterval <= 0 {
		return fmt.Errorf("unexpected first frame: op %d", hello.Op)
	}
	interval := time.Duration(hello.D.HeartbeatInterval) * time.Millisecond

	g.mu.Lock()
	session, seq := g.sessionID, g.seq
	g.mu.Unlock()

	if session != "" {
		send(map[string]any{"op": opResume, "d": map[string]any{
			"token": g.BotToken, "session_id": session, "seq": seq,
		}})
	} else {
		send(map[string]any{"op": opIdentify, "d": map[string]any{
			"token": g.BotToken,
			// No intents at all. Presence needs none, and asking for events
			// we never read would be both wasteful and a larger permission
			// surface than this deserves.
			"intents": 0,
			"properties": map[string]string{
				"os": "linux", "browser": "hopreact", "device": "hopreact",
			},
			"presence": g.presencePayload(),
		}})
	}

	// acked tracks whether the last heartbeat came back. Discord's own
	// guidance: if it didn't, the connection is a zombie — the socket looks
	// open but nothing is flowing — and must be torn down rather than
	// trusted.
	var hbMu sync.Mutex
	acked := true

	go func() {
		// First beat is jittered, as Discord asks, so a fleet of bots
		// reconnecting after an outage doesn't synchronise.
		first := time.Duration(rand.Int63n(int64(interval)))
		select {
		case <-connCtx.Done():
			return
		case <-time.After(first):
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			hbMu.Lock()
			ok := acked
			acked = false
			hbMu.Unlock()
			if !ok {
				g.Log.Warn("discord gateway heartbeat unacknowledged, reconnecting")
				cancel()
				return
			}
			g.mu.Lock()
			s := g.seq
			g.mu.Unlock()
			var d any
			if s > 0 {
				d = s
			}
			send(map[string]any{"op": opHeartbeat, "d": d})

			select {
			case <-connCtx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// Refresh the status text so any live numbers in it stay current.
	go func() {
		t := time.NewTicker(presenceRefresh)
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				send(map[string]any{"op": opPresenceUpdate, "d": g.presencePayload()})
			}
		}
	}()

	for {
		// Generous: the read deadline only needs to catch a truly dead
		// socket, and the heartbeat ACK check above is the real liveness
		// test.
		conn.SetReadDeadline(time.Now().Add(interval*2 + 30*time.Second))
		var frame struct {
			Op int             `json:"op"`
			S  *int64          `json:"s"`
			T  string          `json:"t"`
			D  json.RawMessage `json:"d"`
		}
		if err := conn.ReadJSON(&frame); err != nil {
			if connCtx.Err() != nil {
				return nil // we asked for this
			}
			return err
		}
		if frame.S != nil {
			g.mu.Lock()
			g.seq = *frame.S
			g.mu.Unlock()
		}

		switch frame.Op {
		case opHeartbeatACK:
			hbMu.Lock()
			acked = true
			hbMu.Unlock()

		case opHeartbeat:
			// Discord can ask for one out of band.
			g.mu.Lock()
			s := g.seq
			g.mu.Unlock()
			send(map[string]any{"op": opHeartbeat, "d": s})

		case opDispatch:
			if frame.T == "READY" {
				var ready struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
					User             struct {
						Username string `json:"username"`
					} `json:"user"`
				}
				if err := json.Unmarshal(frame.D, &ready); err == nil {
					g.mu.Lock()
					g.sessionID, g.resumeURL = ready.SessionID, ready.ResumeGatewayURL
					g.mu.Unlock()
					g.Log.Info("discord gateway ready — bot is now shown online",
						"bot", ready.User.Username)
				}
			}

		case opReconnect:
			// Discord is asking us to move; the session can be resumed.
			return errors.New("gateway asked us to reconnect")

		case opInvalidSession:
			// The session cannot be resumed. Drop it so the next attempt
			// does a fresh IDENTIFY, and pause first — reconnecting
			// instantly here is what gets a token rate-limited.
			g.mu.Lock()
			g.sessionID, g.resumeURL, g.seq = "", "", 0
			g.mu.Unlock()
			select {
			case <-connCtx.Done():
			case <-time.After(time.Duration(1+rand.Intn(4)) * time.Second):
			}
			return errors.New("gateway invalidated the session")
		}
	}
}

// presencePayload builds the presence object. Status text comes from the
// caller so it can carry live numbers.
func (g *Gateway) presencePayload() map[string]any {
	name := "the mesh"
	if g.Status != nil {
		if s := g.Status(); s != "" {
			name = s
		}
	}
	return map[string]any{
		"status": "online",
		"afk":    false,
		"since":  nil,
		"activities": []map[string]any{
			{"name": name, "type": activityWatching},
		},
	}
}

// gatewayURL prefers the resume URL Discord handed us with the last READY,
// falling back to a fresh lookup.
func (g *Gateway) gatewayURL(ctx context.Context) (string, error) {
	g.mu.Lock()
	resume := g.resumeURL
	g.mu.Unlock()
	if resume != "" {
		return fmt.Sprintf("%s/?v=%d&encoding=json", resume, gatewayVersion), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+g.BotToken)
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /gateway/bot: status %d", resp.StatusCode)
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.URL == "" {
		return "", errors.New("no gateway url returned")
	}
	return fmt.Sprintf("%s/?v=%d&encoding=json", body.URL, gatewayVersion), nil
}
