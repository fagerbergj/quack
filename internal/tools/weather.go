package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const (
	openMeteoForecastURL = "https://api.open-meteo.com/v1/forecast"
	openMeteoGeocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	maxWeatherBytes      = 1 << 20
	defaultWeatherHours  = 4
	weatherHourly        = "temperature_2m,wind_speed_10m,wind_gusts_10m,precipitation_probability,precipitation,weather_code"
	weatherCachePrefix   = "weather:"
)

type weatherArgs struct {
	Location  string   `json:"location,omitempty" jsonschema:"place name qualified with state/region and country, e.g. 'Green Bay, Wisconsin, United States'; prefer latitude+longitude when known"`
	Latitude  *float64 `json:"latitude,omitempty" jsonschema:"decimal latitude; give with longitude instead of location"`
	Longitude *float64 `json:"longitude,omitempty" jsonschema:"decimal longitude; give with latitude instead of location"`
	Kickoff   string   `json:"kickoff,omitempty" jsonschema:"event start as an RFC3339 instant (e.g. sleeper_schedule's kickoff, passed as is); use this or date"`
	Date      string   `json:"date,omitempty" jsonschema:"local date at the place, YYYY-MM-DD, when there is no kickoff instant"`
	Time      string   `json:"time,omitempty" jsonschema:"local start HH:MM at the place, with date; omit for the whole day"`
	Hours     int      `json:"hours,omitempty" jsonschema:"window length in hours from kickoff/time (default 4, max 24)"`
	Units     string   `json:"units,omitempty" jsonschema:"imperial (default: °F, mph, inch) or metric (°C, km/h, mm)"`
}

type weatherHour struct {
	Time          string   `json:"time"`
	Local         string   `json:"local,omitempty"`
	Temperature   *float64 `json:"temperature"`
	WindSpeed     *float64 `json:"wind_speed"`
	WindGusts     *float64 `json:"wind_gusts"`
	PrecipProb    *float64 `json:"precipitation_probability"`
	Precipitation *float64 `json:"precipitation"`
	Conditions    string   `json:"conditions"`
}

type weatherResult struct {
	Location  string            `json:"location"`
	Latitude  float64           `json:"latitude"`
	Longitude float64           `json:"longitude"`
	Timezone  string            `json:"timezone"`
	Units     map[string]string `json:"units"`
	Hourly    []weatherHour     `json:"hourly"`
	Source    string            `json:"source"`
}

type openMeteoForecast struct {
	Latitude    float64           `json:"latitude"`
	Longitude   float64           `json:"longitude"`
	Timezone    string            `json:"timezone"`
	TZAbbrev    string            `json:"timezone_abbreviation"`
	UTCOffset   int               `json:"utc_offset_seconds"`
	HourlyUnits map[string]string `json:"hourly_units"`
	Hourly      struct {
		Time          []int64    `json:"time"`
		Temperature   []*float64 `json:"temperature_2m"`
		WindSpeed     []*float64 `json:"wind_speed_10m"`
		WindGusts     []*float64 `json:"wind_gusts_10m"`
		PrecipProb    []*float64 `json:"precipitation_probability"`
		Precipitation []*float64 `json:"precipitation"`
		Code          []*int     `json:"weather_code"`
	} `json:"hourly"`
}

// openMeteoError: Open-Meteo rejected the request (bad range, bad parameter), as opposed to a transport failure.
type openMeteoError struct{ reason string }

func (e *openMeteoError) Error() string { return "weather: open-meteo: " + e.reason }

// weatherWindow: either a UTC kickoff instant, or a local day/hour resolved once the place's zone is known.
type weatherWindow struct {
	kickoff time.Time
	day     time.Time
	hour    int // -1 = whole day
	minute  int
	hours   int
}

// weatherAPI: Open-Meteo client; base URLs are fields so tests can point them at a stub server.
type weatherAPI struct {
	client      *http.Client
	cache       *URLCache
	forecastURL string
	geocodeURL  string
	now         func() time.Time
}

func newWeather(d Deps) (tool.Tool, error) {
	w := weatherAPI{client: d.Client, cache: d.Cache, forecastURL: openMeteoForecastURL, geocodeURL: openMeteoGeocodeURL, now: time.Now}
	return functiontool.New[weatherArgs, weatherResult](
		functiontool.Config{
			Name: "weather",
			Description: "Hourly weather (Open-Meteo) for a place and time window: temperature, sustained wind, gusts, " +
				"precipitation probability and amount, and conditions, with units stated. Give `latitude`+`longitude` " +
				"or a `location` name, and either `kickoff` (an RFC3339 instant, passed as is) or a local `date` " +
				"(+ optional `time`). Returns every hour overlapping the window, labeled in UTC and the place's local " +
				"time. Forecasts reach about 15 days ahead; a past window returns archived model data, not observations.",
		},
		func(tc agent.Context, a weatherArgs) (weatherResult, error) { return w.forecast(tc, a) },
	)
}

func (w weatherAPI) forecast(ctx context.Context, a weatherArgs) (weatherResult, error) {
	win, err := parseWeatherWindow(a)
	if err != nil {
		return weatherResult{}, err
	}
	q, err := weatherUnits(a.Units)
	if err != nil {
		return weatherResult{}, err
	}
	name, lat, lon, err := w.resolvePlace(ctx, a)
	if err != nil {
		return weatherResult{}, err
	}
	q.Set("latitude", strconv.FormatFloat(lat, 'f', 4, 64))
	q.Set("longitude", strconv.FormatFloat(lon, 'f', 4, 64))
	f, err := w.fetchForecast(ctx, q, win)
	if err != nil {
		return weatherResult{}, err
	}
	loc := forecastZone(f)
	start, end := win.bounds(loc)
	hours := selectHours(f, loc, start, end)
	if len(hours) == 0 {
		return weatherResult{}, fmt.Errorf("weather: no hourly data for %s to %s; the window is likely past the ~15-day forecast horizon", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}
	source := "Open-Meteo forecast (open-meteo.com)"
	if end.Before(w.now()) {
		source = "Open-Meteo archived model data (open-meteo.com); a past window, not station observations"
	}
	return weatherResult{
		Location: name, Latitude: f.Latitude, Longitude: f.Longitude, Timezone: f.Timezone,
		Units: map[string]string{
			"temperature": f.HourlyUnits["temperature_2m"], "wind_speed": strings.ReplaceAll(f.HourlyUnits["wind_speed_10m"], "mp/h", "mph"),
			"precipitation": f.HourlyUnits["precipitation"], "precipitation_probability": "%",
		},
		Hourly: hours,
		Source: source,
	}, nil
}

func parseWeatherWindow(a weatherArgs) (weatherWindow, error) {
	w := weatherWindow{hours: a.Hours, hour: -1}
	if w.hours < 0 || w.hours > 24 {
		return w, fmt.Errorf("weather: hours %d must be 1-24", a.Hours)
	}
	if w.hours == 0 {
		w.hours = defaultWeatherHours
	}
	if k := strings.TrimSpace(a.Kickoff); k != "" {
		t, err := time.Parse(time.RFC3339, k)
		if err != nil {
			return w, fmt.Errorf("weather: kickoff %q must be RFC3339, e.g. 2026-11-01T18:00:00Z", a.Kickoff)
		}
		w.kickoff = t.UTC()
		return w, nil
	}
	day, err := time.Parse(time.DateOnly, strings.TrimSpace(a.Date))
	if err != nil {
		return w, fmt.Errorf("weather: give kickoff (RFC3339) or date (YYYY-MM-DD); date %q did not parse", a.Date)
	}
	w.day = day
	if s := strings.TrimSpace(a.Time); s != "" {
		t, err := time.Parse("15:04", s)
		if err != nil {
			return w, fmt.Errorf("weather: time %q must be HH:MM (24-hour, local to the place)", a.Time)
		}
		w.hour, w.minute = t.Hour(), t.Minute()
	}
	return w, nil
}

// bounds: the [start, end) instants; a local window is placed in loc so DST on that date is honored.
func (w weatherWindow) bounds(loc *time.Location) (time.Time, time.Time) {
	if !w.kickoff.IsZero() {
		return w.kickoff, w.kickoff.Add(time.Duration(w.hours) * time.Hour)
	}
	y, m, d := w.day.Date()
	if w.hour < 0 {
		start := time.Date(y, m, d, 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 0, 1)
	}
	start := time.Date(y, m, d, w.hour, w.minute, 0, 0, loc)
	return start, start.Add(time.Duration(w.hours) * time.Hour)
}

// requestDays: a naive UTC-ish date span that contains the window wherever the place is.
func (w weatherWindow) requestDays() (time.Time, time.Time) {
	if !w.kickoff.IsZero() {
		return w.kickoff, w.kickoff.Add(time.Duration(w.hours) * time.Hour)
	}
	if w.hour < 0 {
		return w.day, w.day
	}
	start := w.day.Add(time.Duration(w.hour)*time.Hour + time.Duration(w.minute)*time.Minute)
	return start, start.Add(time.Duration(w.hours) * time.Hour)
}

// fetchForecast pads the date span a day each side, since Open-Meteo applies today's UTC offset to
// every date (wrong across a DST change); if the padding crosses the forecast horizon, retry unpadded.
func (w weatherAPI) fetchForecast(ctx context.Context, q url.Values, win weatherWindow) (openMeteoForecast, error) {
	q.Set("hourly", weatherHourly)
	q.Set("timezone", "auto")
	q.Set("timeformat", "unixtime")
	from, to := win.requestDays()
	var f openMeteoForecast
	err := w.getJSON(ctx, w.forecastURL+"?"+withDays(q, from.AddDate(0, 0, -1), to.AddDate(0, 0, 1)), &f)
	var apiErr *openMeteoError
	if errors.As(err, &apiErr) {
		f = openMeteoForecast{}
		err = w.getJSON(ctx, w.forecastURL+"?"+withDays(q, from, to), &f)
	}
	return f, err
}

func withDays(q url.Values, from, to time.Time) string {
	q.Set("start_date", from.Format(time.DateOnly))
	q.Set("end_date", to.Format(time.DateOnly))
	return q.Encode()
}

// forecastZone: the place's IANA zone; a fixed offset only if this host lacks tzdata for it.
func forecastZone(f openMeteoForecast) *time.Location {
	if loc, err := time.LoadLocation(f.Timezone); err == nil && f.Timezone != "" {
		return loc
	}
	return time.FixedZone(f.TZAbbrev, f.UTCOffset)
}

func weatherUnits(units string) (url.Values, error) {
	switch strings.ToLower(strings.TrimSpace(units)) {
	case "", "imperial":
		return url.Values{"temperature_unit": {"fahrenheit"}, "wind_speed_unit": {"mph"}, "precipitation_unit": {"inch"}}, nil
	case "metric":
		return url.Values{}, nil
	default:
		return nil, fmt.Errorf("weather: units %q must be imperial or metric", units)
	}
}

func (w weatherAPI) resolvePlace(ctx context.Context, a weatherArgs) (string, float64, float64, error) {
	if (a.Latitude == nil) != (a.Longitude == nil) {
		return "", 0, 0, errors.New("weather: give both latitude and longitude, or neither")
	}
	if a.Latitude != nil {
		lat, lon := *a.Latitude, *a.Longitude
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return "", 0, 0, fmt.Errorf("weather: latitude %v / longitude %v out of range", lat, lon)
		}
		return fmt.Sprintf("%.4f, %.4f", lat, lon), lat, lon, nil
	}
	loc := strings.TrimSpace(a.Location)
	if loc == "" {
		return "", 0, 0, errors.New("weather: give latitude+longitude or a location")
	}
	var g struct {
		Results []struct {
			Name      string  `json:"name"`
			Admin1    string  `json:"admin1"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	q := url.Values{"name": {loc}, "count": {"1"}, "language": {"en"}, "format": {"json"}}
	if err := w.getJSON(ctx, w.geocodeURL+"?"+q.Encode(), &g); err != nil {
		return "", 0, 0, err
	}
	if len(g.Results) == 0 {
		return "", 0, 0, fmt.Errorf("weather: no place found for %q; qualify it with state and country or pass latitude/longitude", loc)
	}
	r := g.Results[0]
	parts := []string{r.Name}
	for _, p := range []string{r.Admin1, r.Country} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", "), r.Latitude, r.Longitude, nil
}

// getJSON caches under its own prefix so web_fetch's sanitized page bodies never answer a weather URL.
func (w weatherAPI) getJSON(ctx context.Context, u string, out any) error {
	body, ok := "", false
	if w.cache != nil {
		body, ok = w.cache.Get(weatherCachePrefix + u)
	}
	if !ok {
		var err error
		if body, err = w.fetchOpenMeteo(ctx, u); err != nil {
			return err
		}
		if w.cache != nil {
			w.cache.Set(weatherCachePrefix+u, body)
		}
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return fmt.Errorf("weather: decode open-meteo response: %w", err)
	}
	return nil
}

func (w weatherAPI) fetchOpenMeteo(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("weather: open-meteo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxWeatherBytes))
	if err != nil {
		return "", fmt.Errorf("weather: open-meteo read: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return string(b), nil
	}
	var e struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(b, &e) == nil && e.Reason != "" {
		return "", &openMeteoError{reason: e.Reason}
	}
	return "", fmt.Errorf("weather: open-meteo: %s", resp.Status)
}

// selectHours keeps every hourly row overlapping [start, end).
func selectHours(f openMeteoForecast, loc *time.Location, start, end time.Time) []weatherHour {
	h := f.Hourly
	var out []weatherHour
	for i, sec := range h.Time {
		t := time.Unix(sec, 0)
		if !t.Add(time.Hour).After(start) || !t.Before(end) {
			continue
		}
		out = append(out, weatherHour{
			Time: t.UTC().Format("2006-01-02T15:04Z"), Local: t.In(loc).Format("2006-01-02 15:04 MST"),
			Temperature: at(h.Temperature, i), WindSpeed: at(h.WindSpeed, i), WindGusts: at(h.WindGusts, i),
			PrecipProb: at(h.PrecipProb, i), Precipitation: at(h.Precipitation, i), Conditions: wmoConditions(h.Code, i),
		})
	}
	return out
}

func at[T any](s []*T, i int) *T {
	if i < len(s) {
		return s[i]
	}
	return nil
}

func wmoConditions(codes []*int, i int) string {
	c := at(codes, i)
	if c == nil {
		return ""
	}
	if s, ok := wmoCodes[*c]; ok {
		return s
	}
	return "WMO code " + strconv.Itoa(*c)
}

// wmoCodes: WMO weather interpretation codes as Open-Meteo documents them.
var wmoCodes = map[int]string{
	0: "clear", 1: "mainly clear", 2: "partly cloudy", 3: "overcast", 45: "fog", 48: "freezing fog",
	51: "light drizzle", 53: "drizzle", 55: "heavy drizzle", 56: "light freezing drizzle", 57: "freezing drizzle",
	61: "light rain", 63: "rain", 65: "heavy rain", 66: "light freezing rain", 67: "freezing rain",
	71: "light snow", 73: "snow", 75: "heavy snow", 77: "snow grains",
	80: "light rain showers", 81: "rain showers", 82: "violent rain showers", 85: "snow showers", 86: "heavy snow showers",
	95: "thunderstorm", 96: "thunderstorm with hail", 99: "thunderstorm with heavy hail",
}
