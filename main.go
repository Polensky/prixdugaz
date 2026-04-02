package main

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

const (
	geojsonURL  = "https://regieessencequebec.ca/stations.geojson.gz"
	defaultPort = "8080"
	cacheTTL    = 5 * time.Minute
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

// In-memory cache
var (
	cacheMu       sync.RWMutex
	cachedResp    *StationsResponse
	cacheExpiry   time.Time
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static files: %v", err)
	}

	http.HandleFunc("/api/stations", handleStations)
	http.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("Listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleStations(w http.ResponseWriter, r *http.Request) {
	resp, err := getStations()
	if err != nil {
		http.Error(w, fmt.Sprintf("error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(resp)
}

func getStations() (*StationsResponse, error) {
	cacheMu.RLock()
	if cachedResp != nil && time.Now().Before(cacheExpiry) {
		defer cacheMu.RUnlock()
		return cachedResp, nil
	}
	cacheMu.RUnlock()

	resp, err := fetchAndParse()
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	cachedResp = resp
	cacheExpiry = time.Now().Add(cacheTTL)
	cacheMu.Unlock()

	return resp, nil
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
