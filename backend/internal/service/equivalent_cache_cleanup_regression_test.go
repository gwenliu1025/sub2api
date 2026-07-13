package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheCleanupAccountExtraCannotRewriteAnthropicUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		model        = "claude-sonnet-4"
		upstreamBody = `{"id":"msg_cleanup","type":"message","model":"claude-sonnet-4","usage":{"input_tokens":2000,"output_tokens":8000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}}`
	)
	expectedUsage := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	oldV2Config := map[string]any{
		"enabled":                true,
		"mode":                   "active",
		"pricing_profile":        "kiro_unified_5_25_0_6_6_25_10",
		"visible_rate_min":       0.96,
		"visible_rate_max":       0.999,
		"kiro_go_pool_confirmed": true,
	}

	tests := []struct {
		name  string
		extra map[string]any
	}{
		{
			name: "V1 与 V2 旧配置同时存在",
			extra: map[string]any{
				"anthropic_passthrough":            true,
				"equivalent_cache_billing_enabled": true,
				"equivalent_cache_allocation_v2":   oldV2Config,
			},
		},
		{
			name: "仅残留 V2 旧配置",
			extra: map[string]any{
				"anthropic_passthrough":          true,
				"equivalent_cache_allocation_v2": oldV2Config,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			requestBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(requestBody), PlatformAnthropic)
			require.NoError(t, err)
			groupID := int64(301)
			parsed.GroupID = &groupID
			parsed.SessionContext = &SessionContext{APIKeyID: 401}

			upstream := &anthropicHTTPUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(upstreamBody)),
				},
			}
			billing := &BillingService{fallbackPrices: map[string]*ModelPricing{
				model: {
					InputPricePerToken:     5e-6,
					OutputPricePerToken:    25e-6,
					CacheReadPricePerToken: 0.6e-6,
					CacheCreation5mPrice:   6.25e-6,
					CacheCreation1hPrice:   10e-6,
					SupportsCacheBreakdown: true,
				},
			}}
			channelService := &ChannelService{}
			channelCache := newEmptyChannelCache()
			channelCache.loadedAt = time.Now()
			channelService.cache.Store(channelCache)
			svc := &GatewayService{
				cfg:              &config.Config{},
				httpUpstream:     upstream,
				rateLimitService: &RateLimitService{},
				resolver:         NewModelPricingResolver(channelService, billing),
			}
			account := &Account{
				ID:          201,
				Name:        "kiro-apikey-cleanup-regression",
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{
					"api_key":  "test-upstream-key",
					"base_url": "https://api.anthropic.com",
				},
				Extra:       tt.extra,
				Status:      StatusActive,
				Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, parsed)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.JSONEq(t, upstreamBody, rec.Body.String(), "旧账户 Extra 不得改写同步响应体")
			require.Equal(t, expectedUsage, result.Usage, "计费用 ClaudeUsage 必须完全等于上游原生 usage")
		})
	}
}
