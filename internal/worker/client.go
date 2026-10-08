package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// HTTPClient calls a single private worker over authenticated HTTP.
type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHTTPClient(baseURL, token string, timeout time.Duration) (*HTTPClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, errors.New("worker URL must be an absolute http URL")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("worker token must not be empty")
	}
	return &HTTPClient{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, client: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *HTTPClient) Start(ctx context.Context, input StartRequest) (StartResult, error) {
	var result StartResult
	if err := c.doJSON(ctx, http.MethodPost, "/internal/deployments/start", input, &result); err != nil {
		return StartResult{}, err
	}
	return result, nil
}

func (c *HTTPClient) Stop(ctx context.Context, containerID string) error {
	return c.doJSON(ctx, http.MethodPost, "/internal/deployments/stop", map[string]string{"container_id": containerID}, nil)
}

func (c *HTTPClient) Status(ctx context.Context, deploymentID string, version int) (Status, error) {
	var status Status
	endpoint := path.Join("/internal/deployments", deploymentID, "status") + fmt.Sprintf("?version=%d", version)
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func (c *HTTPClient) Resources(ctx context.Context) (Resources, error) {
	var resources Resources
	if err := c.doJSON(ctx, http.MethodGet, "/internal/resources", nil, &resources); err != nil {
		return Resources{}, err
	}
	return resources, nil
}

func (c *HTTPClient) Cleanup(ctx context.Context, id string, version int) error {
	return c.doJSON(ctx, http.MethodPost, "/internal/deployments/cleanup", map[string]any{"deployment_id": id, "version": version}, nil)
}

func (c *HTTPClient) doJSON(ctx context.Context, method, endpoint string, input, output any) error {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return fmt.Errorf("encode worker request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, &body)
	if err != nil {
		return fmt.Errorf("create worker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("call worker: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("worker returned %s", response.Status)
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			return fmt.Errorf("decode worker response: %w", err)
		}
	}
	return nil
}
