package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"database/sql"

	"hopreact/internal/corescope"
	"hopreact/internal/store"
)

// TestDumpPages writes rendered pages to $HOPREACT_DUMP for visual checking.
// Skipped unless that is set.
func TestDumpPages(t *testing.T) {
	dir := os.Getenv("HOPREACT_DUMP")
	if dir == "" {
		t.Skip("set HOPREACT_DUMP to write pages")
	}
	srv, st, h := newServer(t)
	ctx := context.Background()
	const dual = "463f15467ceba721cf903a33e0da069627d86ddfa21f397a14a128fa166d8976"
	if _, err := st.UpsertTargets(ctx, []corescope.Observation{
		{Kind: corescope.KindNode, Key: "aa11bb22cc33dd44ee55ff66778899aabbccddeeff00112233445566778899aa",
			Name: "Ben Nevis Repeater", Role: "repeater",
			LastSeen: t0.Add(-3 * time.Minute), LastRelayed: t0.Add(-9 * time.Minute)},
		{Kind: corescope.KindNode, Key: dual, Name: "Cadham Village", Role: "repeater",
			LastSeen: t0.Add(-time.Minute), LastRelayed: t0.Add(-2 * time.Minute)},
		{Kind: corescope.KindObserver, Key: dual, Name: "Cadham Village Observer",
			LastSeen: t0.Add(-time.Minute), LastPacket: t0.Add(-31 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _, u := signIn(t, srv, st)

	nodeID := addWatch(t, st, u.ID, "aa11bb22cc33dd44ee55ff66778899aabbccddeeff00112233445566778899aa")
	obsID := addObserverWatch(t, st, u.ID, dual)

	// A rich rule set, as someone would actually build.
	if _, err := st.AddRule(ctx, u.ID, store.Rule{
		WatchID: nodeID, Label: "Standard", Source: store.SourceTypes,
		Types:     []int{1, 2, 3, 4, 5, 8, 9, 11},
		Direction: store.DirEither, ThresholdHours: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRule(ctx, u.ID, store.Rule{
		WatchID: nodeID, Label: "Carrying channel traffic", Source: store.SourceTypes,
		Types:     []int{5, 6},
		Direction: store.DirCarried, ThresholdHours: 12,
	}); err != nil {
		t.Fatal(err)
	}
	// Some activity so the table has content.
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error { return nil }); err != nil {
		_ = err
	}
	for _, a := range []struct {
		typ int
		dir store.Direction
		ago time.Duration
		n   int
	}{{4, store.DirSent, 4 * time.Minute, 1204}, {4, store.DirCarried, 18 * time.Minute, 318},
		{5, store.DirCarried, 12 * time.Minute, 217}, {1, store.DirCarried, 2 * time.Hour, 47},
		{2, store.DirCarried, 3 * time.Hour, 63}, {8, store.DirCarried, 26 * time.Hour, 18}} {
		if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
			return st.UpsertActivity(ctx, tx, "node",
				"aa11bb22cc33dd44ee55ff66778899aabbccddeeff00112233445566778899aa",
				a.typ, a.dir, t0.Add(-a.ago), a.n)
		}); err != nil {
			t.Fatal(err)
		}
	}

	pages := map[string]string{
		"index.html":    "/",
		"watches.html":  "/watches",
		"search.html":   "/search",
		"watch.html":    "/watches/" + itoa(nodeID),
		"observer.html": "/watches/" + itoa(obsID),
	}
	for name, path := range pages {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if err := os.WriteFile(filepath.Join(dir, name), rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The stylesheet is inline in layout.html, so the files are self-contained
	// apart from the script, which we copy alongside.
	js, _ := staticFS.ReadFile("static/rules.js")
	os.MkdirAll(filepath.Join(dir, "static"), 0o755)
	os.WriteFile(filepath.Join(dir, "static", "rules.js"), js, 0o644)
	t.Logf("wrote %d pages to %s", len(pages), dir)
}
