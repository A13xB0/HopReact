package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeGateway is a minimal stand-in for Discord's Gateway: it sends HELLO,
// records what the client sends, and can be told to misbehave.
type fakeGateway struct {
	t                 *testing.T
	heartbeatInterval int
	// withholdACK makes the server ignore heartbeats, which is how a real
	// zombie connection looks: the socket is open but nothing is flowing.
	withholdACK bool

	// wsHost is the test server's own address. It cannot come from the
	// request's Host header: the client's transport rewrites the URL to
	// reach us but leaves Host as discord.com, so echoing it back would
	// send the socket dial to the real Discord.
	wsHost string

	mu        sync.Mutex
	identify  map[string]any
	resumes   int
	beats     int
	presences []map[string]any
	srv       *httptest.Server
	closeOnce sync.Once
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{t: t, heartbeatInterval: 60}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v10/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		// Same shape as Discord's own reply: a bare origin. The client
		// appends "/?v=10&encoding=json" itself, so the socket is served
		// from the root below rather than a sub-path.
		json.NewEncoder(w).Encode(map[string]any{
			"url": "ws://" + f.wsHost, "shards": 1,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteJSON(map[string]any{
			"op": opHello,
			"d":  map[string]any{"heartbeat_interval": f.heartbeatInterval},
		})
		var seq int64
		for {
			var frame struct {
				Op int             `json:"op"`
				D  json.RawMessage `json:"d"`
			}
			if err := c.ReadJSON(&frame); err != nil {
				return
			}
			switch frame.Op {
			case opIdentify:
				var d map[string]any
				json.Unmarshal(frame.D, &d)
				f.mu.Lock()
				f.identify = d
				if p, ok := d["presence"].(map[string]any); ok {
					f.presences = append(f.presences, p)
				}
				f.mu.Unlock()
				seq++
				c.WriteJSON(map[string]any{
					"op": opDispatch, "s": seq, "t": "READY",
					"d": map[string]any{
						"session_id":         "sess-1",
						"resume_gateway_url": "ws://" + f.wsHost,
						"user":               map[string]any{"username": "HopReact"},
					},
				})
			case opResume:
				f.mu.Lock()
				f.resumes++
				f.mu.Unlock()
			case opHeartbeat:
				f.mu.Lock()
				f.beats++
				withhold := f.withholdACK
				f.mu.Unlock()
				if !withhold {
					c.WriteJSON(map[string]any{"op": opHeartbeatACK})
				}
			case opPresenceUpdate:
				var d map[string]any
				json.Unmarshal(frame.D, &d)
				f.mu.Lock()
				f.presences = append(f.presences, d)
				f.mu.Unlock()
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	f.wsHost = strings.TrimPrefix(f.srv.URL, "http://")
	t.Cleanup(func() { f.closeOnce.Do(f.srv.Close) })
	return f
}

// client points the Gateway's REST lookup at the fake server. The
// WebSocket URL then comes from that lookup's own response, so the socket
// needs no rewriting.
func (f *fakeGateway) client(g *Gateway) {
	base, err := url.Parse(f.srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	g.HTTP = &http.Client{
		Timeout:   5 * time.Second,
		Transport: rewriteHost{base: base, rt: http.DefaultTransport},
	}
}

func (f *fakeGateway) snapshot() (identify map[string]any, resumes, beats int, presences []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.presences))
	copy(out, f.presences)
	return f.identify, f.resumes, f.beats, out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The whole point: connecting and IDENTIFYing is what makes the bot appear
// online. If the handshake regresses, the bot silently greys out and the
// liveness signal is worthless.
func TestGatewayIdentifiesAndGoesOnline(t *testing.T) {
	f := newFakeGateway(t)
	g := &Gateway{
		BotToken: "bot-token",
		Log:      quietLogger(),
		Status:   func() string { return "782 nodes · 3 watched" },
	}
	f.client(g)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); g.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		id, _, _, _ := f.snapshot()
		return id != nil
	}, "IDENTIFY never arrived")

	id, _, _, presences := f.snapshot()
	if id["token"] != "bot-token" {
		t.Errorf("token = %v", id["token"])
	}
	// No intents: presence needs none, and asking for events we never read
	// would be a larger permission surface than this deserves.
	if iv, ok := id["intents"].(float64); !ok || iv != 0 {
		t.Errorf("intents = %v, want 0", id["intents"])
	}
	if len(presences) == 0 {
		t.Fatal("IDENTIFY carried no presence, so the bot would connect but show no status")
	}
	p := presences[0]
	if p["status"] != "online" {
		t.Errorf("status = %v, want online", p["status"])
	}
	acts, _ := p["activities"].([]any)
	if len(acts) == 0 {
		t.Fatal("no activity in the presence payload")
	}
	a, _ := acts[0].(map[string]any)
	if a["name"] != "782 nodes · 3 watched" {
		t.Errorf("activity name = %v, want the live status text", a["name"])
	}
	if tv, ok := a["type"].(float64); !ok || int(tv) != activityWatching {
		t.Errorf("activity type = %v, want Watching (%d)", a["type"], activityWatching)
	}

	cancel()
	<-done
}

// The session details from READY have to be captured, or every reconnect
// re-IDENTIFYs — which Discord rate-limits hard.
func TestGatewayRemembersSessionForResume(t *testing.T) {
	f := newFakeGateway(t)
	g := &Gateway{BotToken: "t", Log: quietLogger()}
	f.client(g)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go g.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.sessionID != ""
	}, "session id was never recorded from READY")

	g.mu.Lock()
	sid, rurl := g.sessionID, g.resumeURL
	g.mu.Unlock()
	if sid != "sess-1" {
		t.Errorf("sessionID = %q", sid)
	}
	if rurl == "" {
		t.Error("resume_gateway_url was not recorded")
	}
	cancel()
}

// A connection whose heartbeats stop being acknowledged is a zombie: open,
// but dead. It must be torn down and retried rather than left looking
// online while the bot is actually unreachable — which would make the
// presence indicator worse than useless, because it would lie.
func TestGatewayReconnectsWhenHeartbeatsAreNotAcknowledged(t *testing.T) {
	f := newFakeGateway(t)
	f.heartbeatInterval = 40 // ms; keeps the test quick
	f.withholdACK = true

	g := &Gateway{BotToken: "t", Log: quietLogger()}
	f.client(g)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go g.Run(ctx)

	// A reconnect shows up as a second connection attempt — either another
	// IDENTIFY or a RESUME.
	waitFor(t, 3*time.Second, func() bool {
		_, resumes, beats, _ := f.snapshot()
		return beats >= 2 || resumes >= 1
	}, "the zombie connection was never torn down and retried")
	cancel()
}

// Presence must never take the service down with it.
func TestGatewayRunSurvivesAnUnreachableGateway(t *testing.T) {
	g := &Gateway{BotToken: "t", Log: quietLogger()}
	// Nothing listening.
	g.HTTP = &http.Client{Timeout: 200 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); g.Run(ctx) }()

	select {
	case <-done:
		// Returned on context cancellation rather than panicking or spinning.
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
}

func TestPresenceFallsBackWithoutAStatusFunc(t *testing.T) {
	g := &Gateway{}
	p := g.presencePayload()
	acts := p["activities"].([]map[string]any)
	if acts[0]["name"] != "the mesh" {
		t.Errorf("fallback name = %v", acts[0]["name"])
	}
	if p["status"] != "online" {
		t.Errorf("status = %v", p["status"])
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
