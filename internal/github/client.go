// Package github provides a GitHub API client for PR lifecycle management.
//
// It wraps the GitHub REST API v3 and GraphQL API v4 for operations needed
// by the Gas Town merge queue: creating draft PRs, managing reviews,
// converting drafts to ready, and merging.
//
// Authentication uses a GITHUB_TOKEN environment variable.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const (
	defaultRESTBase    = "https://api.github.com"
	defaultGraphQLBase = "https://api.github.com/graphql"
)

// Client wraps HTTP interactions with GitHub's REST and GraphQL APIs.
type Client struct {
	httpClient  *http.Client
	token       string
	restBase    string
	graphqlBase string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client (useful for testing).
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithToken overrides the token (default: GITHUB_TOKEN env var).
func WithToken(t string) Option {
	return func(cl *Client) { cl.token = t }
}

// WithRESTBase overrides the REST API base URL (for testing).
func WithRESTBase(url string) Option {
	return func(cl *Client) { cl.restBase = url }
}

// WithGraphQLBase overrides the GraphQL API base URL (for testing).
func WithGraphQLBase(url string) Option {
	return func(cl *Client) { cl.graphqlBase = url }
}

// NewClient creates a GitHub API client.
// By default it reads GITHUB_TOKEN from the environment.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		httpClient:  http.DefaultClient,
		token:       os.Getenv("GITHUB_TOKEN"),
		restBase:    defaultRESTBase,
		graphqlBase: defaultGraphQLBase,
	}
	for _, o := range opts {
		o(c)
	}
	if c.token == "" {
		return nil, fmt.Errorf("github: GITHUB_TOKEN is required (set env var or use WithToken)")
	}
	return c, nil
}

// Request makes an authenticated REST API request and decodes the JSON response.
func (c *Client) Request(ctx context.Context, method, path string, body any, result any) error {
	reqBody, err := marshalRequestBody(body)
	if err != nil {
		return err
	}
	req, err := c.newRESTRequest(ctx, method, path, body != nil, reqBody)
	if err != nil {
		return err
	}
	respBody, err := c.doRESTRequest(req, method, path)
	if err != nil {
		return err
	}
	return decodeJSONResult(respBody, result)
}

func marshalRequestBody(body any) (io.Reader, error) {
	if body == nil {
		return nil, nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("github: marshal request: %w", err)
	}
	return bytes.NewReader(b), nil
}

func (c *Client) newRESTRequest(ctx context.Context, method, path string, hasBody bool, reqBody io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.restBase+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("github: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) doRESTRequest(req *http.Request, method, path string) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	respBody, err := readClosedBody(resp)
	if err != nil {
		return nil, fmt.Errorf("github: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	return respBody, nil
}

func readClosedBody(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

func decodeJSONResult(respBody []byte, result any) error {
	if result == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("github: decode response: %w", err)
	}
	return nil
}

// GraphQLRequest makes an authenticated GraphQL request.
func (c *Client) GraphQLRequest(ctx context.Context, query string, variables map[string]any, result any) error {
	req, err := c.newGraphQLRequest(ctx, query, variables)
	if err != nil {
		return err
	}
	respBody, err := c.doGraphQLRequest(req)
	if err != nil {
		return err
	}
	return decodeGraphQLResult(respBody, result)
}

func (c *Client) newGraphQLRequest(ctx context.Context, query string, variables map[string]any) (*http.Request, error) {
	b, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, fmt.Errorf("github: marshal graphql: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlBase, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("github: create graphql request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (c *Client) doGraphQLRequest(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: graphql: %w", err)
	}
	respBody, err := readClosedBody(resp)
	if err != nil {
		return nil, fmt.Errorf("github: read graphql response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{
			Method:     "POST",
			Path:       "/graphql",
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	return respBody, nil
}

func decodeGraphQLResult(respBody []byte, result any) error {
	var gqlResp graphQLResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return fmt.Errorf("github: decode graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return fmt.Errorf("github: graphql: %s", gqlResp.Errors[0].Message)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(gqlResp.Data, result); err != nil {
		return fmt.Errorf("github: decode graphql data: %w", err)
	}
	return nil
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// APIError represents a non-2xx response from the GitHub API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}
