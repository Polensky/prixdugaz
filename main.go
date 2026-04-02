package main

import (
	"compress/gzip"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed static/*
var staticFiles embed.FS

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

	// Initial fetch, then background poll every 5 minutes.
	go poller()

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static files: %v", err)
	}

	http.HandleFunc("/api/stations", handleStations)
	http.HandleFunc("/api/stats", handleStats)
	http.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("Listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// initDB opens (or creates) the SQLite database and ensures the schema exists.
func initDB(path string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", path)
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
	return d, nil
}

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

	// Compute aggregates.
	snap := computeSnapshot(resp)

	// Only insert if this generated_at is new.
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
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}

	// days=0 means all data
	var rows *sql.Rows
	var err error
	if daysStr == "0" {
		rows, err = db.Query(`
			SELECT generated_at, fetched_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM snapshots
			ORDER BY generated_at ASC
		`)
	} else {
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		rows, err = db.Query(`
			SELECT generated_at, fetched_at, regular_avg, regular_min, regular_max,
			       super_avg, diesel_avg, station_count
			FROM snapshots
			WHERE generated_at >= ?
			ORDER BY generated_at ASC
		`, since)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("db query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	snapshots := []Snapshot{}
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.GeneratedAt, &s.FetchedAt, &s.RegularAvg, &s.RegularMin, &s.RegularMax,
			&s.SuperAvg, &s.DieselAvg, &s.StationCount); err != nil {
			http.Error(w, fmt.Sprintf("db scan: %v", err), http.StatusInternalServerError)
			return
		}
		snapshots = append(snapshots, s)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsResponse{Snapshots: snapshots})
}

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

	// Handle gzip if the response is compressed
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

	// Parse features
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
	s = strings.TrimSuffix(s, "\u00a2") // cent sign

	var v float64
	_, err := fmt.Sscanf(s, "%f", &v)
	if err != nil {
		return 0
	}
	return v
}
