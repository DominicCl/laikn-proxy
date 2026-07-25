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
		Windspeed80m    []float64 `json:"windspeed_80m"`    // km/h at 80m hub height, 24 values (one per hour)
		DirectRadiation []float64 `json:"direct_radiation"` // W/m², 24 values (one per hour)
	} `json:"hourly"`
}

// RequestData, ResponseData, Region, RegionScore — UNCHANGED
type RequestData struct {
	AllowedRegions []string `json:"allowed_regions"`
	UserID         string   `json:"user_id"`
}

type ResponseData struct {
	Result          string `json:"result"`
	Region          string `json:"region"`
	CarbonIntensity int    `json:"carbon_intensity"`
	Co2Saved        int    `json:"co2_saved"`
	Analysis        string `json:"analysis"`
	Suggestion      string `json:"suggestion"`
	DashboardURL    string `json:"dashboard_url"`
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
	Name     string
	Score    int
	Analysis string
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
		"https://api.open-meteo.com/v1/forecast?latitude=%s&longitude=%s&current_weather=true&hourly=windspeed_80m,direct_radiation&forecast_days=1",
		r.Lat, r.Lon,
	)

	// actually sendding the request to open-meteo
	resp, err := client.Get(url)
	if err != nil { // if there is an error (error-handling)
		results <- RegionScore{r.ID, 999, "API Error"}
		return
	}
	// close the stream we read the response from after storing the response in resp
	defer resp.Body.Close()

	// here we parse the data
	var data OpenMeteoResponse // empty Go structure to hold the weather data
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		results <- RegionScore{r.ID, 999, "JSON Error"}
		return
	}

	// -----------------------------------------------------------------------
	// CURRENT HOUR INDEX
	// Open-Meteo's hourly arrays are indexed 0–23 (midnight to 11pm local UTC).
	// We grab the current UTC hour to index into the right slot.
	// e.g. if it's 14:xx UTC, we read index 14 from windspeed_80m[14].
	// -----------------------------------------------------------------------
	currentHour := time.Now().UTC().Hour()

	// Safely read the hourly values at the current hour.
	// If for any reason the arrays are shorter than expected, we default to 0.
	var windspeed80m float64    // wind speed in km/h at 80m hub height
	var directRadiation float64 // solar energy in W/m² hitting the ground right now

	if currentHour < len(data.Hourly.Windspeed80m) {
		windspeed80m = data.Hourly.Windspeed80m[currentHour] // read the current hour windspeed at 80m high
	}
	if currentHour < len(data.Hourly.DirectRadiation) {
		directRadiation = data.Hourly.DirectRadiation[currentHour] // read the current hour radiation
	}

	temp := data.CurrentWeather.Temperature // still from current_weather, unchanged

	// Start from base load and build up the analysis string
	carbon := r.BaseLoad // the baseload is the average carbon dirtiness of that region's electricity grid, before any weather adjustments, the steady diry yet reliable energy hum, but will clean energy take over??? Let's see!
	analysis := fmt.Sprintf("[%s]: %.1f°C", r.Name, temp)

	// =======================================================================
	// FACTOR 1: WIND — CUBIC POWER FORMULA
	// =======================================================================
	//
	// WHY CUBIC?
	// Wind carries kinetic energy. For a "parcel" of air with mass m moving at
	// speed v, its kinetic energy is: KE = ½ × m × v²
	//
	// But m itself depends on v: faster wind pushes MORE air through the turbine
	// blades per second. Specifically, mass flow rate = ρ × A × v
	// (density × swept area × speed) where the density is the density of air depending
	// on altitude, the swept area is the area of the circle the turbine covers with
	// its blades, and the speed is the speed of the airflow.
	//
	// So total power = (mass per second) × (energy per kg) = (ρ × A × v) × (½ × v²)
	//               = ½ × ρ × A × v³
	//
	// The v³ term is the key insight: doubling wind speed gives 2³ = 8× more power,
	// not 2× as your old linear formula assumed.
	//
	// WHY 80m WIND?
	// Your old code used windspeed from current_weather, which is measured at 10m
	// above ground. Real turbines have hub heights of 80–120m. Wind speed increases
	// with altitude (less friction from the ground), so 10m readings substantially
	// underestimate what turbines actually experience.
	// Open-Meteo gives us windspeed_80m for free — much more accurate.
	//
	// THE FORMULA IN CODE:
	// windMs = speed in m/s (convert from km/h by dividing by 3.6)
	// cubedWind = v³ (the physics)
	// * 0.04 is a normalization constant tuned to our gCO2/kWh scale for determining
	// the best region to choose.
	//   At 10 m/s: 1000 × 0.04 = 40g reduction  (strong wind)
	//   At  5 m/s:  125 × 0.04 =  5g reduction  (light wind — 8× less, not 2×)
	//   At  3 m/s:   27 × 0.04 =  1g reduction  (below cut-in, barely anything)
	//
	// WIND TURBINE CUT-IN / CUT-OUT:
	// Real turbines don't generate below ~3 m/s (cut-in speed) and shut off
	// above ~25 m/s (cut-out) to protect the blades. We model both.
	// So the only window where a wind turbine actually produces electricity is between
	// 3 and 25 m/s.
	// =======================================================================
	if r.Type == "wind" { // if the region uses wind energy
		windMs := windspeed80m / 3.6 // convert km/h → m/s

		if windMs >= 3.0 && windMs <= 25.0 { // checking if it below cut-out (won't make turbine spin) or above cut-in (over-spin)
			// Normal operating range: apply cubic formula
			cubedWind := math.Pow(windMs, 3)
			reduction := int(cubedWind * 0.04) // reduction to fit on our point scale
			carbon -= reduction
			analysis += fmt.Sprintf(", Wind %.1fm/s @80m (cubic reduction: -%dg)", windMs, reduction) // added to the tempature analysis
		} else if windMs > 25.0 {
			// Above cut-out speed: turbines shut down to protect blades.
			// No generation at all — and ironically the grid may need to
			// draw from fossil sources to compensate.
			carbon += 50 // sort of a scramble penalty
			analysis += fmt.Sprintf(", Wind %.1fm/s @80m (cut-out, turbines offline +50g)", windMs)
		} else {
			// Below cut-in speed (< 3 m/s): blades can't turn. No generation.
			analysis += fmt.Sprintf(", Wind %.1fm/s @80m (below cut-in, no generation)", windMs)
		}
	}

	// =======================================================================
	// FACTOR 2: SOLAR — DIRECT RADIATION IN W/m²
	// =======================================================================
	//
	// WHY REPLACE WEATHER CODES?
	// Your old code did: if weatherCode <= 2 → apply flat -200 bonus, else +50 penalty.
	// Problems:
	//   1. Binary — a partly cloudy noon in Sydney and a clear midnight in Paris
	//      both got the same "cloudy penalty." That's wrong.
	//   2. No time-of-day awareness — solar bonuses were applied at 2am.
	//   3. No gradation — 600 W/m² (partly cloudy noon) vs 50 W/m² (thick overcast)
	//      got treated identically.
	//
	// WHAT IS direct_radiation?
	// It's the instantaneous solar energy striking a flat horizontal surface,
	// measured in Watts per square metre (W/m²).
	//   0 W/m²   = nighttime or completely overcast (no solar generation)
	//   100 W/m² = heavily overcast (panels at ~11% of peak)
	//   400 W/m² = partly cloudy (panels at ~44% of peak)
	//   900 W/m² = clear sky at solar noon (near peak — this is our reference max)
	// 	You want to know "how hard are the solar panels working right now?" as a simple number between 0 and 1.
	// That's all solarFraction is.
	// You get there by dividing the current sunlight (whatever W/m² Open-Meteo gives you) by 900.
	// Such is roughly the maximum sunlight you'd ever see on a clear noon. So:
	// No sun at all → 0 / 900 = 0.0 (panels doing nothing)
	// Half sun → 450 / 900 = 0.5 (panels at half capacity)
	// Full sun → 900 / 900 = 1.0 (panels at maximum)
	//
	// THE FORMULA:
	// solarFraction = directRadiation / 900.0
	//   → gives a 0.0 to 1.0 multiplier of "how solar-productive is it right now"
	//   → math.Min(1.0, ...) clamps it so values above 900 W/m² don't overshoot
	//
	// solarReduction = solarFraction × 250
	//   → 250g is the maximum carbon reduction a solar region gets (at full sun) -> 250 is an educated guess
	//   → at 450 W/m²: 0.5 × 250 = 125g reduction
	//   → at 0 W/m² (night): 0 × 250 = 0g reduction — NO bonus at night, automatically
	//
	// This elegantly handles: nighttime (0), partial cloud (gradual), full sun (max).
	// =======================================================================
	if r.Type == "solar" {
		solarFraction := math.Min(1.0, directRadiation/900.0) // overshooting 900 W/m² is unrealistic
		solarReduction := int(solarFraction * 250)
		carbon -= solarReduction // so a higher solar reduction the better
		analysis += fmt.Sprintf(", Solar %.0fW/m² (fraction %.2f, -%dg)", directRadiation, solarFraction, solarReduction)
	}

	// =======================================================================
	// FACTOR 3: PUE TEMPERATURE PENALTY
	// =======================================================================
	//
	// WHAT IS PUE?
	// PUE = Power Usage Effectiveness = Total facility power / IT equipment power
	// A perfect PUE of 1.0 means 100% of electricity goes to compute.
	// Industry average is ~1.5 — for every 1W of compute, 0.5W is wasted on
	// cooling, lighting, power conversion overhead etc.
	// Google's fleet average is ~1.1 (they're exceptional at this).
	//
	// WHY DOES TEMPERATURE MATTER?
	// Cooling is the biggest overhead. When outside air is cold, datacenters use
	// "free cooling" — basically just blowing outside air through the facility,
	// no compressors needed. When it's hot outside, they need mechanical
	// refrigeration, which is energy-hungry.
	//
	// THE MODEL:
	// We use 25°C as the threshold where free cooling stops being viable.
	// Above that, every additional degree forces more mechanical cooling.
	// The penalty scales proportionally with BaseLoad because a dirtier grid
	// amplifies the carbon cost of that extra cooling energy.
	//
	// math.Max(0, temp-25): if temp is 20°C, this is 0 (no penalty — free cooling works)
	//                       if temp is 35°C, this is 10 (penalty applies)
	// × 0.01: each degree above 25°C adds 1% overhead to the base carbon load
	// × float64(r.BaseLoad): scales with grid dirtiness
	//
	// Example — Mumbai (BaseLoad=700) at 38°C:
	//   (38 - 25) × 0.01 × 700 = 13 × 0.01 × 700 = 91g extra carbon
	//   That's a meaningful penalty on top of an already-dirty grid.
	//
	// Example — Finland (BaseLoad=40) at 38°C (hypothetical):
	//   13 × 0.01 × 40 = 5g extra — almost nothing, because the grid is so clean.
	//   The PUE overhead barely matters when your electricity is nuclear.
	// =======================================================================
	// the following applies to all datacenters
	tempOverThreshold := math.Max(0, temp-25.0)
	puePenalty := int(tempOverThreshold * 0.01 * float64(r.BaseLoad))
	if puePenalty > 0 {
		carbon += puePenalty
		analysis += fmt.Sprintf(", Cooling overhead +%dg (%.1f°C over threshold)", puePenalty, tempOverThreshold)
	}

	// =======================================================================
	// FACTOR 4: CURTAILMENT PROXY
	// =======================================================================
	//
	// WHAT IS CURTAILMENT?
	// Curtailment happens when the grid is producing MORE renewable energy than
	// it can use or transmit. The excess energy is simply "thrown away" —
	// turbines are slowed down, solar farms are disconnected.
	//
	// WHY DOES THIS MATTER FOR CARBON?
	// Counterintuitively, if your datacenter runs during a curtailment window,
	// it is consuming electricity that would LITERALLY be wasted otherwise.
	// The marginal carbon cost of that electricity is effectively zero — no extra
	// fossil fuel is burned because of your workload. It's the greenest possible
	// electricity.
	//
	// WHY CAN'T WE GET REAL CURTAILMENT DATA FOR FREE?
	// ISOs (Independent System Operators) publish curtailment data with 30–90 day
	// delays. There's no free real-time API for it globally.
	//
	// THE PROXY APPROACH:
	// Research (NREL, UC Davis) shows curtailment is highly predictable from
	// conditions we CAN observe for free:
	//   - Season: Spring (March–May) is peak curtailment season. Solar output is
	//     high but heating demand has dropped — the grid is often oversupplied.
	//   - Time of day: Midday (10am–3pm) is when solar peaks.
	//   - Irradiance: If direct_radiation > 630 W/m² (70% of max), solar farms
	//     are near full output — oversupply risk is high.
	//
	// When ALL THREE conditions are true for a solar region:
	//   → we set carbon to a floor of 15g (near-zero marginal emissions)
	//   → this correctly rewards running workloads during likely-curtailment windows
	//
	// We intentionally use a floor of 15 (not 0) to be conservative — we're
	// inferring curtailment, not measuring it directly.
	// =======================================================================
	currentMonth := time.Now().UTC().Month()
	isSpring := currentMonth >= 3 && currentMonth <= 5
	isMidday := currentHour >= 10 && currentHour <= 15
	highSolar := directRadiation > 630 // 70% of 900 W/m² max = likely near peak output

	if r.Type == "solar" && isSpring && isMidday && highSolar { // only check for solar regions
		carbon = 15
		analysis += ", ~Curtailment window (near-zero marginal emissions)"
	}

	// Minimum carbon floor — even nuclear has some lifecycle emissions
	if carbon < 15 {
		carbon = 15
	}

	results <- RegionScore{r.ID, carbon, analysis}
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

	// give the suggestion and full analysis
	suggestionText := fmt.Sprintf("🌿 You should run your tasks in %s (%s) for the lowest carbon impact!", winner.Name, winnerID)

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

	resp := ResponseData{
		Result:          "Optimal Route Found",
		Region:          winner.Name,
		CarbonIntensity: scores[0].Score,
		Co2Saved:        savings,
		Analysis:        fullAnalysis,
		Suggestion:      suggestionText,
		DashboardURL:    "https://laikn-dashboard-819883251321.us-central1.run.app",
	}

	w.Header().Set("Content-Type", "application/json") // hey, im sending a JSON
	json.NewEncoder(w).Encode(resp)                    // send it
}

func main() {
	http.HandleFunc("/", handler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Listening on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
