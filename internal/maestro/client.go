// Package maestro provides client functionality for interacting with Maestro services.
package maestro

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openshift-online/maestro/pkg/api/openapi"
	"github.com/openshift-online/maestro/pkg/client/cloudevents/grpcsource"
	"github.com/openshift-online/ocm-sdk-go/logging"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	workv1client "open-cluster-management.io/api/client/work/clientset/versioned/typed/work/v1"
	workv1 "open-cluster-management.io/api/work/v1"
	grpcoptions "open-cluster-management.io/sdk-go/pkg/cloudevents/generic/options/grpc"

	"github.com/hyperfleet/maestro-cli/pkg/logger"
)

const (
	// DefaultPollInterval is the default interval for polling ManifestWork status
	DefaultPollInterval = 1 * time.Second

	// Status constants
	statusTrue = "True"
)

// Client represents a Maestro client
type Client struct {
	workClient workv1client.WorkV1Interface // nil for HTTP-only client
	httpClient *openapi.APIClient
	sourceID   string
	cancelFunc context.CancelFunc // cancel function for gRPC context
}

// ClientConfig contains configuration for creating a Maestro client
type ClientConfig struct {
	GRPCEndpoint        string
	HTTPEndpoint        string
	GRPCInsecure        bool
	GRPCServerCAFile    string
	GRPCBrokerCAFile    string
	GRPCClientCertFile  string
	GRPCClientKeyFile   string
	GRPCClientToken     string
	GRPCClientTokenFile string
	SourceID            string // Source ID for CloudEvents subscription (default: "maestro-cli")
}

// NewHTTPClient creates an HTTP-only Maestro client (no gRPC connection)
// Use this for commands that only need HTTP API: list, get, watch (polling)
func NewHTTPClient(config ClientConfig) (*Client, error) {
	// Create a basic logger for client operations
	log := logger.New(logger.Config{
		Level:  "info",
		Format: "text",
	})

	// Create custom HTTP client to avoid connection issues
	httpClient := createHTTPClient(config.GRPCInsecure, log)

	// Create Maestro HTTP API client
	maestroAPIClient := openapi.NewAPIClient(&openapi.Configuration{
		Servers: openapi.ServerConfigurations{{
			URL: config.HTTPEndpoint,
		}},
		HTTPClient: httpClient,
	})

	return &Client{
		workClient: nil, // No gRPC client
		httpClient: maestroAPIClient,
		sourceID:   "",
	}, nil
}

// NewClient creates a full Maestro client with gRPC connection
// Use this for commands that need gRPC: apply, delete
// The provided context is used for the gRPC connection lifecycle.
// When the context is cancelled (e.g., on SIGINT/SIGTERM), the gRPC connection will be closed.
func NewClient(ctx context.Context, config ClientConfig) (*Client, error) {
	// Create a basic logger for client operations
	log := logger.New(logger.Config{
		Level:  "info",
		Format: "text",
	})

	// Create a cancellable context derived from the parent context
	// This allows us to cancel the gRPC connection on Close() or when parent context is cancelled
	grpcCtx, cancel := context.WithCancel(ctx)

	// Create custom HTTP client with proper TLS config
	httpClient := createHTTPClient(config.GRPCInsecure, log)

	// Create Maestro HTTP API client
	maestroAPIClient := openapi.NewAPIClient(&openapi.Configuration{
		Servers: openapi.ServerConfigurations{{
			URL: config.HTTPEndpoint,
		}},
		HTTPClient: httpClient,
	})

	// Create TLS config if needed
	var tlsConfig *tls.Config
	if !config.GRPCInsecure {
		var err error
		tlsConfig, err = createTLSConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS config: %w", err)
		}
	}

	// Create gRPC dialer
	dialer := &grpcoptions.GRPCDialer{
		URL:       config.GRPCEndpoint,
		TLSConfig: tlsConfig,
		Token:     getToken(config),
	}

	// Create gRPC options
	grpcOpts := &grpcoptions.GRPCOptions{
		Dialer: dialer,
	}

	// Use configured source ID or default to "maestro-cli"
	sourceID := config.SourceID
	if sourceID == "" {
		sourceID = "maestro-cli"
	}

	// Create logger using ocm-sdk-go's StdLogger
	sdkLogger, err := logging.NewStdLoggerBuilder().
		Streams(os.Stdout, os.Stderr).
		Debug(false).
		Build()
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	// Create Maestro gRPC work client
	workClient, err := grpcsource.NewMaestroGRPCSourceWorkClient(
		grpcCtx,
		sdkLogger,
		maestroAPIClient,
		grpcOpts,
		sourceID,
	)
	if err != nil {
		cancel() // Clean up cancel function on error
		return nil, fmt.Errorf("failed to create ManifestWork client: %w", err)
	}

	return &Client{
		workClient: workClient,
		httpClient: maestroAPIClient,
		sourceID:   sourceID,
		cancelFunc: cancel,
	}, nil
}

// HasGRPC returns true if the client has a gRPC connection
func (c *Client) HasGRPC() bool {
	return c.workClient != nil
}

// Close closes the client and releases resources
// This cancels the gRPC context, which will:
// - Stop the reconnect watcher goroutine
// - Close the CloudEvents subscription
// - Clean up gRPC connections
func (c *Client) Close() error {
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	return nil
}

// WorkClient returns the underlying work client interface
func (c *Client) WorkClient() workv1client.WorkV1Interface {
	return c.workClient
}

// SourceID returns the source ID used by this client
func (c *Client) SourceID() string {
	return c.sourceID
}

// createHTTPClient creates an HTTP client with proper configuration
// to avoid connection reset issues
func createHTTPClient(insecure bool, log *logger.Logger) *http.Client {
	transport := &http.Transport{
		DisableKeepAlives:     true, // Disable keep-alive to avoid connection reuse issues
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false, // Force HTTP/1.1
	}

	if insecure {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // This is intentional for insecure development/testing scenarios
		}
		log.Warn(context.Background(), "TLS certificate verification disabled (insecure mode)",
			logger.Fields{"reason": "grpc-insecure flag is set"})
	}

	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
}

// ListConsumers lists all consumers from Maestro HTTP API
func (c *Client) ListConsumers(ctx context.Context) ([]string, error) {
	consumerList, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ConsumersGet(ctx).Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to list consumers: %w", err)
	}

	names := make([]string, 0, len(consumerList.Items))
	for _, consumer := range consumerList.Items {
		if consumer.Name != nil {
			names = append(names, *consumer.Name)
		}
	}
	return names, nil
}

// ValidateConsumer checks if a consumer exists and returns a user-friendly error if not
func (c *Client) ValidateConsumer(ctx context.Context, consumer string) error {
	consumers, err := c.ListConsumers(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate consumer: %w", err)
	}

	for _, c := range consumers {
		if c == consumer {
			return nil
		}
	}

	// Consumer not found - provide helpful error message
	if len(consumers) == 0 {
		return fmt.Errorf("consumer %q not found: no consumers are registered with Maestro", consumer)
	}

	return fmt.Errorf("consumer %q not found. Available consumers: %s", consumer, strings.Join(consumers, ", "))
}

// GetManifestWork retrieves a ManifestWork from Maestro using gRPC (for watch operations)
func (c *Client) GetManifestWork(ctx context.Context, consumer, name string) (*workv1.ManifestWork, error) {
	return c.workClient.ManifestWorks(consumer).Get(ctx, name, metav1.GetOptions{})
}

// ListManifestWorks lists all ManifestWorks for a consumer using gRPC subscription
// Note: This only returns works received via subscription, not from database
func (c *Client) ListManifestWorks(ctx context.Context, consumer string) (*workv1.ManifestWorkList, error) {
	return c.workClient.ManifestWorks(consumer).List(ctx, metav1.ListOptions{})
}

// ListManifestWorksHTTP lists all ManifestWorks for a consumer using HTTP API
// This reads directly from the database without requiring gRPC subscription
func (c *Client) ListManifestWorksHTTP(ctx context.Context, consumer string) ([]ResourceBundleSummary, error) {
	// Validate the consumer name to avoid SQL injection
	if err := validateSearchQuery(consumer); err != nil {
		return nil, fmt.Errorf("invalid consumer name: %w", err)
	}

	// Use search parameter to filter by consumer_name
	search := fmt.Sprintf("consumer_name = '%s'", consumer)

	resourceList, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesGet(ctx).
		Search(search).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to list resource bundles: %w", err)
	}

	summaries := make([]ResourceBundleSummary, 0, len(resourceList.Items))
	for _, rb := range resourceList.Items {
		summary := ResourceBundleSummary{
			ID:           getStringPtr(rb.Id),
			ConsumerName: consumer,
		}

		// Get the original ManifestWork name from metadata
		if rb.Metadata != nil {
			if name, ok := rb.Metadata["name"].(string); ok {
				summary.Name = name
			}
		}
		// Fallback to ID if name not in metadata
		if summary.Name == "" {
			summary.Name = summary.ID
		}

		if rb.Version != nil {
			summary.Version = *rb.Version
		}
		if rb.CreatedAt != nil {
			summary.CreatedAt = rb.CreatedAt.Format(time.RFC3339)
		}
		if rb.UpdatedAt != nil {
			summary.UpdatedAt = rb.UpdatedAt.Format(time.RFC3339)
		}

		// Extract manifests info (rb.Manifests is []map[string]interface{})
		if rb.Manifests != nil {
			summary.Manifests = make([]ManifestInfo, 0, len(rb.Manifests))
			for _, manifest := range rb.Manifests {
				info := ManifestInfo{}
				if kind, ok := manifest["kind"].(string); ok {
					info.Kind = kind
				}
				if metadata, ok := manifest["metadata"].(map[string]interface{}); ok {
					if name, ok := metadata["name"].(string); ok {
						info.Name = name
					}
					if ns, ok := metadata["namespace"].(string); ok {
						info.Namespace = ns
					}
				}
				summary.Manifests = append(summary.Manifests, info)
			}
			summary.ManifestCount = len(summary.Manifests)
		}

		// Extract conditions from status
		if rb.Status != nil {
			if conditions, ok := rb.Status["conditions"].([]interface{}); ok {
				summary.Conditions = make([]ConditionSummary, 0, len(conditions))
				for _, c := range conditions {
					if cond, ok := c.(map[string]interface{}); ok {
						cs := ConditionSummary{}
						if t, ok := cond["type"].(string); ok {
							cs.Type = t
						}
						if s, ok := cond["status"].(string); ok {
							cs.Status = s
						}
						if r, ok := cond["reason"].(string); ok {
							cs.Reason = r
						}
						if m, ok := cond["message"].(string); ok {
							cs.Message = m
						}
						summary.Conditions = append(summary.Conditions, cs)
					}
				}
			}
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

// GetManifestWorkByNameHTTP looks up a ManifestWork by its original name using HTTP API
// This reads from the database and doesn't require gRPC subscription
func (c *Client) GetManifestWorkByNameHTTP(ctx context.Context, consumer, name string) (*ResourceBundleSummary, error) {
	if err := validateSearchQuery(consumer); err != nil {
		return nil, fmt.Errorf("invalid consumer name: %w", err)
	}

	// Search for resource bundle by consumer and metadata name
	search := fmt.Sprintf("consumer_name = '%s'", consumer)

	resourceList, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesGet(ctx).
		Search(search).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to search resource bundles: %w", err)
	}

	// Find the one with matching metadata.name
	for _, rb := range resourceList.Items {
		var rbName string
		if rb.Metadata != nil {
			if n, ok := rb.Metadata["name"].(string); ok {
				rbName = n
			}
		}
		if rbName == name {
			summary := &ResourceBundleSummary{
				ID:           getStringPtr(rb.Id),
				Name:         rbName,
				ConsumerName: consumer,
			}
			if rb.Version != nil {
				summary.Version = *rb.Version
			}
			if rb.CreatedAt != nil {
				summary.CreatedAt = rb.CreatedAt.Format(time.RFC3339)
			}
			if rb.UpdatedAt != nil {
				summary.UpdatedAt = rb.UpdatedAt.Format(time.RFC3339)
			}
			// Extract manifests
			if rb.Manifests != nil {
				summary.Manifests = make([]ManifestInfo, 0, len(rb.Manifests))
				for _, manifest := range rb.Manifests {
					info := ManifestInfo{}
					if kind, ok := manifest["kind"].(string); ok {
						info.Kind = kind
					}
					if metadata, ok := manifest["metadata"].(map[string]interface{}); ok {
						if n, ok := metadata["name"].(string); ok {
							info.Name = n
						}
						if ns, ok := metadata["namespace"].(string); ok {
							info.Namespace = ns
						}
					}
					summary.Manifests = append(summary.Manifests, info)
				}
				summary.ManifestCount = len(summary.Manifests)
			}
			return summary, nil
		}
	}

	return nil, fmt.Errorf("ManifestWork %q not found for consumer %q", name, consumer)
}

// GetManifestWorkDetailsHTTP gets full details of a ManifestWork by name using HTTP API
func (c *Client) GetManifestWorkDetailsHTTP(ctx context.Context, consumer, name string) (*ManifestWorkDetails, error) {
	if err := validateSearchQuery(consumer); err != nil {
		return nil, fmt.Errorf("invalid consumer name: %w", err)
	}

	// Search for resource bundle by consumer
	search := fmt.Sprintf("consumer_name = '%s'", consumer)

	resourceList, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesGet(ctx).
		Search(search).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to search resource bundles: %w", err)
	}

	// Find the one with matching metadata.name
	for _, rb := range resourceList.Items {
		var rbName string
		if rb.Metadata != nil {
			if n, ok := rb.Metadata["name"].(string); ok {
				rbName = n
			}
		}
		if rbName != name {
			continue
		}

		details := &ManifestWorkDetails{
			ID:           getStringPtr(rb.Id),
			Name:         rbName,
			ConsumerName: consumer,
		}

		if rb.Version != nil {
			details.Version = *rb.Version
		}
		if rb.CreatedAt != nil {
			details.CreatedAt = rb.CreatedAt.Format(time.RFC3339)
		}
		if rb.UpdatedAt != nil {
			details.UpdatedAt = rb.UpdatedAt.Format(time.RFC3339)
		}

		// Extract delete option
		if rb.DeleteOption != nil {
			if policy, ok := rb.DeleteOption["propagationPolicy"].(string); ok {
				details.DeleteOption = policy
			}
		}

		// Extract manifests
		if rb.Manifests != nil {
			details.Manifests = make([]ManifestInfo, 0, len(rb.Manifests))
			for _, manifest := range rb.Manifests {
				info := ManifestInfo{}
				if kind, ok := manifest["kind"].(string); ok {
					info.Kind = kind
				}
				if metadata, ok := manifest["metadata"].(map[string]interface{}); ok {
					if n, ok := metadata["name"].(string); ok {
						info.Name = n
					}
					if ns, ok := metadata["namespace"].(string); ok {
						info.Namespace = ns
					}
				}
				details.Manifests = append(details.Manifests, info)
			}
		}

		// Extract conditions from status
		if rb.Status != nil {
			if conditions, ok := rb.Status["conditions"].([]interface{}); ok {
				details.Conditions = make([]ConditionSummary, 0, len(conditions))
				for _, c := range conditions {
					if cond, ok := c.(map[string]interface{}); ok {
						cs := ConditionSummary{}
						if t, ok := cond["type"].(string); ok {
							cs.Type = t
						}
						if s, ok := cond["status"].(string); ok {
							cs.Status = s
						}
						if r, ok := cond["reason"].(string); ok {
							cs.Reason = r
						}
						if m, ok := cond["message"].(string); ok {
							cs.Message = m
						}
						if lt, ok := cond["lastTransitionTime"].(string); ok {
							cs.LastTransitionTime = lt
						}
						details.Conditions = append(details.Conditions, cs)
					}
				}
			}

			// Extract resource status
			if resourceStatus, ok := rb.Status["resourceStatus"].([]interface{}); ok {
				details.ResourceStatus = make([]ResourceStatusInfo, 0, len(resourceStatus))
				for _, rs := range resourceStatus {
					if rsMap, ok := rs.(map[string]interface{}); ok {
						rsi := ResourceStatusInfo{}

						// Extract resource meta
						if meta, ok := rsMap["resourceMeta"].(map[string]interface{}); ok {
							if k, ok := meta["kind"].(string); ok {
								rsi.Kind = k
							}
							if n, ok := meta["name"].(string); ok {
								rsi.Name = n
							}
							if ns, ok := meta["namespace"].(string); ok {
								rsi.Namespace = ns
							}
							if g, ok := meta["group"].(string); ok {
								rsi.Group = g
							}
							if v, ok := meta["version"].(string); ok {
								rsi.Version = v
							}
							if r, ok := meta["resource"].(string); ok {
								rsi.Resource = r
							}
						}

						// Extract conditions
						if conds, ok := rsMap["conditions"].([]interface{}); ok {
							rsi.Conditions = make([]ConditionSummary, 0, len(conds))
							for _, c := range conds {
								if condMap, ok := c.(map[string]interface{}); ok {
									cs := ConditionSummary{}
									if t, ok := condMap["type"].(string); ok {
										cs.Type = t
									}
									if s, ok := condMap["status"].(string); ok {
										cs.Status = s
									}
									if r, ok := condMap["reason"].(string); ok {
										cs.Reason = r
									}
									if m, ok := condMap["message"].(string); ok {
										cs.Message = m
									}
									rsi.Conditions = append(rsi.Conditions, cs)
								}
							}
						}

						// Extract status feedback
						if feedback, ok := rsMap["statusFeedback"].(map[string]interface{}); ok {
							if values, ok := feedback["values"].([]interface{}); ok && len(values) > 0 {
								rsi.StatusFeedback = make(map[string]interface{})
								for _, v := range values {
									if valMap, ok := v.(map[string]interface{}); ok {
										if name, ok := valMap["name"].(string); ok {
											if fv, ok := valMap["fieldValue"].(map[string]interface{}); ok {
												if strVal, ok := fv["string"].(string); ok {
													rsi.StatusFeedback[name] = strVal
												} else if intVal, ok := fv["integer"].(float64); ok {
													rsi.StatusFeedback[name] = int64(intVal)
												} else if boolVal, ok := fv["boolean"].(bool); ok {
													rsi.StatusFeedback[name] = boolVal
												} else if jsonRaw, ok := fv["jsonRaw"].(string); ok {
													// Parse JSON raw string into interface{}
													var parsed interface{}
													if err := json.Unmarshal([]byte(jsonRaw), &parsed); err == nil {
														rsi.StatusFeedback[name] = parsed
													} else {
														// If parsing fails, store as raw string
														rsi.StatusFeedback[name] = jsonRaw
													}
												}
											}
										}
									}
								}
							}
						}

						details.ResourceStatus = append(details.ResourceStatus, rsi)
					}
				}
			}
		}

		return details, nil
	}

	return nil, fmt.Errorf("ManifestWork %q not found for consumer %q", name, consumer)
}

// DeleteManifestWorkByNameHTTP deletes a ManifestWork by its original name using HTTP API
// This works regardless of which source ID created the ManifestWork
func (c *Client) DeleteManifestWorkByNameHTTP(ctx context.Context, consumer, name string) error {
	// First find the ManifestWork to get its ID
	work, err := c.GetManifestWorkByNameHTTP(ctx, consumer, name)
	if err != nil {
		return err
	}

	// Delete by ID using HTTP API
	_, err = c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesIdDelete(ctx, work.ID).Execute()
	if err != nil {
		return fmt.Errorf("failed to delete resource bundle %s: %w", work.ID, err)
	}

	return nil
}

// getStringPtr safely dereferences a string pointer
func getStringPtr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ManifestInfo represents basic info about a manifest within a ManifestWork
type ManifestInfo struct {
	Kind      string `json:"kind" yaml:"kind"`
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// String returns a formatted string for the manifest
func (m ManifestInfo) String() string {
	if m.Namespace != "" {
		return fmt.Sprintf("%s/%s/%s", m.Kind, m.Namespace, m.Name)
	}
	return fmt.Sprintf("%s/%s", m.Kind, m.Name)
}

// ConditionSummary represents a condition status
type ConditionSummary struct {
	Type               string `json:"type" yaml:"type"`
	Status             string `json:"status" yaml:"status"`
	Reason             string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message            string `json:"message,omitempty" yaml:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty" yaml:"lastTransitionTime,omitempty"`
}

// ResourceStatusInfo represents the status of a specific resource in the ManifestWork
type ResourceStatusInfo struct {
	Kind           string                 `json:"kind" yaml:"kind"`
	Name           string                 `json:"name" yaml:"name"`
	Namespace      string                 `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Group          string                 `json:"group,omitempty" yaml:"group,omitempty"`
	Version        string                 `json:"version,omitempty" yaml:"version,omitempty"`
	Resource       string                 `json:"resource,omitempty" yaml:"resource,omitempty"`
	Conditions     []ConditionSummary     `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	StatusFeedback map[string]interface{} `json:"statusFeedback,omitempty" yaml:"statusFeedback,omitempty"`
}

// ManifestWorkDetails contains full details of a ManifestWork
type ManifestWorkDetails struct {
	ID             string               `json:"id" yaml:"id"`
	Name           string               `json:"name" yaml:"name"`
	ConsumerName   string               `json:"consumerName" yaml:"consumerName"`
	Version        int32                `json:"version" yaml:"version"`
	CreatedAt      string               `json:"createdAt" yaml:"createdAt"`
	UpdatedAt      string               `json:"updatedAt" yaml:"updatedAt"`
	Manifests      []ManifestInfo       `json:"manifests" yaml:"manifests"`
	Conditions     []ConditionSummary   `json:"conditions" yaml:"conditions"`
	ResourceStatus []ResourceStatusInfo `json:"resourceStatus,omitempty" yaml:"resourceStatus,omitempty"`
	DeleteOption   string               `json:"deleteOption,omitempty" yaml:"deleteOption,omitempty"`
}

// ResourceBundleSummary represents a summary of a resource bundle from HTTP API
type ResourceBundleSummary struct {
	ID            string             `json:"id" yaml:"id"`
	Name          string             `json:"name" yaml:"name"`
	ConsumerName  string             `json:"consumerName" yaml:"consumerName"`
	Version       int32              `json:"version" yaml:"version"`
	CreatedAt     string             `json:"createdAt" yaml:"createdAt"`
	UpdatedAt     string             `json:"updatedAt" yaml:"updatedAt"`
	ManifestCount int                `json:"manifestCount" yaml:"manifestCount"`
	Manifests     []ManifestInfo     `json:"manifests" yaml:"manifests"`
	Conditions    []ConditionSummary `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// GetResourceBundleHTTP gets a single resource bundle by ID using the HTTP API
func (c *Client) GetResourceBundleHTTP(ctx context.Context, id string) (*openapi.ResourceBundle, error) {
	resource, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesIdGet(ctx, id).Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to get resource bundle: %w", err)
	}
	return resource, nil
}

// GetResourceBundleByNameHTTP gets a resource bundle by name and consumer using the HTTP API
func (c *Client) GetResourceBundleByNameHTTP(ctx context.Context, consumer, name string) (*openapi.ResourceBundle, error) {
	if err := validateSearchQuery(consumer); err != nil {
		return nil, fmt.Errorf("invalid consumer name: %w", err)
	}
	if err := validateSearchQuery(name); err != nil {
		return nil, fmt.Errorf("invalid resource bundle name: %w", err)
	}

	// Search by name and consumer_name
	search := fmt.Sprintf("name = '%s' and consumer_name = '%s'", name, consumer)

	resourceList, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesGet(ctx).
		Search(search).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to search resource bundles: %w", err)
	}

	if len(resourceList.Items) == 0 {
		return nil, fmt.Errorf("resource bundle %s not found for consumer %s", name, consumer)
	}

	return &resourceList.Items[0], nil
}

// ResourceBundleFull represents a full resource bundle with all fields for output
type ResourceBundleFull struct {
	ID           string                   `json:"id" yaml:"id"`
	Name         string                   `json:"name" yaml:"name"`
	ConsumerName string                   `json:"consumerName" yaml:"consumerName"`
	Version      int32                    `json:"version" yaml:"version"`
	CreatedAt    string                   `json:"createdAt" yaml:"createdAt"`
	UpdatedAt    string                   `json:"updatedAt" yaml:"updatedAt"`
	DeleteOption map[string]interface{}   `json:"deleteOption,omitempty" yaml:"deleteOption,omitempty"`
	Manifests    []map[string]interface{} `json:"manifests,omitempty" yaml:"manifests,omitempty"`
	Status       map[string]interface{}   `json:"status,omitempty" yaml:"status,omitempty"`
}

// GetResourceBundleFullHTTP gets a full resource bundle by name and consumer for output
func (c *Client) GetResourceBundleFullHTTP(ctx context.Context, consumer, name string) (*ResourceBundleFull, error) {
	if err := validateSearchQuery(consumer); err != nil {
		return nil, fmt.Errorf("invalid consumer name: %w", err)
	}

	// Search for resource bundle by consumer
	search := fmt.Sprintf("consumer_name = '%s'", consumer)

	resourceList, _, err := c.httpClient.DefaultAPI.ApiMaestroV1ResourceBundlesGet(ctx).
		Search(search).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to search resource bundles: %w", err)
	}

	// Find the one with matching metadata.name
	for _, rb := range resourceList.Items {
		var rbName string
		if rb.Metadata != nil {
			if n, ok := rb.Metadata["name"].(string); ok {
				rbName = n
			}
		}
		if rbName != name {
			continue
		}

		result := &ResourceBundleFull{
			ID:           getStringPtr(rb.Id),
			Name:         rbName,
			ConsumerName: consumer,
		}

		if rb.Version != nil {
			result.Version = *rb.Version
		}
		if rb.CreatedAt != nil {
			result.CreatedAt = rb.CreatedAt.Format(time.RFC3339)
		}
		if rb.UpdatedAt != nil {
			result.UpdatedAt = rb.UpdatedAt.Format(time.RFC3339)
		}
		if rb.DeleteOption != nil {
			result.DeleteOption = rb.DeleteOption
		}
		if rb.Manifests != nil {
			result.Manifests = rb.Manifests
		}
		if rb.Status != nil {
			result.Status = rb.Status
		}

		return result, nil
	}

	return nil, fmt.Errorf("ManifestWork %q not found for consumer %q", name, consumer)
}

// DeleteManifestWork deletes a ManifestWork from the target consumer
func (c *Client) DeleteManifestWork(ctx context.Context, consumer, name string) error {
	return c.workClient.ManifestWorks(consumer).Delete(ctx, name, metav1.DeleteOptions{})
}

// UpdateManifestWork updates an existing ManifestWork
func (c *Client) UpdateManifestWork(ctx context.Context, consumer string, manifestWork *workv1.ManifestWork) (*workv1.ManifestWork, error) {
	manifestWork.Namespace = consumer
	return c.workClient.ManifestWorks(consumer).Update(ctx, manifestWork, metav1.UpdateOptions{})
}

// PatchManifestWork updates an existing ManifestWork using patch (supports gRPC)
func (c *Client) PatchManifestWork(ctx context.Context, consumer string, existingWork, updatedWork *workv1.ManifestWork, log *logger.Logger) (*workv1.ManifestWork, error) {
	updatedWork.Namespace = consumer

	patchData, err := grpcsource.ToWorkPatch(existingWork, updatedWork)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch: %w", err)
	}

	log.Debug(ctx, "Patching ManifestWork", logger.Fields{
		"name":             updatedWork.Name,
		"patch_size":       len(patchData),
		"resource_version": existingWork.ResourceVersion,
	})

	return c.workClient.ManifestWorks(consumer).Patch(ctx, updatedWork.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
}

// ManifestWorkExists checks if a ManifestWork exists
func (c *Client) ManifestWorkExists(ctx context.Context, consumer, name string) (bool, error) {
	_, err := c.workClient.ManifestWorks(consumer).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ApplyManifestWork applies a ManifestWork to the target consumer
func (c *Client) ApplyManifestWork(ctx context.Context, consumer string, manifestWork *workv1.ManifestWork, log *logger.Logger) (*workv1.ManifestWork, error) {
	// Set the namespace to the consumer name (this is how Maestro routing works)
	manifestWork.Namespace = consumer

	// Check if ManifestWork exists using HTTP API (reliable, reads from DB)
	existingSummary, err := c.GetManifestWorkByNameHTTP(ctx, consumer, manifestWork.Name)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return nil, fmt.Errorf("failed to check existing work: %w", err)
	}

	if existingSummary == nil {
		// Work doesn't exist, create it
		log.Info(ctx, "Creating new ManifestWork", logger.Fields{
			"manifest_name": manifestWork.Name,
			"consumer":      consumer,
		})
		return c.workClient.ManifestWorks(consumer).Create(ctx, manifestWork, metav1.CreateOptions{})
	}

	// Work exists - get it via gRPC for update (now subscription should have it after checking HTTP)
	existingWork, err := c.workClient.ManifestWorks(consumer).Get(ctx, manifestWork.Name, metav1.GetOptions{})
	if err != nil {
		// If gRPC can't find it but HTTP found it, fall back to create (replace)
		log.Info(ctx, "ManifestWork exists in DB but not in subscription, recreating", logger.Fields{
			"manifest_name": manifestWork.Name,
			"consumer":      consumer,
			"existing_id":   existingSummary.ID,
		})
		return c.workClient.ManifestWorks(consumer).Create(ctx, manifestWork, metav1.CreateOptions{})
	}

	// Work exists, update with merge patch
	log.Info(ctx, "Updating existing ManifestWork", logger.Fields{
		"manifest_name":            manifestWork.Name,
		"consumer":                 consumer,
		"current_resource_version": existingWork.ResourceVersion,
		"current_generation":       existingWork.Generation,
	})

	patchData, err := grpcsource.ToWorkPatch(existingWork, manifestWork)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch: %w", err)
	}

	return c.workClient.ManifestWorks(consumer).Patch(ctx, manifestWork.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
}

// WaitCallback is called on each poll with current ManifestWork details
// Return true to continue waiting, false to stop
type WaitCallback func(details *ManifestWorkDetails, conditionMet bool) error

// WaitForCondition polls for a ManifestWork condition expression using HTTP API
// Supports logical expressions like "Available AND Job:Complete" or "Job:succeeded>=1 OR Job:Failed"
// The optional callback is invoked on each poll to report progress
func (c *Client) WaitForCondition(ctx context.Context, consumer, workName, conditionExpr string, pollInterval time.Duration, log *logger.Logger, callback WaitCallback) error {
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}

	// First check current status using HTTP API
	details, err := c.GetManifestWorkDetailsHTTP(ctx, consumer, workName)
	if err != nil {
		return fmt.Errorf("failed to get ManifestWork: %w", err)
	}

	conditionMet := evaluateConditionExpression(ctx, details, conditionExpr, log)

	// Call callback with initial status
	if callback != nil {
		if err := callback(details, conditionMet); err != nil {
			log.Warn(ctx, "Callback error (results may not be written)", logger.Fields{"error": err.Error()})
		}
	}

	if conditionMet {
		log.Info(ctx, "Condition already met", logger.Fields{
			"condition": conditionExpr,
			"name":      workName,
		})
		return nil
	}

	log.Info(ctx, "Polling for condition", logger.Fields{
		"name":          workName,
		"condition":     conditionExpr,
		"poll_interval": pollInterval.String(),
	})

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Warn(ctx, "Context cancelled while waiting for condition", logger.Fields{
				"condition": conditionExpr,
				"error":     ctx.Err().Error(),
			})
			return ctx.Err()
		case <-ticker.C:
			details, err := c.GetManifestWorkDetailsHTTP(ctx, consumer, workName)
			if err != nil {
				log.Warn(ctx, "Failed to poll ManifestWork", logger.Fields{
					"error": err.Error(),
				})
				continue
			}

			conditionMet := evaluateConditionExpression(ctx, details, conditionExpr, log)

			// Call callback on each poll
			if callback != nil {
				if err := callback(details, conditionMet); err != nil {
					log.Warn(ctx, "Callback error (results may not be written)", logger.Fields{"error": err.Error()})
				}
			}

			log.Debug(ctx, "Polled ManifestWork status", logger.Fields{
				"name":       workName,
				"conditions": len(details.Conditions),
			})

			if conditionMet {
				log.Info(ctx, "Condition met", logger.Fields{
					"condition": conditionExpr,
					"name":      workName,
				})
				return nil
			}
		}
	}
}

// WaitForDeletion polls for ManifestWork deletion using HTTP API
func (c *Client) WaitForDeletion(ctx context.Context, consumer, workName string, pollInterval time.Duration, log *logger.Logger) error {
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}

	log.Info(ctx, "Polling for deletion", logger.Fields{
		"name":          workName,
		"consumer":      consumer,
		"poll_interval": pollInterval.String(),
	})

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Use HTTP API to check if ManifestWork still exists
			_, err := c.GetManifestWorkByNameHTTP(ctx, consumer, workName)
			if err != nil {
				// Resource not found means deletion complete
				if strings.Contains(err.Error(), "not found") {
					log.Info(ctx, "ManifestWork deleted", logger.Fields{
						"name":     workName,
						"consumer": consumer,
					})
					return nil
				}
				log.Warn(ctx, "Error polling for deletion", logger.Fields{
					"error": err.Error(),
				})
			}
		}
	}
}

// evaluateConditionExpression evaluates a condition expression with AND/OR logic
// Supports:
//   - ManifestWork conditions: "Available", "Applied"
//   - StatusFeedback conditions: "Job:Complete", "Job:succeeded>=1"
//   - Logical operators: "AND", "OR", "&&", "||"
//   - Parentheses for grouping: "(A AND B) OR C"
func evaluateConditionExpression(ctx context.Context, details *ManifestWorkDetails, expr string, log *logger.Logger) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}

	// Handle parentheses - find matching pairs
	if strings.HasPrefix(expr, "(") {
		depth := 0
		for i, ch := range expr {
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
				if depth == 0 {
					// Found matching closing paren
					inner := expr[1:i]
					rest := strings.TrimSpace(expr[i+1:])
					innerResult := evaluateConditionExpression(ctx, details, inner, log)

					if rest == "" {
						return innerResult
					}

					// Check for operator after parentheses
					if strings.HasPrefix(rest, "AND") || strings.HasPrefix(rest, "&&") {
						if strings.HasPrefix(rest, "&&") {
							rest = strings.TrimPrefix(rest, "&&")
						} else {
							rest = strings.TrimPrefix(rest, "AND")
						}
						return innerResult && evaluateConditionExpression(ctx, details, strings.TrimSpace(rest), log)
					}
					if strings.HasPrefix(rest, "OR") || strings.HasPrefix(rest, "||") {
						if strings.HasPrefix(rest, "||") {
							rest = strings.TrimPrefix(rest, "||")
						} else {
							rest = strings.TrimPrefix(rest, "OR")
						}
						return innerResult || evaluateConditionExpression(ctx, details, strings.TrimSpace(rest), log)
					}
					break
				}
			}
		}
	}

	// Check for AND (higher precedence, evaluated first to split)
	andParts := splitByOperator(expr, "AND", "&&")
	if len(andParts) > 1 {
		for _, part := range andParts {
			if !evaluateConditionExpression(ctx, details, strings.TrimSpace(part), log) {
				return false
			}
		}
		return true
	}

	// Check for OR (lower precedence)
	orParts := splitByOperator(expr, "OR", "||")
	if len(orParts) > 1 {
		for _, part := range orParts {
			if evaluateConditionExpression(ctx, details, strings.TrimSpace(part), log) {
				return true
			}
		}
		return false
	}

	// Single condition - evaluate it
	return evaluateSingleCondition(ctx, details, expr, log)
}

// splitByOperator splits expression by operator, respecting parentheses
func splitByOperator(expr, op1, op2 string) []string {
	var parts []string
	var current strings.Builder
	depth := 0

	words := strings.Fields(expr)
	for i, word := range words {
		if word == "(" || strings.HasPrefix(word, "(") {
			depth += strings.Count(word, "(") - strings.Count(word, ")")
		} else if word == ")" || strings.HasSuffix(word, ")") {
			depth += strings.Count(word, "(") - strings.Count(word, ")")
		}

		if depth == 0 && (word == op1 || word == op2) {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			continue
		}

		if current.Len() > 0 {
			current.WriteString(" ")
		}
		current.WriteString(word)

		// Handle inline operators like "A&&B" or "A||B"
		if depth == 0 && i < len(words)-1 {
			if strings.Contains(word, op2) && op2 != "" {
				subParts := strings.SplitN(current.String(), op2, 2)
				if len(subParts) == 2 {
					parts = append(parts, subParts[0])
					current.Reset()
					current.WriteString(subParts[1])
				}
			}
		}
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// evaluateSingleCondition evaluates a single condition (no logical operators)
func evaluateSingleCondition(ctx context.Context, details *ManifestWorkDetails, condition string, log *logger.Logger) bool {
	condition = strings.TrimSpace(condition)

	log.Debug(ctx, "Evaluating single condition", logger.Fields{
		"condition": condition,
		"trimmed":   condition == "",
	})

	// Empty conditions are invalid
	if condition == "" {
		log.Debug(ctx, "Empty condition detected, returning false")
		return false
	}

	// Check if it's a statusFeedback condition (contains ":")
	if strings.Contains(condition, ":") {
		return evaluateStatusFeedbackCondition(ctx, details, condition, log)
	}

	// Otherwise, it's a ManifestWork-level condition
	return checkDetailsCondition(ctx, details, condition, log)
}

// checkDetailsCondition checks ManifestWork-level conditions from details
// For conditions other than "Applied", it also verifies that the condition's
// lastTransitionTime is >= the "Applied" condition's lastTransitionTime to ensure
// we're seeing fresh status and not stale data from a previous apply
func checkDetailsCondition(ctx context.Context, details *ManifestWorkDetails, condType string, log *logger.Logger) bool {
	// Find the target condition and Applied condition
	var targetCond, appliedCond *ConditionSummary
	for i := range details.Conditions {
		cond := &details.Conditions[i]
		if strings.EqualFold(cond.Type, condType) && cond.Status == statusTrue {
			targetCond = cond
		}
		if strings.EqualFold(cond.Type, "Applied") && cond.Status == "True" {
			appliedCond = cond
		}
	}

	// Target condition not met
	if targetCond == nil {
		log.Debug(ctx, "ManifestWork condition not found or not True", logger.Fields{
			"condition": condType,
		})
		return false
	}

	log.Debug(ctx, "ManifestWork condition found", logger.Fields{
		"condition":          condType,
		"status":             targetCond.Status,
		"lastTransitionTime": targetCond.LastTransitionTime,
	})

	// If checking "Applied" itself, or no Applied condition exists, just return true
	if strings.EqualFold(condType, "Applied") || appliedCond == nil {
		return true
	}

	// For other conditions, verify the timestamp is fresh (>= Applied time)
	if targetCond.LastTransitionTime != "" && appliedCond.LastTransitionTime != "" {
		targetTime, err1 := time.Parse(time.RFC3339, targetCond.LastTransitionTime)
		appliedTime, err2 := time.Parse(time.RFC3339, appliedCond.LastTransitionTime)
		if err1 == nil && err2 == nil {
			log.Debug(ctx, "Comparing condition timestamps", logger.Fields{
				"condition":     condType,
				"conditionTime": targetCond.LastTransitionTime,
				"appliedTime":   appliedCond.LastTransitionTime,
				"isFresh":       !targetTime.Before(appliedTime),
			})
			// Condition must have transitioned at or after the Applied time
			if targetTime.Before(appliedTime) {
				log.Debug(ctx, "Condition is stale (before Applied time)", logger.Fields{
					"condition":     condType,
					"conditionTime": targetCond.LastTransitionTime,
					"appliedTime":   appliedCond.LastTransitionTime,
				})
				return false
			}
		}
	}

	return true
}

// evaluateStatusFeedbackCondition evaluates a statusFeedback condition
// Format: "Kind:condition" or "Kind/name:condition" or "Kind/namespace/name:condition"
// Examples: "Job:Complete", "Job/test-job-1:Complete", "Job/default/test-job:succeeded>=1"
func evaluateStatusFeedbackCondition(ctx context.Context, details *ManifestWorkDetails, condition string, log *logger.Logger) bool {
	parts := strings.SplitN(condition, ":", 2)
	if len(parts) != 2 {
		return false
	}

	resourceSelector := strings.TrimSpace(parts[0])
	check := strings.TrimSpace(parts[1])

	// Parse resource selector: Kind, Kind/name, or Kind/namespace/name
	selectorParts := strings.Split(resourceSelector, "/")
	kind := selectorParts[0]
	var name, namespace string
	if len(selectorParts) == 2 {
		// Kind/name format
		name = selectorParts[1]
	} else if len(selectorParts) >= 3 {
		// Kind/namespace/name format
		namespace = selectorParts[1]
		name = selectorParts[2]
	}

	log.Debug(ctx, "Evaluating resource condition", logger.Fields{
		"kind":      kind,
		"name":      name,
		"namespace": namespace,
		"check":     check,
	})

	// Get ManifestWork-level Applied timestamp for freshness check
	var manifestAppliedTime time.Time
	var manifestAppliedTimeStr string
	for _, cond := range details.Conditions {
		if strings.EqualFold(cond.Type, "Applied") && cond.Status == "True" && cond.LastTransitionTime != "" {
			manifestAppliedTimeStr = cond.LastTransitionTime
			if t, err := time.Parse(time.RFC3339, cond.LastTransitionTime); err == nil {
				manifestAppliedTime = t
			}
			break
		}
	}

	// Find matching resource by kind (and optionally name/namespace)
	for _, rs := range details.ResourceStatus {
		if !strings.EqualFold(rs.Kind, kind) {
			continue
		}
		// If name specified, must match
		if name != "" && !strings.EqualFold(rs.Name, name) {
			continue
		}
		// If namespace specified, must match
		if namespace != "" && !strings.EqualFold(rs.Namespace, namespace) {
			continue
		}

		log.Debug(ctx, "Found matching resource", logger.Fields{
			"kind":      rs.Kind,
			"name":      rs.Name,
			"namespace": rs.Namespace,
		})

		// Verify resource has fresh Applied status (>= ManifestWork Applied time)
		// Only skip if resource has Applied timestamp AND it's before ManifestWork Applied time
		if !manifestAppliedTime.IsZero() {
			resourceFresh := true // Default to fresh if we can't determine staleness
			var resourceAppliedTimeStr string
			var foundAppliedCondition bool

			for _, cond := range rs.Conditions {
				if strings.EqualFold(cond.Type, "Applied") && cond.Status == "True" {
					foundAppliedCondition = true
					resourceAppliedTimeStr = cond.LastTransitionTime
					if cond.LastTransitionTime != "" {
						if t, err := time.Parse(time.RFC3339, cond.LastTransitionTime); err == nil {
							resourceFresh = !t.Before(manifestAppliedTime)
						}
					} else {
						// Resource has Applied condition but no timestamp - consider fresh
						resourceFresh = true
					}
					break
				}
			}

			log.Debug(ctx, "Resource freshness check", logger.Fields{
				"resource":              fmt.Sprintf("%s/%s", rs.Kind, rs.Name),
				"foundAppliedCondition": foundAppliedCondition,
				"resourceAppliedTime":   resourceAppliedTimeStr,
				"manifestAppliedTime":   manifestAppliedTimeStr,
				"isFresh":               resourceFresh,
			})

			if !resourceFresh {
				log.Debug(ctx, "Skipping stale resource status", logger.Fields{
					"resource": fmt.Sprintf("%s/%s", rs.Kind, rs.Name),
				})
				continue // Skip stale resource status
			}
		}

		// Check if it's a comparison (=, >=, <=, >, <)
		if strings.Contains(check, ">=") {
			return evaluateComparison(rs.StatusFeedback, check, ">=")
		}
		if strings.Contains(check, "<=") {
			return evaluateComparison(rs.StatusFeedback, check, "<=")
		}
		if strings.Contains(check, ">") && !strings.Contains(check, ">=") {
			return evaluateComparison(rs.StatusFeedback, check, ">")
		}
		if strings.Contains(check, "<") && !strings.Contains(check, "<=") {
			return evaluateComparison(rs.StatusFeedback, check, "<")
		}
		if strings.Contains(check, "=") {
			return evaluateComparison(rs.StatusFeedback, check, "=")
		}

		// Otherwise, check if it's a condition name in statusFeedback.conditions or resource conditions
		// First check resource-level conditions (Applied, Available, StatusFeedbackSynced)
		for _, cond := range rs.Conditions {
			if strings.EqualFold(cond.Type, check) && cond.Status == "True" {
				log.Debug(ctx, "Resource condition matched", logger.Fields{
					"resource":  fmt.Sprintf("%s/%s", rs.Kind, rs.Name),
					"condition": check,
					"status":    cond.Status,
				})
				return true
			}
		}

		// Then check statusFeedback for conditions
		if rs.StatusFeedback != nil {
			// Check if statusFeedback has a "conditions" array
			if feedbackConds, ok := rs.StatusFeedback["conditions"].([]interface{}); ok {
				for _, fc := range feedbackConds {
					if fcMap, ok := fc.(map[string]interface{}); ok {
						if t, ok := fcMap["type"].(string); ok && strings.EqualFold(t, check) {
							if s, ok := fcMap["status"].(string); ok && s == "True" {
								log.Debug(ctx, "StatusFeedback condition matched", logger.Fields{
									"resource":  fmt.Sprintf("%s/%s", rs.Kind, rs.Name),
									"condition": check,
									"status":    s,
								})
								return true
							}
						}
					}
				}
			}
			// Check if statusFeedback.status has conditions
			if status, ok := rs.StatusFeedback["status"].(map[string]interface{}); ok {
				if statusConds, ok := status["conditions"].([]interface{}); ok {
					for _, sc := range statusConds {
						if scMap, ok := sc.(map[string]interface{}); ok {
							if t, ok := scMap["type"].(string); ok && strings.EqualFold(t, check) {
								if s, ok := scMap["status"].(string); ok && s == "True" {
									log.Debug(ctx, "StatusFeedback.status condition matched", logger.Fields{
										"resource":  fmt.Sprintf("%s/%s", rs.Kind, rs.Name),
										"condition": check,
										"status":    s,
									})
									return true
								}
							}
						}
					}
				}
			}
		}

		log.Debug(ctx, "Condition not found in resource", logger.Fields{
			"resource":  fmt.Sprintf("%s/%s", rs.Kind, rs.Name),
			"condition": check,
		})
	}

	log.Debug(ctx, "No matching resource found for condition", logger.Fields{
		"kind":      kind,
		"condition": check,
	})
	return false
}

// evaluateComparison evaluates a comparison like "succeeded>=1" or "status.phase=Active"
func evaluateComparison(feedback map[string]interface{}, check, operator string) bool {
	parts := strings.SplitN(check, operator, 2)
	if len(parts) != 2 {
		return false
	}

	fieldPath := strings.TrimSpace(parts[0])
	expectedValue := strings.TrimSpace(parts[1])

	// Get the actual value from feedback
	actualValue := getValueFromPath(feedback, fieldPath)
	if actualValue == nil {
		return false
	}

	// Compare based on operator
	switch operator {
	case "=":
		return compareEqual(actualValue, expectedValue)
	case ">=":
		return compareNumeric(actualValue, expectedValue, ">=")
	case "<=":
		return compareNumeric(actualValue, expectedValue, "<=")
	case ">":
		return compareNumeric(actualValue, expectedValue, ">")
	case "<":
		return compareNumeric(actualValue, expectedValue, "<")
	}

	return false
}

// getValueFromPath gets a value from nested map using dot notation
// e.g., "status.succeeded" from {"status": {"succeeded": 1}}
func getValueFromPath(data map[string]interface{}, path string) interface{} {
	if data == nil {
		return nil
	}

	parts := strings.Split(path, ".")
	var current interface{} = data

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[part]
		default:
			return nil
		}
	}

	return current
}

// compareEqual compares for equality
func compareEqual(actual interface{}, expected string) bool {
	switch v := actual.(type) {
	case string:
		return strings.EqualFold(v, expected)
	case bool:
		return fmt.Sprintf("%v", v) == expected
	case float64:
		expectedNum, err := strconv.ParseFloat(expected, 64)
		if err != nil {
			return fmt.Sprintf("%v", v) == expected
		}
		return v == expectedNum
	case int, int64:
		return fmt.Sprintf("%v", v) == expected
	default:
		return fmt.Sprintf("%v", v) == expected
	}
}

// compareNumeric compares numeric values
func compareNumeric(actual interface{}, expected string, operator string) bool {
	var actualNum float64

	switch v := actual.(type) {
	case float64:
		actualNum = v
	case int:
		actualNum = float64(v)
	case int64:
		actualNum = float64(v)
	case string:
		var err error
		actualNum, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return false
		}
	default:
		return false
	}

	expectedNum, err := strconv.ParseFloat(expected, 64)
	if err != nil {
		return false
	}

	switch operator {
	case ">=":
		return actualNum >= expectedNum
	case "<=":
		return actualNum <= expectedNum
	case ">":
		return actualNum > expectedNum
	case "<":
		return actualNum < expectedNum
	}

	return false
}

// createTLSConfig creates TLS configuration for gRPC connection
func createTLSConfig(config ClientConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load server CA for verification
	caCertPool := x509.NewCertPool()
	serverCALoaded := false
	brokerCALoaded := false

	if config.GRPCServerCAFile != "" {
		caCert, err := os.ReadFile(config.GRPCServerCAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read server CA file: %w", err)
		}

		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, fmt.Errorf("failed to parse server CA certificate")
		}
		serverCALoaded = true
	}

	// Load broker CA for verification (if different from server CA)
	if config.GRPCBrokerCAFile != "" {
		caCert, err := os.ReadFile(config.GRPCBrokerCAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read broker CA file: %w", err)
		}

		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, fmt.Errorf("failed to parse broker CA certificate")
		}
		brokerCALoaded = true
	}

	// Set RootCAs if any CA certificates were loaded
	if serverCALoaded || brokerCALoaded {
		tlsConfig.RootCAs = caCertPool
	}

	// Load client certificate for mTLS
	if config.GRPCClientCertFile != "" {
		if config.GRPCClientKeyFile == "" {
			return nil, fmt.Errorf("client key file required when client cert file is provided")
		}

		clientCert, err := tls.LoadX509KeyPair(config.GRPCClientCertFile, config.GRPCClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{clientCert}
	}

	return tlsConfig, nil
}

// getToken retrieves the authentication token from config or environment
func getToken(config ClientConfig) string {
	// Priority: direct token > token file > environment
	if config.GRPCClientToken != "" {
		return config.GRPCClientToken
	}

	if config.GRPCClientTokenFile != "" {
		if tokenBytes, err := os.ReadFile(config.GRPCClientTokenFile); err == nil {
			return strings.TrimSpace(string(tokenBytes))
		}
	}

	return os.Getenv("MAESTRO_GRPC_TOKEN")
}

// validateSearchQuery validates search query parameters to prevent SQL injection
// This is a defensive measure - Maestro should have its own validation, but this provides extra safety
func validateSearchQuery(value string) error {
	// Allow only alphanumeric characters, hyphens, underscores, and dots
	// This matches typical Kubernetes resource naming conventions
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return fmt.Errorf("invalid character '%c' in search query, only alphanumeric, hyphens, underscores, and dots allowed", r)
		}
	}

	// Check for reasonable length limits
	if len(value) == 0 {
		return fmt.Errorf("search query cannot be empty")
	}
	if len(value) > 253 { // Max length for Kubernetes resource names
		return fmt.Errorf("search query too long: %d characters (max 253)", len(value))
	}

	return nil
}
