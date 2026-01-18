// Minifies code and calculate energy savings

package main // Go is a compiled language, the compiler needs to know where the program starts. package.main tells the compiler "This file is an executable program, not a helper library."

import (
	"io"
	"log"
	"net/http"
	"os"
	"bytes"
	"context"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/js"
	"cloud.google.com/go/firestore"
)

// Global databse client
var client *firestore.Client

func main() {
	// Connect to the database
	ctx := context.Background()
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")

	// If running locally, you might need to harcode your project ID here
	if projectID == "" {
		projectID = "loyal-theater-484704-g3" // Grabbed from logs
	}

	var err error
	client, err = firestore.NewClient(ctx, projectID)
	if err != nil {
		log.Printf("Warning: Failed to create Firestore client: %v", err)
	} else {
		defer client.Close()
	}

	// Setup the minifier
	m := minify.New()
	m.AddFunc("text/javascript", js.Minify)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request){
		// Read the body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Could not read the body", http.StatusBadRequest)
			return
		}

		// Do the work, minify
		w.Header().Set("Content-Type", "text/javascript")
		if err := m.Minify("text/javascript", w, bytes.NewReader(body)); err != nil {
			http.Error(w, "Minification failed", http.StatusInternalServerError)
			return
		}

		if client != nil {
			_, _, err = client.Collection("history").Add(ctx, map[string]interface{}{
				"action":    "minified",
				"timestamp": firestore.ServerTimestamp,
				"region":    "us-central1", 
				"size":      len(body),
			})
			if err != nil {
				log.Printf("Failed to save to DB: %v", err)
			} else {
				log.Printf("Successfully saved to Firestore!")
			}
		}
	})

	// Get the PORT
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start the server
	log.Printf("Laikn worker listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
