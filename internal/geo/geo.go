// Package geo answers one question: is this point inside the region an
// operator cares about?
//
// HopReact was built against a Scottish mesh, but the packet feed carries far
// more than that — of 659 repeaters on the live instance only 71 are actually
// in Scotland, the rest being Irish, Northern Irish, Manx and northern
// English nodes arriving over MQTT bridging. Any public board that listed all
// of them would be listing mostly other people's infrastructure.
//
// So the region is supplied by the operator as a GeoJSON file rather than
// baked in. Point it at Scotland, at a county, or at nothing at all.
package geo

import (
	"encoding/json"
	"fmt"
	"os"
)

// Region is a set of polygons a point can be tested against.
type Region struct {
	Name string
	// rings holds every outer ring, flattened. Interior rings (holes) are
	// deliberately ignored: on a landmass they are lochs and inlets, and a
	// node sitting on an island in a loch should still count as inside.
	rings [][][2]float64
}

// Empty reports whether this region excludes nothing.
func (r *Region) Empty() bool { return r == nil || len(r.rings) == 0 }

// Contains reports whether a point falls inside, by ray casting. A point on
// no ring at all is outside.
func (r *Region) Contains(lat, lon float64) bool {
	if r.Empty() {
		return false
	}
	for _, ring := range r.rings {
		inside := false
		n := len(ring)
		for i, j := 0, n-1; i < n; j, i = i, i+1 {
			xi, yi := ring[i][0], ring[i][1]
			xj, yj := ring[j][0], ring[j][1]
			if (yi > lat) != (yj > lat) && lon < (xj-xi)*(lat-yi)/(yj-yi)+xi {
				inside = !inside
			}
		}
		if inside {
			return true
		}
	}
	return false
}

// Rings is how many polygons were loaded, for logging.
func (r *Region) Rings() int {
	if r == nil {
		return 0
	}
	return len(r.rings)
}

// geoJSON covers the shapes an operator is likely to hand us: a bare
// geometry, a Feature, or a FeatureCollection.
type geoJSON struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Properties struct {
		Name string `json:"name"`
	} `json:"properties"`
	Coordinates json.RawMessage `json:"coordinates"`
	Geometry    *geoJSON        `json:"geometry"`
	Features    []geoJSON       `json:"features"`
}

// Load reads a GeoJSON file. An empty path returns a nil Region, which means
// "no filter" rather than "nothing matches" — callers check Empty.
func Load(path string) (*Region, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("geo: reading %s: %w", path, err)
	}
	var doc geoJSON
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("geo: parsing %s: %w", path, err)
	}
	r := &Region{Name: firstNonEmpty(doc.Properties.Name, doc.Name)}
	if err := collect(&doc, r); err != nil {
		return nil, fmt.Errorf("geo: %s: %w", path, err)
	}
	if len(r.rings) == 0 {
		return nil, fmt.Errorf("geo: %s contains no polygons", path)
	}
	return r, nil
}

func collect(d *geoJSON, r *Region) error {
	switch d.Type {
	case "FeatureCollection":
		for i := range d.Features {
			if r.Name == "" {
				r.Name = firstNonEmpty(d.Features[i].Properties.Name, d.Features[i].Name)
			}
			if err := collect(&d.Features[i], r); err != nil {
				return err
			}
		}
	case "Feature":
		if d.Geometry == nil {
			return nil
		}
		return collect(d.Geometry, r)
	case "Polygon":
		var poly [][][2]float64
		if err := json.Unmarshal(d.Coordinates, &poly); err != nil {
			return fmt.Errorf("bad Polygon: %w", err)
		}
		if len(poly) > 0 {
			r.rings = append(r.rings, poly[0])
		}
	case "MultiPolygon":
		var multi [][][][2]float64
		if err := json.Unmarshal(d.Coordinates, &multi); err != nil {
			return fmt.Errorf("bad MultiPolygon: %w", err)
		}
		for _, poly := range multi {
			if len(poly) > 0 {
				r.rings = append(r.rings, poly[0])
			}
		}
	case "GeometryCollection":
		return fmt.Errorf("GeometryCollection is not supported; use a Feature, FeatureCollection, Polygon or MultiPolygon")
	default:
		return fmt.Errorf("unsupported GeoJSON type %q", d.Type)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
