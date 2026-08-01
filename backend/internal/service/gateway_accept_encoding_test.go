package service

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayService_AnthropicRequests_DropClientAcceptEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra = nil
	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)

	tests := []struct {
		name  string
		build func() (*http.Request, error)
	}{
		{
			name: "messages",
			build: func() (*http.Request, error) {
				req, _, err := svc.buildUpstreamRequest(
					context.Background(), c, account, body, "key", "apikey", "claude-opus-4-8", true, false,
				)
				return req, err
			},
		},
		{
			name: "API Key passthrough messages",
			build: func() (*http.Request, error) {
				req, _, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(
					context.Background(), c, account, body, "key",
				)
				return req, err
			},
		},
		{
			name: "count_tokens",
			build: func() (*http.Request, error) {
				req, _, err := svc.buildCountTokensRequest(
					context.Background(), c, account, body, "key", "apikey", "claude-opus-4-8", false,
				)
				return req, err
			},
		},
		{
			name: "API Key passthrough count_tokens",
			build: func() (*http.Request, error) {
				return svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(
					context.Background(), c, account, body, "key",
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := tt.build()
			require.NoError(t, err)
			require.Empty(t, getHeaderRaw(req.Header, "accept-encoding"))
			for key := range req.Header {
				require.NotEqual(t, "accept-encoding", strings.ToLower(key))
			}
		})
	}
}

func TestGatewayService_AnthropicAcceptEncoding_TransportNegotiatesOnceAndDecompressesSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	receivedEncodings := make(chan []string, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedEncodings <- append([]string(nil), r.Header.Values("Accept-Encoding")...)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		_ = writer.Close()
	}))
	t.Cleanup(upstream.Close)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
	account := newAnthropicAPIKeyAccountForTest()
	account.Credentials["base_url"] = upstream.URL
	account.Extra = nil

	req, _, err := svc.buildUpstreamRequest(
		context.Background(), c, account,
		[]byte(`{"model":"claude-opus-4-8","messages":[]}`),
		"key", "apikey", "claude-opus-4-8", true, false,
	)
	require.NoError(t, err)

	client := upstream.Client()
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, []string{"gzip"}, <-receivedEncodings)
	require.Contains(t, string(responseBody), "data:")
	require.NotEqual(t, byte(0x1f), responseBody[0])
}
