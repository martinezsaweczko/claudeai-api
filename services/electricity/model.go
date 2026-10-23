// Package electricity implements the "electricidad" command: it fetches the
// Spanish electricity price (ESIOS/REE API), caches it per day, and renders
// a price chart as a PNG image.
package electricity

import (
	"time"
)

// PricePoint is a single hourly electricity price
type PricePoint struct {
	Datetime time.Time
	Value    float64 // €/MWh
}

// cachedPoint is the JSON representation of a price point in the daily cache file
// (same format as the Node version)
type cachedPoint struct {
	Datetime string  `json:"datetime"`
	Hour     string  `json:"hour"`
	Price    string  `json:"price"` // €/kWh, 3 decimals
	RawPrice float64 `json:"rawPrice"`
}

// cachedDay is the JSON representation of the daily cache file
type cachedDay struct {
	Date              string        `json:"date"`
	GeneratedAt       string        `json:"generatedAt"`
	Data              []cachedPoint `json:"data"`
	TomorrowFetchedAt *string       `json:"tomorrowFetchedAt"`
}

// chartData is the input for the chart renderer
type chartData struct {
	TodayDate    string
	TomorrowDate string
	Points       []PricePoint
	IsDualDay    bool
}

// madridLocation returns the Europe/Madrid timezone
func madridLocation() *time.Location {
	loc, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		// Fallback: fixed +1 offset is wrong half the year, but better than crashing
		return time.FixedZone("CET", 3600)
	}
	return loc
}
