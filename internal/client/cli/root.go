package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gopublic/internal/client/config"
	"gopublic/internal/client/events"
	"gopublic/internal/client/inspector"
	"gopublic/internal/client/stats"
	"gopublic/internal/client/tui"
	"gopublic/internal/client/tunnel"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var rootCmd = &cobra.Command{
	Use:   "gopublic",
	Short: "A secure request tunneling tool",
}

// ServerAddr should be injected via ldflags. Default for dev.
var ServerAddr = "localhost:4443"

// Version can be set at build time
var Version = "dev"

func Init(serverAddr string) {
	if serverAddr != "" {
		ServerAddr = serverAddr
	}

	// Set version for TUI
	tui.Version = Version

	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(startCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

var authCmd = &cobra.Command{
	Use:   "auth [token]",
	Short: "Save authentication token",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		token := args[0]
		cfg, err := config.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
		cfg.Token = token
		if err := config.SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
		path, _ := config.GetConfigPath()
		fmt.Printf("Token saved to %s\n", path)
	},
}

var startCmd = &cobra.Command{
	Use:   "start [port]",
	Short: "Start a public tunnel to a local port",
	Args:  cobra.MaximumNArgs(1),
	Run:   runStart,
}

func init() {
	startCmd.Flags().BoolP("all", "a", false, "Start all tunnels from gopublic.yaml")
	startCmd.Flags().Bool("tui", true, "Enable terminal UI (default: true for interactive terminals)")
	startCmd.Flags().Bool("no-tui", false, "Disable terminal UI")
	startCmd.Flags().BoolP("force", "f", false, "Force connect, replacing any existing session")
}

func runStart(cmd *cobra.Command, args []string) {
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	if cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "No token found. Run 'gopublic auth <token>' first.")
		os.Exit(1)
	}

	// Get force flag
	forceFlag, _ := cmd.Flags().GetBool("force")

	// Check local lock file
	if err := config.AcquireLock(); err != nil {
		if errors.Is(err, config.ErrAlreadyRunning) {
			if forceFlag {
				fmt.Println("Force mode: removing stale lock file...")
				config.ForceReleaseLock()
				if err := config.AcquireLock(); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to acquire lock: %v\n", err)
					os.Exit(1)
				}
			} else {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				fmt.Fprintln(os.Stderr, "Use --force to override.")
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Failed to acquire lock: %v\n", err)
			os.Exit(1)
		}
	}
	defer config.ReleaseLock()

	// Determine if we should use TUI
	useTUI := shouldUseTUI(cmd)

	// Setup context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create shared components
	eventBus := events.NewBus()
	statsTracker := stats.New()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		if !useTUI {
			fmt.Println("\nShutdown signal received, closing tunnel...")
		}
		cancel()
	}()

	// Start Inspector in background
	inspector.Start("4040")

	// Check for project config (gopublic.yaml)
	allFlag, _ := cmd.Flags().GetBool("all")
	projectCfg, projectErr := config.LoadProjectConfig("")

	if projectErr == nil && (allFlag || len(args) == 0) {
		// Multi-tunnel mode from gopublic.yaml
		runMultiTunnel(ctx, cfg, projectCfg, eventBus, statsTracker, useTUI, forceFlag)
	} else if len(args) == 1 {
		// Single tunnel mode
		port := args[0]
		runSingleTunnel(ctx, cfg, port, eventBus, statsTracker, useTUI, forceFlag)
	} else {
		fmt.Fprintln(os.Stderr, "Either provide a port or create gopublic.yaml config file")
		os.Exit(1)
	}

	if !useTUI {
		fmt.Println("Tunnel closed")
	}
}

func shouldUseTUI(cmd *cobra.Command) bool {
	// Check explicit flags
	noTUI, _ := cmd.Flags().GetBool("no-tui")
	if noTUI {
		return false
	}

	tuiFlag, _ := cmd.Flags().GetBool("tui")
	if !tuiFlag {
		return false
	}

	// Check if stdout is a terminal
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}

	return true
}

func runSingleTunnel(ctx context.Context, cfg *config.Config, port string, eventBus *events.Bus, statsTracker *stats.Stats, useTUI bool, force bool) {
	// Configure replay with local port
	inspector.SetLocalPort(port)

	// Create tunnel with dependencies
	t := tunnel.NewTunnel(ServerAddr, cfg.Token, port)
	t.SetEventBus(eventBus)
	t.SetStats(statsTracker)
	t.SetForce(force)

	if useTUI {
		// Run with TUI
		runWithTUI(ctx, eventBus, statsTracker, func(ctx context.Context) error {
			return t.StartWithReconnect(ctx, nil)
		})
	} else {
		// Legacy mode
		fmt.Printf("Starting tunnel to localhost:%s on server %s\n", port, ServerAddr)
		fmt.Println("Inspector UI: http://localhost:4040")

		if err := t.StartWithReconnect(ctx, nil); err != nil {
			if err != context.Canceled {
				fmt.Fprintf(os.Stderr, "Tunnel error: %v\n", err)
				os.Exit(1)
			}
		}
	}
}

func runMultiTunnel(ctx context.Context, cfg *config.Config, projectCfg *config.ProjectConfig, eventBus *events.Bus, statsTracker *stats.Stats, useTUI bool, force bool) {
	manager := tunnel.NewTunnelManager(ServerAddr, cfg.Token)
	manager.SetForce(force)

	// Set first tunnel port for replay
	for _, t := range projectCfg.Tunnels {
		inspector.SetLocalPort(t.Addr)
		break
	}

	for name, t := range projectCfg.Tunnels {
		manager.AddTunnel(name, t.Addr, t.Subdomain)
	}

	// TODO: Inject eventBus and stats into manager tunnels
	// For now, multi-tunnel mode doesn't have full TUI integration

	if useTUI {
		// Run with TUI
		runWithTUI(ctx, eventBus, statsTracker, func(ctx context.Context) error {
			return manager.StartAll(ctx)
		})
	} else {
		// Legacy mode
		fmt.Println("Loading tunnels from gopublic.yaml...")
		fmt.Println("Inspector UI: http://localhost:4040")

		if err := manager.StartAll(ctx); err != nil {
			if err != context.Canceled {
				fmt.Fprintf(os.Stderr, "Tunnel error: %v\n", err)
				os.Exit(1)
			}
		}
	}
}

func runWithTUI(ctx context.Context, eventBus *events.Bus, statsTracker *stats.Stats, tunnelFunc func(context.Context) error) {
	// Create context that will be cancelled when TUI exits
	tuiCtx, tuiCancel := context.WithCancel(ctx)
	defer tuiCancel()

	// Start tunnel in background
	tunnelDone := make(chan error, 1)
	go func() {
		tunnelDone <- tunnelFunc(tuiCtx)
	}()

	// Create and run TUI
	model := tui.NewModel(eventBus, statsTracker)
	p := tea.NewProgram(model, tea.WithAltScreen())

	// Run TUI (blocks until quit)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}

	// Cancel tunnel context when TUI exits
	tuiCancel()

	// Wait for tunnel to finish
	<-tunnelDone
}
