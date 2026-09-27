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
	openMeteoTimeLayout  = "2006-01-02T15:04"
	weatherHourly        = "temperature_2m,wind_speed_10m,wind_gusts_10m,precipitation_probability,precipitation,weather_code"
)

type weatherArgs struct {
	Location  string  `json:"location,omitempty" jsonschema:"place name to geocode, e.g. 'Green Bay, Wisconsin'; ignored when latitude/longitude are set"`
	Latitude  float64 `json:"latitude,omitempty" jsonschema:"decimal latitude; set with longitude instead of location"`
	Longitude float64 `json:"longitude,omitempty" jsonschema:"decimal longitude; set with latitude instead of location"`
	Date      string  `json:"date" jsonschema:"local date at the location, YYYY-MM-DD"`
	Time      string  `json:"time,omitempty" jsonschema:"local start time at the location, HH:MM (e.g. kickoff); omit for the whole day"`
	Hours     int     `json:"hours,omitempty" jsonschema:"window length in hours from time (default 4, max 24); ignored without time"`
	Units     string  `json:"units,omitempty" jsonschema:"imperial (default: °F, mph, inch) or metric (°C, km/h, mm)"`
}

type weatherHour struct {
	Time          string   `json:"time"`
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
	HourlyUnits map[string]string `json:"hourly_units"`
	Hourly      struct {
		Time          []string   `json:"time"`
		Temperature   []*float64 `json:"temperature_2m"`
		WindSpeed     []*float64 `json:"wind_speed_10m"`
		WindGusts     []*float64 `json:"wind_gusts_10m"`
		PrecipProb    []*float64 `json:"precipitation_probability"`
		Precipitation []*float64 `json:"precipitation"`
		Code          []*int     `json:"weather_code"`
	} `json:"hourly"`
}

// weatherAPI: Open-Meteo client; base URLs are fields so tests can point them at a stub server.
type weatherAPI struct {
	client      *http.Client
	cache       *URLCache
	forecastURL string
	geocodeURL  string
}

func newWeather(d Deps) (tool.Tool, error) {
	w := weatherAPI{client: d.Client, cache: d.Cache, forecastURL: openMeteoForecastURL, geocodeURL: openMeteoGeocodeURL}
	return functiontool.New[weatherArgs, weatherResult](
		functiontool.Config{
			Name: "weather",
			Description: "Hourly weather forecast (Open-Meteo) for a place and local date: temperature, sustained wind, " +
				"gusts, precipitation probability and amount, and conditions, with units stated. Give `location` " +
				"(a place name) or `latitude`+`longitude`, a `date` (YYYY-MM-DD), and optionally a local `time` " +
				"(HH:MM) with `hours` to narrow the window. Forecasts reach about 16 days ahead.",
		},
		func(tc agent.Context, a weatherArgs) (weatherResult, error) { return w.forecast(tc, a) },
	)
}

func (w weatherAPI) forecast(ctx context.Context, a weatherArgs) (weatherResult, error) {
	start, end, err := weatherWindow(a)
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
	q.Set("hourly", weatherHourly)
	q.Set("timezone", "auto")
	q.Set("start_date", start.Format(time.DateOnly))
	q.Set("end_date", end.Add(-time.Hour).Format(time.DateOnly))
	var f openMeteoForecast
	if err := w.getJSON(ctx, w.forecastURL+"?"+q.Encode(), &f); err != nil {
		return weatherResult{}, err
	}
	hours := selectHours(f, start, end)
	if len(hours) == 0 {
		return weatherResult{}, fmt.Errorf("weather: no forecast hours between %s and %s", start.Format(openMeteoTimeLayout), end.Format(openMeteoTimeLayout))
	}
	return weatherResult{
		Location: name, Latitude: f.Latitude, Longitude: f.Longitude, Timezone: f.Timezone,
		Units: map[string]string{
			"temperature": f.HourlyUnits["temperature_2m"], "wind_speed": f.HourlyUnits["wind_speed_10m"],
			"precipitation": f.HourlyUnits["precipitation"], "precipitation_probability": "%",
		},
		Hourly: hours,
		Source: "Open-Meteo forecast (open-meteo.com)",
	}, nil
}

// weatherWindow: the [start, end) local-time window; naive times, compared against Open-Meteo's timezone=auto rows.
func weatherWindow(a weatherArgs) (time.Time, time.Time, error) {
	day, err := time.Parse(time.DateOnly, strings.TrimSpace(a.Date))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("weather: date %q must be YYYY-MM-DD", a.Date)
	}
	if a.Hours < 0 || a.Hours > 24 {
		return time.Time{}, time.Time{}, fmt.Errorf("weather: hours %d must be 1-24", a.Hours)
	}
	if strings.TrimSpace(a.Time) == "" {
		return day, day.Add(24 * time.Hour), nil
	}
	t, err := time.Parse("15:04", strings.TrimSpace(a.Time))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("weather: time %q must be HH:MM (24-hour, local to the location)", a.Time)
	}
	n := a.Hours
	if n == 0 {
		n = defaultWeatherHours
	}
	start := day.Add(time.Duration(t.Hour()) * time.Hour)
	return start, start.Add(time.Duration(n) * time.Hour), nil
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

// resolvePlace: explicit coordinates win; (0, 0) counts as unset since no one asks for weather at null island.
func (w weatherAPI) resolvePlace(ctx context.Context, a weatherArgs) (string, float64, float64, error) {
	if a.Latitude != 0 || a.Longitude != 0 {
		if a.Latitude < -90 || a.Latitude > 90 || a.Longitude < -180 || a.Longitude > 180 {
			return "", 0, 0, fmt.Errorf("weather: latitude %v / longitude %v out of range", a.Latitude, a.Longitude)
		}
		return fmt.Sprintf("%.4f, %.4f", a.Latitude, a.Longitude), a.Latitude, a.Longitude, nil
	}
	loc := strings.TrimSpace(a.Location)
	if loc == "" {
		return "", 0, 0, errors.New("weather: give a location or latitude+longitude")
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
		return "", 0, 0, fmt.Errorf("weather: no place found for %q; try a city name or pass latitude/longitude", loc)
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

func (w weatherAPI) getJSON(ctx context.Context, u string, out any) error {
	body, ok := "", false
	if w.cache != nil {
		body, ok = w.cache.Get(u)
	}
	if !ok {
		var err error
		if body, err = w.fetchOpenMeteo(ctx, u); err != nil {
			return err
		}
		if w.cache != nil {
			w.cache.Set(u, body)
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
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(b, &e) != nil || e.Reason == "" {
			e.Reason = resp.Status
		}
		return "", fmt.Errorf("weather: open-meteo: %s", e.Reason)
	}
	return string(b), nil
}

func selectHours(f openMeteoForecast, start, end time.Time) []weatherHour {
	h := f.Hourly
	var out []weatherHour
	for i, ts := range h.Time {
		t, err := time.Parse(openMeteoTimeLayout, ts)
		if err != nil || t.Before(start) || !t.Before(end) {
			continue
		}
		out = append(out, weatherHour{
			Time: ts, Temperature: at(h.Temperature, i), WindSpeed: at(h.WindSpeed, i), WindGusts: at(h.WindGusts, i),
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
