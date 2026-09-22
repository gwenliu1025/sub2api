package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 自定义中转的Messages预估与既有Responses计数采用同一估算规则，不请求缺失端点。
func TestMessagesCountTokensCustomRelayUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-5.3","system":"Be precise.","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)
	for _, input := range [][]byte{body, []byte(`{"model":`)} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(input))
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"not found"}}`))}}
		account := &Account{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://relay.example.com"}}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}}
		err := svc.ForwardCountTokensAsAnthropic(context.Background(), c, account, input, "")
		if bytes.Equal(input, body) {
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			prepared, err := prepareOpenAIInputTokensCountRequest(body, account, "")
			require.NoError(t, err)
			estimate, err := estimateOpenAIInputTokens(prepared.Request)
			require.NoError(t, err)
			require.Positive(t, estimate)
			require.EqualValues(t, estimate, gjson.Get(rec.Body.String(), "input_tokens").Int())
		} else {
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		}
		require.Nil(t, upstream.lastReq, "本地估算及输入拒收均不应触发上游副作用")
	}
}
