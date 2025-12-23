package main

import (
	"context"
	"crypto/tls"
	"gopublic/internal/dashboard"
	"gopublic/internal/ingress"
	"gopublic/internal/server"
	"gopublic/internal/storage"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/acme/autocert"
)

const shutdownTimeout = 30 * time.Second

func main() {
	// Load .env file if it exists
	_ = godotenv.Load()
	insecureMode := os.Getenv("INSECURE_HTTP") == "true"

	// 1. Initialize Database
	// It will create the file in the current working directory.
	// In Docker, we set WORKDIR to /app/data to persist it.
	if err := storage.InitDB("gopublic.db"); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// 2. Initialize Registry
	registry := server.NewTunnelRegistry()

	// 3. Initialize Dashboard
	dashHandler, err := dashboard.NewHandler()
	if err != nil {
		log.Fatalf("Failed to initialize dashboard: %v", err)
	}

	// 4. Configure TLS & Autocert (if applicable)
	domain := os.Getenv("DOMAIN_NAME")
	email := os.Getenv("EMAIL")

	if domain == "" || insecureMode {
		storage.SeedData() // Seed data for local dev
	}

	var tlsConfig *tls.Config
	var autocertManager *autocert.Manager

	if domain != "" && !insecureMode {
		log.Printf("Configuring HTTPS/TLS for domain: %s", domain)
		cacheDir := "certs"
		if err := os.MkdirAll(cacheDir, 0700); err != nil {
			log.Fatalf("Failed to create cert cache dir: %v", err)
		}

		autocertManager = &autocert.Manager{
			Cache:      autocert.DirCache(cacheDir),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(domain, "*."+domain),
			Email:      email,
		}
		tlsConfig = autocertManager.TLSConfig()
	}

	// 5. Start Control Plane (TCP :4443)
	// Pass TLS config ONLY if we are in production (non-insecure) mode
	controlPlane := server.NewServer(":4443", registry, tlsConfig)

	// Channel to collect server errors
	serverErrors := make(chan error, 4)

	go func() {
		if err := controlPlane.Start(); err != nil {
			serverErrors <- err
		}
	}()

	// 6. Start Public Ingress
	var ingressPort string
	if insecureMode {
		ingressPort = ":80"
	} else {
		ingressPort = ":8080"
	}
	ing := ingress.NewIngress(ingressPort, registry, dashHandler)

	// Enable HTTPS only if domain is set AND not explicitly disabled
	useTLS := domain != "" && !insecureMode

	// Track HTTP servers for graceful shutdown
	var httpServers []*http.Server

	if useTLS {
		// --- HTTPS Mode (Production) ---
		// TLS Ingress (443)
		httpsServer := &http.Server{
			Addr:      ":443",
			Handler:   ing.Handler(),
			TLSConfig: tlsConfig,
		}
		httpServers = append(httpServers, httpsServer)

		go func() {
			log.Println("Public Ingress listening on :443 (HTTPS)")
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				serverErrors <- err
			}
		}()

		// HTTP Redirect Server (80)
		httpRedirectServer := &http.Server{
			Addr:    ":80",
			Handler: autocertManager.HTTPHandler(nil),
		}
		httpServers = append(httpServers, httpRedirectServer)

		go func() {
			log.Println("Redirect Server listening on :80 (HTTP)")
			if err := httpRedirectServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverErrors <- err
			}
		}()

	} else {
		// --- HTTP Mode (Local/Dev) ---
		if domain != "" {
			log.Printf("Starting in INSECURE HTTP mode for domain: %s. Listening on %s", domain, ingressPort)
		} else {
			log.Printf("DOMAIN_NAME not set. Starting in HTTP-only mode (Local Dev). Listening on %s", ingressPort)
		}

		httpServer := &http.Server{
			Addr:    ingressPort,
			Handler: ing.Handler(),
		}
		httpServers = append(httpServers, httpServer)

		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverErrors <- err
			}
		}()
	}

	// Wait for interrupt or server error
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Printf("Received signal %v, initiating graceful shutdown...", sig)
	case err := <-serverErrors:
		log.Printf("Server error: %v, initiating shutdown...", err)
	}

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// Shutdown all HTTP servers
	for _, srv := range httpServers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}

	// Shutdown control plane
	if err := controlPlane.Shutdown(shutdownCtx); err != nil {
		log.Printf("Control plane shutdown error: %v", err)
	}

	log.Println("Server shutdown complete")
}
