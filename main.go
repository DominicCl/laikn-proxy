package main // Go is a compiled language, the compiler needs to know where the program starts. package.main tells the compiler "This file is an executable program, not a helper library."

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	// Get the PORT
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// When someone visits "/", do this:
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Log to the terminal so we can see it
		log.Println("New Request Received")

		// Send text back to the browser
		fmt.Fprintf(w, "Laikn Proxy is ONLINE.\nRunning in region: %s", os.Getenv("K_SERVICE"))
	})

	// Start the server
	log.Printf("Listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
