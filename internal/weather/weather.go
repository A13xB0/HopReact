// Package weather fetches hourly weather for a place from Open-Meteo and
// keeps it in a cache, so a page view rarely waits on the network.
//
// Positions are rounded to a 0.1° grid square (about 7 by 11 km in the UK)
// before anything is sent, so a repeater's exact site never leaves HopReact,
// and repeaters near each other share one cached copy.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Hour is one hour of weather. At is the end of the hour: sunlight is the
// mean over the hour before At, and temperature and cloud are as at At.
type Hour struct {
	At       time.Time
	TempC    float64
	CloudPct float64
	// SunWm2 is sunlight reaching flat ground (shortwave radiation), W/m².
	SunWm2 float64
}

// Cell is a 0.1° grid square.
type Cell struct {
	Lat, Lon float64
}

// CellFor rounds a position to its grid square.
func CellFor(lat, lon float64) Cell {
	return Cell{Lat: math.Round(lat*10) / 10, Lon: math.Round(lon*10) / 10}
}

// Key names the square in the cache.
func (c Cell) Key() string { return fmt.Sprintf("%.1f,%.1f", c.Lat, c.Lon) }

// MaxPastDays is how far back Open-Meteo's forecast API keeps hourly data.
const MaxPastDays = 92

// ForecastDays is how far ahead we ask for.
const ForecastDays = 8

// Client calls Open-Meteo.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// Fetch gets hourly weather for a grid square: pastDays back and
// ForecastDays ahead.
func (c *Client) Fetch(ctx context.Context, cell Cell, pastDays int) ([]Hour, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.1f", cell.Lat))
	q.Set("longitude", fmt.Sprintf("%.1f", cell.Lon))
	q.Set("hourly", "temperature_2m,cloud_cover,shortwave_radiation")
	q.Set("past_days", fmt.Sprint(pastDays))
	q.Set("forecast_days", fmt.Sprint(ForecastDays))
	q.Set("timezone", "GMT")
	q.Set("timeformat", "unixtime")
	u := strings.TrimRight(c.BaseURL, "/") + "/v1/forecast?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "HopReact (battery analysis)")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weather: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weather: status %d", resp.StatusCode)
	}
	var raw struct {
		Hourly struct {
			Time  []int64    `json:"time"`
			Temp  []*float64 `json:"temperature_2m"`
			Cloud []*float64 `json:"cloud_cover"`
			Sun   []*float64 `json:"shortwave_radiation"`
		} `json:"hourly"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("weather: decoding: %w", err)
	}
	h := raw.Hourly
	if len(h.Temp) != len(h.Time) || len(h.Cloud) != len(h.Time) || len(h.Sun) != len(h.Time) {
		return nil, errors.New("weather: hourly series differ in length")
	}
	out := make([]Hour, 0, len(h.Time))
	for i, t := range h.Time {
		// Open-Meteo leaves the newest hours of some models blank.
		if h.Temp[i] == nil || h.Cloud[i] == nil || h.Sun[i] == nil {
			continue
		}
		out = append(out, Hour{At: time.Unix(t, 0).UTC(), TempC: *h.Temp[i], CloudPct: *h.Cloud[i], SunWm2: *h.Sun[i]})
	}
	return out, nil
}

// Cache stores fetched hours per grid square.
type Cache interface {
	WeatherHours(ctx context.Context, cell string, from, to time.Time) ([]Hour, error)
	// SaveWeather replaces the square's hours from the earliest hour given
	// onwards and records the fetch.
	SaveWeather(ctx context.Context, cell string, hours []Hour, fetchedAt time.Time, pastDays int) error
	// WeatherFetched says when the square was last fetched and how far back
	// that fetch reached.
	WeatherFetched(ctx context.Context, cell string) (at time.Time, pastDays int, ok bool, err error)
}

// Service answers "what was and will the weather be here", from the cache
// when it is fresh enough.
type Service struct {
	Client *Client
	Cache  Cache
	Now    func() time.Time
	// FreshFor is how long a fetch is reused. Past hours barely change once
	// fetched; this mostly decides how current the forecast is.
	FreshFor time.Duration
	// RetryAfter is how long to wait after a failed fetch before trying
	// again, so an outage doesn't add a timeout to every page view.
	RetryAfter time.Duration

	mu     sync.Mutex
	failed map[string]time.Time
}

// ErrUnavailable means the weather couldn't be fetched and nothing cached
// covers the request.
var ErrUnavailable = errors.New("weather unavailable")

// Result is the weather for a request.
type Result struct {
	Hours []Hour
	// Stale is set when a fetch failed and these are older cached hours.
	Stale bool
}

// Hours returns the hourly weather between from and to at a position.
func (s *Service) Hours(ctx context.Context, lat, lon float64, from, to time.Time) (Result, error) {
	cell := CellFor(lat, lon)
	key := cell.Key()
	now := s.now()
	need := int(math.Ceil(now.Sub(from).Hours()/24)) + 1
	need = max(7, min(need, MaxPastDays))

	s.mu.Lock()
	defer s.mu.Unlock()
	at, past, ok, err := s.Cache.WeatherFetched(ctx, key)
	if err != nil {
		return Result{}, err
	}
	fresh := ok && now.Sub(at) < s.freshFor() && past >= need
	stale := false
	if !fresh {
		if t, bad := s.failed[key]; bad && now.Sub(t) < s.retryAfter() {
			stale = true
		} else if hours, ferr := s.Client.Fetch(ctx, cell, need); ferr != nil {
			if s.failed == nil {
				s.failed = map[string]time.Time{}
			}
			s.failed[key] = now
			stale = true
		} else {
			delete(s.failed, key)
			if err := s.Cache.SaveWeather(ctx, key, hours, now, need); err != nil {
				return Result{}, err
			}
		}
	}
	hours, err := s.Cache.WeatherHours(ctx, key, from, to)
	if err != nil {
		return Result{}, err
	}
	if stale && len(hours) == 0 {
		return Result{}, ErrUnavailable
	}
	return Result{Hours: hours, Stale: stale}, nil
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Service) freshFor() time.Duration {
	if s.FreshFor <= 0 {
		return time.Hour
	}
	return s.FreshFor
}

func (s *Service) retryAfter() time.Duration {
	if s.RetryAfter <= 0 {
		return 5 * time.Minute
	}
	return s.RetryAfter
}
