// Package bitbucket provides a Bitbucket Cloud API client for PR lifecycle management.
//
// It wraps the Bitbucket Cloud REST API 2.0 for operations needed
// by the Gas Town merge queue: creating draft PRs, managing approvals,
// and merging.
//
// Authentication uses a BITBUCKET_TOKEN environment variable
// (API token or Repository Access Token with Bearer auth).
package bitbucket

import (
	"fmt"
	"net/http"
	"os"
)

const defaultRESTBase = "https://api.bitbucket.org/2.0"

// Client wraps HTTP interactions with Bitbucket Cloud's REST API 2.0.
type Client struct {
	httpClient *http.Client
	token      string
	restBase   string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client (useful for testing).
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithToken overrides the token (default: BITBUCKET_TOKEN env var).
func WithToken(t string) Option {
	return func(cl *Client) { cl.token = t }
}

// WithRESTBase overrides the REST API base URL (for testing).
func WithRESTBase(url string) Option {
	return func(cl *Client) { cl.restBase = url }
}

// NewClient creates a Bitbucket Cloud API client.
// By default it reads BITBUCKET_TOKEN from the environment.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		httpClient: http.DefaultClient,
		token:      os.Getenv("BITBUCKET_TOKEN"),
		restBase:   defaultRESTBase,
	}
	for _, o := range opts {
		o(c)
	}
	if c.token == "" {
		return nil, fmt.Errorf("bitbucket: BITBUCKET_TOKEN is required (set env var or use WithToken)")
	}
	return c, nil
}

// APIError represents a non-2xx response from the Bitbucket API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("bitbucket: %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}
