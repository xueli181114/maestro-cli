package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyperfleet/maestro-cli/internal/maestro"
	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

// WatchFlags contains flags for the watch command
type WatchFlags struct {
	Name         string
	Consumer     string
	PollInterval time.Duration
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

// NewWatchCommand creates the watch command
func NewWatchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch ManifestWork status changes",
		Long: `Continuously watch and display ManifestWork status changes.

Examples:
  # Watch a specific ManifestWork
  maestro-cli watch --name=hyperfleet-cluster-west-1-job --consumer=agent1

  # Watch with custom poll interval
  maestro-cli watch --name=hyperfleet-cluster-west-1-job --consumer=agent1 --poll-interval=5s

  # Watch with timeout
  maestro-cli watch --name=hyperfleet-cluster-west-1-job --consumer=agent1 --timeout=10m`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &WatchFlags{
				Name:         getStringFlag(cmd, "name"),
				Consumer:     getStringFlag(cmd, "consumer"),
				PollInterval: getDurationFlag(cmd, "poll-interval"),
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

			return runWatchCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("name", "", "ManifestWork name (required)")
	cmd.Flags().String("consumer", "", "Target cluster name (required)")
	cmd.Flags().Duration("poll-interval", maestro.DefaultPollInterval, "Interval between status checks")

	// Mark required flags
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}

	return cmd
}

// runWatchCommand executes the watch command
func runWatchCommand(ctx context.Context, flags *WatchFlags) error {
	// Initialize logger
	logLevel := "info"
	if flags.Verbose {
		logLevel = "debug"
	}
	log := logger.New(logger.Config{
		Level:  logLevel,
		Format: "text",
	})

	// Create HTTP-only client
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

	// Apply timeout if specified
	watchCtx := ctx
	if flags.Timeout > 0 {
		var cancel context.CancelFunc
		watchCtx, cancel = context.WithTimeout(ctx, flags.Timeout)
		defer cancel()
	}

	log.Info(ctx, "Watching ManifestWork", logger.Fields{
		"name":          flags.Name,
		"consumer":      flags.Consumer,
		"poll_interval": flags.PollInterval.String(),
	})

	// Track previous state to detect changes
	var lastVersion int32
	var lastConditions string

	// Exponential backoff for API failures
	consecutiveFailures := 0
	baseInterval := flags.PollInterval
	currentInterval := baseInterval

	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	// Initial check
	if err := printWatchStatus(watchCtx, client, flags, &lastVersion, &lastConditions); err != nil {
		log.Warn(ctx, "Initial status check failed", logger.Fields{"error": err.Error()})
		consecutiveFailures++
		updateBackoffInterval(&currentInterval, &consecutiveFailures, baseInterval, ticker)
	}

	for {
		select {
		case <-watchCtx.Done():
			fmt.Println("\nWatch stopped")
			return nil
		case <-ticker.C:
			if err := printWatchStatus(watchCtx, client, flags, &lastVersion, &lastConditions); err != nil {
				log.Warn(ctx, "Status check failed", logger.Fields{"error": err.Error()})
				consecutiveFailures++
				updateBackoffInterval(&currentInterval, &consecutiveFailures, baseInterval, ticker)
			} else {
				// Success - reset backoff
				if consecutiveFailures > 0 {
					consecutiveFailures = 0
					currentInterval = baseInterval
					ticker.Reset(currentInterval)
					log.Debug(ctx, "Status check succeeded, reset backoff", logger.Fields{
						"interval": currentInterval.String(),
					})
				}
			}
		}
	}
}

// updateBackoffInterval implements exponential backoff for API failures
// Caps at 5 minutes maximum to avoid extremely long delays
func updateBackoffInterval(currentInterval *time.Duration, consecutiveFailures *int, baseInterval time.Duration, ticker *time.Ticker) {
	if *consecutiveFailures <= 0 {
		return
	}

	// Exponential backoff: base * 2^(failures-1), cap at 5 minutes
	maxInterval := 5 * time.Minute
	multiplier := 1 << (*consecutiveFailures - 1) // Use integer bit shift
	newInterval := baseInterval * time.Duration(multiplier)

	if newInterval > maxInterval {
		newInterval = maxInterval
	}

	if newInterval != *currentInterval {
		*currentInterval = newInterval
		ticker.Reset(*currentInterval)

		// Only log significant backoff changes (every 3 failures or when hitting max)
		if *consecutiveFailures%3 == 0 || newInterval == maxInterval {
			log := logger.New(logger.Config{Level: "info", Format: "text"})
			log.Info(context.Background(), "Increased backoff interval due to API failures", logger.Fields{
				"failures":     *consecutiveFailures,
				"new_interval": newInterval.String(),
				"max_interval": maxInterval.String(),
			})
		}
	}
}

// printWatchStatus prints the current ManifestWork status if changed
func printWatchStatus(ctx context.Context, client *maestro.Client, flags *WatchFlags, lastVersion *int32, lastConditions *string) error {
	details, err := client.GetManifestWorkDetailsHTTP(ctx, flags.Consumer, flags.Name)
	if err != nil {
		return err
	}

	// Build current conditions string
	var condStr string
	for _, c := range details.Conditions {
		if condStr != "" {
			condStr += ", "
		}
		condStr += fmt.Sprintf("%s=%s", c.Type, c.Status)
	}

	// Check for resource-level condition changes
	for _, rs := range details.ResourceStatus {
		for _, c := range rs.Conditions {
			key := fmt.Sprintf("%s/%s:%s=%s", rs.Kind, rs.Name, c.Type, c.Status)
			condStr += " " + key
		}
	}

	// Only print if changed
	if details.Version != *lastVersion || condStr != *lastConditions {
		*lastVersion = details.Version
		*lastConditions = condStr

		// Print timestamp and status
		fmt.Printf("[%s] v%d ", time.Now().Format("15:04:05"), details.Version)

		// Print ManifestWork conditions
		for _, c := range details.Conditions {
			status := "✓"
			if c.Status != "True" {
				status = "✗"
			}
			fmt.Printf("%s%s ", c.Type, status)
		}

		// Print resource conditions
		for _, rs := range details.ResourceStatus {
			fmt.Printf("| %s/%s: ", rs.Kind, rs.Name)
			for _, c := range rs.Conditions {
				status := "✓"
				if c.Status != "True" {
					status = "✗"
				}
				fmt.Printf("%s%s ", c.Type, status)
			}
		}
		fmt.Println()
	}

	return nil
}
