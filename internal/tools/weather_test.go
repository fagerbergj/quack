package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubOpenMeteo mimics Open-Meteo's timezone=auto + timeformat=unixtime: start_date/end_date are
// read at a FIXED "today" offset (CDT here), not the offset in force on those dates.
type stubOpenMeteo struct {
	geocodeHits, forecastHits atomic.Int32
	lastForecast              atomic.Value // url.Values
	geocodeBody               string
	maxDate                   string // horizon; later end_date gets a 400 like the real API
	forecastStatus            int
	forecastBody              string
}

var stubTodayOffset = time.FixedZone("CDT", -5*3600)

func (s *stubOpenMeteo) serve(t *testing.T) weatherAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/search":
			s.geocodeHits.Add(1)
			_, _ = w.Write([]byte(s.geocodeBody))
		case "/forecast":
			s.forecastHits.Add(1)
			s.lastForecast.Store(q)
			switch {
			case s.forecastStatus != 0:
				w.WriteHeader(s.forecastStatus)
				_, _ = w.Write([]byte(s.forecastBody))
			case s.maxDate != "" && q.Get("end_date") > s.maxDate:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":true,"reason":"Parameter 'end_date' is out of allowed range to %s"}`, s.maxDate)
			default:
				_, _ = w.Write([]byte(forecastFixture(t, q)))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	now := func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }
	return weatherAPI{client: srv.Client(), cache: NewURLCache(), forecastURL: srv.URL + "/forecast", geocodeURL: srv.URL + "/search", now: now}
}

// forecastFixture: one row per hour; temperature = the row's UTC hour, weather_code 63 (rain).
func forecastFixture(t *testing.T, q url.Values) string {
	start, err := time.ParseInLocation(time.DateOnly, q.Get("start_date"), stubTodayOffset)
	if err != nil {
		t.Errorf("start_date %q: %v", q.Get("start_date"), err)
	}
	end, _ := time.ParseInLocation(time.DateOnly, q.Get("end_date"), stubTodayOffset)
	var ts, temps []string
	for h := start; h.Before(end.AddDate(0, 0, 1)); h = h.Add(time.Hour) {
		ts = append(ts, fmt.Sprint(h.Unix()))
		temps = append(temps, fmt.Sprint(h.UTC().Hour()))
	}
	n := len(ts)
	return fmt.Sprintf(`{"latitude":41.86,"longitude":-87.62,"timezone":"America/Chicago","timezone_abbreviation":"GMT-5","utc_offset_seconds":-18000,
		"hourly_units":{"time":"unixtime","temperature_2m":"°F","wind_speed_10m":"mp/h","precipitation":"inch"},
		"hourly":{"time":[%s],"temperature_2m":[%s],"wind_speed_10m":[%s],"wind_gusts_10m":[%s],
		"precipitation_probability":[%s],"precipitation":[%s],"weather_code":[%s]}}`,
		strings.Join(ts, ","), strings.Join(temps, ","), repeatJSON("22.5", n), repeatJSON("null", n),
		repeatJSON("80", n), repeatJSON("0.1", n), repeatJSON("63", n))
}

func repeatJSON(v string, n int) string {
	return strings.TrimSuffix(strings.Repeat(v+",", n), ",")
}

func f64(v float64) *float64 { return &v }

func rowTimes(h []weatherHour) []string {
	out := make([]string, len(h))
	for i, r := range h {
		out[i] = r.Time + "|" + r.Local
	}
	return out
}

// Nov 1 2026 is the US fall-back date: noon CST is 18:00Z, not the 17:00Z today's CDT offset would give.
func TestWeatherKickoffAcrossDSTChange(t *testing.T) {
	s := &stubOpenMeteo{}
	w := s.serve(t)
	got, err := w.forecast(context.Background(), weatherArgs{Latitude: f64(41.86), Longitude: f64(-87.62), Kickoff: "2026-11-01T18:00:00Z", Hours: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := "[2026-11-01T18:00Z|2026-11-01 12:00 CST 2026-11-01T19:00Z|2026-11-01 13:00 CST 2026-11-01T20:00Z|2026-11-01 14:00 CST]"
	if fmt.Sprint(rowTimes(got.Hourly)) != want {
		t.Errorf("rows = %v\nwant %s", rowTimes(got.Hourly), want)
	}
	h := got.Hourly[0]
	if *h.Temperature != 18 || *h.WindSpeed != 22.5 || h.WindGusts != nil || h.Conditions != "rain" {
		t.Errorf("row = %+v", h)
	}
	if got.Units["wind_speed"] != "mph" || got.Units["temperature"] != "°F" || !strings.HasPrefix(got.Source, "Open-Meteo forecast") {
		t.Errorf("units = %v, source = %q", got.Units, got.Source)
	}
	if q := s.lastForecast.Load().(url.Values); q.Get("timezone") != "auto" || q.Get("timeformat") != "unixtime" || q.Get("wind_speed_unit") != "mph" {
		t.Errorf("forecast query = %v", q)
	}
	if s.geocodeHits.Load() != 0 {
		t.Error("coordinates must skip geocoding")
	}
}

func TestWeatherLocalDateAcrossDSTChange(t *testing.T) {
	s := &stubOpenMeteo{geocodeBody: `{"results":[{"name":"Chicago","admin1":"Illinois","country":"United States","latitude":41.85,"longitude":-87.65}]}`}
	w := s.serve(t)
	got, err := w.forecast(context.Background(), weatherArgs{Location: "Chicago, Illinois, United States", Date: "2026-11-01", Time: "12:25", Hours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != "Chicago, Illinois, United States" || got.Timezone != "America/Chicago" {
		t.Errorf("place = %q / %q", got.Location, got.Timezone)
	}
	if len(got.Hourly) != 3 || got.Hourly[0].Time != "2026-11-01T18:00Z" || got.Hourly[2].Time != "2026-11-01T20:00Z" {
		t.Errorf("12:25 CST for 2h = %v, want the 18:00Z-20:00Z hours", rowTimes(got.Hourly))
	}
	day, err := w.forecast(context.Background(), weatherArgs{Latitude: f64(41.86), Longitude: f64(-87.62), Date: "2026-11-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(day.Hourly) != 25 || day.Hourly[0].Local != "2026-11-01 00:00 CDT" || day.Hourly[24].Local != "2026-11-01 23:00 CST" {
		t.Errorf("fall-back day = %d rows %v, want 25 from 00:00 CDT to 23:00 CST", len(day.Hourly), rowTimes(day.Hourly))
	}
}

func TestWeatherHorizonRetriesUnpadded(t *testing.T) {
	s := &stubOpenMeteo{maxDate: "2026-10-11"}
	w := s.serve(t)
	got, err := w.forecast(context.Background(), weatherArgs{Latitude: f64(41.86), Longitude: f64(-87.62), Kickoff: "2026-10-11T17:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hourly) != 4 || s.forecastHits.Load() != 2 {
		t.Errorf("rows = %d, hits = %d; want 4 rows after one unpadded retry", len(got.Hourly), s.forecastHits.Load())
	}
	_, err = w.forecast(context.Background(), weatherArgs{Latitude: f64(41.86), Longitude: f64(-87.62), Kickoff: "2026-10-20T17:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "out of allowed range") {
		t.Errorf("past-horizon err = %v", err)
	}
}

func TestWeatherPastWindowAndCache(t *testing.T) {
	s := &stubOpenMeteo{}
	w := s.serve(t)
	a := weatherArgs{Latitude: f64(41.86), Longitude: f64(-87.62), Kickoff: "2026-09-20T17:00:00-04:00", Units: "metric"}
	for range 2 {
		got, err := w.forecast(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Source, "archived") || got.Hourly[0].Time != "2026-09-20T21:00Z" {
			t.Errorf("past window source = %q, first row %s", got.Source, got.Hourly[0].Time)
		}
	}
	if q := s.lastForecast.Load().(url.Values); q.Has("temperature_unit") {
		t.Errorf("metric must use Open-Meteo defaults, got %v", q)
	}
	if s.forecastHits.Load() != 1 {
		t.Errorf("forecast hits = %d, want 1 (second call cached)", s.forecastHits.Load())
	}
}

func TestWeatherErrors(t *testing.T) {
	coords := weatherArgs{Latitude: f64(1), Longitude: f64(1), Kickoff: "2026-10-11T17:00:00Z"}
	cases := []struct {
		name string
		stub *stubOpenMeteo
		args weatherArgs
		want string
	}{
		{"no window", &stubOpenMeteo{}, weatherArgs{Location: "x"}, "kickoff (RFC3339) or date"},
		{"bad kickoff", &stubOpenMeteo{}, weatherArgs{Location: "x", Kickoff: "Sun 1pm"}, "RFC3339"},
		{"bad time", &stubOpenMeteo{}, weatherArgs{Location: "x", Date: "2026-10-11", Time: "1pm"}, "HH:MM"},
		{"bad hours", &stubOpenMeteo{}, weatherArgs{Location: "x", Date: "2026-10-11", Hours: 30}, "1-24"},
		{"bad units", &stubOpenMeteo{}, weatherArgs{Location: "x", Date: "2026-10-11", Units: "kelvin"}, "imperial or metric"},
		{"no place", &stubOpenMeteo{}, weatherArgs{Date: "2026-10-11"}, "latitude+longitude or a location"},
		{"latitude only", &stubOpenMeteo{}, weatherArgs{Latitude: f64(40.4), Date: "2026-10-11"}, "both latitude and longitude"},
		{"lat out of range", &stubOpenMeteo{}, weatherArgs{Latitude: f64(95), Longitude: f64(1), Date: "2026-10-11"}, "out of range"},
		{"geocode miss", &stubOpenMeteo{geocodeBody: `{}`}, weatherArgs{Location: "Nowhereville", Date: "2026-10-11"}, "no place found"},
		{"api status", &stubOpenMeteo{forecastStatus: 502, forecastBody: "bad gateway"}, coords, "502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.stub.serve(t)
			_, err := w.forecast(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestWeatherRegistered(t *testing.T) {
	got, err := Build([]string{"weather"}, Deps{})
	if err != nil || len(got) != 1 || got[0].Name() != "weather" {
		t.Fatalf("Build(weather) = %v, %v", got, err)
	}
}
