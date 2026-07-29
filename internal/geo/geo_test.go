package geo

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.geojson")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A square over the middle of Scotland, in GeoJSON's lon,lat order.
const square = `{"type":"Feature","properties":{"name":"Test region"},
 "geometry":{"type":"Polygon","coordinates":[[[-5,55],[-2,55],[-2,58],[-5,58],[-5,55]]]}}`

func TestLoadAndContains(t *testing.T) {
	r, err := Load(write(t, square))
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "Test region" {
		t.Errorf("name = %q", r.Name)
	}
	// Coordinates are lon,lat in the file but Contains takes lat,lon — the
	// easiest thing in the world to get backwards, so it is asserted.
	if !r.Contains(56.5, -3.5) {
		t.Error("a point in the middle should be inside")
	}
	if r.Contains(-3.5, 56.5) {
		t.Error("lat and lon are being read the wrong way round")
	}
	if r.Contains(51.5, -0.1) {
		t.Error("London is not in the square")
	}
}

// An empty path means "no filter", not "nothing matches".
func TestNoPathIsNoFilter(t *testing.T) {
	r, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Empty() {
		t.Error("no path should give an empty region")
	}
	if r.Contains(56, -3) {
		t.Error("an empty region contains nothing; callers check Empty first")
	}
}

// Whatever an operator exports, it should load: bare geometry, Feature, or
// FeatureCollection, Polygon or MultiPolygon.
func TestAcceptsTheUsualShapes(t *testing.T) {
	for name, body := range map[string]string{
		"bare Polygon":      `{"type":"Polygon","coordinates":[[[-5,55],[-2,55],[-2,58],[-5,58],[-5,55]]]}`,
		"MultiPolygon":      `{"type":"MultiPolygon","coordinates":[[[[-5,55],[-2,55],[-2,58],[-5,58],[-5,55]]]]}`,
		"Feature":           square,
		"FeatureCollection": `{"type":"FeatureCollection","features":[` + square + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Load(write(t, body))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !r.Contains(56.5, -3.5) {
				t.Errorf("%s: loaded but does not contain the test point", name)
			}
		})
	}
}

// A misconfigured region must fail loudly at startup. A board that silently
// covers the world instead of one country is worse than one that won't boot.
func TestBadInputFails(t *testing.T) {
	for name, body := range map[string]string{
		"not json":    `{`,
		"no polygons": `{"type":"FeatureCollection","features":[]}`,
		"a point":     `{"type":"Point","coordinates":[-3,56]}`,
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := Load("/nonexistent/nope.geojson"); err == nil {
		t.Error("a missing file should error")
	}
}

// The real thing, if it is to hand: the boundary HopReach ships.
func TestAgainstScotlandIfPresent(t *testing.T) {
	const p = "../../../mccoverage/internal/geo/scotland.geojson"
	if _, err := os.Stat(p); err != nil {
		t.Skip("scotland.geojson not available")
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		lat, lon float64
		in       bool
	}{
		{"Inverness", 57.4667, -4.1633, true},
		{"Edinburgh", 55.953, -3.188, true},
		{"Stornoway", 58.21, -6.39, true},
		{"Preston", 53.76, -2.69, false},
		{"Dublin", 53.35, -6.26, false},
		{"Belfast", 54.6, -5.93, false},
	} {
		if got := r.Contains(c.lat, c.lon); got != c.in {
			t.Errorf("%s: inside=%v, want %v", c.name, got, c.in)
		}
	}
}
