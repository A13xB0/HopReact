package web

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"hopreact/internal/corescope"
	"hopreact/internal/store"
)

// batteryServer is newServer with battery alerts on at ScotMesh's levels, and
// one watched repeater with a draining week of readings.
func batteryServer(t *testing.T) (*Server, *store.Store, http.Handler, *http.Cookie, string, store.User, int64) {
	t.Helper()
	srv, st, h := newServer(t)
	srv.Cfg.Battery.Enabled = true
	srv.Cfg.Battery.LowMV, srv.Cfg.Battery.CriticalMV = 3650, 3500
	cookie, csrf, u := signIn(t, srv, st)
	ctx := context.Background()
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{{
		Kind: corescope.KindNode, Key: "aa", Name: "Bowden_R63", Role: "repeater", LastSeen: t0.Add(-time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	id := addWatch(t, st, u.ID, "aa")
	var samples []corescope.BatterySample
	for i := 0; i <= 7*24; i++ {
		samples = append(samples, corescope.BatterySample{At: t0.Add(-time.Duration(7*24-i) * time.Hour), MV: 3950 - i*2})
	}
	if err := st.SaveBatteryReadings(ctx, store.BatteryReadings{
		Key: "aa", Status: corescope.BatteryOK, LatestMV: 3614, LatestAt: t0.Add(-time.Hour),
		Reporter: "WMF RakPi OH", Samples: samples, FetchedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	return srv, st, h, cookie, csrf, u, id
}

func get(t *testing.T, h http.Handler, cookie *http.Cookie, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, rec.Code)
	}
	return rec.Body.String()
}

func flashOf(rec *httptest.ResponseRecorder) string {
	loc, _ := url.Parse(rec.Header().Get("Location"))
	return loc.Query().Get("msg")
}

func TestBatteryCardOffThenOn(t *testing.T) {
	_, st, h, cookie, csrf, _, id := batteryServer(t)
	page := "/watches/" + itoa(id)
	body := get(t, h, cookie, page)
	for _, want := range []string{"3.61 V", "Battery alerts are off for this repeater", "↓ Draining about"} {
		if !strings.Contains(body, want) {
			t.Errorf("off card lacks %q", want)
		}
	}
	rec := postForm(t, h, cookie, page+"/battery", url.Values{"csrf_token": {csrf}, "action": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on: %d", rec.Code)
	}
	if _, has, _ := st.BatteryRuleFor(context.Background(), id); !has {
		t.Fatal("no rule after turning on")
	}
	body = get(t, h, cookie, page)
	for _, want := range []string{"Low below 3.65 V · critical below 3.50 V · clears at 3.75 V",
		"Overcharge alert", "Edit levels", "Turn off battery alerts", `<svg class="spark"`, "Critical (3.50 V)"} {
		if !strings.Contains(body, want) {
			t.Errorf("on card lacks %q", want)
		}
	}
	list := get(t, h, cookie, "/watches")
	if !strings.Contains(list, "3.61 V low") || !strings.Contains(list, "Low battery · below 3.65 V") {
		t.Errorf("watches list lacks the battery chip or rule")
	}
}

func TestBatteryLevelsValidated(t *testing.T) {
	_, st, h, cookie, csrf, u, id := batteryServer(t)
	ctx := context.Background()
	if err := st.AddBatteryRule(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	page := "/watches/" + itoa(id) + "/battery"
	post := func(v url.Values) string {
		v.Set("csrf_token", csrf)
		v.Set("action", "levels")
		return flashOf(postForm(t, h, cookie, page, v))
	}
	if msg := post(url.Values{"critical": {"3.70"}}); !strings.Contains(msg, "must be lower than low") {
		t.Errorf("critical above the site's low accepted: %q", msg)
	}
	if msg := post(url.Values{"low": {"banana"}}); !strings.Contains(msg, "isn't a voltage") {
		t.Errorf("bad number: %q", msg)
	}
	if msg := post(url.Values{"low": {"5"}}); !strings.Contains(msg, "outside") {
		t.Errorf("out of range: %q", msg)
	}
	if msg := post(url.Values{"low": {"3.10"}, "critical": {"3.00"}}); !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("LiFePO4 levels refused: %q", msg)
	}
	r, _, _ := st.BatteryRuleFor(ctx, id)
	if r.LowMV != 3100 || r.CriticalMV != 3000 || r.OverchargeOn {
		t.Errorf("stored %+v", r)
	}
	// Blank goes back to following the site.
	if msg := post(url.Values{"low": {""}, "critical": {""}, "overcharge": {"on"}}); !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("blank refused: %q", msg)
	}
	r, _, _ = st.BatteryRuleFor(ctx, id)
	if r.LowMV != 0 || r.CriticalMV != 0 || !r.OverchargeOn || r.OverchargeMV != 0 {
		t.Errorf("stored %+v", r)
	}
}

func TestBatteryOverchargeOneClick(t *testing.T) {
	_, st, h, cookie, csrf, u, id := batteryServer(t)
	ctx := context.Background()
	if err := st.AddBatteryRule(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	postForm(t, h, cookie, "/watches/"+itoa(id)+"/battery", url.Values{"csrf_token": {csrf}, "action": {"overcharge_on"}})
	r, _, _ := st.BatteryRuleFor(ctx, id)
	if !r.OverchargeOn {
		t.Fatal("overcharge not on")
	}
	if body := get(t, h, cookie, "/watches/"+itoa(id)); !strings.Contains(body, "On · at or above 4.25 V") {
		t.Error("page doesn't show the overcharge alert on at 4.25 V")
	}
}

func TestBatteryCannotTouchAnotherUsersWatch(t *testing.T) {
	_, st, h, _, _, _, id := batteryServer(t)
	ctx := context.Background()
	other, _ := st.UpsertUser(ctx, "d2", "bob", "")
	token, _ := randomToken()
	csrf2, _ := randomToken()
	sum := sha256Sum(token)
	if err := st.CreateSession(ctx, sum, other.ID, csrf2, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cookie2 := &http.Cookie{Name: "hopreact_session", Value: token}
	rec := postForm(t, h, cookie2, "/watches/"+itoa(id)+"/battery", url.Values{"csrf_token": {csrf2}, "action": {"on"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if _, has, _ := st.BatteryRuleFor(ctx, id); has {
		t.Fatal("another user's watch was changed")
	}
}

func TestBatteryHiddenWhenOff(t *testing.T) {
	srv, st, h := newServer(t)
	cookie, _, u := signIn(t, srv, st)
	id := addWatch(t, st, u.ID, "aa")
	body := get(t, h, cookie, "/watches/"+itoa(id))
	if strings.Contains(body, "Turn off battery alerts") || strings.Contains(body, "Edit levels") {
		t.Error("battery card shown with battery alerts off")
	}
	if strings.Contains(get(t, h, cookie, "/search?q=a"), "battery gets low") {
		t.Error("battery box offered with battery alerts off")
	}
}

func TestAddNodeBatteryBox(t *testing.T) {
	_, st, h, cookie, csrf, u, _ := batteryServer(t)
	ctx := context.Background()
	for _, k := range []string{"bb", "cc"} {
		if _, err := st.UpsertTargets(ctx, []corescope.Observation{{Kind: corescope.KindNode, Key: k, Name: k, Role: "repeater", LastSeen: t0}}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(get(t, h, cookie, "/search?q=bb"), "battery gets low") {
		t.Fatal("search doesn't offer the battery box")
	}
	postForm(t, h, cookie, "/watches", url.Values{"csrf_token": {csrf}, "kind": {"node"}, "key": {"bb"},
		"battery_offered": {"1"}, "battery": {"on"}})
	postForm(t, h, cookie, "/watches", url.Values{"csrf_token": {csrf}, "kind": {"node"}, "key": {"cc"},
		"battery_offered": {"1"}})
	views, _ := st.WatchViews(ctx, u.ID)
	for _, v := range views {
		_, has, _ := st.BatteryRuleFor(ctx, v.Watch.ID)
		switch v.Watch.TargetKey {
		case "bb":
			if !has {
				t.Error("ticked box gave no battery rule")
			}
		case "cc":
			if has || v.Watch.BatteryAddedAt.IsZero() {
				t.Errorf("unticked box: rule=%v, opted out=%v", has, !v.Watch.BatteryAddedAt.IsZero())
			}
		}
	}
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
