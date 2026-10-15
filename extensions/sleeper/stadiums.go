package sleeper

// stadium: a venue's coordinates for the weather tool, and the roof when Sleeper's own value is wrong.
type stadium struct {
	lat, lon float64
	roof     string
}

// stadiums is keyed by venue name as Sleeper's 2026 schedule reports it; geocoding a city
// instead goes wrong ("Santa Clara" resolves to Cuba).
var stadiums = map[string]stadium{
	"Acrisure Stadium":           {40.45, -80.02, ""},
	"Allegiant Stadium":          {36.09, -115.18, ""},
	"Allianz Arena":              {48.22, 11.62, ""},
	"Arrowhead Stadium":          {39.05, -94.48, ""},
	"AT&T Stadium":               {32.75, -97.09, ""},
	"Bank of America Stadium":    {35.23, -80.85, ""},
	"Caesars Superdome":          {29.95, -90.08, ""},
	"Empower Field at Mile High": {39.74, -105.02, ""},
	"Estadio Banorte":            {19.30, -99.15, ""},
	"EverBank Stadium":           {30.32, -81.64, ""},
	"Ford Field":                 {42.34, -83.05, ""},
	"Gillette Stadium":           {42.09, -71.26, ""},
	"Hard Rock Stadium":          {25.96, -80.24, ""},
	"Highmark Stadium":           {42.77, -78.79, ""},
	"Huntington Bank Field":      {41.51, -81.70, ""},
	"Lambeau Field":              {44.50, -88.06, ""},
	"Levi's Stadium":             {37.40, -121.97, ""},
	"Lincoln Financial Field":    {39.90, -75.17, ""},
	"Lucas Oil Stadium":          {39.76, -86.16, ""},
	"Lumen Field":                {47.60, -122.33, ""},
	"M&T Bank Stadium":           {39.28, -76.62, ""},
	"Maracanã Stadium":           {-22.91, -43.23, ""},
	"Melbourne Cricket Ground":   {-37.82, 144.99, ""},
	"Mercedes-Benz Stadium":      {33.76, -84.40, ""},
	"MetLife Stadium":            {40.81, -74.07, ""},
	"Nissan Stadium":             {36.17, -86.77, ""},
	"Northwest Stadium":          {38.91, -76.86, ""},
	"NRG Stadium":                {29.68, -95.41, ""},
	"Paycor Stadium":             {39.10, -84.52, ""},
	"Raymond James Stadium":      {27.98, -82.50, ""},
	"Reliant Stadium":            {29.68, -95.41, ""}, // Sleeper's old name for NRG Stadium
	"Santiago Bernabéu Stadium":  {40.45, -3.69, ""},
	"SoFi Stadium":               {33.95, -118.34, "dome"}, // Sleeper says outdoor; the fixed roof covers the field
	"Soldier Field":              {41.86, -87.62, ""},
	"Stade de France":            {48.92, 2.36, ""},
	"State Farm Stadium":         {33.53, -112.26, ""},
	"Tottenham Hotspur Stadium":  {51.60, -0.07, ""},
	"U.S. Bank Stadium":          {44.97, -93.26, ""},
	"Wembley Stadium":            {51.56, -0.28, "outdoor"}, // Sleeper says retractable_dome; the roof covers only the stands
}
