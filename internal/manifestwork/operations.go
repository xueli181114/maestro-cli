// Package manifestwork provides utilities for working with Open Cluster Management ManifestWork resources.
package manifestwork

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/yaml"

	"github.com/hyperfleet/maestro-cli/internal/maestro"
)

// StatusResult represents the result of a maestro-cli operation for status-reporter integration
type StatusResult struct {
	// Resource bundle identification
	ID        string `json:"id,omitempty"`        // Resource bundle UUID
	Name      string `json:"name"`                // Original metadata.name from ManifestWork
	Consumer  string `json:"consumer"`            // Consumer/cluster name
	Version   int32  `json:"version,omitempty"`   // Resource bundle version
	CreatedAt string `json:"createdAt,omitempty"` // Creation timestamp
	UpdatedAt string `json:"updatedAt,omitempty"` // Last update timestamp

	// Operation result
	Status    string    `json:"status"`    // Applied, Failed, InProgress, Available, Progressing, Degraded
	Message   string    `json:"message"`   // Human-readable message
	Timestamp time.Time `json:"timestamp"` // When this result was recorded

	// Detailed status
	Conditions []ConditionInfo  `json:"conditions,omitempty"` // ManifestWork-level conditions
	Resources  []ResourceStatus `json:"resources,omitempty"`  // Per-manifest status with K8s conditions
}

// ConditionInfo represents a ManifestWork condition
type ConditionInfo struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// ResourceStatus represents the status of individual resources within a ManifestWork
type ResourceStatus struct {
	Name           string                 `json:"name"`
	Namespace      string                 `json:"namespace,omitempty"`
	Kind           string                 `json:"kind"`
	Group          string                 `json:"group,omitempty"`
	Version        string                 `json:"version,omitempty"`
	Status         string                 `json:"status"`
	Message        string                 `json:"message,omitempty"`
	Conditions     []ConditionInfo        `json:"conditions,omitempty"`
	StatusFeedback map[string]interface{} `json:"statusFeedback,omitempty"`
}

// SourceFile represents a source configuration file that can be either
// a full ManifestWork or just the spec/manifests portion
type SourceFile struct {
	// Full ManifestWork fields (optional)
	APIVersion string                   `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string                   `json:"kind,omitempty" yaml:"kind,omitempty"`
	Metadata   map[string]any           `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Spec       *workv1.ManifestWorkSpec `json:"spec,omitempty" yaml:"spec,omitempty"`

	// Direct manifests array (for simple source files)
	Manifests []workv1.Manifest `json:"manifests,omitempty" yaml:"manifests,omitempty"`

	// Workload wrapper (alternative format)
	Workload *workv1.ManifestsTemplate `json:"workload,omitempty" yaml:"workload,omitempty"`
}

// LoadFromFile loads a ManifestWork from a YAML or JSON file
func LoadFromFile(filePath string) (*workv1.ManifestWork, error) {
	data, err := os.ReadFile(filePath) //nolint:gosec // This is intentional - CLI tool reads user-specified files
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	var manifestWork workv1.ManifestWork

	// Try to unmarshal as YAML first (YAML is a superset of JSON)
	if err := yaml.Unmarshal(data, &manifestWork); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ManifestWork from %s: %w", filePath, err)
	}

	// Validate that it's actually a ManifestWork
	if manifestWork.APIVersion == "" {
		manifestWork.APIVersion = "work.open-cluster-management.io/v1"
	}
	if manifestWork.Kind == "" {
		manifestWork.Kind = "ManifestWork"
	}

	if manifestWork.APIVersion != "work.open-cluster-management.io/v1" || manifestWork.Kind != "ManifestWork" {
		return nil, fmt.Errorf("file %s does not contain a valid ManifestWork resource", filePath)
	}

	if manifestWork.Name == "" {
		return nil, fmt.Errorf("ManifestWork in %s must have a name", filePath)
	}

	return &manifestWork, nil
}

// LoadSourceFile loads a source configuration file that can be:
// - A full ManifestWork
// - Just the spec portion
// - Just the manifests array
// - A workload object with manifests
func LoadSourceFile(filePath string) (*SourceFile, error) {
	data, err := os.ReadFile(filePath) //nolint:gosec // This is intentional - CLI tool reads user-specified files
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	var source SourceFile
	if err := yaml.Unmarshal(data, &source); err != nil {
		return nil, fmt.Errorf("failed to unmarshal source file %s: %w", filePath, err)
	}

	return &source, nil
}

// LoadManifestWorkFromFile is an alias for LoadFromFile for backward compatibility
func LoadManifestWorkFromFile(filePath string) (*workv1.ManifestWork, error) {
	return LoadFromFile(filePath)
}

// UnmarshalManifest unmarshals raw manifest bytes into a target struct
func UnmarshalManifest(data []byte, v interface{}) error {
	// Try JSON first (stricter)
	if err := json.Unmarshal(data, v); err == nil {
		return nil
	}
	// Fall back to YAML
	return yaml.Unmarshal(data, v)
}

// ToManifestWork converts a SourceFile to a ManifestWork, using the provided name
func (s *SourceFile) ToManifestWork(name string) *workv1.ManifestWork {
	mw := &workv1.ManifestWork{}
	mw.APIVersion = "work.open-cluster-management.io/v1"
	mw.Kind = "ManifestWork"
	mw.Name = name

	// If source has a full spec, use it
	if s.Spec != nil {
		mw.Spec = *s.Spec
		return mw
	}

	// If source has workload, use it
	if s.Workload != nil {
		mw.Spec.Workload = *s.Workload
		return mw
	}

	// If source has direct manifests, wrap them
	if len(s.Manifests) > 0 {
		mw.Spec.Workload.Manifests = s.Manifests
		return mw
	}

	return mw
}

// GetManifests returns the manifests from the source file
func (s *SourceFile) GetManifests() []workv1.Manifest {
	if s.Spec != nil {
		return s.Spec.Workload.Manifests
	}
	if s.Workload != nil {
		return s.Workload.Manifests
	}
	return s.Manifests
}

// IsFullManifestWork returns true if the source file is a complete ManifestWork
func (s *SourceFile) IsFullManifestWork() bool {
	return s.APIVersion != "" && s.Kind == "ManifestWork"
}

// MergeManifestWorks merges source manifests into an existing ManifestWork
// Strategy "merge": adds new manifests and updates existing ones (by kind+name)
// Strategy "replace": replaces the entire spec with source
func MergeManifestWorks(existing *workv1.ManifestWork, source *SourceFile, strategy string) (*workv1.ManifestWork, error) {
	result := existing.DeepCopy()

	switch strategy {
	case "replace":
		// Replace entire spec with source
		if source.Spec != nil {
			result.Spec = *source.Spec
		} else if source.Workload != nil {
			result.Spec.Workload = *source.Workload
		} else if len(source.Manifests) > 0 {
			result.Spec.Workload.Manifests = source.Manifests
		}
	case "merge":
		// Merge manifests: add new, update existing
		sourceManifests := source.GetManifests()
		if len(sourceManifests) > 0 {
			result.Spec.Workload.Manifests = mergeManifests(
				result.Spec.Workload.Manifests,
				sourceManifests,
			)
		}

		// Merge manifestConfigs if source has spec
		if source.Spec != nil && len(source.Spec.ManifestConfigs) > 0 {
			result.Spec.ManifestConfigs = mergeManifestConfigs(
				result.Spec.ManifestConfigs,
				source.Spec.ManifestConfigs,
			)
		}

		// Update deleteOption if source specifies it
		if source.Spec != nil && source.Spec.DeleteOption != nil {
			result.Spec.DeleteOption = source.Spec.DeleteOption
		}
	default:
		return nil, fmt.Errorf("unknown merge strategy: %s (use 'merge' or 'replace')", strategy)
	}

	return result, nil
}

// mergeManifests merges source manifests into existing manifests
// Matching is done by extracting kind and name from the raw manifest
func mergeManifests(existing, source []workv1.Manifest) []workv1.Manifest {
	// Create index of existing manifests by kind/name
	index := make(map[string]int)
	for i, m := range existing {
		key := getManifestKey(m)
		if key != "" {
			index[key] = i
		}
	}

	result := make([]workv1.Manifest, len(existing))
	copy(result, existing)

	for _, srcManifest := range source {
		key := getManifestKey(srcManifest)
		if key == "" {
			// Can't identify manifest, append it
			result = append(result, srcManifest)
			continue
		}

		if idx, found := index[key]; found {
			// Update existing manifest
			result[idx] = srcManifest
		} else {
			// Add new manifest
			result = append(result, srcManifest)
			index[key] = len(result) - 1
		}
	}

	return result
}

// getManifestKey extracts a unique key (kind/namespace/name) from a manifest
func getManifestKey(m workv1.Manifest) string {
	// Parse the raw manifest to extract kind and name
	var obj struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}

	if err := json.Unmarshal(m.Raw, &obj); err != nil {
		return ""
	}

	if obj.Kind == "" || obj.Metadata.Name == "" {
		return ""
	}

	return fmt.Sprintf("%s/%s/%s", obj.Kind, obj.Metadata.Namespace, obj.Metadata.Name)
}

// mergeManifestConfigs merges source configs into existing configs
func mergeManifestConfigs(existing, source []workv1.ManifestConfigOption) []workv1.ManifestConfigOption {
	// Create index by resource identifier
	index := make(map[string]int)
	for i, c := range existing {
		key := fmt.Sprintf("%s/%s/%s/%s",
			c.ResourceIdentifier.Group,
			c.ResourceIdentifier.Resource,
			c.ResourceIdentifier.Namespace,
			c.ResourceIdentifier.Name,
		)
		index[key] = i
	}

	result := make([]workv1.ManifestConfigOption, len(existing))
	copy(result, existing)

	for _, srcConfig := range source {
		key := fmt.Sprintf("%s/%s/%s/%s",
			srcConfig.ResourceIdentifier.Group,
			srcConfig.ResourceIdentifier.Resource,
			srcConfig.ResourceIdentifier.Namespace,
			srcConfig.ResourceIdentifier.Name,
		)

		if idx, found := index[key]; found {
			// Update existing config
			result[idx] = srcConfig
		} else {
			// Add new config
			result = append(result, srcConfig)
			index[key] = len(result) - 1
		}
	}

	return result
}

// RemoveManifests removes specified manifests from a ManifestWork
// Manifests are identified by "kind/namespace/name" or "kind/name" (for cluster-scoped resources)
func RemoveManifests(mw *workv1.ManifestWork, toRemove []string) (*workv1.ManifestWork, []string) {
	result := mw.DeepCopy()
	removed := []string{}

	// Build a set of keys to remove (support both kind/ns/name and kind/name formats)
	removeSet := make(map[string]bool)
	for _, r := range toRemove {
		removeSet[r] = true
	}

	// Filter manifests
	filteredManifests := []workv1.Manifest{}
	for _, m := range result.Spec.Workload.Manifests {
		key := getManifestKey(m)
		shortKey := getManifestShortKey(m) // kind/name without namespace

		if removeSet[key] || removeSet[shortKey] {
			removed = append(removed, key)
		} else {
			filteredManifests = append(filteredManifests, m)
		}
	}

	result.Spec.Workload.Manifests = filteredManifests

	// Also remove corresponding manifest configs
	if len(result.Spec.ManifestConfigs) > 0 {
		filteredConfigs := []workv1.ManifestConfigOption{}
		for _, c := range result.Spec.ManifestConfigs {
			configKey := fmt.Sprintf("%s/%s/%s",
				c.ResourceIdentifier.Resource,
				c.ResourceIdentifier.Namespace,
				c.ResourceIdentifier.Name,
			)
			shortConfigKey := fmt.Sprintf("%s/%s",
				c.ResourceIdentifier.Resource,
				c.ResourceIdentifier.Name,
			)

			if !removeSet[configKey] && !removeSet[shortConfigKey] {
				filteredConfigs = append(filteredConfigs, c)
			}
		}
		result.Spec.ManifestConfigs = filteredConfigs
	}

	return result, removed
}

// getManifestShortKey extracts kind/name key (without namespace) from a manifest
func getManifestShortKey(m workv1.Manifest) string {
	var obj struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}

	if err := json.Unmarshal(m.Raw, &obj); err != nil {
		return ""
	}

	if obj.Kind == "" || obj.Metadata.Name == "" {
		return ""
	}

	return fmt.Sprintf("%s/%s", obj.Kind, obj.Metadata.Name)
}

// ListManifestKeys returns all manifest keys (kind/namespace/name) in a ManifestWork
func ListManifestKeys(mw *workv1.ManifestWork) []string {
	keys := []string{}
	for _, m := range mw.Spec.Workload.Manifests {
		key := getManifestKey(m)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// ToYAML converts a ManifestWork to YAML format
func ToYAML(mw *workv1.ManifestWork) ([]byte, error) {
	return yaml.Marshal(mw)
}

// ToJSON converts a ManifestWork to JSON format
func ToJSON(mw *workv1.ManifestWork) ([]byte, error) {
	return json.MarshalIndent(mw, "", "  ")
}

// WriteToFile writes a ManifestWork to a file in YAML or JSON format
// Format is determined by file extension (.json for JSON, otherwise YAML)
func WriteToFile(mw *workv1.ManifestWork, filePath string) error {
	var data []byte
	var err error

	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".json" {
		data, err = ToJSON(mw)
	} else {
		data, err = ToYAML(mw)
	}

	if err != nil {
		return fmt.Errorf("failed to marshal ManifestWork: %w", err)
	}

	// Use 0640: owner read/write, group read (more secure than 0644 world-readable)
	// ManifestWork files may contain sensitive Kubernetes manifests
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	return nil
}

// WriteResult writes the status result to the specified path for status-reporter integration
func WriteResult(resultsPath string, result StatusResult) error {
	if resultsPath == "" {
		// Check environment variable
		resultsPath = os.Getenv("RESULTS_PATH")
		if resultsPath == "" {
			return nil // No results output requested
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal status result: %w", err)
	}

	// Use 0640: owner read/write, group read (restrictive but allows CI/CD group access)
	// Results files contain status info for status-reporter integration
	if err := os.WriteFile(resultsPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write results to %s: %w", resultsPath, err)
	}

	return nil
}

// BuildStatusResult creates a StatusResult from ManifestWorkDetails
// This is a shared helper function used by multiple commands to avoid code duplication
func BuildStatusResult(name, consumer, status, message string, details *maestro.ManifestWorkDetails) StatusResult {
	result := StatusResult{
		Name:      name,
		Consumer:  consumer,
		Status:    status,
		Message:   message,
		Timestamp: time.Now(),
	}

	if details != nil {
		result.ID = details.ID
		result.Version = details.Version
		result.CreatedAt = details.CreatedAt
		result.UpdatedAt = details.UpdatedAt

		for _, c := range details.Conditions {
			result.Conditions = append(result.Conditions, ConditionInfo{
				Type:               c.Type,
				Status:             c.Status,
				Reason:             c.Reason,
				Message:            c.Message,
				LastTransitionTime: c.LastTransitionTime,
			})
		}

		for _, rs := range details.ResourceStatus {
			resStatus := ResourceStatus{
				Name:           rs.Name,
				Namespace:      rs.Namespace,
				Kind:           rs.Kind,
				Group:          rs.Group,
				Version:        rs.Version,
				Status:         getResourceConditionStatus(rs.Conditions),
				StatusFeedback: rs.StatusFeedback,
			}
			for _, c := range rs.Conditions {
				resStatus.Conditions = append(resStatus.Conditions, ConditionInfo{
					Type:    c.Type,
					Status:  c.Status,
					Reason:  c.Reason,
					Message: c.Message,
				})
			}
			result.Resources = append(result.Resources, resStatus)
		}
	}

	return result
}

// getResourceConditionStatus extracts the overall status from conditions
func getResourceConditionStatus(conditions []maestro.ConditionSummary) string {
	for _, c := range conditions {
		if c.Type == "Available" && c.Status == "True" {
			return "Available"
		}
		if c.Type == "Applied" && c.Status == "True" {
			return "Applied"
		}
	}
	return "Unknown"
}
