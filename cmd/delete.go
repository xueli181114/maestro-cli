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

// DeleteFlags contains flags for the delete command
type DeleteFlags struct {
	Name     string // Original ManifestWork name (metadata.name)
	Consumer string
	Wait     bool // Wait for deletion completion
	DryRun   bool
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

// NewDeleteCommand creates the delete command
func NewDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a ManifestWork from Maestro",
		Long: `Delete a ManifestWork resource from Maestro.

Use --name with the original metadata.name from your ManifestWork file.
Use 'maestro-cli list' to see available ManifestWorks and their names.

Note: Maestro does not support removing individual manifests from a ManifestWork.
To remove specific manifests, delete the entire ManifestWork and re-apply with
the updated manifest file.

Examples:
  # Delete a ManifestWork by its original name
  maestro-cli delete --name=hyperfleet-cluster-west-1-namespace --consumer=cluster-west-1

  # Delete and wait for completion (like kubectl wait --for=delete)
  maestro-cli delete --name=my-manifestwork --consumer=cluster-west-1 --wait

  # Dry run to see what would be deleted
  maestro-cli delete --name=nginx-work --consumer=cluster-west-1 --dry-run`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &DeleteFlags{
				Name:     getStringFlag(cmd, "name"),
				Consumer: getStringFlag(cmd, "consumer"),
				Wait:     getBoolFlag(cmd, "wait"),
				DryRun:   getBoolFlag(cmd, "dry-run"),
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

			return runDeleteCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("name", "", "ManifestWork name (original metadata.name from your ManifestWork file)")
	cmd.Flags().String("consumer", "", "Target cluster name (required)")
	cmd.Flags().Bool("wait", false, "Wait for deletion completion (like kubectl wait --for=delete)")
	cmd.Flags().Bool("dry-run", false, "Show what would be deleted without making changes")

	// Mark required flags
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}

	return cmd
}

// runDeleteCommand executes the delete command
func runDeleteCommand(ctx context.Context, flags *DeleteFlags) error {
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

	// Create HTTP-only client (no gRPC needed for delete)
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

	// Handle ManifestWork deletion
	return deleteManifestWork(ctx, client, flags, log)
}

// deleteManifestWork deletes an entire ManifestWork
func deleteManifestWork(ctx context.Context, client *maestro.Client, flags *DeleteFlags, log *logger.Logger) error {
	// Check if the ManifestWork exists using HTTP API (doesn't require gRPC subscription)
	work, err := client.GetManifestWorkByNameHTTP(ctx, flags.Consumer, flags.Name)
	if err != nil {
		// ManifestWork doesn't exist - just warn and exit successfully
		log.Warn(ctx, "ManifestWork not found, nothing to delete", logger.Fields{
			"name":     flags.Name,
			"consumer": flags.Consumer,
		})
		return nil
	}

	log.Debug(ctx, "Found ManifestWork", logger.Fields{
		"name":            work.Name,
		"id":              work.ID,
		"version":         work.Version,
		"manifests_count": work.ManifestCount,
	})

	// Show contents if dry-run
	if flags.DryRun {
		log.Info(ctx, "[DRY RUN] Would delete ManifestWork:", logger.Fields{
			"name":            flags.Name,
			"consumer":        flags.Consumer,
			"manifests_count": work.ManifestCount,
		})
		return nil
	}

	// Delete the ManifestWork (using HTTP API - works regardless of source ID)
	log.Info(ctx, "Deleting ManifestWork", logger.Fields{
		"name":     flags.Name,
		"consumer": flags.Consumer,
	})

	if err := client.DeleteManifestWorkByNameHTTP(ctx, flags.Consumer, flags.Name); err != nil {
		return fmt.Errorf("failed to delete ManifestWork: %w", err)
	}

	// Wait for deletion completion if requested (using HTTP polling, like kubectl wait --for=delete)
	if flags.Wait {
		// Use timeout if specified, otherwise default to 5 minutes
		waitTimeout := flags.Timeout
		if waitTimeout == 0 {
			waitTimeout = 5 * time.Minute
		}

		log.Info(ctx, "Waiting for deletion completion...", logger.Fields{
			"timeout": waitTimeout.String(),
		})

		// Create wait context with timeout
		waitCtx, waitCancel := context.WithTimeout(ctx, waitTimeout)
		defer waitCancel()

		if err := client.WaitForDeletion(waitCtx, flags.Consumer, flags.Name, maestro.DefaultPollInterval, log); err != nil {
			return fmt.Errorf("error waiting for deletion: %w", err)
		}
	}

	log.Info(ctx, "Successfully deleted ManifestWork", logger.Fields{
		"name":     flags.Name,
		"consumer": flags.Consumer,
	})

	// Write result for status-reporter integration
	result := manifestwork.StatusResult{
		Name:      flags.Name,
		Consumer:  flags.Consumer,
		Status:    "Deleted",
		Message:   "ManifestWork deleted successfully",
		Timestamp: time.Now(),
	}
	return manifestwork.WriteResult(flags.ResultsPath, result)
}
