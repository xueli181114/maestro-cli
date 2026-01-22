// Package cmd provides CLI commands for the maestro-cli application.
package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyperfleet/maestro-cli/internal/maestro"
	"github.com/hyperfleet/maestro-cli/internal/manifestwork"
	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

// ApplyFlags contains flags for the apply command
type ApplyFlags struct {
	ManifestFile string
	Consumer     string
	Wait         string // Condition to wait for (empty = no wait)
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

// NewApplyCommand creates the apply command
func NewApplyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a ManifestWork to a target cluster",
		Long: `Apply a ManifestWork resource to a target cluster via Maestro.
Creates a new ManifestWork or updates an existing one with the same name.

Examples:
  # Apply a ManifestWork (no wait)
  maestro-cli apply --manifest-file=nodepool.yaml --consumer=cluster-west-1

  # Apply and wait for Available condition (default, like kubectl wait)
  maestro-cli apply --manifest-file=nodepool.yaml --consumer=cluster-west-1 --wait

  # Apply and wait for specific condition
  maestro-cli apply --manifest-file=job.yaml --consumer=cluster-west-1 --wait="Job:Complete"

  # Apply and wait for complex condition
  maestro-cli apply --manifest-file=job.yaml --consumer=cluster-west-1 \
    --wait="Job:Complete OR Job:Failed"

  # Apply with timeout (default 5m if not specified)
  maestro-cli apply --manifest-file=nodepool.yaml --consumer=cluster-west-1 \
    --wait --timeout=10m --results-path=/shared/results.json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &ApplyFlags{
				ManifestFile:        getStringFlag(cmd, "manifest-file"),
				Consumer:            getStringFlag(cmd, "consumer"),
				Wait:                getStringFlag(cmd, "wait"),
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

			return runApplyCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("manifest-file", "", "Path to ManifestWork YAML/JSON file (required)")
	cmd.Flags().String("consumer", "", "Target cluster name (required)")
	cmd.Flags().String("wait", "", "Wait for condition before exit (e.g., 'Available', 'Job:Complete', 'Job:Complete OR Job:Failed')")
	cmd.Flags().Lookup("wait").NoOptDefVal = "Available" // Default when --wait is used without value

	// Mark required flags
	if err := cmd.MarkFlagRequired("manifest-file"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}

	return cmd
}

// runApplyCommand executes the apply command
func runApplyCommand(ctx context.Context, flags *ApplyFlags) error {
	// Setup context with timeout if specified
	if flags.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, flags.Timeout)
		defer cancel()
	}

	// Initialize logger with HyperFleet standards
	log := logger.New(logger.Config{
		Level:     getLogLevel(flags.Verbose),
		Format:    "text",
		Component: "maestro-cli",
		Version:   "dev",
	})

	// Load ManifestWork from file
	mw, err := manifestwork.LoadFromFile(flags.ManifestFile)
	if err != nil {
		log.Error(ctx, err, "Failed to load manifest file", logger.Fields{
			"manifest_file": flags.ManifestFile,
		})
		return fmt.Errorf("failed to load manifest file: %w", err)
	}

	// Add cluster context for logging
	ctx = logger.ContextWithClusterID(ctx, flags.Consumer)
	ctx = logger.ContextWithResource(ctx, "manifestwork", mw.Name)

	log.Info(ctx, "Loaded ManifestWork", logger.Fields{
		"manifest_name": mw.Name,
		"consumer":      flags.Consumer,
		"manifests":     len(mw.Spec.Workload.Manifests),
	})

	// Create Maestro client (passes context for proper signal handling)
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
		log.Error(ctx, err, "Failed to create Maestro client", logger.Fields{
			"grpc_endpoint": flags.GRPCEndpoint,
			"grpc_insecure": flags.GRPCInsecure,
		})
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

	// Apply ManifestWork
	applyResult, err := client.ApplyManifestWork(ctx, flags.Consumer, mw, log)
	if err != nil {
		if writeErr := manifestwork.WriteResult(flags.ResultsPath, manifestwork.StatusResult{
			Name:      mw.Name,
			Consumer:  flags.Consumer,
			Status:    "Failed",
			Message:   err.Error(),
			Timestamp: time.Now(),
		}); writeErr != nil {
			log.Warn(ctx, "Failed to write results file", logger.Fields{
				"results_path": flags.ResultsPath,
				"error":        writeErr.Error(),
			})
		}

		log.Error(ctx, err, "Failed to apply ManifestWork", logger.Fields{
			"manifest_name": mw.Name,
			"consumer":      flags.Consumer,
		})
		return fmt.Errorf("failed to apply ManifestWork: %w", err)
	}

	log.Info(ctx, "ManifestWork applied successfully", logger.Fields{
		"manifest_name":    applyResult.Name,
		"resource_version": applyResult.ResourceVersion,
		"generation":       applyResult.Generation,
	})

	// Write initial success result
	if writeErr := manifestwork.WriteResult(flags.ResultsPath, manifestwork.StatusResult{
		Name:      mw.Name,
		Consumer:  flags.Consumer,
		Status:    "Applied",
		Message:   "ManifestWork applied successfully",
		Timestamp: time.Now(),
	}); writeErr != nil {
		log.Warn(ctx, "Failed to write results file", logger.Fields{
			"results_path": flags.ResultsPath,
			"error":        writeErr.Error(),
		})
		return fmt.Errorf("failed to write results file: %w", writeErr)
	}

	// Wait for condition if requested (using HTTP polling, like kubectl wait)
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
				result := manifestwork.BuildStatusResult(mw.Name, flags.Consumer, status, message, details)
				return manifestwork.WriteResult(flags.ResultsPath, result)
			}
		}

		// Poll every 2 seconds by default
		if err := client.WaitForCondition(waitCtx, flags.Consumer, mw.Name, flags.Wait, maestro.DefaultPollInterval, log, callback); err != nil {
			return err
		}
	}

	return nil
}

// getLogLevel determines the log level based on verbose flag
func getLogLevel(verbose bool) string {
	if verbose {
		return "debug"
	}
	return "info"
}
