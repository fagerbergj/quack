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

// stubOpenMeteo serves /search and /forecast; forecast rows cover the requested
// start_date..end_date so window selection is exercised against real timestamps.
type stubOpenMeteo struct {
	geocodeHits, forecastHits atomic.Int32
	lastForecast              atomic.Value // url.Values
	geocodeBody               string
	forecastStatus            int
	forecastBody              string
}

func (s *stubOpenMeteo) serve(t *testing.T) weatherAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			s.geocodeHits.Add(1)
			_, _ = w.Write([]byte(s.geocodeBody))
		case "/forecast":
			s.forecastHits.Add(1)
			s.lastForecast.Store(r.URL.Query())
			if s.forecastStatus != 0 {
				w.WriteHeader(s.forecastStatus)
				_, _ = w.Write([]byte(s.forecastBody))
				return
			}
			_, _ = w.Write([]byte(forecastFixture(t, r.URL.Query())))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return weatherAPI{client: srv.Client(), cache: NewURLCache(), forecastURL: srv.URL + "/forecast", geocodeURL: srv.URL + "/search"}
}

// forecastFixture: one row per hour; temperature = hour of day, weather_code 63 (rain).
func forecastFixture(t *testing.T, q url.Values) string {
	start, err := time.Parse(time.DateOnly, q.Get("start_date"))
	if err != nil {
		t.Errorf("start_date %q: %v", q.Get("start_date"), err)
	}
	end, _ := time.Parse(time.DateOnly, q.Get("end_date"))
	var ts, temps, codes []string
	for h := start; h.Before(end.Add(24 * time.Hour)); h = h.Add(time.Hour) {
		ts = append(ts, fmt.Sprintf("%q", h.Format(openMeteoTimeLayout)))
		temps = append(temps, fmt.Sprint(h.Hour()))
		codes = append(codes, "63")
	}
	n := len(ts)
	return fmt.Sprintf(`{"latitude":44.5,"longitude":-88.0,"timezone":"America/Chicago",
		"hourly_units":{"temperature_2m":"°F","wind_speed_10m":"mp/h","precipitation":"inch"},
		"hourly":{"time":[%s],"temperature_2m":[%s],"wind_speed_10m":[%s],"wind_gusts_10m":[%s],
		"precipitation_probability":[%s],"precipitation":[%s],"weather_code":[%s]}}`,
		strings.Join(ts, ","), strings.Join(temps, ","), repeatJSON("22.5", n), repeatJSON("null", n),
		repeatJSON("80", n), repeatJSON("0.1", n), strings.Join(codes, ","))
}

func repeatJSON(v string, n int) string {
	return strings.TrimSuffix(strings.Repeat(v+",", n), ",")
}

const greenBayGeocode = `{"results":[{"name":"Green Bay","admin1":"Wisconsin","country":"United States","latitude":44.51916,"longitude":-88.01983}]}`

func TestWeatherGeocodesAndSelectsKickoffWindow(t *testing.T) {
	s := &stubOpenMeteo{geocodeBody: greenBayGeocode}
	w := s.serve(t)
	got, err := w.forecast(context.Background(), weatherArgs{Location: "Green Bay, Wisconsin", Date: "2026-12-27", Time: "13:25", Hours: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != "Green Bay, Wisconsin, United States" || got.Timezone != "America/Chicago" {
		t.Errorf("resolved place = %q / %q", got.Location, got.Timezone)
	}
	q := s.lastForecast.Load().(url.Values)
	for k, want := range map[string]string{"latitude": "44.5192", "longitude": "-88.0198", "start_date": "2026-12-27", "end_date": "2026-12-27", "temperature_unit": "fahrenheit", "wind_speed_unit": "mph", "timezone": "auto"} {
		if q.Get(k) != want {
			t.Errorf("forecast %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(got.Hourly) != 3 || got.Hourly[0].Time != "2026-12-27T13:00" || got.Hourly[2].Time != "2026-12-27T15:00" {
		t.Fatalf("hourly = %+v, want 13:00-15:00", got.Hourly)
	}
	h := got.Hourly[0]
	if *h.Temperature != 13 || *h.WindSpeed != 22.5 || h.WindGusts != nil || h.Conditions != "rain" {
		t.Errorf("row = %+v", h)
	}
	if got.Units["temperature"] != "°F" || got.Units["wind_speed"] != "mp/h" {
		t.Errorf("units = %v", got.Units)
	}
}

func TestWeatherWindowCrossesMidnight(t *testing.T) {
	s := &stubOpenMeteo{}
	w := s.serve(t)
	got, err := w.forecast(context.Background(), weatherArgs{Latitude: 39.05, Longitude: -94.48, Date: "2026-12-28", Time: "22:00"})
	if err != nil {
		t.Fatal(err)
	}
	if q := s.lastForecast.Load().(url.Values); q.Get("end_date") != "2026-12-29" {
		t.Errorf("end_date = %q, want next day", q.Get("end_date"))
	}
	if len(got.Hourly) != 4 || got.Hourly[3].Time != "2026-12-29T01:00" {
		t.Errorf("hourly = %+v, want 22:00-01:00", got.Hourly)
	}
	if s.geocodeHits.Load() != 0 {
		t.Error("coordinates must skip geocoding")
	}
}

func TestWeatherWholeDayMetricAndCache(t *testing.T) {
	s := &stubOpenMeteo{}
	w := s.serve(t)
	a := weatherArgs{Latitude: 51.5, Longitude: -0.1, Date: "2026-10-11", Units: "metric"}
	for range 2 {
		got, err := w.forecast(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Hourly) != 24 {
			t.Fatalf("whole day = %d rows, want 24", len(got.Hourly))
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
	cases := []struct {
		name string
		stub *stubOpenMeteo
		args weatherArgs
		want string
	}{
		{"bad date", &stubOpenMeteo{}, weatherArgs{Location: "x", Date: "Sunday"}, "YYYY-MM-DD"},
		{"bad time", &stubOpenMeteo{}, weatherArgs{Location: "x", Date: "2026-10-11", Time: "1pm"}, "HH:MM"},
		{"bad units", &stubOpenMeteo{}, weatherArgs{Location: "x", Date: "2026-10-11", Units: "kelvin"}, "imperial or metric"},
		{"no place", &stubOpenMeteo{}, weatherArgs{Date: "2026-10-11"}, "location or latitude"},
		{"lat out of range", &stubOpenMeteo{}, weatherArgs{Latitude: 95, Longitude: 1, Date: "2026-10-11"}, "out of range"},
		{"geocode miss", &stubOpenMeteo{geocodeBody: `{}`}, weatherArgs{Location: "Nowhereville", Date: "2026-10-11"}, "no place found"},
		{"api reason", &stubOpenMeteo{forecastStatus: 400, forecastBody: `{"error":true,"reason":"start_date out of allowed range"}`},
			weatherArgs{Latitude: 1, Longitude: 1, Date: "2027-06-01"}, "start_date out of allowed range"},
		{"api status", &stubOpenMeteo{forecastStatus: 502, forecastBody: "bad gateway"},
			weatherArgs{Latitude: 1, Longitude: 1, Date: "2026-10-11"}, "502"},
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
