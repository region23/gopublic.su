package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/acme/autocert"

	"gopublic/internal/config"
	"gopublic/internal/dashboard"
	"gopublic/internal/ingress"
	"gopublic/internal/metrics"
	"gopublic/internal/server"
	"gopublic/internal/storage"
	"gopublic/internal/telegram"
)

const shutdownTimeout = 30 * time.Second

func main() {
	// Load .env file if it exists
	_ = godotenv.Load()

	// 1. Load and validate configuration
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Initialize Sentry (if configured)
	if cfg.HasSentry() {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			Environment:      cfg.SentryEnvironment,
			SampleRate:       cfg.SentrySampleRate,
			AttachStacktrace: true,
		})
		if err != nil {
			log.Printf("Warning: Sentry initialization failed: %v", err)
		} else {
			log.Println("Sentry error tracking initialized")
			defer sentry.Flush(2 * time.Second)
		}
	}

	// 2. Initialize Database
	if err := storage.InitDB(cfg.DBPath); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Seed data for local development
	if cfg.IsLocalDev() || cfg.InsecureMode {
		storage.SeedData()
	}

	// 3. Initialize Registry
	registry := server.NewTunnelRegistry()

	// 3.5 Initialize Metrics
	appMetrics := metrics.NewAppMetrics()
	if userCount, err := storage.GetTotalUserCount(); err == nil {
		appMetrics.SetUsersTotal(float64(userCount))
	}

	// 4. Initialize Dashboard
	dashHandler, err := dashboard.NewHandlerWithConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize dashboard: %v", err)
	}
	dashHandler.AppMetrics = appMetrics
	dashHandler.MetricsToken = cfg.MetricsToken

	// 5. Start Telegram Bot (for admin commands and auth)
	var telegramBot *telegram.Bot
	if cfg.HasTelegramOAuth() {
		telegramBot = telegram.NewBot(cfg.TelegramBotToken, cfg.TelegramBotName, cfg.AdminTelegramID)
		telegramBot.Start()
	}

	// Connect bot to dashboard for Telegram auth
	dashHandler.TelegramBot = telegramBot
	dashHandler.TelegramWidgetEnabled = cfg.TelegramWidgetEnabled

	// 6. Configure TLS & Autocert (if applicable)
	var tlsConfig *tls.Config
	var autocertManager *autocert.Manager

	if cfg.IsSecure() {
		log.Printf("Configuring HTTPS/TLS for domain: %s", cfg.Domain)
		cacheDir := "certs"
		if err := os.MkdirAll(cacheDir, 0700); err != nil {
			log.Fatalf("Failed to create cert cache dir: %v", err)
		}

		autocertManager = &autocert.Manager{
			Cache:  autocert.DirCache(cacheDir),
			Prompt: autocert.AcceptTOS,
			HostPolicy: func(ctx context.Context, host string) error {
				// Allow exact domain match or any subdomain
				if host == cfg.Domain || strings.HasSuffix(host, "."+cfg.Domain) {
					return nil
				}
				return errors.New("host not configured")
			},
			Email: cfg.Email,
		}
		tlsConfig = autocertManager.TLSConfig()
	}

	// 7. Start Control Plane
	controlPlane := server.NewServerWithConfig(cfg, registry, tlsConfig)
	controlPlane.AppMetrics = appMetrics

	// Connect dashboard to user sessions for connection status display
	dashHandler.SetUserSessions(controlPlane.UserSessions)

	serverErrors := make(chan error, 4)

	go func() {
		if err := controlPlane.Start(); err != nil {
			serverErrors <- err
		}
	}()

	// 8. Start Public Ingress
	ing := ingress.NewIngressWithConfig(cfg, registry, dashHandler)

	var httpServers []*http.Server

	if cfg.IsSecure() {
		// HTTPS Mode (Production)
		httpsServer := &http.Server{
			Addr:      ":443",
			Handler:   ing.Handler(),
			TLSConfig: tlsConfig,
			// WebSocket upgrades require connection hijacking, which is not supported
			// when the request is served over HTTP/2. Disable HTTP/2 to ensure
			// WebSocket-based features (e.g. Next.js HMR at /_next/webpack-hmr) work.
			TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
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
		// HTTP Mode (Local/Dev)
		ingressPort := cfg.IngressPort()
		if cfg.Domain != "" {
			log.Printf("Starting in INSECURE HTTP mode for domain: %s. Listening on %s", cfg.Domain, ingressPort)
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

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	for _, srv := range httpServers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}

	if err := controlPlane.Shutdown(shutdownCtx); err != nil {
		log.Printf("Control plane shutdown error: %v", err)
	}

	// Stop Telegram bot
	if telegramBot != nil {
		telegramBot.Stop()
		telegramBot.GetPendingLogins().Stop()
	}

	log.Println("Server shutdown complete")
}
