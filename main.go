package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var Version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	printTpl := flag.Bool("print-template", false, "print configuration template and exit")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("api-proxy version %s\n", Version)
		return
	}

	if *printTpl {
		fmt.Print(ConfigTemplate)
		return
	}

	// Load configuration
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Printf("Failed to load config: %v", err)
		log.Fatalf("Hint: use '-print-template' to see a valid configuration example.")
	}

	// Print startup banner
	printBanner(cfg)

	// Initialize shared KeyManagers per auth provider
	keyManagers := make(map[string]*KeyManager)
	for _, ap := range cfg.Auth.Providers {
		keyManagers[ap.Name] = NewKeyManager(ap.Keys, ap.RateLimit)
	}

	// Build the proxy router and middleware chain
	router := NewProxyRouter(cfg, keyManagers)
	handler := buildMiddlewareChain(router, cfg.MaxBodySize)

	// Create HTTP server
	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-quit
		log.Printf("Received signal %v, shutting down gracefully...", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("Server forced to shutdown: %v", err)
		}
	}()

	// Start the server
	log.Printf("Listening on %s", cfg.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server stopped")
}
