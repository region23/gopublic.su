package cli

import (
	"fmt"
	"gopublic/internal/client/config"
	"gopublic/internal/client/inspector"
	"gopublic/internal/client/tunnel"
	"log"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "gopublic",
	Short: "A secure request tunneling tool",
}

// ServerAddr should be injected via ldflags. Default for dev.
var ServerAddr = "localhost:4443"

func Init(serverAddr string) {
	if serverAddr != "" {
		ServerAddr = serverAddr
	}

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
			log.Fatalf("Error loading config: %v", err)
		}
		cfg.Token = token
		if err := config.SaveConfig(cfg); err != nil {
			log.Fatalf("Error saving config: %v", err)
		}
		path, _ := config.GetConfigPath()
		fmt.Printf("Token saved to %s\n", path)
	},
}

var startCmd = &cobra.Command{
	Use:   "start [port]",
	Short: "Start a public tunnel to a local port",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		port := args[0]

		cfg, err := config.LoadConfig()
		if err != nil {
			log.Fatalf("Error loading config: %v", err)
		}

		if cfg.Token == "" {
			log.Fatal("No token found. Run 'gopublic auth <token>' first.")
		}

		fmt.Printf("Starting tunnel to localhost:%s on server %s\n", port, ServerAddr)

		// Start Inspector
		inspector.Start("4040")
		fmt.Printf("Inspector UI running on http://localhost:4040\n")

		// Start Tunnel
		t := tunnel.NewTunnel(ServerAddr, cfg.Token, port)
		if err := t.Start(); err != nil {
			log.Fatalf("Tunnel error: %v", err)
		}
	},
}
