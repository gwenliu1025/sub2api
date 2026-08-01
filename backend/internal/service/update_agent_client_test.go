//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type updateAgentRoundTripFunc func(*http.Request) (*http.Response, error)

func (f updateAgentRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestUnixUpdateAgentClientPrepareUsesUnixSocketAndExactVersion(t *testing.T) {
	client := &UnixUpdateAgentClient{
		httpClient: &http.Client{Transport: updateAgentRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "http://unix/v1/prepare", r.URL.String())
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var request struct {
				Version string `json:"version"`
			}
			require.NoError(t, json.Unmarshal(body, &request))
			require.Equal(t, "0.1.169", request.Version)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"state":"prepared","target_image":"ghcr.io/gwenliu1025/sub2api:0.1.169"}`)),
				Header:     make(http.Header),
			}, nil
		})},
		expectedRepository: "ghcr.io/gwenliu1025/sub2api",
	}
	status, err := client.Prepare(t.Context(), "v0.1.169")

	require.NoError(t, err)
	require.Equal(t, UpdateAgentPrepared, status.State)
	require.Equal(t, "ghcr.io/gwenliu1025/sub2api:0.1.169", status.TargetImage)
}

func TestUnixUpdateAgentClientRejectsUnexpectedTargetRepository(t *testing.T) {
	client := &UnixUpdateAgentClient{
		httpClient: &http.Client{Transport: updateAgentRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "http://unix/v1/prepare", r.URL.String())
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"state":"prepared","target_image":"ghcr.io/other/sub2api:0.1.169"}`))),
				Header:     make(http.Header),
			}, nil
		})},
		expectedRepository: "ghcr.io/gwenliu1025/sub2api",
	}
	_, err := client.Prepare(t.Context(), "0.1.169")

	require.Error(t, err)
	require.Contains(t, err.Error(), "UPDATE_TARGET_REPOSITORY_MISMATCH")
}
