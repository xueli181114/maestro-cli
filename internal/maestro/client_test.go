package maestro

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

func TestCreateTLSConfig(t *testing.T) {
	tests := []struct {
		name        string
		config      ClientConfig
		expectError bool
		validate    func(t *testing.T, config *tls.Config)
	}{
		{
			name: "insecure config",
			config: ClientConfig{
				GRPCInsecure: true,
			},
			expectError: false,
			validate: func(t *testing.T, config *tls.Config) {
				// For insecure connections, we still return a TLS config but it won't be used
				if config == nil {
					t.Error("expected TLS config, got nil")
				}
			},
		},
		{
			name: "missing client key file",
			config: ClientConfig{
				GRPCInsecure:       false,
				GRPCClientCertFile: "/nonexistent/cert.pem",
				// GRPCClientKeyFile is empty
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsConfig, err := createTLSConfig(tt.config)

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
				tt.validate(t, tlsConfig)
			}
		})
	}
}

func TestGetToken(t *testing.T) {
	tests := []struct {
		name     string
		config   ClientConfig
		envVar   string
		expected string
	}{
		{
			name: "direct token has highest priority",
			config: ClientConfig{
				GRPCClientToken:     "direct-token",
				GRPCClientTokenFile: "/tmp/token.txt",
			},
			envVar:   "env-token",
			expected: "direct-token",
		},
		{
			name: "token file when no direct token",
			config: ClientConfig{
				GRPCClientTokenFile: "/tmp/test-token.txt",
			},
			expected: "file-token-content",
		},
		{
			name:     "environment variable as fallback",
			config:   ClientConfig{},
			envVar:   "env-token-fallback",
			expected: "env-token-fallback",
		},
		{
			name:     "empty when no token sources",
			config:   ClientConfig{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up environment variable if needed
			if tt.envVar != "" {
				oldEnv := os.Getenv("MAESTRO_GRPC_TOKEN")
				if err := os.Setenv("MAESTRO_GRPC_TOKEN", tt.envVar); err != nil {
					t.Fatalf("failed to set environment variable: %v", err)
				}
				defer func() {
					if oldEnv == "" {
						if err := os.Unsetenv("MAESTRO_GRPC_TOKEN"); err != nil {
							t.Errorf("failed to unset environment variable: %v", err)
						}
					} else {
						if err := os.Setenv("MAESTRO_GRPC_TOKEN", oldEnv); err != nil {
							t.Errorf("failed to restore environment variable: %v", err)
						}
					}
				}()
			}

			// Set up token file if needed
			var cleanup func()
			if tt.config.GRPCClientTokenFile != "" {
				dir := t.TempDir()
				tokenFile := filepath.Join(dir, "test-token.txt")
				if err := os.WriteFile(tokenFile, []byte("file-token-content"), 0600); err != nil {
					t.Fatalf("failed to create token file: %v", err)
				}
				tt.config.GRPCClientTokenFile = tokenFile
				cleanup = func() {
					if err := os.RemoveAll(dir); err != nil {
						t.Errorf("failed to clean up temp dir: %v", err)
					}
				}
				defer cleanup()
			}

			result := getToken(tt.config)
			if result != tt.expected {
				t.Errorf("getToken() = %q, expected %q", result, tt.expected)
			}
		})
	}
}

func TestValidateSearchQuery(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		expectError bool
	}{
		{
			name:        "valid alphanumeric",
			query:       "test123",
			expectError: false,
		},
		{
			name:        "valid with hyphens and underscores",
			query:       "test-name_123",
			expectError: false,
		},
		{
			name:        "valid with dots",
			query:       "test.name.123",
			expectError: false,
		},
		{
			name:        "empty query",
			query:       "",
			expectError: true,
		},
		{
			name:        "invalid character",
			query:       "test@name",
			expectError: true,
		},
		{
			name:        "too long",
			query:       string(make([]byte, 254)), // 253 is max
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSearchQuery(tt.query)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error for query %q, got none", tt.query)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for query %q: %v", tt.query, err)
				}
			}
		})
	}
}

func TestEvaluateConditionExpression(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		details  *ManifestWorkDetails
		expected bool
	}{
		{
			name: "simple condition true",
			expr: "Available",
			details: &ManifestWorkDetails{
				Conditions: []ConditionSummary{
					{Type: "Available", Status: "True"},
				},
			},
			expected: true,
		},
		{
			name: "simple condition false",
			expr: "Available",
			details: &ManifestWorkDetails{
				Conditions: []ConditionSummary{
					{Type: "Available", Status: "False"},
				},
			},
			expected: false,
		},
		{
			name: "AND expression both true",
			expr: "Available AND Progressing",
			details: &ManifestWorkDetails{
				Conditions: []ConditionSummary{
					{Type: "Available", Status: "True"},
					{Type: "Progressing", Status: "True"},
				},
			},
			expected: true,
		},
		{
			name: "AND expression one false",
			expr: "Available AND Progressing",
			details: &ManifestWorkDetails{
				Conditions: []ConditionSummary{
					{Type: "Available", Status: "True"},
					{Type: "Progressing", Status: "False"},
				},
			},
			expected: false,
		},
		{
			name: "OR expression one true",
			expr: "Available OR Progressing",
			details: &ManifestWorkDetails{
				Conditions: []ConditionSummary{
					{Type: "Available", Status: "False"},
					{Type: "Progressing", Status: "True"},
				},
			},
			expected: true,
		},
		{
			name: "OR expression both false",
			expr: "Available OR Progressing",
			details: &ManifestWorkDetails{
				Conditions: []ConditionSummary{
					{Type: "Available", Status: "False"},
					{Type: "Progressing", Status: "False"},
				},
			},
			expected: false,
		},
		{
			name:     "empty expression",
			expr:     "",
			details:  &ManifestWorkDetails{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a proper logger and context to avoid nil pointer panic
			log := logger.New(logger.Config{
				Level:  "debug",
				Format: "text",
			})
			ctx := context.Background()
			result := evaluateConditionExpression(ctx, tt.details, tt.expr, log)
			if result != tt.expected {
				t.Errorf("evaluateConditionExpression(%q) = %v, expected %v", tt.expr, result, tt.expected)
			}
		})
	}
}
