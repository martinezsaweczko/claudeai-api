package electricity

import (
	"bytes"
	"fmt"
	"image/color"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
)

// chart dimensions and margins
const (
	chartWidth        = 1000
	chartHeight       = 650
	chartMarginLeft   = 70
	chartMarginRight  = 40
	chartMarginTop    = 90
	chartMarginBottom = 80
)

// tier colors (same semantic meaning as the Node version)
var (
	colorCheap     = color.RGBA{R: 0x00, G: 0xAA, B: 0x00, A: 0xff}
	colorNormal    = color.RGBA{R: 0xDD, G: 0x77, B: 0x00, A: 0xff}
	colorExpensive = color.RGBA{R: 0xDD, G: 0x00, B: 0x00, A: 0xff}
	colorGrid      = color.RGBA{R: 0xE0, G: 0xE0, B: 0xE0, A: 0xff}
	colorAnnotate  = color.RGBA{R: 0x00, G: 0x00, B: 0xEE, A: 0xb3}
	colorFill      = color.RGBA{R: 0xD6, G: 0xEB, B: 0xF7, A: 0xff}
)

// Embedded fonts parsed once per process. A parsed *opentype.Font is safe
// for concurrent use (each Face owns its own sfnt.Buffer), while a font.Face
// is not, so every render creates its own faces from these.
var (
	parseRegularFont = sync.OnceValues(func() (*opentype.Font, error) { return parseFont(goregular.TTF) })
	parseBoldFont    = sync.OnceValues(func() (*opentype.Font, error) { return parseFont(gobold.TTF) })
)

func parseFont(data []byte) (*opentype.Font, error) {
	f, err := opentype.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse font: %w", err)
	}
	return f, nil
}

// chartFaces holds the font faces owned by a single render
type chartFaces struct {
	title    font.Face // bold 22
	subtitle font.Face // regular 14
	label    font.Face // regular 12
}

func newChartFaces() (*chartFaces, error) {
	regular, err := parseRegularFont()
	if err != nil {
		return nil, err
	}
	bold, err := parseBoldFont()
	if err != nil {
		return nil, err
	}
	faces := &chartFaces{}
	if faces.title, err = newFace(bold, 22); err != nil {
		return nil, err
	}
	if faces.subtitle, err = newFace(regular, 14); err != nil {
		faces.Close()
		return nil, err
	}
	if faces.label, err = newFace(regular, 12); err != nil {
		faces.Close()
		return nil, err
	}
	return faces, nil
}

func newFace(f *opentype.Font, size float64) (font.Face, error) {
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size: size,
		DPI:  72,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create font face: %w", err)
	}
	return face, nil
}

// Close releases the faces created so far
func (c *chartFaces) Close() {
	for _, face := range []font.Face{c.title, c.subtitle, c.label} {
		if face != nil {
			_ = face.Close()
		}
	}
}

// tierForPrice returns the price tier color (€/kWh)
func tierForPrice(priceKWh float64) color.Color {
	switch {
	case priceKWh < 0.10:
		return colorCheap
	case priceKWh <= 0.15:
		return colorNormal
	default:
		return colorExpensive
	}
}

// tierRun is a contiguous run of same-tier points drawn as one series
// (kept for test compatibility)
type tierRun struct {
	name    string
	color   color.Color
	xValues []float64
	yValues []float64
}

// buildTierRuns groups the points into contiguous same-tier runs.
// The first run of each tier carries the legend name.
// Kept for backward compatibility with existing unit tests.
func buildTierRuns(prices []float64) []tierRun {
	legendNames := map[string]string{
		"cheap":     "Barato: < 0.10 €/kWh",
		"normal":    "Normal: 0.10-0.15 €/kWh",
		"expensive": "Caro: > 0.15 €/kWh",
	}
	tierOf := func(c color.Color) string {
		switch c {
		case colorCheap:
			return "cheap"
		case colorNormal:
			return "normal"
		default:
			return "expensive"
		}
	}

	var runs []tierRun
	named := map[string]bool{}

	currentColor := tierForPrice(prices[0])
	current := tierRun{color: currentColor}
	current.xValues = append(current.xValues, 0)
	current.yValues = append(current.yValues, prices[0])

	for i := 1; i < len(prices); i++ {
		c := tierForPrice(prices[i])
		// Close the current run when the tier changes, sharing the boundary
		// point so the line stays connected
		current.xValues = append(current.xValues, float64(i))
		current.yValues = append(current.yValues, prices[i])
		if c != currentColor {
			runs = append(runs, current)
			currentColor = c
			current = tierRun{color: c}
			current.xValues = append(current.xValues, float64(i))
			current.yValues = append(current.yValues, prices[i])
		}
	}
	runs = append(runs, current)

	for i := range runs {
		tier := tierOf(runs[i].color)
		if !named[tier] {
			runs[i].name = legendNames[tier]
			named[tier] = true
		}
	}

	return runs
}

// renderChart renders the price chart as PNG using a custom anti-aliased
// renderer. It preserves the tier coloring, current-hour marker and midnight
// separator from the previous go-chart implementation but with a much cleaner,
// modern look.
func (s *Service) renderChart(data *chartData) ([]byte, error) {
	s.log.Info("Rendering chart", "dual_day", data.IsDualDay, "points", len(data.Points))

	if len(data.Points) == 0 {
		return nil, fmt.Errorf("no data available for chart")
	}

	points := make([]PricePoint, len(data.Points))
	copy(points, data.Points)
	sort.Slice(points, func(i, j int) bool {
		return points[i].Datetime.Before(points[j].Datetime)
	})

	madrid := madridLocation()
	prices := make([]float64, len(points))
	for i, p := range points {
		prices[i] = p.Value / 1000 // €/kWh
	}

	width := chartWidth
	if data.IsDualDay {
		width = 1400
	}

	minPrice, maxPrice := prices[0], prices[0]
	for _, p := range prices {
		if p < minPrice {
			minPrice = p
		}
		if p > maxPrice {
			maxPrice = p
		}
	}
	// Round to nice 0.01 boundaries with a little padding
	minPrice = math.Floor(math.Max(0, minPrice-0.015)*100) / 100
	maxPrice = math.Ceil((maxPrice+0.015)*100) / 100

	faces, err := newChartFaces()
	if err != nil {
		return nil, err
	}
	defer faces.Close()

	dc := gg.NewContext(width, chartHeight)
	dc.SetRGB(1, 1, 1)
	dc.Clear()

	plotX := float64(chartMarginLeft)
	plotY := float64(chartMarginTop)
	plotW := float64(width - chartMarginLeft - chartMarginRight)
	plotH := float64(chartHeight - chartMarginTop - chartMarginBottom)

	xFor := func(i int) float64 {
		return plotX + float64(i)*plotW/float64(len(points)-1)
	}
	yFor := func(v float64) float64 {
		return plotY + plotH - (v-minPrice)/(maxPrice-minPrice)*plotH
	}

	// Title
	title := "Precio de la electricidad: " + data.TodayDate
	if data.IsDualDay {
		title = "Precio de la electricidad: " + data.TodayDate + " a " + data.TomorrowDate
	}
	dc.SetFontFace(faces.title)
	dc.SetRGB(0.1, 0.1, 0.1)
	dc.DrawStringAnchored(title, float64(width)/2, 35, 0.5, 0.5)

	// Subtitle
	dc.SetFontFace(faces.subtitle)
	dc.SetRGB(0.4, 0.4, 0.4)
	dc.DrawStringAnchored("€/kWh (PVPC)", float64(width)/2, 58, 0.5, 0.5)

	// Horizontal grid + Y labels
	dc.SetFontFace(faces.label)
	step := 0.02
	for v := minPrice; v <= maxPrice+1e-9; v += step {
		y := yFor(v)
		dc.SetColor(colorGrid)
		dc.SetLineWidth(1)
		dc.DrawLine(plotX, y, plotX+plotW, y)
		dc.Stroke()
		dc.SetRGB(0.25, 0.25, 0.25)
		dc.DrawStringAnchored(fmt.Sprintf("%.2f", v), plotX-12, y, 1, 0.5)
	}

	// Vertical grid + X labels
	labelStep := 3
	if data.IsDualDay {
		labelStep = 4
	}
	for i, p := range points {
		if i%labelStep != 0 {
			continue
		}
		x := xFor(i)
		dc.SetColor(colorGrid)
		dc.SetLineWidth(1)
		dc.DrawLine(x, plotY, x, plotY+plotH)
		dc.Stroke()

		label := fmt.Sprintf("%02d:00", p.Datetime.In(madrid).Hour())
		if i == 24 {
			label = "00(+1)"
		}
		dc.SetRGB(0.25, 0.25, 0.25)
		dc.DrawStringAnchored(label, x, plotY+plotH+20, 0.5, 0.5)
	}

	// Area fill
	dc.SetColor(colorFill)
	dc.NewSubPath()
	dc.MoveTo(xFor(0), yFor(prices[0]))
	for i := 1; i < len(prices); i++ {
		dc.LineTo(xFor(i), yFor(prices[i]))
	}
	dc.LineTo(xFor(len(prices)-1), plotY+plotH)
	dc.LineTo(xFor(0), plotY+plotH)
	dc.ClosePath()
	dc.Fill()

	// Line segments colored by tier
	for i := 1; i < len(prices); i++ {
		mid := (prices[i-1] + prices[i]) / 2
		dc.SetColor(tierForPrice(mid))
		dc.SetLineWidth(3.5)
		dc.DrawLine(xFor(i-1), yFor(prices[i-1]), xFor(i), yFor(prices[i]))
		dc.Stroke()
	}

	// Dots
	for i, p := range prices {
		dc.SetColor(tierForPrice(p))
		dc.DrawCircle(xFor(i), yFor(p), 4)
		dc.Fill()
	}

	// Axes
	dc.SetRGB(0.2, 0.2, 0.2)
	dc.SetLineWidth(1.5)
	dc.DrawLine(plotX, plotY+plotH, plotX+plotW, plotY+plotH)
	dc.DrawLine(plotX, plotY, plotX, plotY+plotH)
	dc.Stroke()

	// Current hour line
	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	for i, p := range points {
		pUTC := p.Datetime.UTC()
		if pUTC.Format("2006-01-02") == todayStr && pUTC.Hour() == now.Hour() {
			x := xFor(i)
			dc.SetColor(colorAnnotate)
			dc.SetDash(5, 5)
			dc.SetLineWidth(2)
			dc.DrawLine(x, plotY, x, plotY+plotH)
			dc.Stroke()
			dc.SetDash()
			dc.SetRGB(0, 0, 0.9)
			dc.DrawStringAnchored("Hora actual", x+5, plotY+15, 0, 0.5)
			break
		}
	}

	// Midnight separator
	if data.IsDualDay && len(points) > 24 {
		x := xFor(24) - (xFor(24)-xFor(23))/2
		dc.SetColor(colorAnnotate)
		dc.SetDash(5, 5)
		dc.SetLineWidth(2)
		dc.DrawLine(x, plotY, x, plotY+plotH)
		dc.Stroke()
		dc.SetDash()
		dc.SetRGB(0, 0, 0.9)
		dc.DrawStringAnchored("MEDIANOCHE", x+5, plotY+15, 0, 0.5)
	}

	// Legend box
	legendX := plotX + plotW - 230
	legendY := plotY + 10
	dc.SetRGBA(1, 1, 1, 0.9)
	dc.DrawRoundedRectangle(legendX-10, legendY-8, 220, 70, 6)
	dc.Fill()
	dc.SetRGB(0.7, 0.7, 0.7)
	dc.SetLineWidth(1)
	dc.DrawRoundedRectangle(legendX-10, legendY-8, 220, 70, 6)
	dc.Stroke()

	legendItems := []struct {
		label string
		color color.Color
	}{
		{"Barato: < 0.10 €/kWh", colorCheap},
		{"Normal: 0.10-0.15 €/kWh", colorNormal},
		{"Caro: > 0.15 €/kWh", colorExpensive},
	}
	for i, item := range legendItems {
		y := legendY + float64(i)*20
		dc.SetColor(item.color)
		dc.SetLineWidth(3)
		dc.DrawLine(legendX, y, legendX+20, y)
		dc.Stroke()
		dc.SetRGB(0.2, 0.2, 0.2)
		dc.DrawStringAnchored(item.label, legendX+28, y, 0, 0.5)
	}

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, fmt.Errorf("failed to render chart: %w", err)
	}

	s.log.Info("PNG chart rendered successfully", "bytes", buf.Len())
	return buf.Bytes(), nil
}
