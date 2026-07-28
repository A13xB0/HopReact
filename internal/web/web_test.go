package web

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

// "Stop passing traffic" is only a question you can ask of a repeater. An
// observer reports what it hears rather than forwarding it, and a companion
// never relays at all — offering the option there is a control that can
// never do anything, which is exactly what it looked like in the UI.
func TestRelayOptionOnlyOfferedForRepeaters(t *testing.T) {
	if !canRelay("node", "repeater") {
		t.Error("a repeater must be offered the relay option")
	}
	for _, tt := range []struct{ kind, role string }{
		{"node", "companion"}, // never relays
		{"node", "room"},
		{"node", ""},
		{"observer", ""},
		{"observer", "repeater"}, // observed as a station, not a relay here
	} {
		if canRelay(tt.kind, tt.role) {
			t.Errorf("kind=%q role=%q must not be offered the relay option", tt.kind, tt.role)
		}
	}
}

// Rendered end to end: a companion in the results must not carry the
// checkbox, a repeater must.
func TestSearchPageOffersRelayOptionOnlyForRepeaters(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{
		{Kind: corescope.KindNode, Key: "aa", Name: "A Repeater", Role: "repeater", LastSeen: t0.Add(-time.Minute)},
		{Kind: corescope.KindNode, Key: "bb", Name: "A Companion", Role: "companion", LastSeen: t0.Add(-time.Minute)},
		{Kind: corescope.KindObserver, Key: "cc", Name: "An Observer", LastSeen: t0.Add(-time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _, _ := signIn(t, srv, st)

	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if n := strings.Count(body, `name="alert_on_relay"`); n != 1 {
		t.Errorf("found %d relay checkboxes, want exactly 1 (only the repeater)", n)
	}
	// The old wording said "also if not relaying", which meant nothing to
	// anyone who hadn't read the source.
	if !strings.Contains(body, "stops passing traffic") {
		t.Error("the relay option should explain itself in plain words")
	}
	if strings.Contains(body, "also if not relaying") {
		t.Error("the old opaque wording is still present")
	}
}

func TestFreshnessAndKeyHelpers(t *testing.T) {
	now := t0
	for _, tt := range []struct {
		age  time.Duration
		want string
	}{
		{time.Minute, "good"},
		{12 * time.Hour, "warn"},
		{72 * time.Hour, "bad"},
	} {
		if got := freshClass(now.Add(-tt.age), now); got != tt.want {
			t.Errorf("freshClass(%v ago) = %q, want %q", tt.age, got, tt.want)
		}
	}
	if got := freshClass(time.Time{}, now); got != "muted" {
		t.Errorf("freshClass(never) = %q, want muted", got)
	}

	full := "d41ee22644b0ea3aee70958cc5f4e87a1cfdbb1f404396dc0a7be3e7030df741"
	short := shortKey(full)
	if len(short) >= len(full) || !strings.HasPrefix(short, "d41ee22644") {
		t.Errorf("shortKey = %q", short)
	}
	if got := shortKey("abc"); got != "abc" {
		t.Errorf("a short key should be left alone, got %q", got)
	}
}

// ------------------------------------------------------- rules & detail --

// addWatch creates a watch through the store and returns its id.
func addWatch(t *testing.T, st *store.Store, userID int64, key string) int64 {
	t.Helper()
	id, err := st.CreateWatch(context.Background(), store.Watch{
		UserID: userID, TargetKind: "node", TargetKey: key, ThresholdHours: 6,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWatchDetailRenders(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{
		Kind: corescope.KindNode, Key: "aa", Name: "Ben Nevis", Role: "repeater",
		LastSeen: t0.Add(-time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	cookie, _, u := signIn(t, srv, st)
	id := addWatch(t, st, u.ID, "aa")

	req := httptest.NewRequest(http.MethodGet, "/watches/"+itoa(id), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Ben Nevis",
		"Adverts, responses or channel messages", // the default per-type rule
		"Not heard at all",                       // the backstop
		"ADVERT", "GRP_TXT",                      // the activity table
		"Add a rule",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page is missing %q", want)
		}
	}
	// The honesty note is load-bearing, not decoration: without it a blank
	// "sent" column reads as "this never happened" rather than "nobody can
	// know this".
	if !strings.Contains(body, "Only adverts carry their sender") {
		t.Error("the page must explain why the sent column is blank for most types")
	}
}

// One person must not be able to read, or attach rules to, another's watch.
func TestCannotSeeOrEditAnotherUsersWatch(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	other, err := st.UpsertUser(ctx, "d2", "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	victim := addWatch(t, st, other.ID, "aa")
	cookie, csrf, _ := signIn(t, srv, st)

	req := httptest.NewRequest(http.MethodGet, "/watches/"+itoa(victim), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET another user's watch: status %d, want 404", rec.Code)
	}

	form := url.Values{"csrf_token": {csrf}, "threshold_hours": {"6"}, "types": {"4"}}
	req = httptest.NewRequest(http.MethodPost, "/watches/"+itoa(victim)+"/rules",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST a rule onto another user's watch: status %d, want 404", rec.Code)
	}
	rules, _ := st.RulesForWatch(ctx, victim)
	for _, r := range rules {
		if r.Source == store.SourceTypes && len(r.Types) == 1 && r.Types[0] == 4 {
			t.Fatal("a rule was attached to another user's watch")
		}
	}
}

// Groups are expanded to their members on save. Storing the group name
// instead would mean redefining a group later silently rewrites what existing
// users are alerted on.
func TestAddRuleStoresExpandedTypes(t *testing.T) {
	srv, st, h := newServer(t)
	ctx := context.Background()
	cookie, csrf, u := signIn(t, srv, st)
	id := addWatch(t, st, u.ID, "aa")

	form := url.Values{
		"csrf_token": {csrf}, "threshold_hours": {"12"}, "direction": {"carried"},
		"label": {"Messaging"},
		"types": {"2", "5", "6", "10"},
	}
	req := httptest.NewRequest(http.MethodPost, "/watches/"+itoa(id)+"/rules",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want a redirect", rec.Code)
	}

	rules, _ := st.RulesForWatch(ctx, id)
	var got *store.Rule
	for i := range rules {
		if rules[i].Label == "Messaging" {
			got = &rules[i]
		}
	}
	if got == nil {
		t.Fatal("the rule was not stored")
	}
	if len(got.Types) != 4 || got.Types[0] != 2 || got.Types[3] != 10 {
		t.Errorf("types = %v, want the group expanded to [2 5 6 10]", got.Types)
	}
	if got.Direction != store.DirCarried || got.ThresholdHours != 12 {
		t.Errorf("direction=%q threshold=%d", got.Direction, got.ThresholdHours)
	}
}

// A per-type rule with no types selected would match nothing and could never
// fire, so it must be refused rather than silently stored.
func TestRuleWithNoTypesIsRefused(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, csrf, u := signIn(t, srv, st)
	id := addWatch(t, st, u.ID, "aa")
	before, _ := st.RulesForWatch(context.Background(), id)

	form := url.Values{"csrf_token": {csrf}, "threshold_hours": {"6"}}
	req := httptest.NewRequest(http.MethodPost, "/watches/"+itoa(id)+"/rules",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	after, _ := st.RulesForWatch(context.Background(), id)
	if len(after) != len(before) {
		t.Errorf("rules went from %d to %d; an empty rule must be refused", len(before), len(after))
	}
}

// An unknown payload type would match nothing, so it is dropped rather than
// stored as a rule that can never fire.
func TestUnknownTypesAreDropped(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, csrf, u := signIn(t, srv, st)
	id := addWatch(t, st, u.ID, "aa")

	form := url.Values{
		"csrf_token": {csrf}, "threshold_hours": {"6"},
		"label": {"Mixed"}, "types": {"4", "99", "13"},
	}
	req := httptest.NewRequest(http.MethodPost, "/watches/"+itoa(id)+"/rules",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(httptest.NewRecorder(), req)

	rules, _ := st.RulesForWatch(context.Background(), id)
	for _, r := range rules {
		if r.Label != "Mixed" {
			continue
		}
		if len(r.Types) != 1 || r.Types[0] != 4 {
			t.Errorf("types = %v, want only the real one [4]", r.Types)
		}
	}
}

// ---------------------------------------------------------- feed banner --

func insertPoll(t *testing.T, st *store.Store, at time.Time, status store.PollStatus, errText string) {
	t.Helper()
	ctx := context.Background()
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO poll_runs (started_at, finished_at, status, node_count, error)
			 VALUES (?,?,?,?,?)`, at.Unix(), at.Unix(), string(status), 778, errText)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// One failed poll is not news. The upstream is someone else's public API
// behind a DNS lookup and the occasional blip is routine; a single miss
// delays detection by one interval and nothing else. Shouting about it trains
// people to ignore the banner, which is expensive the day it matters.
func TestOneFailedPollDoesNotRaiseTheBanner(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, _ := signIn(t, srv, st)

	insertPoll(t, st, t0.Add(-10*time.Minute), store.PollOK, "")
	insertPoll(t, st, t0.Add(-5*time.Minute), store.PollOK, "")
	insertPoll(t, st, t0, store.PollFailed,
		`dial tcp: lookup scotmesh-corescope on 127.0.0.11:53: server misbehaving`)

	req := httptest.NewRequest(http.MethodGet, "/watches", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Upstream data is stale") {
		t.Error("a single failed poll should not raise the stale banner")
	}
	if strings.Contains(body, "127.0.0.11:53") {
		t.Error("a raw Go dial error must never reach the page")
	}
}

// A feed that has genuinely stopped must still be reported — plainly, and
// without the networking stack's own words.
func TestSustainedOutageRaisesAReadableBanner(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, _ := signIn(t, srv, st)

	insertPoll(t, st, t0.Add(-2*time.Hour), store.PollOK, "")
	for i := 4; i >= 1; i-- {
		insertPoll(t, st, t0.Add(-time.Duration(i)*5*time.Minute), store.PollFailed,
			`dial tcp: lookup scotmesh-corescope on 127.0.0.11:53: server misbehaving`)
	}

	req := httptest.NewRequest(http.MethodGet, "/watches", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "can&#39;t reach the mesh data feed") &&
		!strings.Contains(body, "can't reach the mesh data feed") {
		t.Errorf("a sustained outage should be reported in plain words, got:\n%s", body)
	}
	if strings.Contains(body, "127.0.0.11:53") || strings.Contains(body, "dial tcp") {
		t.Error("the raw Go error must stay in poll_runs, not go on the page")
	}
	// It must also say the user's own nodes are not the problem.
	if !strings.Contains(body, "nothing has gone wrong with your nodes") {
		t.Error("the banner should make clear this is our problem, not theirs")
	}
}

// A suspect poll's reason is written for people ("only 5 nodes returned,
// below the floor of 100") and is worth showing.
func TestSuspectFeedShowsItsReason(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, _ := signIn(t, srv, st)
	ctx := context.Background()

	insertPoll(t, st, t0.Add(-2*time.Hour), store.PollOK, "")
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO poll_runs (started_at, status, suppressed_reason)
			 VALUES (?, 'suspect', 'only 5 nodes returned, below the floor of 100')`,
			t0.Unix())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/watches", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "below the floor of 100") {
		t.Error("a suspect poll's human-readable reason should be shown")
	}
}
