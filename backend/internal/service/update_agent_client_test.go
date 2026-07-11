//go:build unit

package service

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const updateAgentTestRepository = "ghcr.io/gwenliu1025/sub2api"

var preparedUpdateAgentStatus = UpdateAgentStatus{
	State:         UpdateAgentPrepared,
	CurrentImage:  "xqian/sub2api:equivalent-cache-20260709-083433",
	TargetImage:   "ghcr.io/gwenliu1025/sub2api:0.1.150",
	PreviousImage: "xqian/sub2api:equivalent-cache-20260709-083433",
	Message:       "image prepared",
	UpdatedAt:     "2026-07-10T08:00:00Z",
}

func TestUnixUpdateAgentClientPrepareUsesUnixSocketAndExactVersion(t *testing.T) {
	socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/prepare", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.JSONEq(t, `{"version":"0.1.150"}`, string(body))

		writeUpdateAgentTestJSON(t, w, http.StatusOK, preparedUpdateAgentStatus)
	}))
	client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

	status, err := client.Prepare(context.Background(), "  0.1.150  ")

	require.NoError(t, err)
	require.Equal(t, preparedUpdateAgentStatus, *status)
}

func TestUnixUpdateAgentClientActivateAccepts202(t *testing.T) {
	want := UpdateAgentStatus{
		State:         UpdateAgentActivating,
		CurrentImage:  preparedUpdateAgentStatus.CurrentImage,
		TargetImage:   preparedUpdateAgentStatus.TargetImage,
		PreviousImage: preparedUpdateAgentStatus.PreviousImage,
		Message:       "activation started",
		UpdatedAt:     "2026-07-10T08:01:00Z",
	}
	socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/activate", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Empty(t, body)

		writeUpdateAgentTestJSON(t, w, http.StatusAccepted, want)
	}))
	client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

	status, err := client.Activate(context.Background())

	require.NoError(t, err)
	require.Equal(t, want, *status)
}

func TestUnixUpdateAgentClientStatusDecodesAllFields(t *testing.T) {
	want := UpdateAgentStatus{
		State:         UpdateAgentRolledBack,
		CurrentImage:  "ghcr.io/gwenliu1025/sub2api:0.1.149",
		TargetImage:   "ghcr.io/gwenliu1025/sub2api:0.1.150",
		PreviousImage: "xqian/sub2api:equivalent-cache-20260709-083433",
		Message:       "health check failed; rollback completed",
		UpdatedAt:     "2026-07-10T08:05:00Z",
	}
	socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/v1/status", r.URL.Path)
		writeUpdateAgentTestJSON(t, w, http.StatusOK, want)
	}))
	client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

	status, err := client.Status(context.Background())

	require.NoError(t, err)
	require.Equal(t, want, *status)
}

func TestUnixUpdateAgentClientRejectsInvalidSuccessStates(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "null", body: "null"},
		{name: "empty_object", body: "{}"},
		{name: "unknown_state", body: `{"state":"unknown"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.body)
			}))
			client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

			status, err := client.Status(context.Background())

			require.Nil(t, status)
			requireUpdateAgentApplicationError(t, err, http.StatusBadGateway, "UPDATE_AGENT_INVALID_RESPONSE")
		})
	}
}

func TestUnixUpdateAgentClientAcceptsAllDeclaredStatesWithoutImageFields(t *testing.T) {
	states := []UpdateAgentState{
		UpdateAgentIdle,
		UpdateAgentPreparing,
		UpdateAgentPrepared,
		UpdateAgentActivating,
		UpdateAgentHealthy,
		UpdateAgentRolledBack,
		UpdateAgentFailed,
		UpdateAgentRollbackFailed,
	}

	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeUpdateAgentTestJSON(t, w, http.StatusOK, map[string]UpdateAgentState{
					"state": state,
				})
			}))
			client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

			status, err := client.Status(context.Background())

			require.NoError(t, err)
			require.Equal(t, state, status.State)
			require.Empty(t, status.CurrentImage)
			require.Empty(t, status.TargetImage)
			require.Empty(t, status.PreviousImage)
		})
	}
}

func TestUnixUpdateAgentClientRejectsUnexpectedTargetRepository(t *testing.T) {
	response := preparedUpdateAgentStatus
	response.TargetImage = "registry.example/attacker/sub2api:latest"
	response.Message = "do-not-leak-agent-response"

	socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeUpdateAgentTestJSON(t, w, http.StatusOK, response)
	}))
	client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

	status, err := client.Prepare(context.Background(), "0.1.150")

	require.Nil(t, status)
	appErr := requireUpdateAgentApplicationError(t, err, http.StatusBadGateway, "UPDATE_TARGET_REPOSITORY_MISMATCH")
	require.NotContains(t, err.Error(), "registry.example")
	require.NotContains(t, err.Error(), "do-not-leak-agent-response")
	require.NotContains(t, appErr.Message, "registry.example")
	require.NotContains(t, appErr.Message, "do-not-leak-agent-response")
}

func TestUnixUpdateAgentClientMapsAgentErrors(t *testing.T) {
	tests := []struct {
		name       string
		agentCode  string
		agentHTTP  int
		wantHTTP   int
		wantReason string
	}{
		{
			name:       "busy",
			agentCode:  "AGENT_BUSY",
			agentHTTP:  http.StatusConflict,
			wantHTTP:   http.StatusConflict,
			wantReason: "UPDATE_AGENT_BUSY",
		},
		{
			name:       "invalid_version",
			agentCode:  "INVALID_VERSION",
			agentHTTP:  http.StatusBadRequest,
			wantHTTP:   http.StatusBadRequest,
			wantReason: "UPDATE_TARGET_INVALID",
		},
		{
			name:       "image_pull_failed",
			agentCode:  "IMAGE_PULL_FAILED",
			agentHTTP:  http.StatusBadGateway,
			wantHTTP:   http.StatusBadGateway,
			wantReason: "UPDATE_IMAGE_PULL_FAILED",
		},
		{
			name:       "image_verification_failed",
			agentCode:  "IMAGE_VERIFICATION_FAILED",
			agentHTTP:  http.StatusBadGateway,
			wantHTTP:   http.StatusBadGateway,
			wantReason: "UPDATE_IMAGE_VERIFICATION_FAILED",
		},
		{
			name:       "no_prepared_update",
			agentCode:  "NO_PREPARED_UPDATE",
			agentHTTP:  http.StatusConflict,
			wantHTTP:   http.StatusConflict,
			wantReason: "UPDATE_NOT_PREPARED",
		},
		{
			name:       "activation_in_progress",
			agentCode:  "ACTIVATION_IN_PROGRESS",
			agentHTTP:  http.StatusConflict,
			wantHTTP:   http.StatusConflict,
			wantReason: "UPDATE_ACTIVATION_IN_PROGRESS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeUpdateAgentTestJSON(t, w, tt.agentHTTP, map[string]string{
					"code":    tt.agentCode,
					"message": "agent detail is not the public error contract",
				})
			}))
			client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

			status, err := client.Prepare(context.Background(), "0.1.150")

			require.Nil(t, status)
			appErr := requireUpdateAgentApplicationError(t, err, tt.wantHTTP, tt.wantReason)
			require.NotEmpty(t, appErr.Message)
		})
	}

	t.Run("unknown_error_is_sanitized", func(t *testing.T) {
		const secret = "secret-agent-diagnostic"
		socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeUpdateAgentTestJSON(t, w, http.StatusTeapot, map[string]string{
				"code":    "UNRECOGNIZED_AGENT_ERROR",
				"message": secret,
			})
		}))
		client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

		status, err := client.Prepare(context.Background(), "0.1.150")

		require.Nil(t, status)
		appErr := requireUpdateAgentApplicationError(t, err, http.StatusBadGateway, "UPDATE_AGENT_ERROR")
		require.NotContains(t, err.Error(), secret)
		require.NotContains(t, appErr.Message, secret)
		require.NotContains(t, err.Error(), "UNRECOGNIZED_AGENT_ERROR")
	})

	t.Run("missing_socket_is_unavailable", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "missing.sock")
		client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

		status, err := client.Status(context.Background())

		require.Nil(t, status)
		requireUpdateAgentApplicationError(t, err, http.StatusServiceUnavailable, "UPDATE_AGENT_UNAVAILABLE")
	})

	t.Run("permission_error_is_distinct", func(t *testing.T) {
		err := mapUpdateAgentRequestError(context.Background(), &net.OpError{
			Op:  "dial",
			Net: "unix",
			Err: os.ErrPermission,
		})

		requireUpdateAgentApplicationError(t, err, http.StatusServiceUnavailable, "UPDATE_AGENT_PERMISSION_DENIED")
		require.ErrorIs(t, err, os.ErrPermission)
	})
}

func TestUnixUpdateAgentClientHonorsContextCancellation(t *testing.T) {
	t.Run("caller_cancellation", func(t *testing.T) {
		started := make(chan struct{})
		var startedOnce sync.Once
		socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			startedOnce.Do(func() { close(started) })
			<-r.Context().Done()
		}))
		client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)

		go func() {
			_, err := client.Status(ctx)
			errCh <- err
		}()

		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("request did not reach Unix socket server")
		}
		cancel()

		select {
		case err := <-errCh:
			requireUpdateAgentApplicationError(t, err, 499, "UPDATE_AGENT_REQUEST_CANCELED")
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("client did not return after context cancellation")
		}
	})

	t.Run("context_deadline", func(t *testing.T) {
		socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		status, err := client.Status(ctx)

		require.Nil(t, status)
		requireUpdateAgentApplicationError(t, err, http.StatusGatewayTimeout, "UPDATE_AGENT_TIMEOUT")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("http_client_timeout", func(t *testing.T) {
		socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		client := newUnixUpdateAgentTestClient(t, socketPath, 50*time.Millisecond)

		status, err := client.Status(context.Background())

		require.Nil(t, status)
		requireUpdateAgentApplicationError(t, err, http.StatusGatewayTimeout, "UPDATE_AGENT_TIMEOUT")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestUnixUpdateAgentClientMapsBodyReadCancellationAndTimeouts(t *testing.T) {
	t.Run("caller_cancellation", func(t *testing.T) {
		started := make(chan struct{})
		socketPath := startUnixUpdateAgentTestServer(t, blockingUpdateAgentBodyHandler(t, started))
		client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)

		go func() {
			_, err := client.Status(ctx)
			errCh <- err
		}()

		waitForUpdateAgentBodyRead(t, started)
		cancel()

		err := waitForUpdateAgentError(t, errCh)
		requireUpdateAgentApplicationError(t, err, 499, "UPDATE_AGENT_REQUEST_CANCELED")
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("context_deadline", func(t *testing.T) {
		started := make(chan struct{})
		socketPath := startUnixUpdateAgentTestServer(t, blockingUpdateAgentBodyHandler(t, started))
		client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		errCh := make(chan error, 1)

		go func() {
			_, err := client.Status(ctx)
			errCh <- err
		}()

		waitForUpdateAgentBodyRead(t, started)

		err := waitForUpdateAgentError(t, errCh)
		requireUpdateAgentApplicationError(t, err, http.StatusGatewayTimeout, "UPDATE_AGENT_TIMEOUT")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("http_client_timeout", func(t *testing.T) {
		started := make(chan struct{})
		socketPath := startUnixUpdateAgentTestServer(t, blockingUpdateAgentBodyHandler(t, started))
		client := newUnixUpdateAgentTestClient(t, socketPath, 200*time.Millisecond)
		errCh := make(chan error, 1)

		go func() {
			_, err := client.Status(context.Background())
			errCh <- err
		}()

		waitForUpdateAgentBodyRead(t, started)

		err := waitForUpdateAgentError(t, errCh)
		requireUpdateAgentApplicationError(t, err, http.StatusGatewayTimeout, "UPDATE_AGENT_TIMEOUT")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestUnixUpdateAgentClientLimitsResponseBody(t *testing.T) {
	validPrefix, err := json.Marshal(preparedUpdateAgentStatus)
	require.NoError(t, err)
	oversizedBody := append(
		validPrefix,
		[]byte(strings.Repeat(" ", (1<<20)+1-len(validPrefix)))...,
	)

	socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(oversizedBody)
	}))
	client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

	status, err := client.Status(context.Background())

	require.Nil(t, status)
	appErr := requireUpdateAgentApplicationError(t, err, http.StatusBadGateway, "UPDATE_AGENT_INVALID_RESPONSE")
	require.Less(t, len(err.Error()), 1024)
	require.Less(t, len(appErr.Message), 256)
}

func TestUnixUpdateAgentClientDoesNotFollowRedirects(t *testing.T) {
	for _, redirectStatus := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(redirectStatus), func(t *testing.T) {
			var targetRequests atomic.Int32
			socketPath := startUnixUpdateAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/status":
					http.Redirect(w, r, "/redirect-target", redirectStatus)
				case "/redirect-target":
					targetRequests.Add(1)
					writeUpdateAgentTestJSON(t, w, http.StatusOK, preparedUpdateAgentStatus)
				default:
					http.NotFound(w, r)
				}
			}))
			client := newUnixUpdateAgentTestClient(t, socketPath, 2*time.Second)

			status, err := client.Status(context.Background())

			assert.Nil(t, status)
			assert.Zero(t, targetRequests.Load())
			requireUpdateAgentApplicationError(t, err, http.StatusBadGateway, "UPDATE_AGENT_ERROR")
		})
	}
}

func newUnixUpdateAgentTestClient(t *testing.T, socketPath string, timeout time.Duration) *UnixUpdateAgentClient {
	t.Helper()
	client := NewUnixUpdateAgentClient(socketPath, updateAgentTestRepository, timeout)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func startUnixUpdateAgentTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "sub2api-update-agent-")
	require.NoError(t, err)
	socketPath := filepath.Join(dir, "agent.sock")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil && !stderrors.Is(err, context.DeadlineExceeded) {
			t.Errorf("shutdown Unix socket test server: %v", err)
		}
		_ = listener.Close()

		select {
		case err := <-serveDone:
			if err != nil && !stderrors.Is(err, http.ErrServerClosed) && !stderrors.Is(err, net.ErrClosed) {
				t.Errorf("serve Unix socket test server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Unix socket test server did not stop")
		}

		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove Unix socket test directory: %v", err)
		}
	})

	return socketPath
}

func blockingUpdateAgentBodyHandler(t *testing.T, started chan<- struct{}) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !assert.True(t, ok, "Unix socket response writer must support flushing") {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		close(started)
		<-r.Context().Done()
	})
}

func waitForUpdateAgentBodyRead(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not begin reading the response body")
	}
}

func waitForUpdateAgentError(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("update agent client did not return")
		return nil
	}
}

func writeUpdateAgentTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	assert.NoError(t, json.NewEncoder(w).Encode(value))
}

func requireUpdateAgentApplicationError(
	t *testing.T,
	err error,
	wantHTTP int,
	wantReason string,
) *infraerrors.ApplicationError {
	t.Helper()
	require.Error(t, err)

	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, int32(wantHTTP), appErr.Code)
	require.Equal(t, wantReason, appErr.Reason)
	return appErr
}
