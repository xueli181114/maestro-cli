package cmd

import (
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	rootShortDescription = "maestro-cli - ManifestWork lifecycle management for HyperFleet"
	rootLongDescription  = `maestro-cli is a gRPC-based CLI tool for managing ManifestWork resources
in Maestro. It enables HyperFleet adapters to apply, monitor, and sync Kubernetes
resources to target clusters via job-based execution.

Environment Variables:
  MAESTRO_GRPC_ENDPOINT        Maestro gRPC server endpoint
  MAESTRO_HTTP_ENDPOINT        Maestro HTTP server endpoint
  MAESTRO_GRPC_INSECURE        Skip TLS verification (true/false)
  MAESTRO_GRPC_SERVER_CA_FILE  Path to server CA certificate file
  MAESTRO_GRPC_CLIENT_CERT     Path to client certificate file
  MAESTRO_GRPC_CLIENT_KEY      Path to client key file
  MAESTRO_GRPC_TOKEN           Bearer token for authentication
  MAESTRO_GRPC_TOKEN_FILE      Path to file containing bearer token
  MAESTRO_SOURCE_ID            Source ID for CloudEvents subscription (default: maestro-cli)

Note: Command-line flags take priority over environment variables.

Examples:
  # Apply a ManifestWork to a target cluster
  maestro-cli apply --manifest-file=nodepool.yaml --consumer=cluster-west-1 --watch

  # Get status of a ManifestWork
  maestro-cli get --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1

  # Wait for a condition
  maestro-cli wait --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1 --condition=Applied

  # Delete a ManifestWork
  maestro-cli delete --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1

  # Using environment variables
  export MAESTRO_GRPC_ENDPOINT=maestro.example.com:8090
  export MAESTRO_HTTP_ENDPOINT=https://maestro.example.com:8000
  maestro-cli apply --manifest-file=nodepool.yaml --consumer=cluster-west-1`
)

// Environment variable names
const (
	EnvGRPCEndpoint       = "MAESTRO_GRPC_ENDPOINT"
	EnvHTTPEndpoint       = "MAESTRO_HTTP_ENDPOINT"
	EnvGRPCInsecure       = "MAESTRO_GRPC_INSECURE"
	EnvGRPCServerCAFile   = "MAESTRO_GRPC_SERVER_CA_FILE"
	EnvGRPCClientCertFile = "MAESTRO_GRPC_CLIENT_CERT"
	EnvGRPCClientKeyFile  = "MAESTRO_GRPC_CLIENT_KEY"
	EnvGRPCToken          = "MAESTRO_GRPC_TOKEN"      //nolint:gosec // This is an environment variable name, not a credential
	EnvGRPCTokenFile      = "MAESTRO_GRPC_TOKEN_FILE" //nolint:gosec // This is an environment variable name, not a credential
	EnvSourceID           = "MAESTRO_SOURCE_ID"
)

// Default values
const (
	DefaultGRPCEndpoint = "localhost:8090"
	DefaultHTTPEndpoint = "http://localhost:8000"
	DefaultSourceID     = "maestro-cli"
)

// NewRootCommand creates the root cobra command for maestro-cli
func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maestro-cli",
		Short: rootShortDescription,
		Long:  rootLongDescription,
		// Don't show usage on runtime errors (only on flag/arg errors)
		SilenceUsage: true,
		// Don't print errors automatically (we handle it in main.go)
		SilenceErrors: true,
		// Disable auto-completion by default
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

	// Add global flags
	addGlobalFlags(cmd)

	// Add subcommands
	cmd.AddCommand(
		NewApplyCommand(),
		NewGetCommand(),
		NewListCommand(),
		NewDeleteCommand(),
		NewWaitCommand(),
		NewWatchCommand(),
		NewDescribeCommand(),
		NewValidateCommand(),
		NewDiffCommand(),
		NewBuildCommand(),
		NewVersionCommand(),
	)

	return cmd
}

// addGlobalFlags adds global flags that apply to all commands
func addGlobalFlags(cmd *cobra.Command) {
	// Global connection flags
	cmd.PersistentFlags().String("grpc-endpoint", getEnvOrDefault(EnvGRPCEndpoint, DefaultGRPCEndpoint),
		"Maestro gRPC server endpoint (env: MAESTRO_GRPC_ENDPOINT)")
	cmd.PersistentFlags().String("http-endpoint", getEnvOrDefault(EnvHTTPEndpoint, DefaultHTTPEndpoint),
		"Maestro HTTP server endpoint (env: MAESTRO_HTTP_ENDPOINT)")

	// Global authentication flags
	cmd.PersistentFlags().Bool("grpc-insecure", getEnvBool(EnvGRPCInsecure),
		"Skip TLS verification for gRPC connection (env: MAESTRO_GRPC_INSECURE)")
	cmd.PersistentFlags().String("grpc-server-ca-file", os.Getenv(EnvGRPCServerCAFile),
		"Path to server CA certificate file (env: MAESTRO_GRPC_SERVER_CA_FILE)")
	cmd.PersistentFlags().String("grpc-client-cert-file", os.Getenv(EnvGRPCClientCertFile),
		"Path to client certificate file for mTLS (env: MAESTRO_GRPC_CLIENT_CERT)")
	cmd.PersistentFlags().String("grpc-client-key-file", os.Getenv(EnvGRPCClientKeyFile),
		"Path to client key file for mTLS (env: MAESTRO_GRPC_CLIENT_KEY)")
	cmd.PersistentFlags().String("grpc-broker-ca-file", "",
		"Path to broker CA certificate file")
	cmd.PersistentFlags().String("grpc-client-token", os.Getenv(EnvGRPCToken),
		"Bearer token for authentication (env: MAESTRO_GRPC_TOKEN)")
	cmd.PersistentFlags().String("grpc-client-token-file", os.Getenv(EnvGRPCTokenFile),
		"Path to file containing bearer token (env: MAESTRO_GRPC_TOKEN_FILE)")

	// Source ID for CloudEvents subscription
	cmd.PersistentFlags().String("source-id", getEnvOrDefault(EnvSourceID, DefaultSourceID),
		"Source ID for CloudEvents subscription (env: MAESTRO_SOURCE_ID)")

	// Global output flags
	cmd.PersistentFlags().String("results-path", "", "Path to write command results for status-reporter integration")
	cmd.PersistentFlags().String("output", "yaml", "Output format: yaml, json")

	// Global behavior flags
	cmd.PersistentFlags().Duration("timeout", 0, "Maximum time to wait for operation completion")
	cmd.PersistentFlags().Bool("verbose", false, "Enable verbose output")
}

// getEnvOrDefault returns the environment variable value or the default if not set
func getEnvOrDefault(envKey, defaultValue string) string {
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool returns true if the environment variable is set to "true", "1", or "yes"
// Accepts values with surrounding whitespace and different cases (e.g., " TRUE ", "True", " yes")
func getEnvBool(envKey string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(envKey)))
	return value == "true" || value == "1" || value == "yes"
}

// Helper functions to get flags from cobra command
func getStringFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func getBoolFlag(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func getDurationFlag(cmd *cobra.Command, name string) time.Duration {
	value, _ := cmd.Flags().GetDuration(name)
	return value
}
