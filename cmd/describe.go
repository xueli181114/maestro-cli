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

// DescribeFlags contains flags for the describe command
type DescribeFlags struct {
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

// NewDescribeCommand creates the describe command
func NewDescribeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe",
		Short: "Describe a ManifestWork with detailed information",
		Long: `Show detailed information about a ManifestWork including status, conditions, and resource details.

Examples:
  # Describe a ManifestWork
  maestro-cli describe --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1

  # Describe with JSON output
  maestro-cli describe --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1 --output=json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &DescribeFlags{
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

			return runDescribeCommand(cmd.Context(), flags)
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

// runDescribeCommand executes the describe command
func runDescribeCommand(ctx context.Context, flags *DescribeFlags) error {
	// Set up context with timeout
	if flags.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, flags.Timeout)
		defer cancel()
	}

	// Initialize logger
	logLevel := "info"
	if flags.Verbose {
		logLevel = "debug"
	}
	log := logger.New(logger.Config{
		Level:  logLevel,
		Format: "text",
	})

	// Create HTTP-only client (no gRPC needed for describe)
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

	log.Debug(ctx, "Fetching ManifestWork details", logger.Fields{
		"name":     flags.Name,
		"consumer": flags.Consumer,
	})

	// Get the full ManifestWork details
	details, err := client.GetManifestWorkDetailsHTTP(ctx, flags.Consumer, flags.Name)
	if err != nil {
		return err
	}

	// Output based on format
	switch strings.ToLower(flags.Output) {
	case "json":
		return outputDescribeJSON(details)
	case "yaml":
		return outputDescribeYAML(details)
	default:
		outputDescribeHuman(details)
		return nil
	}
}

// outputDescribeJSON outputs ManifestWork details in JSON format
func outputDescribeJSON(details *maestro.ManifestWorkDetails) error {
	data, err := json.MarshalIndent(details, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// outputDescribeYAML outputs ManifestWork details in YAML format
func outputDescribeYAML(details *maestro.ManifestWorkDetails) error {
	data, err := yaml.Marshal(details)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// outputDescribeHuman outputs ManifestWork details in human-readable format
func outputDescribeHuman(details *maestro.ManifestWorkDetails) {
	fmt.Printf("Name:         %s\n", details.Name)
	fmt.Printf("ID:           %s\n", details.ID)
	fmt.Printf("Consumer:     %s\n", details.ConsumerName)
	fmt.Printf("Version:      %d\n", details.Version)
	fmt.Printf("Created:      %s\n", details.CreatedAt)
	fmt.Printf("Updated:      %s\n", details.UpdatedAt)

	// Conditions
	fmt.Printf("\nConditions:\n")
	if len(details.Conditions) == 0 {
		fmt.Printf("  (none)\n")
	} else {
		for _, cond := range details.Conditions {
			fmt.Printf("  %s:\n", cond.Type)
			fmt.Printf("    Status:  %s\n", cond.Status)
			if cond.Reason != "" {
				fmt.Printf("    Reason:  %s\n", cond.Reason)
			}
			if cond.Message != "" {
				fmt.Printf("    Message: %s\n", cond.Message)
			}
			if cond.LastTransitionTime != "" {
				fmt.Printf("    LastTransitionTime: %s\n", cond.LastTransitionTime)
			}
		}
	}

	// Manifests
	fmt.Printf("\nManifests (%d):\n", len(details.Manifests))
	for i, m := range details.Manifests {
		fmt.Printf("  [%d] %s\n", i, m.String())
	}

	// Resource Status
	if len(details.ResourceStatus) > 0 {
		fmt.Printf("\nResource Status:\n")
		for _, rs := range details.ResourceStatus {
			fmt.Printf("  %s/%s:\n", rs.Kind, rs.Name)
			if rs.Namespace != "" {
				fmt.Printf("    Namespace: %s\n", rs.Namespace)
			}
			for _, cond := range rs.Conditions {
				fmt.Printf("    %s: %s\n", cond.Type, cond.Status)
			}
			if len(rs.StatusFeedback) > 0 {
				fmt.Printf("    Feedback:\n")
				for k, v := range rs.StatusFeedback {
					fmt.Printf("      %s: %v\n", k, v)
				}
			}
		}
	}

	// Delete Option
	if details.DeleteOption != "" {
		fmt.Printf("\nDelete Option: %s\n", details.DeleteOption)
	}
}
