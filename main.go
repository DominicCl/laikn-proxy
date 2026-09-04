package main

// v3

// ========== IMPORTS ==========

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math" // NEW: needed for math.Pow (cubic wind) and math.Max (PUE floor)
	"net/http"
	"os"
	"sort"
	"sync"
	"time" // NEW: needed for curtailment time-of-day/season check

	"cloud.google.com/go/firestore"
)

// ========== DATA STRUCTURES ==========

// UPDATED: OpenMeteoResponse now captures hourly data in addition to current weather.
// Previously we only pulled current_weather (which gives windspeed at 10m ground level
// and a blunt weathercode). Now we also pull:
//   - windspeed_80m: wind speed at 80 metres height, where real turbines actually spin
//   - direct_radiation: actual solar energy hitting the ground in W/m²
//
// Both are free from Open-Meteo's hourly endpoint.
type OpenMeteoResponse struct {
	CurrentWeather struct {
		Temperature float64 `json:"temperature"`
		WeatherCode int     `json:"weathercode"`
	} `json:"current_weather"`
	Hourly struct {
		Temperature2m   []float64 `json:"temperature_2m"`
		Windspeed80m    []float64 `json:"windspeed_80m"`
		DirectRadiation []float64 `json:"direct_radiation"`
	} `json:"hourly"`
}

// RequestData, ResponseData, Region, RegionScore — UNCHANGED
type RequestData struct {
	AllowedRegions []string `json:"allowed_regions"`
	UserID         string   `json:"user_id"`
}

type ResponseData struct {
	Result           string `json:"result"`
	Region           string `json:"region"`
	CarbonIntensity  int    `json:"carbon_intensity"`
	Co2Saved         int    `json:"co2_saved"`
	Analysis         string `json:"analysis"`
	Suggestion       string `json:"suggestion"`
	DashboardURL     string `json:"dashboard_url"`
	BestFutureRegion string `json:"best_future_region"` // NEW
	OptimalWait      int    `json:"optimal_wait"`       // NEW
}

type DecisionData struct {
	UserID       string `json:"user_id"`
	Decision     string `json:"decision"`
	ChosenOption string `json:"chosen_option"`
}

var projectID = "loyal-theater-484704-g3"

type Region struct {
	ID       string
	Name     string
	Lat      string
	Lon      string
	BaseLoad int
	Type     string
}

type RegionScore struct {
	Name        string
	Score       int
	Analysis    string
	FutureScore int
	WaitHours   int
}

// globalRegions — UNCHANGED
var globalRegions = []Region{
	{"us-central1", "Iowa (US)", "41.26", "-95.86", 550, "wind"},
	{"us-west1", "Oregon (US)", "45.63", "-121.20", 150, "hydro"},
	{"us-east4", "Virginia (US)", "39.04", "-77.48", 450, "mixed"},
	{"southamerica-east1", "São Paulo (BR)", "-23.55", "-46.63", 100, "hydro"},
	{"europe-west9", "Paris (FR)", "48.85", "2.35", 50, "nuclear"},
	{"europe-west1", "St. Ghislain (BE)", "50.45", "3.82", 200, "mixed"},
	{"europe-west2", "London (UK)", "51.50", "-0.12", 250, "wind"},
	{"europe-west3", "Frankfurt (DE)", "50.11", "8.68", 400, "coal"},
	{"europe-north1", "Finland (FI)", "60.16", "24.93", 40, "nuclear"},
	{"asia-northeast1", "Tokyo (JP)", "35.67", "139.65", 500, "gas"},
	{"australia-southeast1", "Sydney (AU)", "-33.86", "151.20", 600, "solar"},
	{"asia-south1", "Mumbai (IN)", "19.07", "72.87", 700, "coal"},
	// --- NEW REGIONS ---
	{"europe-southwest1", "Madrid (ES)", "40.41", "-3.70", 200, "solar"},
	{"europe-north2", "Stockholm (SE)", "59.32", "18.06", 30, "hydro"},
	{"asia-southeast1", "Singapore (SG)", "1.35", "103.82", 450, "gas"},
}

// ========== THE INFERENCE ENGINE ==========

func calculateScoreForHour(r Region, temp, windspeed80m, directRadiation float64, hourUTC int) (int, string) {
	carbon := r.BaseLoad
	analysis := fmt.Sprintf("[%s]: %.1f°C", r.Name, temp)

	if r.Type == "wind" {
		windMs := windspeed80m / 3.6
		if windMs >= 3.0 && windMs <= 25.0 {
			reduction := int(math.Pow(windMs, 3) * 0.04)
			carbon -= reduction
			analysis += fmt.Sprintf(", Wind %.1fm/s (-%dg)", windMs, reduction)
		} else if windMs > 25.0 {
			carbon += 50
			analysis += fmt.Sprintf(", Wind %.1fm/s (cut-out +50g)", windMs)
		}
	}

	if r.Type == "solar" {
		solarFraction := math.Min(1.0, directRadiation/900.0)
		solarReduction := int(solarFraction * 250)
		carbon -= solarReduction
		analysis += fmt.Sprintf(", Solar %.0fW/m² (-%dg)", directRadiation, solarReduction)
	}

	tempOverThreshold := math.Max(0, temp-25.0)
	puePenalty := int(tempOverThreshold * 0.01 * float64(r.BaseLoad))
	if puePenalty > 0 {
		carbon += puePenalty
		analysis += fmt.Sprintf(", Cooling overhead +%dg", puePenalty)
	}

	currentMonth := time.Now().UTC().Month()
	isSpring := currentMonth >= 3 && currentMonth <= 5
	isMidday := hourUTC >= 10 && hourUTC <= 15
	if r.Type == "solar" && isSpring && isMidday && directRadiation > 630 {
		carbon = 15
		analysis += ", ~Curtailment window"
	}

	if carbon < 15 {
		carbon = 15
	}
	return carbon, analysis
}

// calculateRegionCarbon — FULLY UPDATED
// This is the only function that changed. Everything else (handler, main, Firestore) is identical.
func calculateRegionCarbon(r Region, wg *sync.WaitGroup, results chan<- RegionScore) {
	defer wg.Done() // defer tells go to only run this when the function is done, wg.Done says we can send the response back

	// create an http client with a 5 second timeout, meteo-API might take more time due to rich response
	client := &http.Client{Timeout: 5 * time.Second}

	// -----------------------------------------------------------------------
	// UPDATED API CALL
	// We now request hourly data in addition to current_weather.
	// &hourly=windspeed_80m,direct_radiation asks Open-Meteo to return 24 values
	// (one per hour of today) for hub-height wind and solar irradiance.
	// &forecast_days=1 keeps the response small — we only need today's data.
	// This is still completely free from Open-Meteo.
	// -----------------------------------------------------------------------
	// building a string for the editable API endpoint according to the latitude and longitude of the region
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%s&longitude=%s&current_weather=true&hourly=temperature_2m,windspeed_80m,direct_radiation&forecast_days=2",
		r.Lat, r.Lon,
	)

	// actually sendding the request to open-meteo
	resp, err := client.Get(url)
	if err != nil { // if there is an error (error-handling)
		results <- RegionScore{r.ID, 999, "API Error", 999, 0} // <--- ADDED 999, 0
		return
	}
	// close the stream we read the response from after storing the response in resp
	defer resp.Body.Close()

	// here we parse the data
	var data OpenMeteoResponse // empty Go structure to hold the weather data
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		results <- RegionScore{r.ID, 999, "JSON Error", 999, 0} // <--- ADDED 999, 0
		return
	}

	// -----------------------------------------------------------------------
	// CURRENT HOUR INDEX
	// Open-Meteo's hourly arrays are indexed 0–23 (midnight to 11pm local UTC).
	// We grab the current UTC hour to index into the right slot.
	// e.g. if it's 14:xx UTC, we read index 14 from windspeed_80m[14].
	// -----------------------------------------------------------------------
	currentHour := time.Now().UTC().Hour()

	var windspeed80m, directRadiation, currTemp float64
	if currentHour < len(data.Hourly.Windspeed80m) {
		windspeed80m = data.Hourly.Windspeed80m[currentHour]
	}
	if currentHour < len(data.Hourly.DirectRadiation) {
		directRadiation = data.Hourly.DirectRadiation[currentHour]
	}
	if currentHour < len(data.Hourly.Temperature2m) {
		currTemp = data.Hourly.Temperature2m[currentHour]
	} else {
		currTemp = data.CurrentWeather.Temperature
	}

	immediateScore, analysis := calculateScoreForHour(r, currTemp, windspeed80m, directRadiation, currentHour)

	bestFutureScore := immediateScore
	optimalWait := 0

	// Look ahead 24 hours instead of 12
	for i := 1; i <= 24; i++ {
		idx := currentHour + i
		if idx < len(data.Hourly.Temperature2m) && idx < len(data.Hourly.Windspeed80m) && idx < len(data.Hourly.DirectRadiation) {
			fScore, _ := calculateScoreForHour(r, data.Hourly.Temperature2m[idx], data.Hourly.Windspeed80m[idx], data.Hourly.DirectRadiation[idx], (currentHour+i)%24)

			// If waiting drops emissions even slightly, save it
			if fScore < bestFutureScore {
				bestFutureScore = fScore
				optimalWait = i
			}
		}
	}

	results <- RegionScore{r.ID, immediateScore, analysis, bestFutureScore, optimalWait}
}

// ========== HANDLER & MAIN  ==========

func handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")              // I will accept requests from any origin
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS") // Types of requests we accept
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")  // I will accept JSON data

	if r.Method == http.MethodOptions {
		return
	}

	var reqData RequestData                  // empty Go variable based off of the RequestData struct defined above
	json.NewDecoder(r.Body).Decode(&reqData) // pours the JSON into the reqData variable

	// building and filling the regions to check array
	var regionsToCheck []Region
	if len(reqData.AllowedRegions) > 0 {
		for _, region := range globalRegions {
			for _, allowed := range reqData.AllowedRegions {
				if region.ID == allowed {
					regionsToCheck = append(regionsToCheck, region)
				}
			}
		}
	} else {
		regionsToCheck = globalRegions // if the list is empty we default to all regions
	}

	if len(regionsToCheck) == 0 {
		regionsToCheck = []Region{globalRegions[0]}
	}

	var wg sync.WaitGroup                                      // counter for the workers
	resultsChan := make(chan RegionScore, len(regionsToCheck)) // channel for each region to report their region score

	for _, region := range regionsToCheck { // loop through the filtered list of regions
		wg.Add(1)                                          // increment the worker counter
		go calculateRegionCarbon(region, &wg, resultsChan) // the Go workers get to work in parallel on the various regions
	}

	wg.Wait()          // waiting for all workers to be done
	close(resultsChan) // close the channel

	// pull all the results
	var scores []RegionScore
	for s := range resultsChan {
		scores = append(scores, s)
	}

	// sort all the results
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score < scores[j].Score
	})

	// grabing the id of the top city
	winnerID := scores[0].Name
	worst := scores[len(scores)-1]
	savings := worst.Score - scores[0].Score

	// get the human readable name
	var winner Region
	for _, reg := range globalRegions {
		if reg.ID == winnerID {
			winner = reg
		}
	}

	var bestFuture RegionScore
	hasFuture := false
	for _, s := range scores {
		// Check if the future score is strictly less than the current winner
		if s.WaitHours > 0 && s.FutureScore < scores[0].Score {
			if !hasFuture || s.FutureScore < bestFuture.FutureScore {
				bestFuture = s
				hasFuture = true
			}
		}
	}

	suggestionText := fmt.Sprintf("You should run your tasks in %s (%s) for the lowest carbon impact!", winner.Name, winnerID)

	if hasFuture {
		var futureWinner Region
		for _, reg := range globalRegions {
			if reg.ID == bestFuture.Name {
				futureWinner = reg
			}
		}
		suggestionText += fmt.Sprintf(" Or wait %d hours and route to %s (%s) to drop emissions down to %dg!", bestFuture.WaitHours, futureWinner.Name, bestFuture.Name, bestFuture.FutureScore)
	} else {
		suggestionText += " Even with temporal shifting considered, this is the best option right now."
	}

	fullAnalysis := "🏆 WINNER: " + winner.Name + "\n"
	for _, s := range scores {
		fullAnalysis += fmt.Sprintf("%s: %dg %s\n", s.Name, s.Score, s.Analysis)
	}

	// storring info in the DB
	ctx := context.Background() // set of rules/timings for the db request
	dbClient, err := firestore.NewClient(ctx, projectID)
	if err == nil {
		defer dbClient.Close()
		dbClient.Collection("history").Add(ctx, map[string]interface{}{
			"action":           "global_inference_v3",
			"region":           winner.Name,
			"carbon_intensity": scores[0].Score,
			"analysis":         fullAnalysis,
			"co2_saved":        savings,
			"user_id":          reqData.UserID,
			"timestamp":        firestore.ServerTimestamp,
		})
	}

	var futureWinnerName string
	if hasFuture {
		for _, reg := range globalRegions {
			if reg.ID == bestFuture.Name {
				futureWinnerName = reg.Name
			}
		}
	}

	resp := ResponseData{
		Result:           "Optimal Route Found",
		Region:           winner.Name,
		CarbonIntensity:  scores[0].Score,
		Co2Saved:         savings,
		Analysis:         fullAnalysis,
		Suggestion:       suggestionText,
		DashboardURL:     "https://laikn-dashboard-819883251321.us-central1.run.app",
		BestFutureRegion: futureWinnerName,     // <--- Added this
		OptimalWait:      bestFuture.WaitHours, // <--- Added this
	}
	w.Header().Set("Content-Type", "application/json") // hey, im sending a JSON
	json.NewEncoder(w).Encode(resp)                    // send it
}

func decisionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		return
	}

	var d DecisionData
	json.NewDecoder(r.Body).Decode(&d)

	ctx := context.Background()
	dbClient, err := firestore.NewClient(ctx, projectID)
	if err == nil {
		defer dbClient.Close()
		dbClient.Collection("decisions").Add(ctx, map[string]interface{}{
			"user_id":       d.UserID,
			"decision":      d.Decision,
			"chosen_option": d.ChosenOption,
			"timestamp":     firestore.ServerTimestamp,
		})
	}
	w.WriteHeader(http.StatusOK)
}

func main() {
	http.HandleFunc("/", handler)
	http.HandleFunc("/decision", decisionHandler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Listening on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
