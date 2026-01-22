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

// ListFlags contains flags for the list command
type ListFlags struct {
	Consumer string
	Filter   string // Filter by manifest content (kind, name, or kind/name)
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
	SourceID            string
	ResultsPath         string
	Output              string
	Timeout             time.Duration
	Verbose             bool
}

// NewListCommand creates the list command
func NewListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List ManifestWorks for a target cluster",
		Long: `List ManifestWork resources for a specific target cluster using the HTTP API.

This command uses the Maestro HTTP API to list resource bundles (ManifestWorks)
without requiring a gRPC connection.

Note: Maestro generates UUIDs for ManifestWork names. Use --filter to find
ManifestWorks by the resources they contain.

Examples:
  # List all ManifestWorks for a cluster
  maestro-cli list --consumer=cluster-west-1

  # Find ManifestWork containing a specific resource
  maestro-cli list --consumer=cluster-west-1 --filter=hyperfleet-system
  maestro-cli list --consumer=cluster-west-1 --filter=Namespace/hyperfleet
  maestro-cli list --consumer=cluster-west-1 --filter=Deployment/nginx

  # List with JSON output
  maestro-cli list --consumer=cluster-west-1 --output=json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &ListFlags{
				Consumer: getStringFlag(cmd, "consumer"),
				Filter:   getStringFlag(cmd, "filter"),
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
				SourceID:            getStringFlag(cmd, "source-id"),
				ResultsPath:         getStringFlag(cmd, "results-path"),
				Output:              getStringFlag(cmd, "output"),
				Timeout:             getDurationFlag(cmd, "timeout"),
				Verbose:             getBoolFlag(cmd, "verbose"),
			}

			return runListCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("consumer", "", "Target cluster name (required)")
	cmd.Flags().String("filter", "", "Filter by manifest content (e.g., 'nginx', 'Namespace/hyperfleet', 'Deployment/default/nginx')")

	// Mark required flags
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}

	return cmd
}

// runListCommand executes the list command using HTTP API
func runListCommand(ctx context.Context, flags *ListFlags) error {
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

	// Create HTTP-only client (no gRPC subscription needed for list)
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

	// List ManifestWorks using HTTP API (reads directly from database)
	log.Debug(ctx, "Listing ManifestWorks via HTTP API", logger.Fields{
		"consumer":      flags.Consumer,
		"http_endpoint": flags.HTTPEndpoint,
		"filter":        flags.Filter,
	})

	works, err := client.ListManifestWorksHTTP(ctx, flags.Consumer)
	if err != nil {
		return fmt.Errorf("failed to list ManifestWorks: %w", err)
	}

	// Apply filter if specified
	if flags.Filter != "" {
		works = filterResourceBundles(works, flags.Filter)
		log.Debug(ctx, "Filtered results", logger.Fields{
			"filter":  flags.Filter,
			"matched": len(works),
		})
	}

	// Output based on format
	switch strings.ToLower(flags.Output) {
	case "json":
		return outputResourceBundlesJSON(works)
	case "yaml":
		return outputResourceBundlesYAML(works)
	default:
		outputResourceBundlesTable(works, flags.Consumer, flags.Filter)
		return nil
	}
}

// filterResourceBundles filters ResourceBundleSummary by manifest content
// Supports patterns like:
//   - "nginx"                     - matches any manifest containing "nginx" in name
//   - "Namespace"                 - matches manifests of kind "Namespace"
//   - "Namespace/hyperfleet"      - matches Namespace with name containing "hyperfleet"
//   - "Deployment/default/nginx"  - matches Deployment in namespace "default" with name containing "nginx"
func filterResourceBundles(items []maestro.ResourceBundleSummary, filter string) []maestro.ResourceBundleSummary {
	if filter == "" {
		return items
	}

	// Parse filter pattern
	parts := strings.Split(filter, "/")
	var filterKind, filterNamespace, filterName string

	switch len(parts) {
	case 1:
		filterName = strings.ToLower(parts[0])
	case 2:
		filterKind = parts[0]
		filterName = strings.ToLower(parts[1])
	case 3:
		filterKind = parts[0]
		filterNamespace = strings.ToLower(parts[1])
		filterName = strings.ToLower(parts[2])
	default:
		filterKind = parts[0]
		filterNamespace = strings.ToLower(parts[1])
		filterName = strings.ToLower(strings.Join(parts[2:], "/"))
	}

	var filtered []maestro.ResourceBundleSummary
	for _, rb := range items {
		if matchesResourceBundleFilter(rb, filterKind, filterNamespace, filterName) {
			filtered = append(filtered, rb)
		}
	}
	return filtered
}

// matchesResourceBundleFilter checks if a ResourceBundleSummary matches the filter criteria
func matchesResourceBundleFilter(rb maestro.ResourceBundleSummary, filterKind, filterNamespace, filterName string) bool {
	// Check if filter matches the work name itself
	if filterName != "" && strings.Contains(strings.ToLower(rb.Name), filterName) {
		return true
	}

	// Check manifest contents
	for _, info := range rb.Manifests {
		if filterKind != "" && !strings.EqualFold(info.Kind, filterKind) {
			continue
		}
		if filterNamespace != "" && !strings.Contains(strings.ToLower(info.Namespace), filterNamespace) {
			continue
		}
		if filterName != "" {
			if strings.Contains(strings.ToLower(info.Name), filterName) {
				return true
			}
			continue
		}
		if filterKind != "" {
			return true
		}
	}
	return false
}

// outputResourceBundlesTable outputs ResourceBundleSummary in table format with details
func outputResourceBundlesTable(items []maestro.ResourceBundleSummary, consumer, filter string) {
	if len(items) == 0 {
		if filter != "" {
			fmt.Printf("No ManifestWorks matching '%s' found for consumer %s\n", filter, consumer)
		} else {
			fmt.Printf("No ManifestWorks found for consumer %s\n", consumer)
		}
		return
	}

	for i, rb := range items {
		if i > 0 {
			fmt.Println()
		}

		// Print ManifestWork header
		fmt.Printf("ManifestWork: %s\n", rb.Name)
		fmt.Printf("  ID:        %s\n", rb.ID)
		fmt.Printf("  Version:   %d\n", rb.Version)
		fmt.Printf("  Created:   %s\n", rb.CreatedAt)
		fmt.Printf("  Updated:   %s\n", rb.UpdatedAt)

		// Print manifests
		fmt.Printf("  Manifests (%d):\n", rb.ManifestCount)
		for _, info := range rb.Manifests {
			fmt.Printf("    - %s\n", info.String())
		}

		// Print conditions
		if len(rb.Conditions) > 0 {
			fmt.Printf("  Conditions:\n")
			for _, cond := range rb.Conditions {
				fmt.Printf("    - %s: %s\n", cond.Type, cond.Status)
			}
		}
	}

	fmt.Printf("\n─────────────────────────────────────────\n")
	fmt.Printf("Total: %d ManifestWork(s) for consumer %s\n", len(items), consumer)
}

// outputResourceBundlesJSON outputs ResourceBundleSummary in JSON format
func outputResourceBundlesJSON(items []maestro.ResourceBundleSummary) error {
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// outputResourceBundlesYAML outputs ResourceBundleSummary in YAML format
func outputResourceBundlesYAML(items []maestro.ResourceBundleSummary) error {
	data, err := yaml.Marshal(items)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML: %w", err)
	}
	fmt.Println(string(data))
	return nil
}
