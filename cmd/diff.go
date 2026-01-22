package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/hyperfleet/maestro-cli/internal/maestro"
	"github.com/hyperfleet/maestro-cli/internal/manifestwork"
	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

// DiffFlags contains flags for the diff command
type DiffFlags struct {
	ManifestFile string
	Consumer     string
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

// NewDiffCommand creates the diff command
func NewDiffCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show differences between local and remote ManifestWork",
		Long: `Compare a local ManifestWork file with the current state in Maestro.

Examples:
  # Show differences
  maestro-cli diff --manifest-file=job-manifestwork.json --consumer=agent1

  # Show differences with verbose output
  maestro-cli diff --manifest-file=job-manifestwork.json --consumer=agent1 --verbose`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := &DiffFlags{
				ManifestFile: getStringFlag(cmd, "manifest-file"),
				Consumer:     getStringFlag(cmd, "consumer"),
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

			return runDiffCommand(cmd.Context(), flags)
		},
	}

	// Command-specific flags
	cmd.Flags().String("manifest-file", "", "Path to ManifestWork YAML/JSON file (required)")
	cmd.Flags().String("consumer", "", "Target cluster name (required)")

	// Mark required flags
	if err := cmd.MarkFlagRequired("manifest-file"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("consumer"); err != nil {
		panic(err)
	}

	return cmd
}

// runDiffCommand executes the diff command
func runDiffCommand(ctx context.Context, flags *DiffFlags) error {
	// Setup context with timeout if specified
	ctxWithTimeout := ctx
	if flags.Timeout > 0 {
		var cancel context.CancelFunc
		ctxWithTimeout, cancel = context.WithTimeout(ctx, flags.Timeout)
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

	// Load local ManifestWork
	log.Debug(ctx, "Loading local ManifestWork", logger.Fields{
		"manifest_file": flags.ManifestFile,
	})

	localMW, err := manifestwork.LoadManifestWorkFromFile(flags.ManifestFile)
	if err != nil {
		return fmt.Errorf("failed to load local ManifestWork: %w", err)
	}

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

	// Validate consumer exists (with timeout)
	if err := client.ValidateConsumer(ctxWithTimeout, flags.Consumer); err != nil {
		return err
	}

	// Get remote ManifestWork (with timeout)
	log.Debug(ctx, "Fetching remote ManifestWork", logger.Fields{
		"name":     localMW.Name,
		"consumer": flags.Consumer,
	})

	remoteMW, err := client.GetResourceBundleFullHTTP(ctxWithTimeout, flags.Consumer, localMW.Name)
	if err != nil {
		// Check if this is a "not found" error (404) vs other errors
		if strings.Contains(err.Error(), "not found") {
			// ManifestWork doesn't exist remotely
			fmt.Printf("ManifestWork %q does not exist on consumer %q\n", localMW.Name, flags.Consumer)
			fmt.Printf("\nLocal ManifestWork would CREATE:\n")
			fmt.Printf("  Name: %s\n", localMW.Name)
			fmt.Printf("  Manifests: %d\n", len(localMW.Spec.Workload.Manifests))
			for i, m := range localMW.Spec.Workload.Manifests {
				info := getManifestInfo(m.Raw)
				fmt.Printf("    [%d] %s\n", i, info)
			}
			return nil
		}
		// Return other errors as-is (network issues, auth problems, etc.)
		return fmt.Errorf("failed to fetch remote ManifestWork: %w", err)
	}

	// Compare manifests
	fmt.Printf("Comparing ManifestWork %q\n", localMW.Name)
	fmt.Printf("  Remote ID: %s\n", remoteMW.ID)
	fmt.Printf("  Remote Version: %d\n", remoteMW.Version)
	fmt.Println()

	// Convert local manifests to comparable format
	localManifests := make([]map[string]interface{}, 0, len(localMW.Spec.Workload.Manifests))
	for i, m := range localMW.Spec.Workload.Manifests {
		var manifest map[string]interface{}
		if err := json.Unmarshal(m.Raw, &manifest); err != nil {
			return fmt.Errorf("failed to parse local manifest %d: %w", i, err)
		}
		localManifests = append(localManifests, manifest)
	}

	// Compare manifest counts
	localCount := len(localManifests)
	remoteCount := len(remoteMW.Manifests)

	if localCount != remoteCount {
		fmt.Printf("Manifest count differs: local=%d, remote=%d\n", localCount, remoteCount)
	}

	// Track differences
	var added, removed, modified []string

	// Build maps by kind/namespace/name
	localMap := make(map[string]map[string]interface{})
	for _, m := range localManifests {
		key := getManifestKey(m)
		localMap[key] = m
	}

	remoteMap := make(map[string]map[string]interface{})
	for _, m := range remoteMW.Manifests {
		key := getManifestKey(m)
		remoteMap[key] = m
	}

	// Find added and modified
	for key, localM := range localMap {
		if remoteM, exists := remoteMap[key]; exists {
			// Check if modified
			if !manifestsEqual(localM, remoteM) {
				modified = append(modified, key)
			}
		} else {
			added = append(added, key)
		}
	}

	// Find removed
	for key := range remoteMap {
		if _, exists := localMap[key]; !exists {
			removed = append(removed, key)
		}
	}

	// Print summary
	if len(added) == 0 && len(removed) == 0 && len(modified) == 0 {
		fmt.Println("No differences found - manifests are identical")
		return nil
	}

	fmt.Println("Differences found:")

	if len(added) > 0 {
		fmt.Printf("\n  + Added (%d):\n", len(added))
		for _, key := range added {
			fmt.Printf("    + %s\n", key)
		}
	}

	if len(removed) > 0 {
		fmt.Printf("\n  - Removed (%d):\n", len(removed))
		for _, key := range removed {
			fmt.Printf("    - %s\n", key)
		}
	}

	if len(modified) > 0 {
		fmt.Printf("\n  ~ Modified (%d):\n", len(modified))
		for _, key := range modified {
			fmt.Printf("    ~ %s\n", key)
			// Show detailed diff
			localM := localMap[key]
			remoteM := remoteMap[key]
			printManifestDiff(localM, remoteM, "      ")
		}
	}

	fmt.Printf("\nSummary: %d added, %d removed, %d modified\n", len(added), len(removed), len(modified))

	return nil
}

// printManifestDiff prints the differences between two manifests
func printManifestDiff(local, remote map[string]interface{}, indent string) {
	localClean := copyMapWithoutMetadata(local)
	remoteClean := copyMapWithoutMetadata(remote)

	diffs := findDiffs("", localClean, remoteClean)
	for _, d := range diffs {
		fmt.Printf("%s%s\n", indent, d)
	}
}

// findDiffs recursively finds differences between two maps
func findDiffs(path string, local, remote interface{}) []string {
	var diffs []string

	localMap, localIsMap := local.(map[string]interface{})
	remoteMap, remoteIsMap := remote.(map[string]interface{})

	if localIsMap && remoteIsMap {
		// Get all keys
		keys := make(map[string]bool)
		for k := range localMap {
			keys[k] = true
		}
		for k := range remoteMap {
			keys[k] = true
		}

		// Sort keys for consistent output
		sortedKeys := make([]string, 0, len(keys))
		for k := range keys {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)

		for _, k := range sortedKeys {
			newPath := k
			if path != "" {
				newPath = path + "." + k
			}

			localVal, localHas := localMap[k]
			remoteVal, remoteHas := remoteMap[k]

			if localHas && !remoteHas {
				diffs = append(diffs, fmt.Sprintf("+ %s: %s", newPath, formatValue(localVal)))
			} else if !localHas && remoteHas {
				diffs = append(diffs, fmt.Sprintf("- %s: %s", newPath, formatValue(remoteVal)))
			} else if !reflect.DeepEqual(localVal, remoteVal) {
				// Recursively check nested maps
				_, localIsNested := localVal.(map[string]interface{})
				_, remoteIsNested := remoteVal.(map[string]interface{})
				if localIsNested && remoteIsNested {
					diffs = append(diffs, findDiffs(newPath, localVal, remoteVal)...)
				} else {
					diffs = append(diffs, fmt.Sprintf("~ %s: %s → %s", newPath, formatValue(remoteVal), formatValue(localVal)))
				}
			}
		}
	} else if !reflect.DeepEqual(local, remote) {
		if path == "" {
			diffs = append(diffs, fmt.Sprintf("~ %s → %s", formatValue(remote), formatValue(local)))
		} else {
			diffs = append(diffs, fmt.Sprintf("~ %s: %s → %s", path, formatValue(remote), formatValue(local)))
		}
	}

	return diffs
}

// formatValue formats a value for display
func formatValue(v interface{}) string {
	if v == nil {
		return "null"
	}

	switch val := v.(type) {
	case string:
		if len(val) > 50 {
			return fmt.Sprintf("%q...", val[:47])
		}
		return fmt.Sprintf("%q", val)
	case map[string]interface{}, []interface{}:
		// For complex types, show as YAML snippet
		data, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		s := strings.TrimSpace(string(data))
		if len(s) > 60 {
			return s[:57] + "..."
		}
		return s
	default:
		return fmt.Sprintf("%v", v)
	}
}

// getManifestInfo returns a string describing a manifest
func getManifestInfo(raw []byte) string {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "(invalid)"
	}

	kind, _ := m["kind"].(string)
	metadata, _ := m["metadata"].(map[string]interface{})
	name, _ := metadata["name"].(string)
	ns, _ := metadata["namespace"].(string)

	if ns != "" {
		return fmt.Sprintf("%s/%s/%s", kind, ns, name)
	}
	return fmt.Sprintf("%s/%s", kind, name)
}

// getManifestKey returns a unique key for a manifest
func getManifestKey(m map[string]interface{}) string {
	kind, _ := m["kind"].(string)
	metadata, _ := m["metadata"].(map[string]interface{})
	name, _ := metadata["name"].(string)
	ns, _ := metadata["namespace"].(string)

	if ns != "" {
		return fmt.Sprintf("%s/%s/%s", kind, ns, name)
	}
	return fmt.Sprintf("%s/%s", kind, name)
}

// manifestsEqual compares two manifests for equality (ignoring metadata like resourceVersion)
func manifestsEqual(a, b map[string]interface{}) bool {
	// Remove fields that shouldn't be compared
	aCopy := copyMapWithoutMetadata(a)
	bCopy := copyMapWithoutMetadata(b)

	return reflect.DeepEqual(aCopy, bCopy)
}

// copyMapWithoutMetadata creates a copy of a manifest map without transient metadata
func copyMapWithoutMetadata(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for k, v := range m {
		if k == "metadata" {
			if metadata, ok := v.(map[string]interface{}); ok {
				newMetadata := make(map[string]interface{})
				for mk, mv := range metadata {
					// Skip transient fields
					if mk == "resourceVersion" || mk == "uid" || mk == "creationTimestamp" ||
						mk == "generation" || mk == "managedFields" || mk == "selfLink" {
						continue
					}
					newMetadata[mk] = mv
				}
				result[k] = newMetadata
			}
		} else if k == "status" {
			// Skip status field entirely
			continue
		} else {
			result[k] = v
		}
	}

	return result
}
