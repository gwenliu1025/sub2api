package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	updateAgentBaseURL              = "http://unix"
	updateAgentMaxResponseBodyBytes = int64(1 << 20)
)

type UpdateAgentState string

const (
	UpdateAgentIdle           UpdateAgentState = "idle"
	UpdateAgentPreparing      UpdateAgentState = "preparing"
	UpdateAgentPrepared       UpdateAgentState = "prepared"
	UpdateAgentActivating     UpdateAgentState = "activating"
	UpdateAgentHealthy        UpdateAgentState = "healthy"
	UpdateAgentRolledBack     UpdateAgentState = "rolled_back"
	UpdateAgentFailed         UpdateAgentState = "failed"
	UpdateAgentRollbackFailed UpdateAgentState = "rollback_failed"
)

type UpdateAgentStatus struct {
	State         UpdateAgentState `json:"state"`
	CurrentImage  string           `json:"current_image"`
	TargetImage   string           `json:"target_image"`
	PreviousImage string           `json:"previous_image"`
	Message       string           `json:"message"`
	UpdatedAt     string           `json:"updated_at"`
}

type UpdateAgentClient interface {
	Prepare(ctx context.Context, version string) (*UpdateAgentStatus, error)
	Activate(ctx context.Context) (*UpdateAgentStatus, error)
	Status(ctx context.Context) (*UpdateAgentStatus, error)
}

type UnixUpdateAgentClient struct {
	httpClient         *http.Client
	transport          *http.Transport
	expectedRepository string
}

func NewUnixUpdateAgentClient(
	socketPath string,
	expectedRepository string,
	timeout time.Duration,
) *UnixUpdateAgentClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}

	return &UnixUpdateAgentClient{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		transport:          transport,
		expectedRepository: strings.TrimSpace(expectedRepository),
	}
}

func (c *UnixUpdateAgentClient) Prepare(ctx context.Context, version string) (*UpdateAgentStatus, error) {
	normalizedVersion := normalizeUpdateAgentVersion(version)
	body, err := json.Marshal(struct {
		Version string `json:"version"`
	}{
		Version: normalizedVersion,
	})
	if err != nil {
		return nil, infraerrors.InternalServer(
			"UPDATE_AGENT_REQUEST_INVALID",
			"failed to build update agent request",
		)
	}

	status, err := c.do(ctx, http.MethodPost, "/v1/prepare", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	expectedTarget := c.expectedRepository + ":" + normalizedVersion
	if status.TargetImage != expectedTarget {
		return nil, infraerrors.New(
			http.StatusBadGateway,
			"UPDATE_TARGET_REPOSITORY_MISMATCH",
			"update agent returned an unexpected target image",
		)
	}
	return status, nil
}

func (c *UnixUpdateAgentClient) Activate(ctx context.Context) (*UpdateAgentStatus, error) {
	return c.do(ctx, http.MethodPost, "/v1/activate", nil)
}

func (c *UnixUpdateAgentClient) Status(ctx context.Context) (*UpdateAgentStatus, error) {
	return c.do(ctx, http.MethodGet, "/v1/status", nil)
}

func (c *UnixUpdateAgentClient) CloseIdleConnections() {
	if c == nil || c.transport == nil {
		return
	}
	c.transport.CloseIdleConnections()
}

func (c *UnixUpdateAgentClient) do(
	ctx context.Context,
	method string,
	path string,
	body io.Reader,
) (*UpdateAgentStatus, error) {
	req, err := http.NewRequestWithContext(ctx, method, updateAgentBaseURL+path, body)
	if err != nil {
		return nil, infraerrors.InternalServer(
			"UPDATE_AGENT_REQUEST_INVALID",
			"failed to build update agent request",
		)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, mapUpdateAgentRequestError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := readUpdateAgentResponseBody(resp.Body)
	if err != nil {
		if isUpdateAgentCancellation(ctx, err) || isUpdateAgentTimeout(ctx, err) {
			return nil, mapUpdateAgentRequestError(ctx, err)
		}
		return nil, invalidUpdateAgentResponseError()
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, mapUpdateAgentResponseError(responseBody)
	}

	var status UpdateAgentStatus
	if err := json.Unmarshal(responseBody, &status); err != nil {
		return nil, invalidUpdateAgentResponseError()
	}
	if !isValidUpdateAgentState(status.State) {
		return nil, invalidUpdateAgentResponseError()
	}
	return &status, nil
}

func normalizeUpdateAgentVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func readUpdateAgentResponseBody(body io.Reader) ([]byte, error) {
	responseBody, err := io.ReadAll(io.LimitReader(body, updateAgentMaxResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(responseBody)) > updateAgentMaxResponseBodyBytes {
		return nil, errors.New("update agent response body too large")
	}
	return responseBody, nil
}

func isValidUpdateAgentState(state UpdateAgentState) bool {
	switch state {
	case UpdateAgentIdle,
		UpdateAgentPreparing,
		UpdateAgentPrepared,
		UpdateAgentActivating,
		UpdateAgentHealthy,
		UpdateAgentRolledBack,
		UpdateAgentFailed,
		UpdateAgentRollbackFailed:
		return true
	default:
		return false
	}
}

func invalidUpdateAgentResponseError() error {
	return infraerrors.New(
		http.StatusBadGateway,
		"UPDATE_AGENT_INVALID_RESPONSE",
		"update agent returned an invalid response",
	)
}

type updateAgentErrorResponse struct {
	Code    string                    `json:"code"`
	Message string                    `json:"message"`
	Error   *updateAgentErrorResponse `json:"error"`
}

func mapUpdateAgentResponseError(responseBody []byte) error {
	var response updateAgentErrorResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return genericUpdateAgentError()
	}

	code := strings.TrimSpace(response.Code)
	if code == "" && response.Error != nil {
		code = strings.TrimSpace(response.Error.Code)
	}

	switch code {
	case "AGENT_BUSY":
		return infraerrors.Conflict("UPDATE_AGENT_BUSY", "update agent is busy")
	case "INVALID_VERSION":
		return infraerrors.BadRequest("UPDATE_TARGET_INVALID", "update target version is invalid")
	case "IMAGE_PULL_FAILED":
		return infraerrors.New(
			http.StatusBadGateway,
			"UPDATE_IMAGE_PULL_FAILED",
			"update image pull failed",
		)
	case "IMAGE_VERIFICATION_FAILED":
		return infraerrors.New(
			http.StatusBadGateway,
			"UPDATE_IMAGE_VERIFICATION_FAILED",
			"update image verification failed",
		)
	case "NO_PREPARED_UPDATE":
		return infraerrors.Conflict("UPDATE_NOT_PREPARED", "no prepared update is available")
	case "ACTIVATION_IN_PROGRESS":
		return infraerrors.Conflict(
			"UPDATE_ACTIVATION_IN_PROGRESS",
			"update activation is already in progress",
		)
	default:
		return genericUpdateAgentError()
	}
}

func genericUpdateAgentError() error {
	return infraerrors.New(
		http.StatusBadGateway,
		"UPDATE_AGENT_ERROR",
		"update agent request failed",
	)
}

func mapUpdateAgentRequestError(ctx context.Context, err error) error {
	if isUpdateAgentCancellation(ctx, err) {
		return infraerrors.ClientClosed(
			"UPDATE_AGENT_REQUEST_CANCELED",
			"update agent request was canceled",
		).WithCause(context.Canceled)
	}
	if isUpdateAgentTimeout(ctx, err) {
		return updateAgentTimeoutError()
	}
	if errors.Is(err, os.ErrPermission) {
		return infraerrors.ServiceUnavailable(
			"UPDATE_AGENT_PERMISSION_DENIED",
			"permission denied while connecting to update agent",
		).WithCause(os.ErrPermission)
	}
	return infraerrors.ServiceUnavailable(
		"UPDATE_AGENT_UNAVAILABLE",
		"update agent is unavailable",
	)
}

func isUpdateAgentCancellation(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled)
}

func isUpdateAgentTimeout(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func updateAgentTimeoutError() error {
	return infraerrors.GatewayTimeout(
		"UPDATE_AGENT_TIMEOUT",
		"update agent request timed out",
	).WithCause(context.DeadlineExceeded)
}
