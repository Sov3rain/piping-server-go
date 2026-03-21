package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := "0.0.0.0:" + port
	log.Printf("Piping Server %s", Version)
	log.Printf("Listening on http://%s", addr)

	if err := http.ListenAndServe(addr, NewServer(Version)); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
