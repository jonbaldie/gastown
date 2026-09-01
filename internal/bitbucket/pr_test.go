package bitbucket

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newTestClient creates a Client pointing at a test HTTP server.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(
		WithToken("test-token"),
		WithHTTPClient(srv.Client()),
		WithRESTBase(srv.URL),
	)
	require.NoError(t, err)
	return c, srv
}

func TestNewClient_RequiresToken(t *testing.T) {
	t.Setenv("BITBUCKET_TOKEN", "")
	_, err := NewClient()
	assert.ErrorContains(t, err, "BITBUCKET_TOKEN is required")
}

func TestNewClient_FromEnv(t *testing.T) {
	t.Setenv("BITBUCKET_TOKEN", "env-token")
	c, err := NewClient()
	require.NoError(t, err)
	assert.Equal(t, "env-token", c.token)
}

func TestCreateDraftPR(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repositories/myws/myrepo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, true, body["draft"])
		assert.Equal(t, "Add feature", body["title"])

		source := body["source"].(map[string]any)
		srcBranch := source["branch"].(map[string]any)
		assert.Equal(t, "feat-branch", srcBranch["name"])

		dest := body["destination"].(map[string]any)
		destBranch := dest["branch"].(map[string]any)
		assert.Equal(t, "main", destBranch["name"])

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 42,
			"links": map[string]any{
				"html": map[string]any{
					"href": "https://bitbucket.org/myws/myrepo/pull-requests/42",
				},
			},
		})
	})

	c, _ := newTestClient(t, mux)
	result, err := c.CreateDraftPR(t.Context(), "myws", "myrepo", "feat-branch", "main", "Add feature", "Description")
	require.NoError(t, err)
	assert.Equal(t, 42, result.ID)
	assert.Equal(t, "https://bitbucket.org/myws/myrepo/pull-requests/42", result.URL)
}

func TestUpdatePRDescription(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "Updated body", body["description"])
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})

	c, _ := newTestClient(t, mux)
	err := c.UpdatePRDescription(t.Context(), "myws", "myrepo", 42, "Updated body")
	require.NoError(t, err)
}

func TestGetPRApprovalStatus_Approved(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{
				{"role": "REVIEWER", "approved": true, "state": "approved", "user": map[string]any{"display_name": "alice"}},
				{"role": "REVIEWER", "approved": true, "state": "approved", "user": map[string]any{"display_name": "bob"}},
			},
		})
	})

	c, _ := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewApproved, state)
}

func TestGetPRApprovalStatus_ChangesRequested(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{
				{"role": "REVIEWER", "approved": true, "state": "approved", "user": map[string]any{"display_name": "alice"}},
				{"role": "REVIEWER", "approved": false, "state": "changes_requested", "user": map[string]any{"display_name": "bob"}},
			},
		})
	})

	c, _ := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewChangesRequired, state)
}

func TestGetPRApprovalStatus_NoParticipants(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{},
		})
	})

	c, _ := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewPending, state)
}

func TestGetPRApprovalStatus_NonReviewerIgnored(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{
				{"role": "AUTHOR", "approved": false, "state": "", "user": map[string]any{"display_name": "author"}},
				{"role": "PARTICIPANT", "approved": true, "state": "approved", "user": map[string]any{"display_name": "viewer"}},
			},
		})
	})

	c, _ := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewPending, state)
}

func TestGetPRComments(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42/comments", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"values": []map[string]any{
				{
					"id":         101,
					"content":    map[string]any{"raw": "Fix this"},
					"inline":     map[string]any{"path": "main.go", "to": 10},
					"created_on": "2026-01-01T00:00:00+00:00",
					"user":       map[string]any{"display_name": "alice"},
					"links":      map[string]any{"html": map[string]any{"href": "https://bitbucket.org/myws/myrepo/pull-requests/42#comment-101"}},
				},
			},
		})
	})

	c, _ := newTestClient(t, mux)
	comments, err := c.GetPRComments(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, int64(101), comments[0].ID)
	assert.Equal(t, "Fix this", comments[0].Body)
	assert.Equal(t, "alice", comments[0].User)
	assert.Equal(t, "main.go", comments[0].Path)
	assert.Equal(t, 10, comments[0].Line)
}

func TestReplyToPRComment(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repositories/myws/myrepo/pullrequests/42/comments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		content := body["content"].(map[string]any)
		assert.Equal(t, "Thanks, fixed!", content["raw"])
		parent := body["parent"].(map[string]any)
		assert.Equal(t, float64(101), parent["id"])
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	})

	c, _ := newTestClient(t, mux)
	err := c.ReplyToPRComment(t.Context(), "myws", "myrepo", 42, 101, "Thanks, fixed!")
	require.NoError(t, err)
}

func TestMergePR(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repositories/myws/myrepo/pullrequests/42/merge", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "squash", body["merge_strategy"])
		assert.Equal(t, false, body["close_source_branch"])
		json.NewEncoder(w).Encode(map[string]any{"state": "MERGED"})
	})

	c, _ := newTestClient(t, mux)
	err := c.MergePR(t.Context(), "myws", "myrepo", 42, "squash")
	require.NoError(t, err)
}

func TestGetRepoMergeStrategies(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/branching-model", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"development": map[string]any{
				"merge_strategy": "fast_forward",
			},
		})
	})

	c, _ := newTestClient(t, mux)
	strategy, err := c.GetRepoMergeStrategies(t.Context(), "myws", "myrepo")
	require.NoError(t, err)
	assert.Equal(t, "fast_forward", strategy)
}

func TestGetRepoMergeStrategies_DefaultsToSquash(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/branching-model", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Not Found"}}`))
	})

	c, _ := newTestClient(t, mux)
	strategy, err := c.GetRepoMergeStrategies(t.Context(), "myws", "myrepo")
	require.NoError(t, err)
	assert.Equal(t, "squash", strategy)
}

func TestAPIError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/999", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Not Found"}}`))
	})

	c, _ := newTestClient(t, mux)
	_, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 999)
	require.Error(t, err)

	var apiErr *APIError
	assert.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 404, apiErr.StatusCode)
}

func TestRequestPropagatesConstructionAndTransportErrors(t *testing.T) {
	t.Run("marshal request body", func(t *testing.T) {
		client, err := NewClient(WithToken("test-token"))
		require.NoError(t, err)
		err = client.Request(t.Context(), http.MethodPost, "/path", make(chan int), nil)
		require.ErrorContains(t, err, "marshal request")
	})

	t.Run("create request", func(t *testing.T) {
		client, err := NewClient(WithToken("test-token"), WithRESTBase(":"))
		require.NoError(t, err)
		err = client.Request(t.Context(), http.MethodGet, "/path", nil, nil)
		require.ErrorContains(t, err, "create request")
	})

	t.Run("HTTP transport", func(t *testing.T) {
		transportErr := errors.New("transport failed")
		httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})}
		client, err := NewClient(WithToken("test-token"), WithHTTPClient(httpClient))
		require.NoError(t, err)
		err = client.Request(t.Context(), http.MethodGet, "/path", nil, nil)
		require.ErrorIs(t, err, transportErr)
		require.ErrorContains(t, err, "GET /path")
	})
}

func TestRequestBodyNil(t *testing.T) {
	body, err := requestBody(nil)
	require.NoError(t, err)
	assert.Nil(t, body)
}

func TestPROperationsWrapAPIErrors(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "failed", http.StatusInternalServerError)
	}))
	tests := []struct {
		name       string
		wantPrefix string
		run        func() error
	}{
		{
			name:       "create draft",
			wantPrefix: "create draft PR",
			run: func() error {
				_, err := client.CreateDraftPR(t.Context(), "ws", "repo", "source", "main", "title", "description")
				return err
			},
		},
		{
			name:       "update description",
			wantPrefix: "update PR description",
			run: func() error {
				return client.UpdatePRDescription(t.Context(), "ws", "repo", 1, "description")
			},
		},
		{
			name:       "get comments",
			wantPrefix: "get PR comments",
			run: func() error {
				_, err := client.GetPRComments(t.Context(), "ws", "repo", 1)
				return err
			},
		},
		{
			name:       "reply to comment",
			wantPrefix: "reply to PR comment",
			run: func() error {
				return client.ReplyToPRComment(t.Context(), "ws", "repo", 1, 2, "reply")
			},
		},
		{
			name:       "merge",
			wantPrefix: "merge PR",
			run: func() error {
				return client.MergePR(t.Context(), "ws", "repo", 1, "squash")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorContains(t, tt.run(), tt.wantPrefix)
		})
	}
}
