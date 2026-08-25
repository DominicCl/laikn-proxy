# 🍃 Laikn: Carbon-Aware VS Code Router

Laikn is an intelligent, carbon-aware developer tool built to dynamically route workloads and optimize cloud energy efficiency across global server regions in real-time. By coupling a VS Code extension, a high-performance Go proxy, and a Ruby on Rails analytics dashboard, Laikn bridges the gap between infrastructure deployment and environmental impact.

## 🚀 Technical Architecture

* **Client Interface (VS Code Extension):** Built using TypeScript and the VS Code Extension API. It manages local state via `globalState` for region preferences, features a custom multi-select quick pick supporting up to 16 global nodes, and formats live telemetry directly into a dedicated output channel.
* **Core Backend Proxy (Go):** Engineered in Go utilizing concurrent worker routines (`sync.WaitGroup` and buffered channels) to query the Open-Meteo API simultaneously across all target regions without blocking. It processes live meteorological metrics—including 80-meter wind speeds, direct solar radiation, and ambient temperatures—applying custom physics formulas for cubic wind turbine power curves, cut-off thresholds, solar curtailment windows, and datacenter PUE cooling overhead penalties.
* **Analytics Dashboard (Ruby on Rails):** A server-side Rails application utilizing Tailwind CSS and Google Cloud Firestore. It aggregates telemetry and user routing history, serving live insights and metric tracking.

## ⚙️ How the Routing Engine Works

When a routing request is triggered, the Go microservice evaluates real-time grid conditions using the following pipeline:

1. **Concurrent Data Fetching:** Fires parallel HTTP requests to fetch 72-hour hourly forecasts for temperature, wind speed at 80m, and direct normal irradiance.
2. **Physics & Emissions Modeling:** Computes baseline emissions adjusted for real-time renewable generation:
   * *Wind Nodes:* Calculates cubic velocity scaling ($v^3$) between cut-in and cut-out thresholds, factoring in penalty spikes if winds exceed safe operational limits.
   * *Solar Nodes:* Determines solar fractions based on direct radiation intensity, factoring in spring curtailment windows where marginal emissions drop near-zero.
   * *Cooling Overhead:* Dynamically scales datacenter Power Usage Effectiveness (PUE) penalties based on how much ambient temperatures exceed $25^\circ\text{C}$.
3. **Temporal Shifting & Selection:** Identifies the absolute greenest immediate region or evaluates if waiting a specific number of hours yields a better carbon score via temporal shifting.

## 🛠️ Quick Start & Setup

### 1. Backend Proxy (Go)
Navigate to the proxy directory, install dependencies, and run your local server or build your container for Google Cloud Run:
```bash
go mod init laikn-proxy
go build -o main main.go
./main
