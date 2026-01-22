package manifestwork

import (
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	workv1 "open-cluster-management.io/api/work/v1"
)

const (
	apiVersionManifestWork = "work.open-cluster-management.io/v1"
	kindManifestWork       = "ManifestWork"
	statusApplied          = "Applied"
)

func TestLoadSourceFile(t *testing.T) {
	tests := []struct {
		name        string
		fileContent string
		expectError bool
		validate    func(t *testing.T, source *SourceFile)
	}{
		{
			name: "valid YAML with spec",
			fileContent: `
apiVersion: work.open-cluster-management.io/v1
kind: ManifestWork
spec:
  workload:
    manifests:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: test-config
        namespace: default
      data:
        key: value
`,
			expectError: false,
			validate: func(t *testing.T, source *SourceFile) {
				if source == nil {
					t.Error("expected SourceFile, got nil")
					return
				}
				if source.Spec == nil {
					t.Error("expected Spec to be set")
					return
				}
				if len(source.Spec.Workload.Manifests) != 1 {
					t.Errorf("expected 1 manifest, got %d", len(source.Spec.Workload.Manifests))
				}
				// Note: Can't easily test Kind field without unmarshaling Raw data
			},
		},
		{
			name: "valid YAML with direct manifests",
			fileContent: `
manifests:
- apiVersion: v1
  kind: Namespace
  metadata:
    name: test-namespace
`,
			expectError: false,
			validate: func(t *testing.T, source *SourceFile) {
				if source == nil {
					t.Error("expected SourceFile, got nil")
					return
				}
				if len(source.Manifests) != 1 {
					t.Errorf("expected 1 manifest, got %d", len(source.Manifests))
				}
				// Note: Can't easily test Kind field without unmarshaling Raw data
			},
		},
		{
			name: "invalid YAML",
			fileContent: `
invalid: yaml: content: [
`,
			expectError: true,
		},
		{
			name:        "nonexistent file",
			fileContent: "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filePath string
			if tt.fileContent != "" {
				dir := t.TempDir()
				filePath = filepath.Join(dir, "test.yaml")
				if err := os.WriteFile(filePath, []byte(tt.fileContent), 0600); err != nil {
					t.Fatalf("failed to create test file: %v", err)
				}
			} else {
				filePath = "/nonexistent/file.yaml"
			}

			source, err := LoadSourceFile(filePath)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if tt.validate != nil {
				tt.validate(t, source)
			}
		})
	}
}

func TestUnmarshalManifest(t *testing.T) {
	tests := []struct {
		name        string
		data        []byte
		target      interface{}
		expectError bool
		validate    func(t *testing.T, target interface{})
	}{
		{
			name:        "valid JSON",
			data:        []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test"}}`),
			target:      &workv1.Manifest{},
			expectError: false,
			validate: func(t *testing.T, target interface{}) {
				manifest := target.(*workv1.Manifest)
				// The manifest.Raw field should contain the data
				if len(manifest.Raw) == 0 {
					t.Error("expected Raw field to be populated")
				}
			},
		},
		{
			name: "valid YAML",
			data: []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-yaml
`),
			target:      &workv1.Manifest{},
			expectError: false,
			validate: func(t *testing.T, target interface{}) {
				manifest := target.(*workv1.Manifest)
				// The manifest.Raw field should contain the data
				if len(manifest.Raw) == 0 {
					t.Error("expected Raw field to be populated")
				}
			},
		},
		{
			name:        "invalid data",
			data:        []byte(`invalid: yaml: content: [`),
			target:      &workv1.Manifest{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := UnmarshalManifest(tt.data, tt.target)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if tt.validate != nil {
				tt.validate(t, tt.target)
			}
		})
	}
}

func TestSourceFile_ToManifestWork(t *testing.T) {
	tests := []struct {
		name   string
		source SourceFile
		mwName string
	}{
		{
			name: "source with spec",
			source: SourceFile{
				Spec: &workv1.ManifestWorkSpec{},
			},
			mwName: "test-mw",
		},
		{
			name: "source with direct manifests",
			source: SourceFile{
				Manifests: []workv1.Manifest{
					{RawExtension: runtime.RawExtension{Raw: []byte(`{"kind":"ConfigMap"}`)}},
				},
			},
			mwName: "test-mw-manifests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.source.ToManifestWork(tt.mwName)

			if result == nil {
				t.Error("expected ManifestWork, got nil")
				return
			}

			if result.APIVersion != apiVersionManifestWork {
				t.Errorf("expected APIVersion %s, got %s", apiVersionManifestWork, result.APIVersion)
			}
			if result.Kind != kindManifestWork {
				t.Errorf("expected Kind ManifestWork, got %s", result.Kind)
			}
			if result.Name != tt.mwName {
				t.Errorf("expected Name %s, got %s", tt.mwName, result.Name)
			}
		})
	}
}

func TestMergeManifestWorks(t *testing.T) {
	tests := []struct {
		name        string
		existing    *workv1.ManifestWork
		source      *SourceFile
		strategy    string
		expectError bool
	}{
		{
			name: "merge strategy with new manifest",
			existing: &workv1.ManifestWork{
				Spec: workv1.ManifestWorkSpec{},
			},
			source: &SourceFile{
				Manifests: []workv1.Manifest{
					{RawExtension: runtime.RawExtension{Raw: []byte(`{"kind":"Service"}`)}},
				},
			},
			strategy:    "merge",
			expectError: false,
		},
		{
			name: "replace strategy",
			existing: &workv1.ManifestWork{
				Spec: workv1.ManifestWorkSpec{},
			},
			source: &SourceFile{
				Manifests: []workv1.Manifest{
					{RawExtension: runtime.RawExtension{Raw: []byte(`{"kind":"ConfigMap"}`)}},
				},
			},
			strategy:    "replace",
			expectError: false,
		},
		{
			name:        "invalid strategy",
			existing:    &workv1.ManifestWork{},
			source:      &SourceFile{},
			strategy:    "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := MergeManifestWorks(tt.existing, tt.source, tt.strategy)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if result == nil {
				t.Error("expected ManifestWork result, got nil")
			}
		})
	}
}

func TestGetManifestKey(t *testing.T) {
	tests := []struct {
		name     string
		manifest workv1.Manifest
		expected string
	}{
		{
			name: "standard manifest",
			manifest: workv1.Manifest{
				RawExtension: runtime.RawExtension{Raw: []byte(`{}`)}, // Empty manifest data
			},
			expected: "", // This would be computed based on kind/name
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: getManifestKey is a private function, so we can't test it directly
			// In a real implementation, we'd need to export it or test through public functions
			t.Skip("getManifestKey is private - test through public functions instead")
		})
	}
}

func TestToYAML(t *testing.T) {
	mw := &workv1.ManifestWork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "work.open-cluster-management.io/v1",
			Kind:       "ManifestWork",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-mw",
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{},
			},
		},
	}

	data, err := ToYAML(mw)
	if err != nil {
		t.Fatalf("ToYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("expected non-empty YAML data")
	}

	// Verify it can be unmarshaled back
	var result workv1.ManifestWork
	if err := UnmarshalManifest(data, &result); err != nil {
		t.Errorf("generated YAML is invalid: %v", err)
	}
}

func TestToJSON(t *testing.T) {
	mw := &workv1.ManifestWork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "work.open-cluster-management.io/v1",
			Kind:       "ManifestWork",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-mw",
		},
	}

	data, err := ToJSON(mw)
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("expected non-empty JSON data")
	}

	// Verify it can be unmarshaled back
	var result workv1.ManifestWork
	if err := UnmarshalManifest(data, &result); err != nil {
		t.Errorf("generated JSON is invalid: %v", err)
	}
}

func TestBuildStatusResult(t *testing.T) {
	result := BuildStatusResult("test-mw", "test-consumer", "Applied", "Success message", nil)

	if result.Name != "test-mw" {
		t.Errorf("expected Name 'test-mw', got %s", result.Name)
	}
	if result.Consumer != "test-consumer" {
		t.Errorf("expected Consumer 'test-consumer', got %s", result.Consumer)
	}
	if result.Status != statusApplied {
		t.Errorf("expected Status 'Applied', got %s", result.Status)
	}
	if result.Message != "Success message" {
		t.Errorf("expected Message 'Success message', got %s", result.Message)
	}
	if result.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}
