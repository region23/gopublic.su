package main

import (
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

	"github.com/joho/godotenv"
	"golang.org/x/crypto/acme/autocert"
)

func main() {
	// Load .env file if it exists
	_ = godotenv.Load()

	// 1. Initialize Database
	// It will create the file in the current working directory.
	// In Docker, we set WORKDIR to /app/data to persist it.
	storage.InitDB("gopublic.db")
	// storage.SeedData() // Disable auto-seed in favor of real auth

	// 2. Initialize Registry
	registry := server.NewTunnelRegistry()

	// 3. Initialize Dashboard
	dashHandler := dashboard.NewHandler()

	// 4. Configure TLS & Autocert (if applicable)
	domain := os.Getenv("DOMAIN_NAME")
	email := os.Getenv("EMAIL")

	if domain == "" {
		storage.SeedData() // Seed data for local dev
	}

	var tlsConfig *tls.Config
	var autocertManager *autocert.Manager

	if domain != "" {
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
	// Pass TLS config if available
	controlPlane := server.NewServer(":4443", registry, tlsConfig)
	go func() {
		if err := controlPlane.Start(); err != nil {
			log.Fatalf("Control Plane failed: %v", err)
		}
	}()

	// 6. Start Public Ingress
	ingress := ingress.NewIngress(":8080", registry, dashHandler)

	if domain != "" {
		// --- HTTPS Mode (Production) ---
		// TLS Ingress (443)
		httpsServer := &http.Server{
			Addr:      ":443",
			Handler:   ingress.Handler(),
			TLSConfig: tlsConfig,
		}

		go func() {
			log.Println("Public Ingress listening on :443 (HTTPS)")
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil {
				log.Fatalf("HTTPS Ingress failed: %v", err)
			}
		}()

		// HTTP Redirect Server (80)
		go func() {
			log.Println("Redirect Server listening on :80 (HTTP)")
			if err := http.ListenAndServe(":80", autocertManager.HTTPHandler(nil)); err != nil {
				log.Fatalf("HTTP Redirect Server failed: %v", err)
			}
		}()

	} else {
		// --- HTTP Mode (Local/Dev) ---
		log.Println("DOMAIN_NAME not set. Starting in HTTP-only mode (Local Dev).")
		go func() {
			if err := ingress.Start(); err != nil {
				log.Fatalf("Ingress failed: %v", err)
			}
		}()
	}

	// Wait for interrupt
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")
}
