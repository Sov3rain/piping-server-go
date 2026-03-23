package main

import (
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := "0.0.0.0:" + port
	logger := newLogger()
	logger.Info("server starting",
		"version", Version,
		"addr", addr,
	)

	if err := http.ListenAndServe(addr, NewServer(Version)); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
