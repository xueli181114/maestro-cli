package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	workv1 "open-cluster-management.io/api/work/v1"

	"github.com/hyperfleet/maestro-cli/internal/maestro"
	"github.com/hyperfleet/maestro-cli/internal/manifestwork"
	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

const (
	defaultOutputFormat = "json"
)

// BuildFlags contains flags for the build command
type BuildFlags struct {
	Name       string
	Consumer   string
	SourceFile string
	OutputFile string
	Strategy   string
	Apply      bool
	Wait       string // Condition to wait for (empty = no wait)
	DryRun     bool
	Force      bool
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

// NewBuildCommand creates the build command
// NOTE: This command is currently disabled (hidden) because Maestro does not support
// changing the number of manifests in a ManifestWork via patch/update.
// The code is preserved for future research and potential re-enablement.
func NewBuildCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "build",
		Short:  "Build ManifestWork by merging configuration with remote state",
		Hidden: true, // Disabled: Maestro doesn't support manifest count changes
		Long: `Build a ManifestWork by fetching existing state from Maestro and merging with
local configuration. Useful for updating specific resources while preserving others.

NOTE: This command is currently disabled because Maestro does not support changing
the number of manifests in a ManifestWork. Use delete + apply workflow instead.

The source file can be:
  - A full ManifestWork (YAML or JSON)
  - Just the spec portion
  - Just an array of manifests
  - A workload object with manifests

Merge strategies:
  - merge (default): Add new manifests and update existing ones (by kind+name)
  - replace: Replace the entire spec with source

Examples:
  # Build ManifestWork and output to file
  maestro-cli build --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1 \
    --source-file=nodepool-config.json --output-file=complete-manifestwork.yaml

  # Build and output to stdout as JSON
  maestro-cli build --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1 \
    --source-file=nodepool-config.json --output=json

  # Build and apply directly with wait
  maestro-cli build --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1 \
    --source-file=nodepool-config.json --apply --wait

  # Force replace strategy (replace entire spec)
  maestro-cli build --name=hyperfleet-cluster-west-1-nodepool --consumer=cluster-west-1 \
    --source-file=nodepool-config.json --strategy=replace --apply

  # Build from non-existent (create new from source)
  maestro-cli build --name=new-manifestwork --consumer=cluster-west-1 \
    --source-file=full-manifestwork.yaml --force --apply`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &BuildFlags{
				Name:       getStringFlag(cmd, "name"),
				Consumer:   getStringFlag(cmd, "consumer"),
				SourceFile: getStringFlag(cmd, "source-file"),
				OutputFile: getStringFlag(cmd, "output-file"),
				Strategy:   getStringFlag(cmd, "strategy"),
				Apply:      getBoolFlag(cmd, "apply"),
				Wait:       getStringFlag(cmd, "wait"),
				DryRun:     getBoolFlag(cmd, "dry-run"),
				Force:      getBoolFlag(cmd, "force"),
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

			return runBuildCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("name", "", "ManifestWork name (required)")
	cmd.Flags().String("consumer", "", "Target cluster name (required)")
	cmd.Flags().String("source-file", "", "Path to source configuration file - YAML or JSON (required)")
	cmd.Flags().String("output-file", "", "Output file path (default: stdout)")
	cmd.Flags().String("strategy", "merge", "Merge strategy: merge or replace")
	cmd.Flags().Bool("apply", false, "Apply the built ManifestWork after building")
	cmd.Flags().String("wait", "", "Wait for condition after applying (e.g., 'Available', 'Job:Complete') - requires --apply")
	cmd.Flags().Lookup("wait").NoOptDefVal = "Available" // Default when --wait is used without value
	cmd.Flags().Bool("dry-run", false, "Show what would be built without making changes")
	cmd.Flags().Bool("force", false, "Create new ManifestWork if it doesn't exist")

	// Mark required flags
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("source-file"); err != nil {
		panic(err)
	}

	return cmd
}

// runBuildCommand executes the build command
func runBuildCommand(ctx context.Context, flags *BuildFlags) error {
	// Setup context with timeout if specified
	if flags.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, flags.Timeout)
		defer cancel()
	}

	// Initialize logger
	log := logger.New(logger.Config{
		Level:     getLogLevel(flags.Verbose),
		Format:    "text",
		Component: "maestro-cli",
		Version:   "dev",
	})

	// Add context for logging
	ctx = logger.ContextWithClusterID(ctx, flags.Consumer)
	ctx = logger.ContextWithResource(ctx, "manifestwork", flags.Name)

	// Load source file
	log.Info(ctx, "Loading source file", logger.Fields{
		"source_file": flags.SourceFile,
	})

	source, err := manifestwork.LoadSourceFile(flags.SourceFile)
	if err != nil {
		log.Error(ctx, err, "Failed to load source file", logger.Fields{
			"source_file": flags.SourceFile,
		})
		return fmt.Errorf("failed to load source file: %w", err)
	}

	// Create gRPC client for ManifestWork operations (passes context for proper signal handling)
	client, err := maestro.NewClient(ctx, maestro.ClientConfig{
		GRPCEndpoint:        flags.GRPCEndpoint,
		HTTPEndpoint:        flags.HTTPEndpoint,
		GRPCInsecure:        flags.GRPCInsecure,
		GRPCServerCAFile:    flags.GRPCServerCAFile,
		GRPCBrokerCAFile:    flags.GRPCBrokerCAFile,
		GRPCClientCertFile:  flags.GRPCClientCertFile,
		GRPCClientKeyFile:   flags.GRPCClientKeyFile,
		GRPCClientToken:     flags.GRPCClientToken,
		GRPCClientTokenFile: flags.GRPCClientTokenFile,
		SourceID:            flags.SourceID,
	})
	if err != nil {
		log.Error(ctx, err, "Failed to create Maestro client", nil)
		return fmt.Errorf("failed to create Maestro client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Warn(ctx, "Failed to close client", logger.Fields{"error": err.Error()})
		}
	}()

	// Validate consumer exists
	if err := client.ValidateConsumer(ctx, flags.Consumer); err != nil {
		log.Error(ctx, err, "Consumer validation failed", logger.Fields{
			"consumer": flags.Consumer,
		})
		return err
	}

	// Fetch existing ManifestWork via gRPC
	log.Info(ctx, "Fetching existing ManifestWork via gRPC", logger.Fields{
		"name":     flags.Name,
		"consumer": flags.Consumer,
	})

	existing, err := client.GetManifestWork(ctx, flags.Consumer, flags.Name)
	if err != nil {
		if errors.IsNotFound(err) || strings.Contains(err.Error(), "not found") {
			if !flags.Force {
				log.Error(ctx, err, "ManifestWork not found (use --force to create new)", logger.Fields{
					"name":     flags.Name,
					"consumer": flags.Consumer,
				})
				return fmt.Errorf("ManifestWork %s not found in consumer %s (use --force to create new)", flags.Name, flags.Consumer)
			}

			// Create new ManifestWork from source
			log.Info(ctx, "ManifestWork not found, creating new from source", logger.Fields{
				"name":     flags.Name,
				"consumer": flags.Consumer,
			})

			existing = source.ToManifestWork(flags.Name)
			existing.Namespace = flags.Consumer
		} else {
			log.Error(ctx, err, "Failed to fetch ManifestWork", logger.Fields{
				"name":     flags.Name,
				"consumer": flags.Consumer,
			})
			return fmt.Errorf("failed to fetch ManifestWork: %w", err)
		}
	} else {
		// Merge with existing ManifestWork
		log.Info(ctx, "Merging with existing ManifestWork", logger.Fields{
			"name":               flags.Name,
			"consumer":           flags.Consumer,
			"strategy":           flags.Strategy,
			"existing_manifests": len(existing.Spec.Workload.Manifests),
			"source_manifests":   len(source.GetManifests()),
		})

		existing, err = manifestwork.MergeManifestWorks(existing, source, flags.Strategy)
		if err != nil {
			log.Error(ctx, err, "Failed to merge ManifestWorks", logger.Fields{
				"strategy": flags.Strategy,
			})
			return fmt.Errorf("failed to merge ManifestWorks: %w", err)
		}
	}

	log.Info(ctx, "Build complete", logger.Fields{
		"name":      existing.Name,
		"manifests": len(existing.Spec.Workload.Manifests),
		"strategy":  flags.Strategy,
	})

	// Early guard: cannot use --wait without --apply
	if flags.Wait != "" && !flags.Apply {
		return fmt.Errorf("cannot use --wait without --apply")
	}

	// Dry run - just show what would happen
	if flags.DryRun {
		log.Info(ctx, "Dry run - showing built ManifestWork", nil)
		return outputManifestWork(existing, flags.OutputFile, flags.Output)
	}

	// Output to file or stdout (if not applying)
	if !flags.Apply {
		if flags.Wait != "" {
			return fmt.Errorf("cannot use --wait when not using --apply")
		}
		return outputManifestWork(existing, flags.OutputFile, flags.Output)
	}

	// Apply the built ManifestWork
	log.Info(ctx, "Applying built ManifestWork", logger.Fields{
		"name":     existing.Name,
		"consumer": flags.Consumer,
	})

	result, err := client.ApplyManifestWork(ctx, flags.Consumer, existing, log)
	if err != nil {
		writeErr := manifestwork.WriteResult(flags.ResultsPath, manifestwork.StatusResult{
			Name:      existing.Name,
			Consumer:  flags.Consumer,
			Status:    "Failed",
			Message:   err.Error(),
			Timestamp: time.Now(),
		})
		if writeErr != nil {
			log.Error(ctx, writeErr, "Failed to write results file", logger.Fields{
				"path": flags.ResultsPath,
			})
			return fmt.Errorf("failed to apply ManifestWork: %w; also failed to write results: %v", err, writeErr)
		}

		log.Error(ctx, err, "Failed to apply ManifestWork", logger.Fields{
			"name":     existing.Name,
			"consumer": flags.Consumer,
		})
		return fmt.Errorf("failed to apply ManifestWork: %w", err)
	}

	log.Info(ctx, "ManifestWork applied successfully", logger.Fields{
		"name":             result.Name,
		"resource_version": result.ResourceVersion,
		"generation":       result.Generation,
	})

	if err := manifestwork.WriteResult(flags.ResultsPath, manifestwork.StatusResult{
		Name:      existing.Name,
		Consumer:  flags.Consumer,
		Status:    "Applied",
		Message:   "ManifestWork built and applied successfully",
		Timestamp: time.Now(),
	}); err != nil {
		log.Error(ctx, err, "Failed to write results file", logger.Fields{
			"path": flags.ResultsPath,
		})
		return fmt.Errorf("ManifestWork applied successfully but failed to write results: %w", err)
	}

	// Wait for condition if requested
	if flags.Wait != "" {
		// Use timeout if specified, otherwise default to 5 minutes
		waitTimeout := flags.Timeout
		if waitTimeout == 0 {
			waitTimeout = 5 * time.Minute
		}

		log.Info(ctx, "Waiting for condition", logger.Fields{
			"condition": flags.Wait,
			"timeout":   waitTimeout.String(),
		})

		// Create wait context with timeout
		waitCtx, waitCancel := context.WithTimeout(ctx, waitTimeout)
		defer waitCancel()

		// Create callback to update results file on each poll
		var callback maestro.WaitCallback
		if flags.ResultsPath != "" {
			callback = func(details *maestro.ManifestWorkDetails, conditionMet bool) error {
				status := "Waiting"
				message := fmt.Sprintf("Waiting for condition '%s'", flags.Wait)
				if conditionMet {
					status = flags.Wait
					message = fmt.Sprintf("Condition '%s' met", flags.Wait)
				}
				result := manifestwork.BuildStatusResult(existing.Name, flags.Consumer, status, message, details)
				return manifestwork.WriteResult(flags.ResultsPath, result)
			}
		}

		if err := client.WaitForCondition(waitCtx, flags.Consumer, existing.Name, flags.Wait, maestro.DefaultPollInterval, log, callback); err != nil {
			return err
		}
	}

	return nil
}

// outputManifestWork outputs the ManifestWork to file or stdout
func outputManifestWork(mw *workv1.ManifestWork, outputFile, format string) error {
	var data []byte
	var err error

	// Determine format
	if outputFile != "" {
		ext := strings.ToLower(filepath.Ext(outputFile))
		if ext == ".json" {
			format = defaultOutputFormat
		}
	}

	if format == "json" {
		data, err = manifestwork.ToJSON(mw)
	} else {
		data, err = manifestwork.ToYAML(mw)
	}

	if err != nil {
		return fmt.Errorf("failed to marshal ManifestWork: %w", err)
	}

	// Output to file or stdout
	if outputFile != "" {
		if err := os.WriteFile(outputFile, data, 0600); err != nil {
			return fmt.Errorf("failed to write to %s: %w", outputFile, err)
		}
		fmt.Printf("ManifestWork written to %s\n", outputFile)
	} else {
		fmt.Println(string(data))
	}

	return nil
}
