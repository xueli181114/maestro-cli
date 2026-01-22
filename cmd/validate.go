package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyperfleet/maestro-cli/internal/manifestwork"
	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

const (
	logLevelInfo  = "info"
	logLevelDebug = "debug"
)

// ValidateFlags contains flags for the validate command
type ValidateFlags struct {
	ManifestFile string
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

// NewValidateCommand creates the validate command
func NewValidateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a ManifestWork file",
		Long: `Validate a ManifestWork YAML/JSON file for correctness before applying.

Validates:
  - File can be parsed as valid YAML/JSON
  - Required fields are present (apiVersion, kind, metadata.name)
  - Manifests array is not empty
  - Each manifest has required fields (apiVersion, kind, metadata.name)

Examples:
  # Validate a ManifestWork file
  maestro-cli validate --manifest-file=job-manifestwork.json

  # Validate with verbose output
  maestro-cli validate --manifest-file=job-manifestwork.yaml --verbose`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &ValidateFlags{
				ManifestFile: getStringFlag(cmd, "manifest-file"),
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

			return runValidateCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("manifest-file", "", "Path to ManifestWork YAML/JSON file (required)")

	// Mark required flags
	if err := cmd.MarkFlagRequired("manifest-file"); err != nil {
		panic(err)
	}

	return cmd
}

// runValidateCommand executes the validate command
func runValidateCommand(ctx context.Context, flags *ValidateFlags) error {
	// Initialize logger
	logLevel := logLevelInfo
	if flags.Verbose {
		logLevel = logLevelDebug
	}
	log := logger.New(logger.Config{
		Level:  logLevel,
		Format: "text",
	})

	log.Info(ctx, "Validating ManifestWork file", logger.Fields{
		"manifest_file": flags.ManifestFile,
	})

	// Load and parse the ManifestWork file
	mw, err := manifestwork.LoadManifestWorkFromFile(flags.ManifestFile)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Validate required fields
	var errors []string

	// Check metadata.name
	if mw.Name == "" {
		errors = append(errors, "metadata.name is required")
	}

	// Check manifests
	if len(mw.Spec.Workload.Manifests) == 0 {
		errors = append(errors, "spec.workload.manifests cannot be empty")
	}

	// Check top-level type metadata
	if mw.APIVersion == "" {
		errors = append(errors, "apiVersion is required")
	}
	if mw.Kind == "" {
		errors = append(errors, "kind is required")
	}

	// Validate each manifest
	for i, manifest := range mw.Spec.Workload.Manifests {
		manifestData := manifest.Raw
		if len(manifestData) == 0 {
			errors = append(errors, fmt.Sprintf("manifest[%d] is empty", i))
			continue
		}

		// Try to extract basic fields
		var m map[string]interface{}
		if err := manifestwork.UnmarshalManifest(manifestData, &m); err != nil {
			errors = append(errors, fmt.Sprintf("manifest[%d] is not valid YAML/JSON: %v", i, err))
			continue
		}

		if _, ok := m["apiVersion"]; !ok {
			errors = append(errors, fmt.Sprintf("manifest[%d] missing apiVersion", i))
		}
		if _, ok := m["kind"]; !ok {
			errors = append(errors, fmt.Sprintf("manifest[%d] missing kind", i))
		}
		if metadata, ok := m["metadata"].(map[string]interface{}); ok {
			if _, ok := metadata["name"]; !ok {
				errors = append(errors, fmt.Sprintf("manifest[%d] missing metadata.name", i))
			}
		} else {
			errors = append(errors, fmt.Sprintf("manifest[%d] missing metadata", i))
		}
	}

	if len(errors) > 0 {
		fmt.Println("Validation FAILED:")
		for _, e := range errors {
			fmt.Printf("  - %s\n", e)
		}
		return fmt.Errorf("validation failed with %d error(s)", len(errors))
	}

	// Print success
	fmt.Printf("Validation PASSED\n")
	fmt.Printf("  Name: %s\n", mw.Name)
	fmt.Printf("  Manifests: %d\n", len(mw.Spec.Workload.Manifests))

	if flags.Verbose {
		fmt.Printf("\nManifests:\n")
		for i, manifest := range mw.Spec.Workload.Manifests {
			var m map[string]interface{}
			if err := manifestwork.UnmarshalManifest(manifest.Raw, &m); err == nil {
				kind, _ := m["kind"].(string)
				metadata, _ := m["metadata"].(map[string]interface{})
				name, _ := metadata["name"].(string)
				ns, _ := metadata["namespace"].(string)
				if ns != "" {
					fmt.Printf("  [%d] %s/%s/%s\n", i, kind, ns, name)
				} else {
					fmt.Printf("  [%d] %s/%s\n", i, kind, name)
				}
			}
		}

		// Show manifestConfigs if present
		if len(mw.Spec.ManifestConfigs) > 0 {
			fmt.Printf("\nManifestConfigs: %d\n", len(mw.Spec.ManifestConfigs))
			for i, cfg := range mw.Spec.ManifestConfigs {
				fmt.Printf("  [%d] %s/%s\n", i, cfg.ResourceIdentifier.Resource, cfg.ResourceIdentifier.Name)
			}
		}
	}

	return nil
}
