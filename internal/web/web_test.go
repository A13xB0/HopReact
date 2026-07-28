package web

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/discord"
	"hopreact/internal/store"
)

var t0 = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func newServer(t *testing.T) (*Server, *store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Site.BaseURL = "http://localhost:8080"
	cfg.Discord = config.DiscordConfig{ClientID: "c", ClientSecret: "s", BotToken: "b", GuildID: "g"}

	dc := discord.New("c", "s", "b", "g", cfg.Site.BaseURL+"/auth/callback", nil)
	srv, err := New(st, dc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, st, srv.Routes()
}

// signIn creates a user and session directly, returning the cookie and CSRF
// token a browser would hold.
func signIn(t *testing.T, srv *Server, st *store.Store) (*http.Cookie, string, store.User) {
	t.Helper()
	ctx := context.Background()
	u, err := st.UpsertUser(ctx, "d1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	token, _ := randomToken()
	csrf, _ := randomToken()
	sum := sha256.Sum256([]byte(token))
	if err := st.CreateSession(ctx, sum[:], u.ID, csrf, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: srv.sessionCookieName(), Value: token}, csrf, u
}

func TestIndexRendersForAnonymousVisitor(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sign in with Discord") {
		t.Error("the landing page should offer sign-in")
	}
	// The server-membership requirement is load-bearing, so it must be said
	// before someone signs up, not discovered later.
	if !strings.Contains(body, "Stay in it") {
		t.Error("the landing page should warn that leaving the Discord stops alerts")
	}
}

// The healthcheck must not depend on CoreScope: if it did, Docker would
// restart the container during exactly the upstream outage the alert engine
// is built to survive.
func TestHealthzIsIndependentOfUpstream(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 regardless of upstream", rec.Code)
	}
}

func TestSignedOutUserIsRedirectedFromDashboard(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/watches", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status %d, want a redirect to the landing page", rec.Code)
	}
}

func TestDashboardListsWatches(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{
		Kind: corescope.KindNode, Key: "aa", Name: "Ben Nevis", Role: "repeater",
		LastSeen: t0.Add(-time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	cookie, _, u := signIn(t, srv, st)
	if _, err := st.CreateWatch(ctx, store.Watch{
		UserID: u.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 6,
	}, 50); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/watches", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Ben Nevis") {
		t.Error("the dashboard should show the watched node")
	}
}

// Every state-changing request needs a CSRF token. This is checked globally
// rather than per-route precisely so a route added later cannot forget it.
func TestPostWithoutCSRFIsRejected(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, _ := signIn(t, srv, st)

	form := url.Values{"kind": {"node"}, "key": {"aa"}, "threshold_hours": {"6"}}
	req := httptest.NewRequest(http.MethodPost, "/watches", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 without a CSRF token", rec.Code)
	}
}

func TestPostWithWrongCSRFIsRejected(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, _ := signIn(t, srv, st)

	form := url.Values{"csrf_token": {"nope"}, "kind": {"node"}, "key": {"aa"}, "threshold_hours": {"6"}}
	req := httptest.NewRequest(http.MethodPost, "/watches", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 for a bad CSRF token", rec.Code)
	}
}

// A cross-origin POST is refused even if it somehow carried a valid token.
func TestForeignOriginIsRejected(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, csrf, _ := signIn(t, srv, st)

	form := url.Values{"csrf_token": {csrf}, "kind": {"node"}, "key": {"aa"}, "threshold_hours": {"6"}}
	req := httptest.NewRequest(http.MethodPost, "/watches", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 for a foreign origin", rec.Code)
	}
}

func TestAddAndRemoveWatch(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{
		Kind: corescope.KindNode, Key: "aa", Name: "Ben Nevis", LastSeen: t0.Add(-time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf, u := signIn(t, srv, st)

	form := url.Values{"csrf_token": {csrf}, "kind": {"node"}, "key": {"AA"}, "threshold_hours": {"6"}}
	req := httptest.NewRequest(http.MethodPost, "/watches", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want a redirect after adding", rec.Code)
	}

	views, err := st.WatchViews(ctx, u.ID)
	if err != nil || len(views) != 1 {
		t.Fatalf("views=%d err=%v", len(views), err)
	}
	// The key must be normalised, or the watch would never match the feed.
	if views[0].Watch.TargetKey != "aa" {
		t.Errorf("stored key %q, want it lowercased", views[0].Watch.TargetKey)
	}

	form = url.Values{"csrf_token": {csrf}}
	req = httptest.NewRequest(http.MethodPost,
		"/watches/"+itoa(views[0].Watch.ID)+"/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d on delete", rec.Code)
	}
	if views, _ := st.WatchViews(ctx, u.ID); len(views) != 0 {
		t.Error("the watch should be gone")
	}
}

// A threshold below the minimum must be clamped, not accepted — an
// hour is the documented floor.
func TestThresholdIsClampedToTheMinimum(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{
		Kind: corescope.KindNode, Key: "aa", LastSeen: t0,
	}}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf, u := signIn(t, srv, st)

	form := url.Values{"csrf_token": {csrf}, "kind": {"node"}, "key": {"aa"}, "threshold_hours": {"0"}}
	req := httptest.NewRequest(http.MethodPost, "/watches", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(httptest.NewRecorder(), req)

	views, _ := st.WatchViews(ctx, u.ID)
	if len(views) != 1 || views[0].Watch.ThresholdHours < config.MinThresholdHours {
		t.Errorf("threshold = %v, want it clamped to at least %d", views, config.MinThresholdHours)
	}
}

// One user must not be able to delete another's watch by guessing its id.
func TestCannotDeleteAnotherUsersWatch(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	victim, _ := st.UpsertUser(ctx, "d2", "bob", "")
	id, err := st.CreateWatch(ctx, store.Watch{
		UserID: victim.ID, TargetKind: "node", TargetKey: "aa", ThresholdHours: 6,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}

	cookie, csrf, _ := signIn(t, srv, st) // signs in as alice
	form := url.Values{"csrf_token": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/watches/"+itoa(id)+"/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if views, _ := st.WatchViews(ctx, victim.ID); len(views) != 1 {
		t.Error("another user's watch was deleted")
	}
}

// An undeliverable user must be told, prominently — silent failure is the
// worst outcome for an alerting tool.
func TestDashboardWarnsWhenUndeliverable(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, u := signIn(t, srv, st)
	if err := st.SetDMStatus(context.Background(), u.ID, false,
		"You are no longer in the HopReact Discord server, so the bot cannot DM you."); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/watches", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "can't be delivered") {
		t.Error("the dashboard must warn when alerts cannot be delivered")
	}
	if !strings.Contains(body, "Rejoin") {
		t.Error("the warning should offer a way to fix it")
	}
}

// With no successful poll the dashboard must say so rather than showing a
// page of reassuring green.
func TestDashboardWarnsWhenFeedIsStale(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, _ := signIn(t, srv, st)

	req := httptest.NewRequest(http.MethodGet, "/watches", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Upstream data is stale") {
		t.Error("the dashboard should say when the data cannot be trusted")
	}
}

func TestLoginRedirectsToDiscord(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "discord.com") || !strings.Contains(loc, "guilds.join") {
		t.Errorf("Location = %q, want a Discord consent URL including guilds.join", loc)
	}
	// The state must be pinned in a cookie or the callback cannot verify it.
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("sign-in should set the OAuth state cookie")
	}
}

// A callback whose state doesn't match the cookie is a forged or replayed
// sign-in and must be refused.
func TestCallbackRejectsStateMismatch(t *testing.T) {
	_, _, h := newServer(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=wrong", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: "right"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a state mismatch", rec.Code)
	}
}

func TestSearchRequiresSignIn(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status %d, want a redirect", rec.Code)
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	_, _, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{"X-Content-Type-Options", "Content-Security-Policy", "X-Frame-Options"} {
		if rec.Header().Get(want) == "" {
			t.Errorf("missing %s", want)
		}
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
