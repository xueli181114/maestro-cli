package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/hyperfleet/maestro-cli/internal/maestro"
	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

// GetFlags contains flags for the get command
type GetFlags struct {
	Name     string
	Consumer string
	// Global flags
	GRPCEndpoint        string
	HTTPEndpoint        string
	GRPCInsecure        bool
	GRPCServerCAFile    string
	GRPCClientCertFile  string
	GRPCClientKeyFile   string
	GRPCBrokerCAFile    string
	GRPCClientToken     string
	GRPCClientTokenFile string
	ResultsPath         string
	Output              string
	Timeout             time.Duration
	Verbose             bool
}

// NewGetCommand creates the get command
func NewGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get a ManifestWork from Maestro",
		Long: `Get the ManifestWork resource from Maestro and display its spec.

Examples:
  # Get ManifestWork in YAML format (default)
  maestro-cli get --name=hyperfleet-cluster-west-1-job --consumer=agent1

  # Get with JSON output
  maestro-cli get --name=hyperfleet-cluster-west-1-job --consumer=agent1 --output=json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &GetFlags{
				Name:     getStringFlag(cmd, "name"),
				Consumer: getStringFlag(cmd, "consumer"),
				// Global flags
				GRPCEndpoint:        getStringFlag(cmd, "grpc-endpoint"),
				HTTPEndpoint:        getStringFlag(cmd, "http-endpoint"),
				GRPCInsecure:        getBoolFlag(cmd, "grpc-insecure"),
				GRPCServerCAFile:    getStringFlag(cmd, "grpc-server-ca-file"),
				GRPCClientCertFile:  getStringFlag(cmd, "grpc-client-cert-file"),
				GRPCClientKeyFile:   getStringFlag(cmd, "grpc-client-key-file"),
				GRPCBrokerCAFile:    getStringFlag(cmd, "grpc-broker-ca-file"),
				GRPCClientToken:     getStringFlag(cmd, "grpc-client-token"),
				GRPCClientTokenFile: getStringFlag(cmd, "grpc-client-token-file"),
				ResultsPath:         getStringFlag(cmd, "results-path"),
				Output:              getStringFlag(cmd, "output"),
				Timeout:             getDurationFlag(cmd, "timeout"),
				Verbose:             getBoolFlag(cmd, "verbose"),
			}

			return runGetCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("name", "", "ManifestWork name (required)")
	cmd.Flags().String("consumer", "", "Target cluster name (required)")

	// Mark required flags
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}

	return cmd
}

// runGetCommand executes the get command
func runGetCommand(ctx context.Context, flags *GetFlags) error {
	// Initialize logger
	logLevel := "info"
	if flags.Verbose {
		logLevel = "debug"
	}
	log := logger.New(logger.Config{
		Level:  logLevel,
		Format: "text",
	})

	// Create HTTP-only client (no gRPC needed for get)
	client, err := maestro.NewHTTPClient(maestro.ClientConfig{
		HTTPEndpoint: flags.HTTPEndpoint,
		GRPCInsecure: flags.GRPCInsecure,
	})
	if err != nil {
		return fmt.Errorf("failed to create Maestro client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Warn(ctx, "Failed to close client", logger.Fields{"error": err.Error()})
		}
	}()

	// Validate consumer exists
	if err := client.ValidateConsumer(ctx, flags.Consumer); err != nil {
		return err
	}

	log.Debug(ctx, "Fetching ManifestWork", logger.Fields{
		"name":     flags.Name,
		"consumer": flags.Consumer,
	})

	// Get the ManifestWork
	rb, err := client.GetResourceBundleFullHTTP(ctx, flags.Consumer, flags.Name)
	if err != nil {
		return err
	}

	// Output based on format
	switch strings.ToLower(flags.Output) {
	case "json":
		data, err := json.MarshalIndent(rb, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(data))
	default: // yaml
		data, err := yaml.Marshal(rb)
		if err != nil {
			return fmt.Errorf("failed to marshal YAML: %w", err)
		}
		fmt.Println(string(data))
	}

	return nil
}
