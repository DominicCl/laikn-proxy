package main // Go is a compiled language, the compiler needs to know where the program starts. package.main tells the compiler "This file is an executable program, not a helper library."

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/js"
)

func main() {
	// Setup the minifier (starting up the crunching machine)
	m := minify.New()
	m.AddFunc("application/javascript", js.Minify)
	m.AddFunc("text/javascript", js.Minify)
	m.AddFunc("js", js.Minify)

	// Define the handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests (sending data), not GET (just looking)
		if r.Method != http.MethodPost {
			http.Error(w, "Please send a POST request with code", http.StatusMethodNotAllowed)
			return
		}

		// Read the code the user sent
		body, err := io.ReadAll(r.Body) // If successful err = nil
		if err != nil { 
			http.Error(w, "Could not read code", http.StatusBadRequest)
			return
		}
		defer r.Body.Close() // Wait for the parent function to return first

		// Convert bytes to string
		inputCode := string(body)

		// Log what we received (for debugging)
		log.Printf("Received %d bytes of code to minify", len(inputCode))

		// Do the minification
		minifiedCode, err := m.String("text/javascript", inputCode)
		if err != nil {
			log.Printf("Minification Error: %v", err)
			http.Error(w, "Failed to minify", http.StatusInternalServerError)
			return
		}

		// Send the result back
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, minifiedCode)
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
