package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// withAPI points the package's API base at a test server for the duration of
// the test. apiBase is a const, so the client is redirected by swapping the
// transport instead — which also keeps the real URLs in the assertions.
func withAPI(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	target, _ := url.Parse(srv.URL)
	hc := &http.Client{
		Timeout:   5 * time.Second,
		Transport: rewriteHost{base: target, rt: http.DefaultTransport},
	}
	return New("cid", "secret", "bot-token", "guild-1", "https://example.com/auth/callback", hc)
}

// rewriteHost sends every request to the test server, preserving the path so
// handlers can assert on it.
type rewriteHost struct {
	base *url.URL
	rt   http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.base.Scheme
	req.URL.Host = r.base.Host
	return r.rt.RoundTrip(req)
}

func TestAuthorizeURLRequestsTheRightScopes(t *testing.T) {
	c := New("cid", "s", "b", "g", "https://example.com/auth/callback", nil)
	raw := c.AuthorizeURL("state-123")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()

	// guilds.join is the whole mechanism: without it we cannot put the user
	// in the alert server, and without that the bot can never DM them.
	scopes := strings.Fields(q.Get("scope"))
	want := map[string]bool{"identify": false, "guilds.join": false}
	for _, s := range scopes {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, present := range want {
		if !present {
			t.Errorf("scope %q missing from %q", s, q.Get("scope"))
		}
	}
	// We deliberately never ask for email — that is the point of using
	// Discord for delivery.
	if strings.Contains(q.Get("scope"), "email") {
		t.Error("must not request the email scope")
	}
	if q.Get("state") != "state-123" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("client_id") != "cid" || q.Get("redirect_uri") != "https://example.com/auth/callback" {
		t.Errorf("client_id/redirect_uri wrong: %v", q)
	}
}

func TestExchangeFetchesUserAndJoinsGuild(t *testing.T) {
	var joined bool
	var joinAuth string

	c := withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v10/oauth2/token":
			r.ParseForm()
			if r.Form.Get("code") != "the-code" || r.Form.Get("grant_type") != "authorization_code" {
				t.Errorf("unexpected token form: %v", r.Form)
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1", "scope": oauthScopes})
		case r.URL.Path == "/api/v10/users/@me":
			if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
				t.Errorf("user fetch auth = %q, want the bearer token", got)
			}
			json.NewEncoder(w).Encode(User{ID: "u1", Username: "alice", GlobalName: "Alice", Avatar: "av"})
		case strings.HasPrefix(r.URL.Path, "/api/v10/guilds/guild-1/members/u1") && r.Method == http.MethodPut:
			joined = true
			joinAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))

	u, err := c.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if u.ID != "u1" || u.DisplayName() != "Alice" {
		t.Errorf("user = %+v", u)
	}
	if !joined {
		t.Error("Exchange must add the user to the alert server — otherwise no DM is ever possible")
	}
	// The join uses the BOT token; the user's access token goes in the body.
	if joinAuth != "Bot bot-token" {
		t.Errorf("guild add auth = %q, want the bot token", joinAuth)
	}
}

// Sign-in must still work if the guild add fails — the user gets an account
// and the dashboard tells them delivery is not set up, which is much better
// than a dead-end error page.
func TestExchangeSucceedsEvenIfGuildAddFails(t *testing.T) {
	c := withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v10/oauth2/token":
			json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1"})
		case r.URL.Path == "/api/v10/users/@me":
			json.NewEncoder(w).Encode(User{ID: "u1", Username: "alice"})
		default:
			http.Error(w, `{"code":50013,"message":"Missing Permissions"}`, http.StatusForbidden)
		}
	}))
	u, err := c.Exchange(context.Background(), "code")
	if err != nil {
		t.Fatalf("a failed guild add must not fail sign-in: %v", err)
	}
	if u.ID != "u1" {
		t.Errorf("user = %+v", u)
	}
}

func TestCheckMembership(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
		hasErr bool
	}{
		{"member", http.StatusOK, true, false},
		{"left the server", http.StatusNotFound, false, false},
		{"api trouble", http.StatusInternalServerError, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			got, err := c.CheckMembership(context.Background(), "u1")
			if got != tt.want {
				t.Errorf("member = %v, want %v", got, tt.want)
			}
			if (err != nil) != tt.hasErr {
				t.Errorf("err = %v, wantErr %v", err, tt.hasErr)
			}
		})
	}
}

func TestSendDMOpensChannelThenPosts(t *testing.T) {
	var posted string
	c := withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v10/users/@me/channels":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["recipient_id"] != "u1" {
				t.Errorf("recipient = %q", body["recipient_id"])
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "dm-1"})
		case "/api/v10/channels/dm-1/messages":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			posted, _ = body["content"].(string)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"m1"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))

	if err := c.SendDM(context.Background(), "u1", "your repeater is quiet"); err != nil {
		t.Fatalf("SendDM: %v", err)
	}
	if posted != "your repeater is quiet" {
		t.Errorf("posted %q", posted)
	}
}

// 50007 means the user left the server, blocked the bot, or turned off DMs
// from server members. Retrying will never help, so it has to be
// distinguishable from a transient failure — otherwise the outbox retries
// forever and the user is never told why they hear nothing.
func TestUndeliverableIsDistinguishable(t *testing.T) {
	c := withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":50007,"message":"Cannot send messages to this user"}`))
	}))
	err := c.SendDM(context.Background(), "u1", "hello")
	if !errors.Is(err, ErrUndeliverable) {
		t.Fatalf("err = %v, want it to wrap ErrUndeliverable", err)
	}
}

// A generic failure must NOT be mistaken for undeliverable — that would
// permanently disable someone's alerts because of a blip.
func TestTransientFailureIsNotUndeliverable(t *testing.T) {
	c := withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":0,"message":"Internal Server Error"}`, http.StatusInternalServerError)
	}))
	err := c.SendDM(context.Background(), "u1", "hello")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrUndeliverable) {
		t.Error("a 500 must not be treated as permanently undeliverable")
	}
}

func TestDisplayNameFallsBackToUsername(t *testing.T) {
	if got := (User{Username: "alice"}).DisplayName(); got != "alice" {
		t.Errorf("DisplayName = %q", got)
	}
	if got := (User{Username: "alice", GlobalName: "Alice A"}).DisplayName(); got != "Alice A" {
		t.Errorf("DisplayName = %q, want the global name", got)
	}
}
