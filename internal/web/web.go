// Package web is the HTTP surface: sign-in, the dashboard, and managing
// watches. Server-rendered HTML with html/template — there is no API to
// consume and no client-side state worth a framework.
//
// Authentication is Discord OAuth and nothing else. There are no passwords
// to hash, no verification mail, and no reset flow, because there is no
// email address in the system at all.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"hopreact/internal/buildinfo"
	"hopreact/internal/config"
	"hopreact/internal/corescope"
	"hopreact/internal/discord"
	"hopreact/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// sessionTTL is how long a sign-in lasts.
const sessionTTL = 30 * 24 * time.Hour

// oauthStateCookie carries the CSRF state across the Discord round trip.
const oauthStateCookie = "hopreact_oauth_state"

// Server holds the HTTP dependencies.
type Server struct {
	Store   *store.Store
	Discord *discord.Client
	Cfg     config.Config
	Log     *slog.Logger

	// One template set PER PAGE, not one for everything. Each page file
	// defines its own {{define "content"}}, and parsing them all into a
	// single set means the last one parsed silently wins — the landing page
	// would render the dashboard's content. Pairing each page with the
	// layout on its own keeps the names from colliding.
	pages map[string]*template.Template
	// secure mirrors whether base_url is https, which decides the cookie's
	// Secure flag. Local development over plain HTTP would otherwise never
	// receive a session cookie at all.
	secure bool
}

// New parses the templates and returns a ready Server.
func New(st *store.Store, dc *discord.Client, cfg config.Config, log *slog.Logger) (*Server, error) {
	funcs := template.FuncMap{
		"since":      humanSince,
		"stamp":      stamp,
		"stateLabel": stateLabel,
		"stateClass": stateClass,
		"freshClass": freshClass,
		"shortKey":   shortKey,
		"canRelay":   canRelay,
		"ruleTitle":  ruleTitle,
		"ruleDesc":   ruleDescription,
		"ruleHas":    ruleHasType,
		"typeName":   corescope.TypeName,
		"add":        func(a, b int) int { return a + b },
	}
	pages := map[string]*template.Template{}
	for _, page := range []string{"index.html", "watches.html", "search.html", "watch.html"} {
		t, err := template.New(page).Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("web: parsing %s: %w", page, err)
		}
		pages[page] = t
	}
	return &Server{
		Store: st, Discord: dc, Cfg: cfg, Log: log, pages: pages,
		secure: strings.HasPrefix(cfg.Site.BaseURL, "https://"),
	}, nil
}

func (s *Server) sessionCookieName() string {
	// The __Host- prefix requires Secure, which plain-HTTP local dev cannot
	// satisfy, so it is only used where it will actually work.
	if s.secure {
		return "__Host-hopreact_session"
	}
	return "hopreact_session"
}

// Routes builds the mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /watches", s.handleWatches)
	mux.HandleFunc("POST /watches", s.handleAddWatch)
	mux.HandleFunc("GET /watches/{id}", s.handleWatchDetail)
	mux.HandleFunc("POST /watches/{id}/delete", s.handleDeleteWatch)
	mux.HandleFunc("POST /watches/{id}/update", s.handleUpdateWatch)
	mux.HandleFunc("POST /watches/{id}/rules", s.handleAddRule)
	mux.HandleFunc("POST /rules/{id}/delete", s.handleDeleteRule)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("POST /account/test-dm", s.handleTestDM)
	mux.HandleFunc("POST /account/delete", s.handleDeleteAccount)

	// The CSRF guard is applied globally and gated on method rather than
	// per-route, so a route added later cannot silently forget it.
	return s.recoverPanic(s.securityHeaders(s.withSession(s.requireCSRF(mux))))
}

// ------------------------------------------------------------ middleware --

type ctxKey struct{ name string }

var (
	userKey = ctxKey{"user"}
	csrfKey = ctxKey{"csrf"}
)

func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

func csrfFrom(ctx context.Context) string {
	v, _ := ctx.Value(csrfKey).(string)
	return v
}

// withSession resolves the session cookie if present. It never rejects —
// public pages need to render differently for signed-in and anonymous
// visitors, so authorisation is a per-route concern.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.sessionCookieName())
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sum := sha256.Sum256([]byte(c.Value))
		user, csrf, err := s.Store.SessionUser(r.Context(), sum[:])
		if err != nil {
			// Expired or unknown: clear the stale cookie so the browser
			// stops sending it.
			s.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, csrfKey, csrf)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF checks a token on every state-changing request, plus an Origin
// check. Fail-closed: anything that is not a safe method must present a
// matching token.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != s.Cfg.Site.BaseURL {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		want := csrfFrom(r.Context())
		if want == "" {
			http.Error(w, "sign in first", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		got := r.PostFormValue("csrf_token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "bad csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https://cdn.discordapp.com data:")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("panic serving request", "path", r.URL.Path, "panic", p)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --------------------------------------------------------------- pages ---

type pageData struct {
	Title     string
	User      *store.User
	CSRF      string
	Cfg       config.Config
	Version   string
	Flash     string
	FlashKind string

	Watches    []store.WatchView
	Watch      *store.WatchView
	Activity   []activityRow
	Groups     []typeGroup
	Results    []store.Target
	Query      string
	FeedHealth feedHealth
	Now        time.Time
}

// typeGroup is a one-click preset in the rule editor. Groups are expanded to
// their member types when a rule is saved, never stored by name — otherwise
// redefining a group later would silently change what existing rules alert
// on, which is not a thing an alerting tool may do quietly.
type typeGroup struct {
	Name  string
	Note  string
	Types []int
}

var typeGroups = []typeGroup{
	{"Adverts", "How a node announces itself. The most reliable heartbeat.",
		[]int{corescope.TypeADVERT}},
	{"Messages", "Direct and channel messages, and their multipart pieces.",
		[]int{corescope.TypeTXTMsg, corescope.TypeGRPTXT, corescope.TypeGRPData, corescope.TypeMULTIPART}},
	{"Requests & replies", "Status polls, logins and their responses.",
		[]int{corescope.TypeREQ, corescope.TypeRESPONSE, corescope.TypeANONReq}},
	{"Routing & housekeeping", "Acknowledgements, path discovery and traces.",
		[]int{corescope.TypeACK, corescope.TypePATH, corescope.TypeTRACE, corescope.TypeCONTROL}},
	{"Other", "Anything custom.", []int{corescope.TypeRAWCustom}},
}

func knownType(t int) bool {
	for _, k := range corescope.AllTypes {
		if k == t {
			return true
		}
	}
	return false
}

// activityRow is one payload type's line in the detail table.
type activityRow struct {
	Type int
	Name string

	Sent            time.Time
	SentEvidence    int
	Carried         time.Time
	CarriedEvidence int

	// SentKnowable is true only for adverts. Every other payload type is
	// encrypted and identifies its sender with a single byte, so a blank in
	// the "sent" column means "cannot be known", not "never happened" — and
	// the table has to say which.
	SentKnowable bool
}

func activityRows(acts []store.Activity) []activityRow {
	byType := map[int]*activityRow{}
	for _, t := range corescope.AllTypes {
		byType[t] = &activityRow{
			Type: t, Name: corescope.TypeName(t),
			SentKnowable: t == corescope.TypeADVERT,
		}
	}
	for _, a := range acts {
		row := byType[a.PayloadType]
		if row == nil {
			row = &activityRow{Type: a.PayloadType, Name: corescope.TypeName(a.PayloadType)}
			byType[a.PayloadType] = row
		}
		switch a.Direction {
		case store.DirSent:
			row.Sent, row.SentEvidence = a.LastAt, a.EvidenceCount
		case store.DirCarried:
			row.Carried, row.CarriedEvidence = a.LastAt, a.EvidenceCount
		}
	}
	out := make([]activityRow, 0, len(byType))
	for _, t := range corescope.AllTypes {
		out = append(out, *byType[t])
	}
	// Types with something to show come first; the rest keep wire order so
	// the table doesn't reshuffle between page loads.
	sort.SliceStable(out, func(i, j int) bool {
		return (out[i].SentEvidence+out[i].CarriedEvidence > 0) &&
			(out[j].SentEvidence+out[j].CarriedEvidence == 0)
	})
	return out
}

// ruleDescription says in words what a rule watches for.
func ruleDescription(r store.Rule) string {
	switch r.Source {
	case store.SourceSeen:
		return "any packet at all, as reported by CoreScope"
	case store.SourceRelayed:
		return "any traffic passed on, as reported by CoreScope"
	}
	names := make([]string, 0, len(r.Types))
	for _, t := range r.Types {
		names = append(names, corescope.TypeName(t))
	}
	what := strings.Join(names, ", ")
	switch r.Direction {
	case store.DirSent:
		return what + ", sent by it"
	case store.DirCarried:
		return what + ", passed on by it"
	}
	return what + ", sent or passed on"
}

// ruleTitle is the heading for a rule.
func ruleTitle(r store.Rule) string {
	if strings.TrimSpace(r.Label) != "" {
		return r.Label
	}
	return ruleDescription(r)
}

// ruleHasType reports whether a rule already covers a type, so the editor can
// pre-tick boxes.
func ruleHasType(r store.Rule, t int) bool {
	for _, v := range r.Types {
		if v == t {
			return true
		}
	}
	return false
}

type feedHealth struct {
	Healthy   bool
	Reason    string
	LastOK    time.Time
	TargetsKn int
}

func (s *Server) page(r *http.Request, title string) pageData {
	d := pageData{Title: title, Cfg: s.Cfg, Version: buildinfo.Version, Now: s.Store.Now().UTC()}
	if u, ok := userFrom(r.Context()); ok {
		d.User = &u
		d.CSRF = csrfFrom(r.Context())
	}
	return d
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	t, ok := s.pages[name]
	if !ok {
		s.Log.Error("no such page template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.Log.Error("rendering template", "template", name, "err", err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	d := s.page(r, "HopReact")
	if d.User != nil {
		http.Redirect(w, r, "/watches", http.StatusSeeOther)
		return
	}
	s.render(w, "index.html", d)
}

// handleHealthz deliberately reports only whether this process is alive. If
// it also checked CoreScope, Docker would restart the container during
// exactly the upstream outage the alert engine is designed to ride out.
// Feed freshness is a UI concern, shown on the dashboard.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

// ---------------------------------------------------------------- auth ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.DiscordConfigured() {
		http.Error(w, "Discord sign-in is not configured on this instance", http.StatusServiceUnavailable)
		return
	}
	state, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: 600,
	})
	http.Redirect(w, r, s.Discord.AuthorizeURL(state), http.StatusSeeOther)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateCookie.Value == "" ||
		subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "sign-in state mismatch — please try again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	du, err := s.Discord.Exchange(r.Context(), code)
	if err != nil {
		s.Log.Error("discord exchange failed", "err", err)
		http.Error(w, "Discord sign-in failed", http.StatusBadGateway)
		return
	}

	user, err := s.Store.UpsertUser(r.Context(), du.ID, du.DisplayName(), du.Avatar)
	if err != nil {
		s.Log.Error("upserting user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Whether we can actually reach them is a separate question from whether
	// they signed in, and worth answering now rather than at the moment
	// their repeater dies.
	if ok, err := s.Discord.CheckMembership(r.Context(), du.ID); err == nil {
		reason := ""
		if !ok {
			reason = "You are not in the HopReact Discord server, so the bot cannot DM you."
		}
		_ = s.Store.SetDMStatus(r.Context(), user.ID, ok, reason)
	}

	token, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	csrf, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256([]byte(token))
	expires := s.Store.Now().UTC().Add(sessionTTL)
	if err := s.Store.CreateSession(r.Context(), sum[:], user.ID, csrf, expires); err != nil {
		s.Log.Error("creating session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookieName(), Value: token, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
		Expires: expires,
	})
	http.Redirect(w, r, "/watches", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessionCookieName()); err == nil && c.Value != "" {
		sum := sha256.Sum256([]byte(c.Value))
		_ = s.Store.DeleteSession(r.Context(), sum[:])
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookieName(), Value: "", Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ------------------------------------------------------------- watches ---

func (s *Server) handleWatches(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := s.page(r, "Your nodes")
	views, err := s.Store.WatchViews(r.Context(), u.ID)
	if err != nil {
		s.Log.Error("loading watches", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d.Watches = views
	d.FeedHealth = s.feedHealth(r)
	d.Flash, d.FlashKind = flashFrom(r)
	s.render(w, "watches.html", d)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r.Context()); !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	q := r.URL.Query().Get("q")
	results, err := s.Store.SearchTargets(r.Context(), q, 40)
	if err != nil {
		s.Log.Error("searching targets", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d := s.page(r, "Add a node")
	d.Results = results
	d.Query = q
	d.FeedHealth = s.feedHealth(r)
	s.render(w, "search.html", d)
}

func (s *Server) handleAddWatch(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	kind := r.PostFormValue("kind")
	key := strings.ToLower(strings.TrimSpace(r.PostFormValue("key")))
	hours, _ := strconv.Atoi(r.PostFormValue("threshold_hours"))
	if hours < config.MinThresholdHours {
		hours = config.MinThresholdHours
	}
	if kind != "node" && kind != "observer" {
		redirectFlash(w, r, "/search", "That target type isn't valid.", "error")
		return
	}
	if key == "" {
		redirectFlash(w, r, "/search", "Pick a node or observer to watch.", "error")
		return
	}

	_, err := s.Store.CreateWatch(r.Context(), store.Watch{
		UserID: u.ID, TargetKind: kind, TargetKey: key,
		ThresholdHours: hours,
		AlertOnRelay:   r.PostFormValue("alert_on_relay") == "on",
	}, s.Cfg.Alerts.MaxWatchesPerUser)

	switch {
	case errors.Is(err, store.ErrDuplicateWatch):
		redirectFlash(w, r, "/watches", "You're already watching that one.", "error")
	case errors.Is(err, store.ErrWatchLimit):
		redirectFlash(w, r, "/watches",
			fmt.Sprintf("You've reached the limit of %d watched nodes.", s.Cfg.Alerts.MaxWatchesPerUser), "error")
	case err != nil:
		s.Log.Error("creating watch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		redirectFlash(w, r, "/watches", "Added. You'll get a DM if it goes quiet.", "ok")
	}
}

func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Store.DeleteWatch(r.Context(), u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("deleting watch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	redirectFlash(w, r, "/watches", "Removed.", "ok")
}

// handleUpdateWatch now only mutes: thresholds moved onto individual rules,
// since a watch can hold several with different ones.
func (s *Server) handleUpdateWatch(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var muted time.Time
	if h, _ := strconv.Atoi(r.PostFormValue("mute_hours")); h > 0 {
		muted = s.Store.Now().UTC().Add(time.Duration(h) * time.Hour)
	}
	if err := s.Store.SetWatchMute(r.Context(), u.ID, id, muted); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("updating watch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	msg := "Alerts muted."
	if muted.IsZero() {
		msg = "Alerts unmuted."
	}
	redirectFlash(w, r, fmt.Sprintf("/watches/%d", id), msg, "ok")
}

// handleWatchDetail renders one watch: its rules, and what each payload type
// was last seen doing.
func (s *Server) handleWatchDetail(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	view, err := s.Store.WatchViewByID(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("loading watch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	acts, err := s.Store.ActivityFor(r.Context(), view.Watch.TargetKind, view.Watch.TargetKey)
	if err != nil {
		s.Log.Error("loading activity", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	d := s.page(r, "Watch settings")
	d.Watch = &view
	d.Activity = activityRows(acts)
	d.Groups = typeGroups
	d.FeedHealth = s.feedHealth(r)
	d.Flash, d.FlashKind = flashFrom(r)
	s.render(w, "watch.html", d)
}

func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	dest := fmt.Sprintf("/watches/%d", id)

	hours, _ := strconv.Atoi(r.PostFormValue("threshold_hours"))
	if hours < config.MinThresholdHours {
		hours = config.MinThresholdHours
	}

	rule := store.Rule{WatchID: id, ThresholdHours: hours,
		Label: strings.TrimSpace(r.PostFormValue("label"))}

	switch r.PostFormValue("source") {
	case "seen":
		rule.Source, rule.Direction = store.SourceSeen, store.DirEither
	case "relayed":
		rule.Source, rule.Direction = store.SourceRelayed, store.DirCarried
	default:
		rule.Source = store.SourceTypes
		switch d := r.PostFormValue("direction"); d {
		case "sent", "carried":
			rule.Direction = store.Direction(d)
		default:
			rule.Direction = store.DirEither
		}
		// Types arrive as repeated checkbox values. Unknown numbers are
		// dropped rather than stored: a rule referring to a type that does
		// not exist would silently never match.
		if err := r.ParseForm(); err == nil {
			seen := map[int]bool{}
			for _, v := range r.PostForm["types"] {
				n, err := strconv.Atoi(v)
				if err != nil || seen[n] || !knownType(n) {
					continue
				}
				seen[n] = true
				rule.Types = append(rule.Types, n)
			}
		}
		sort.Ints(rule.Types)
		if len(rule.Types) == 0 {
			redirectFlash(w, r, dest, "Pick at least one kind of traffic for that rule.", "error")
			return
		}
	}

	_, err := s.Store.AddRule(r.Context(), u.ID, rule)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, store.ErrRuleLimit):
		redirectFlash(w, r, dest,
			fmt.Sprintf("That's the limit of %d rules on one node.", store.MaxRulesPerWatch), "error")
	case err != nil:
		s.Log.Error("adding rule", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		redirectFlash(w, r, dest, "Rule added.", "ok")
	}
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	dest := "/watches"
	if v := strings.TrimSpace(r.PostFormValue("watch_id")); v != "" {
		if wid, err := strconv.ParseInt(v, 10, 64); err == nil {
			dest = fmt.Sprintf("/watches/%d", wid)
		}
	}
	if err := s.Store.DeleteRule(r.Context(), u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("deleting rule", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	redirectFlash(w, r, dest, "Rule removed.", "ok")
}

// handleTestDM lets someone confirm delivery works before they rely on it,
// which matters because the failure mode is silence.
func (s *Server) handleTestDM(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	err := s.Discord.SendDM(r.Context(), u.DiscordID,
		"👋 This is a test from HopReact. Alerts about your nodes will arrive here.")
	if err != nil {
		reason := "Discord would not deliver the message."
		if errors.Is(err, discord.ErrUndeliverable) {
			reason = "Discord would not deliver the message — check you are still in the HopReact server and that DMs from server members are enabled."
		}
		_ = s.Store.SetDMStatus(r.Context(), u.ID, false, reason)
		redirectFlash(w, r, "/watches", reason, "error")
		return
	}
	_ = s.Store.SetDMStatus(r.Context(), u.ID, true, "")
	redirectFlash(w, r, "/watches", "Sent — check your Discord DMs.", "ok")
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.Store.DeleteUser(r.Context(), u.ID); err != nil {
		s.Log.Error("deleting account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// feedHealth summarises whether the upstream data can currently be trusted.
// Shown as a banner: someone looking at a page of green rows deserves to
// know if it is actually stale.
func (s *Server) feedHealth(r *http.Request) feedHealth {
	fh := feedHealth{Healthy: true}
	runs, err := s.Store.RecentPollRuns(r.Context(), 5)
	if err != nil || len(runs) == 0 {
		fh.Healthy = false
		fh.Reason = "No successful poll yet."
		return fh
	}
	latest := runs[0]
	if latest.Status != store.PollOK {
		fh.Healthy = false
		fh.Reason = latest.SuppressedReason
		if fh.Reason == "" {
			fh.Reason = latest.Error
		}
	}
	for _, run := range runs {
		if run.Status == store.PollOK {
			fh.LastOK = run.StartedAt
			break
		}
	}
	fh.TargetsKn, _ = s.Store.CountTargets(r.Context())
	return fh
}

// ------------------------------------------------------------- helpers ---

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Flashes ride in the query string. There is no server-side flash store
// because there is nothing here worth the extra state.
func redirectFlash(w http.ResponseWriter, r *http.Request, path, msg, kind string) {
	q := url.Values{}
	q.Set("msg", msg)
	q.Set("kind", kind)
	http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
}

func flashFrom(r *http.Request) (string, string) {
	return r.URL.Query().Get("msg"), r.URL.Query().Get("kind")
}

// canRelay reports whether "stopped passing traffic" is a question that can
// even be asked of this target.
//
// Only a repeater forwards other people's packets. An observer reports what
// it hears rather than relaying it, and a companion originates and receives
// only — it never relays, so offering the option there would be a control
// that can never do anything. (The alert engine already refuses to fire on
// a target that has never relayed; this stops the UI implying otherwise in
// the first place.)
func canRelay(kind, role string) bool {
	return kind == string(corescope.KindNode) && role == "repeater"
}

// freshClass maps a last-seen time onto the same three-state colour the
// dashboard uses, so "seen 4m ago" and "seen 9d ago" don't look alike at a
// glance.
func freshClass(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "muted"
	}
	switch age := now.Sub(t); {
	case age <= 6*time.Hour:
		return "good"
	case age <= 24*time.Hour:
		return "warn"
	default:
		return "bad"
	}
}

// shortKey trims a 64-hex public key to something readable. The full value
// stays in the element's title attribute, so it is still copyable.
func shortKey(k string) string {
	if len(k) <= 20 {
		return k
	}
	return k[:10] + "…" + k[len(k)-6:]
}

func humanSince(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04 UTC")
}

func stateLabel(st store.WatchState) string {
	switch st.State {
	case store.StateAlerting:
		if st.Seeded {
			// Worth distinguishing: this one was already down when it was
			// added, so no alert was sent and none is owed.
			return "Quiet (was already down when you added it)"
		}
		return "Quiet — alerted"
	case store.StatePending:
		return "Going quiet…"
	case store.StateRecovering:
		return "Coming back…"
	case store.StateOK:
		return "OK"
	default:
		return "No data yet"
	}
}

func stateClass(st store.WatchState) string {
	switch st.State {
	case store.StateAlerting:
		return "bad"
	case store.StatePending, store.StateRecovering:
		return "warn"
	case store.StateOK:
		return "good"
	default:
		return "muted"
	}
}
