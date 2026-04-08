package main

import (
	"compress/gzip"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed static/*
var staticFiles embed.FS

//go:embed templates/*
var templateFiles embed.FS

const (
	geojsonURL   = "https://regieessencequebec.ca/stations.geojson.gz"
	defaultPort  = "8080"
	pollInterval = 5 * time.Minute
)

// GeoJSON structures matching the upstream format.
type GeoJSONResponse struct {
	Type     string          `json:"type"`
	Metadata *GeoJSONMeta    `json:"metadata,omitempty"`
	Features json.RawMessage `json:"features"`
}

type GeoJSONMeta struct {
	GeneratedAt    string `json:"generated_at"`
	ExcelURL       string `json:"excel_url"`
	TotalStations  int    `json:"total_stations"`
	ExcelSizeBytes int    `json:"excel_size_bytes"`
}

// Station is our simplified JSON shape for the frontend.
type Station struct {
	Name       string  `json:"name"`
	Brand      string  `json:"brand"`
	Address    string  `json:"address"`
	Region     string  `json:"region"`
	PostalCode string  `json:"postalCode"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	Regular    float64 `json:"regular"`
	Super      float64 `json:"super"`
	Diesel     float64 `json:"diesel"`
}

type StationsResponse struct {
	LastUpdated string    `json:"lastUpdated"`
	Stations    []Station `json:"stations"`
}

// Snapshot holds the aggregated statistics for one fetch.
type Snapshot struct {
	GeneratedAt  string  `json:"generatedAt"`
	FetchedAt    string  `json:"fetchedAt"`
	RegularAvg   float64 `json:"regularAvg"`
	RegularMin   float64 `json:"regularMin"`
	RegularMax   float64 `json:"regularMax"`
	SuperAvg     float64 `json:"superAvg"`
	DieselAvg    float64 `json:"dieselAvg"`
	StationCount int     `json:"stationCount"`
}

type StatsResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// In-memory cache
var (
	cacheMu    sync.RWMutex
	cachedResp *StationsResponse
)

var db *sql.DB

// tmpl is the parsed template set, loaded once at startup.
var tmpl *template.Template

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	dbPath := os.Getenv("ESSENCE_DB")
	if dbPath == "" {
		dbPath = "./essence.db"
	}

	var err error
	db, err = initDB(dbPath)
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer db.Close()

	tmpl, err = loadTemplates()
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	// Initial fetch, then background poll every 5 minutes.
	go poller()

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static files: %v", err)
	}

	// HTML page routes
	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/map", handleMapPage)
	http.HandleFunc("/stats", handleStatsPage)
	http.HandleFunc("/stats/content", handleStatsContent)

	// JSON API routes (kept for backwards-compatibility)
	http.HandleFunc("/api/stations", handleStations)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/regions", handleRegions)
	http.HandleFunc("/api/stats/region", handleRegionStats)
	http.HandleFunc("/api/station-deltas", handleStationDeltas)

	// Static assets (map.js, stats.js, etc.)
	http.Handle("/map.js", http.FileServer(http.FS(staticSub)))
	http.Handle("/stats.js", http.FileServer(http.FS(staticSub)))

	log.Printf("Listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// loadTemplates parses all templates from the embedded templates/ directory.
// It registers helper functions used in templates.
func loadTemplates() (*template.Template, error) {
	funcMap := template.FuncMap{
		"fmtPrice": func(v float64) string {
			if v <= 0 {
				return "—"
			}
			return fmt.Sprintf("%.1f", v)
		},
	}

	t, err := template.New("").Funcs(funcMap).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return t, nil
}

// renderTemplate executes a named template with data, writing the result to w.
// On error it falls back to a plain HTTP 500.
func renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %q: %v", name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// isHTMX returns true when the request was issued by htmx.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// ── Page data structs ──────────────────────────────────────────────────────

// mapPageData holds everything needed to render the map page.
type mapPageData struct {
	Page         string
	StationsJSON template.JS
	DeltasJSON   template.JS
	Regions      []string
	LastUpdated  string
	StationCount int
}

// statsPageData holds everything needed to render the stats page shell.
type statsPageData struct {
	Page    string
	Regions []string
}

// historyRow is one row in the history table, pre-formatted for the template.
type historyRow struct {
	Date         string
	RegularAvg   string
	RegularMin   string
	RegularMax   string
	SuperAvg     string
	DieselAvg    string
	StationCount int
}

// statsContentData holds everything needed to render the stats-content fragment.
type statsContentData struct {
	Days          int
	Snapshots     []Snapshot
	SnapshotsJSON template.JS
	Last          Snapshot
	HistoryRows   []historyRow
}

// ── HTML page handlers ────────────────────────────────────────────────────

// handleRoot redirects bare "/" to the map page.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/map", http.StatusSeeOther)
}

// handleMapPage renders the full map page (or just the content block for htmx).
func handleMapPage(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	resp := cachedResp
	cacheMu.RUnlock()

	if resp == nil {
		// Data not yet ready; render layout with a loading message.
		renderTemplate(w, "layout.html", mapPageData{Page: "map"})
		return
	}

	// Build sorted unique region list from in-memory station data.
	regionSet := map[string]struct{}{}
	for _, s := range resp.Stations {
		if s.Region != "" {
			regionSet[s.Region] = struct{}{}
		}
	}
	regions := make([]string, 0, len(regionSet))
	for r := range regionSet {
		regions = append(regions, r)
	}
	sort.Strings(regions)

	// Encode station and delta data for inline script injection.
	stationsJSON, err := json.Marshal(resp.Stations)
	if err != nil {
		http.Error(w, "encoding stations", http.StatusInternalServerError)
		return
	}

	deltas, err := buildDeltas()
	if err != nil {
		log.Printf("build deltas: %v", err)
		deltas = map[string]StationDelta{}
	}
	deltasJSON, err := json.Marshal(deltas)
	if err != nil {
		http.Error(w, "encoding deltas", http.StatusInternalServerError)
		return
	}

	lastUpdated := ""
	if resp.LastUpdated != "" {
		if t, err := time.Parse(time.RFC3339, resp.LastUpdated); err == nil {
			lastUpdated = "Dernière mise à jour: " + t.In(time.FixedZone("ET", -4*3600)).
				Format("2 January 2006, 15:04")
		}
	}

	data := mapPageData{
		Page:         "map",
		StationsJSON: template.JS(stationsJSON),
		DeltasJSON:   template.JS(deltasJSON),
		Regions:      regions,
		LastUpdated:  lastUpdated,
		StationCount: len(resp.Stations),
	}

	if isHTMX(r) {
		// Return only the page fragment (content + inline scripts), not the full layout.
		renderTemplate(w, "map-content", data)
		return
	}
	renderTemplate(w, "layout.html", data)
}

// handleStatsPage renders the full stats page shell (or just the content block for htmx).
func handleStatsPage(w http.ResponseWriter, r *http.Request) {
	regions, err := queryRegions()
	if err != nil {
		log.Printf("query regions: %v", err)
		regions = []string{}
	}

	data := statsPageData{
		Page:    "stats",
		Regions: regions,
	}

	if isHTMX(r) {
		renderTemplate(w, "stats-content-shell", data)
		return
	}
	renderTemplate(w, "layout.html", data)
}

// handleStatsContent renders only the stats-content fragment (cards + chart + table).
// This is the htmx target for region and range changes.
func handleStatsContent(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 0 {
			days = d
		}
	}
	region := r.URL.Query().Get("region")

	snapshots, err := querySnapshots(region, daysStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}

	var last Snapshot
	if len(snapshots) > 0 {
		last = snapshots[len(snapshots)-1]
	}

	// Build history rows (last 10, most-recent first).
	rows := snapshots
	if len(rows) > 10 {
		rows = rows[len(rows)-10:]
	}
	history := make([]historyRow, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		s := rows[i]
		d := "—"
		if t, err := time.Parse(time.RFC3339, s.GeneratedAt); err == nil {
			d = t.In(time.FixedZone("ET", -4*3600)).
				Format("2 Jan 2006, 15:04")
		}
		history = append(history, historyRow{
			Date:         d,
			RegularAvg:   fmtPrice(s.RegularAvg),
			RegularMin:   fmtPrice(s.RegularMin),
			RegularMax:   fmtPrice(s.RegularMax),
			SuperAvg:     fmtPrice(s.SuperAvg),
			DieselAvg:    fmtPrice(s.DieselAvg),
			StationCount: s.StationCount,
		})
	}

	snapshotsJSON, err := json.Marshal(snapshots)
	if err != nil {
		http.Error(w, "encoding snapshots", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, "stats-content.html", statsContentData{
		Days:          days,
		Snapshots:     snapshots,
		SnapshotsJSON: template.JS(snapshotsJSON),
		Last:          last,
		HistoryRows:   history,
	})
}

// fmtPrice formats a price value for display; returns "—" for zero/negative.
func fmtPrice(v float64) string {
	if v <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f¢", v)
}

// ── Shared DB helpers ─────────────────────────────────────────────────────

// queryRegions returns sorted distinct region names.
func queryRegions() ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT region FROM region_snapshots ORDER BY region ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var regions []string
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			return nil, err
		}
		regions = append(regions, region)
	}
	return regions, nil
}

// querySnapshots fetches time-series snapshots for the given region (empty = global)
// and day window (daysStr "0" or "" means all).
func querySnapshots(region, daysStr string) ([]Snapshot, error) {
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 0 {
			days = d
		}
	}

	allTime := daysStr == "0"

	var (
		sqlRows *sql.Rows
		err     error
	)

	if region != "" {
		if allTime {
			sqlRows, err = db.Query(`
				SELECT generated_at, regular_avg, regular_min, regular_max,
				       super_avg, diesel_avg, station_count
				FROM region_snapshots
				WHERE region = ?
				ORDER BY generated_at ASC
			`, region)
		} else {
			since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
			sqlRows, err = db.Query(`
				SELECT generated_at, regular_avg, regular_min, regular_max,
				       super_avg, diesel_avg, station_count
				FROM region_snapshots
				WHERE region = ? AND generated_at >= ?
				ORDER BY generated_at ASC
			`, region, since)
		}
		if err != nil {
			return nil, err
		}
		defer sqlRows.Close()

		var snaps []Snapshot
		for sqlRows.Next() {
			var s Snapshot
			if err := sqlRows.Scan(&s.GeneratedAt, &s.RegularAvg, &s.RegularMin, &s.RegularMax,
				&s.SuperAvg, &s.DieselAvg, &s.StationCount); err != nil {
				return nil, err
			}
			snaps = append(snaps, s)
		}
		return snaps, nil
	}

	// Global snapshots
	if allTime {
		sqlRows, err = db.Query(`
			SELECT generated_at, fetched_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM snapshots
			ORDER BY generated_at ASC
		`)
	} else {
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		sqlRows, err = db.Query(`
			SELECT generated_at, fetched_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM snapshots
			WHERE generated_at >= ?
			ORDER BY generated_at ASC
		`, since)
	}
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()

	var snaps []Snapshot
	for sqlRows.Next() {
		var s Snapshot
		if err := sqlRows.Scan(&s.GeneratedAt, &s.FetchedAt, &s.RegularAvg, &s.RegularMin, &s.RegularMax,
			&s.SuperAvg, &s.DieselAvg, &s.StationCount); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}

// buildDeltas computes per-station price deltas from the station_prices table.
func buildDeltas() (map[string]StationDelta, error) {
	since := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)

	rows, err := db.Query(`
		SELECT address, generated_at, regular, super, diesel
		FROM station_prices
		WHERE generated_at >= ?
		ORDER BY address ASC, generated_at ASC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type priceRow struct {
		generatedAt string
		regular     float64
		super       float64
		diesel      float64
	}
	type addrRows struct {
		old *priceRow
		cur *priceRow
	}
	byAddr := map[string]*addrRows{}

	for rows.Next() {
		var addr, genAt string
		var reg, sup, die float64
		if err := rows.Scan(&addr, &genAt, &reg, &sup, &die); err != nil {
			return nil, err
		}
		ar, ok := byAddr[addr]
		if !ok {
			ar = &addrRows{}
			byAddr[addr] = ar
		}
		row := &priceRow{generatedAt: genAt, regular: reg, super: sup, diesel: die}
		if ar.old == nil {
			ar.old = row
		}
		ar.cur = row
	}

	pctChange := func(cur, old float64) *float64 {
		if old <= 0 || cur <= 0 {
			return nil
		}
		v := (cur - old) / old * 100
		return &v
	}

	result := make(map[string]StationDelta, len(byAddr))
	for addr, ar := range byAddr {
		if ar.old == nil || ar.cur == nil || ar.old.generatedAt == ar.cur.generatedAt {
			continue
		}
		oldT, err1 := time.Parse(time.RFC3339, ar.old.generatedAt)
		curT, err2 := time.Parse(time.RFC3339, ar.cur.generatedAt)
		if err1 != nil || err2 != nil {
			continue
		}
		result[addr] = StationDelta{
			Regular:      pctChange(ar.cur.regular, ar.old.regular),
			Super:        pctChange(ar.cur.super, ar.old.super),
			Diesel:       pctChange(ar.cur.diesel, ar.old.diesel),
			ElapsedHours: curT.Sub(oldT).Hours(),
		}
	}
	return result, nil
}

// ── JSON API handlers (kept for backwards-compatibility) ──────────────────

func handleStations(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	resp := cachedResp
	cacheMu.RUnlock()

	if resp == nil {
		http.Error(w, "data not yet available, please retry shortly", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(resp)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")
	snapshots, err := querySnapshots("", daysStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsResponse{Snapshots: snapshots})
}

// handleRegions returns a sorted list of distinct region names from region_snapshots.
func handleRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := queryRegions()
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(regions)
}

// handleRegionStats returns time-series snapshots for a specific region.
func handleRegionStats(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		http.Error(w, "missing region parameter", http.StatusBadRequest)
		return
	}
	daysStr := r.URL.Query().Get("days")
	snapshots, err := querySnapshots(region, daysStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsResponse{Snapshots: snapshots})
}

// StationDelta holds the price change percentage for a station over the available
// history window. Fields are pointers so that missing data serialises as JSON null.
// ElapsedHours is the actual time span between the oldest and newest snapshot used.
type StationDelta struct {
	Regular      *float64 `json:"regular"`
	Super        *float64 `json:"super"`
	Diesel       *float64 `json:"diesel"`
	ElapsedHours float64  `json:"elapsedHours"`
}

// handleStationDeltas returns a map of address → StationDelta.
func handleStationDeltas(w http.ResponseWriter, r *http.Request) {
	result, err := buildDeltas()
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(result)
}

// ── DB init ───────────────────────────────────────────────────────────────

// initDB opens (or creates) the SQLite database and ensures the schema exists.
func initDB(path string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	_, err = d.Exec(`
		CREATE TABLE IF NOT EXISTS snapshots (
			generated_at  TEXT PRIMARY KEY,
			fetched_at    TEXT NOT NULL,
			regular_avg   REAL,
			regular_min   REAL,
			regular_max   REAL,
			super_avg     REAL,
			diesel_avg    REAL,
			station_count INTEGER
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}

	_, err = d.Exec(`
		CREATE TABLE IF NOT EXISTS region_snapshots (
			generated_at  TEXT NOT NULL,
			region        TEXT NOT NULL,
			regular_avg   REAL,
			regular_min   REAL,
			regular_max   REAL,
			super_avg     REAL,
			diesel_avg    REAL,
			station_count INTEGER,
			PRIMARY KEY (generated_at, region)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create region_snapshots table: %w", err)
	}

	_, err = d.Exec(`
		CREATE TABLE IF NOT EXISTS station_prices (
			address      TEXT NOT NULL,
			generated_at TEXT NOT NULL,
			regular      REAL,
			super        REAL,
			diesel       REAL,
			PRIMARY KEY (address, generated_at)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create station_prices table: %w", err)
	}

	_, err = d.Exec(`
		CREATE INDEX IF NOT EXISTS idx_station_prices_generated_at
		ON station_prices (generated_at)
	`)
	if err != nil {
		return nil, fmt.Errorf("create station_prices index: %w", err)
	}

	return d, nil
}

// ── Poller ────────────────────────────────────────────────────────────────

// poller fetches immediately then every pollInterval.
func poller() {
	fetchAndStore()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		fetchAndStore()
	}
}

// fetchAndStore fetches upstream data, updates the in-memory cache, and
// persists a snapshot to SQLite if the data has a new generated_at value.
func fetchAndStore() {
	resp, err := fetchAndParse()
	if err != nil {
		log.Printf("fetch error: %v", err)
		return
	}

	cacheMu.Lock()
	cachedResp = resp
	cacheMu.Unlock()

	if resp.LastUpdated == "" {
		return
	}

	// Compute and persist global aggregate.
	snap := computeSnapshot(resp)

	_, err = db.Exec(`
		INSERT OR IGNORE INTO snapshots
			(generated_at, fetched_at, regular_avg, regular_min, regular_max, super_avg, diesel_avg, station_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.GeneratedAt,
		snap.FetchedAt,
		snap.RegularAvg,
		snap.RegularMin,
		snap.RegularMax,
		snap.SuperAvg,
		snap.DieselAvg,
		snap.StationCount,
	)
	if err != nil {
		log.Printf("db insert error: %v", err)
	}

	// Compute and persist per-region aggregates.
	regionSnaps := computeRegionSnapshots(resp)
	for _, rs := range regionSnaps {
		_, err = db.Exec(`
			INSERT OR IGNORE INTO region_snapshots
				(generated_at, region, regular_avg, regular_min, regular_max, super_avg, diesel_avg, station_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			rs.GeneratedAt,
			rs.Region,
			rs.RegularAvg,
			rs.RegularMin,
			rs.RegularMax,
			rs.SuperAvg,
			rs.DieselAvg,
			rs.StationCount,
		)
		if err != nil {
			log.Printf("db region insert error: %v", err)
		}
	}

	// Persist per-station prices for 24h delta calculations.
	for _, s := range resp.Stations {
		_, err = db.Exec(`
			INSERT OR IGNORE INTO station_prices (address, generated_at, regular, super, diesel)
			VALUES (?, ?, ?, ?, ?)`,
			s.Address,
			resp.LastUpdated,
			s.Regular,
			s.Super,
			s.Diesel,
		)
		if err != nil {
			log.Printf("db station_prices insert error: %v", err)
		}
	}

	// Prune station_prices rows older than 48 hours.
	cutoff := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err = db.Exec(`DELETE FROM station_prices WHERE generated_at < ?`, cutoff); err != nil {
		log.Printf("db station_prices prune error: %v", err)
	}
}

// computeSnapshot derives aggregate statistics from a StationsResponse.
func computeSnapshot(resp *StationsResponse) Snapshot {
	now := time.Now().UTC().Format(time.RFC3339)
	snap := Snapshot{
		GeneratedAt: resp.LastUpdated,
		FetchedAt:   now,
	}

	var regularSum, superSum, dieselSum float64
	var regularCount, superCount, dieselCount int
	snap.RegularMin = 1<<53 - 1
	snap.RegularMax = 0

	for _, s := range resp.Stations {
		if s.Regular > 0 {
			regularSum += s.Regular
			regularCount++
			if s.Regular < snap.RegularMin {
				snap.RegularMin = s.Regular
			}
			if s.Regular > snap.RegularMax {
				snap.RegularMax = s.Regular
			}
		}
		if s.Super > 0 {
			superSum += s.Super
			superCount++
		}
		if s.Diesel > 0 {
			dieselSum += s.Diesel
			dieselCount++
		}
	}

	snap.StationCount = len(resp.Stations)
	if regularCount > 0 {
		snap.RegularAvg = regularSum / float64(regularCount)
	} else {
		snap.RegularMin = 0
	}
	if superCount > 0 {
		snap.SuperAvg = superSum / float64(superCount)
	}
	if dieselCount > 0 {
		snap.DieselAvg = dieselSum / float64(dieselCount)
	}

	return snap
}

// RegionSnapshot holds aggregated statistics for one fetch scoped to a region.
type RegionSnapshot struct {
	GeneratedAt  string  `json:"generatedAt"`
	Region       string  `json:"region"`
	RegularAvg   float64 `json:"regularAvg"`
	RegularMin   float64 `json:"regularMin"`
	RegularMax   float64 `json:"regularMax"`
	SuperAvg     float64 `json:"superAvg"`
	DieselAvg    float64 `json:"dieselAvg"`
	StationCount int     `json:"stationCount"`
}

// computeRegionSnapshots derives per-region aggregate statistics from a StationsResponse.
func computeRegionSnapshots(resp *StationsResponse) []RegionSnapshot {
	type acc struct {
		regularSum, superSum, dieselSum       float64
		regularCount, superCount, dieselCount int
		regularMin, regularMax                float64
		stationCount                          int
	}

	byRegion := map[string]*acc{}
	for _, s := range resp.Stations {
		if s.Region == "" {
			continue
		}
		a, ok := byRegion[s.Region]
		if !ok {
			a = &acc{regularMin: 1<<53 - 1}
			byRegion[s.Region] = a
		}
		a.stationCount++
		if s.Regular > 0 {
			a.regularSum += s.Regular
			a.regularCount++
			if s.Regular < a.regularMin {
				a.regularMin = s.Regular
			}
			if s.Regular > a.regularMax {
				a.regularMax = s.Regular
			}
		}
		if s.Super > 0 {
			a.superSum += s.Super
			a.superCount++
		}
		if s.Diesel > 0 {
			a.dieselSum += s.Diesel
			a.dieselCount++
		}
	}

	snaps := make([]RegionSnapshot, 0, len(byRegion))
	for region, a := range byRegion {
		rs := RegionSnapshot{
			GeneratedAt:  resp.LastUpdated,
			Region:       region,
			StationCount: a.stationCount,
		}
		if a.regularCount > 0 {
			rs.RegularAvg = a.regularSum / float64(a.regularCount)
			rs.RegularMin = a.regularMin
			rs.RegularMax = a.regularMax
		}
		if a.superCount > 0 {
			rs.SuperAvg = a.superSum / float64(a.superCount)
		}
		if a.dieselCount > 0 {
			rs.DieselAvg = a.dieselSum / float64(a.dieselCount)
		}
		snaps = append(snaps, rs)
	}
	return snaps
}

// ── Upstream fetch ────────────────────────────────────────────────────────

func fetchAndParse() (*StationsResponse, error) {
	log.Println("Fetching GeoJSON data from upstream...")

	req, err := http.NewRequest("GET", geojsonURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "essence-quebec-map/1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" || strings.HasSuffix(geojsonURL, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	var geojson GeoJSONResponse
	if err := json.NewDecoder(reader).Decode(&geojson); err != nil {
		return nil, fmt.Errorf("decoding geojson: %w", err)
	}

	var features []struct {
		Geometry struct {
			Coordinates [2]float64 `json:"coordinates"`
		} `json:"geometry"`
		Properties struct {
			Name       string `json:"Name"`
			Brand      string `json:"brand"`
			Address    string `json:"Address"`
			PostalCode string `json:"PostalCode"`
			Region     string `json:"Region"`
			Prices     []struct {
				GasType     string  `json:"GasType"`
				Price       *string `json:"Price"`
				IsAvailable bool    `json:"IsAvailable"`
			} `json:"Prices"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(geojson.Features, &features); err != nil {
		return nil, fmt.Errorf("parsing features: %w", err)
	}

	var stations []Station
	for _, f := range features {
		lng := f.Geometry.Coordinates[0]
		lat := f.Geometry.Coordinates[1]
		if lat == 0 && lng == 0 {
			continue
		}

		s := Station{
			Name:       f.Properties.Name,
			Brand:      f.Properties.Brand,
			Address:    f.Properties.Address,
			Region:     f.Properties.Region,
			PostalCode: f.Properties.PostalCode,
			Lat:        lat,
			Lng:        lng,
		}

		for _, p := range f.Properties.Prices {
			if p.Price == nil || !p.IsAvailable {
				continue
			}
			price := parsePrice(*p.Price)
			if price <= 0 {
				continue
			}
			switch p.GasType {
			case "Régulier":
				s.Regular = price
			case "Super":
				s.Super = price
			case "Diesel":
				s.Diesel = price
			}
		}

		if s.Regular <= 0 {
			continue
		}

		stations = append(stations, s)
	}

	lastUpdated := ""
	if geojson.Metadata != nil && geojson.Metadata.GeneratedAt != "" {
		lastUpdated = geojson.Metadata.GeneratedAt
	}

	log.Printf("Parsed %d stations (last updated: %s)", len(stations), lastUpdated)

	return &StationsResponse{
		LastUpdated: lastUpdated,
		Stations:    stations,
	}, nil
}

// parsePrice converts "190.9¢" to 190.9
func parsePrice(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/D" {
		return 0
	}
	s = strings.TrimSuffix(s, "¢")
	s = strings.TrimSuffix(s, "\u00a2")

	var v float64
	_, err := fmt.Sscanf(s, "%f", &v)
	if err != nil {
		return 0
	}
	return v
}
