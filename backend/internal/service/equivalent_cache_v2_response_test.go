package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type equivalentCacheV2ResponseStateStoreStub struct {
	decision EquivalentCacheV2StateDecision
	err      error
	calls    int
}

func (s *equivalentCacheV2ResponseStateStoreStub) DecideAndUpdate(
	_ context.Context,
	_ EquivalentCacheV2StateInput,
) (EquivalentCacheV2StateDecision, error) {
	s.calls++
	return s.decision, s.err
}

func TestEquivalentCacheV2Response_ActiveRewritesOnlyStandardUsageFields(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"hello"}],
		"stop_reason":"end_turn",
		"usage":{
			"input_tokens":2000,
			"output_tokens":8000,
			"cache_read_input_tokens":0,
			"cache_creation_input_tokens":0,
			"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},
			"cached_tokens":0,
			"vendor_extension":{"kept":true}
		}
	}`)
	store := &equivalentCacheV2ResponseStateStoreStub{
		decision: EquivalentCacheV2StateDecision{Create: false, Generation: 3},
	}

	result := applyEquivalentCacheV2JSON(context.Background(), body, "usage", equivalentCacheV2ResponsePlan{
		Config: equivalentCacheV2Config{
			Enabled:             true,
			Mode:                equivalentCacheV2ModeActive,
			PricingProfile:      equivalentCacheV2PricingProfile,
			VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
			VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
			KiroGoPoolConfirmed: true,
		},
		AccountID:  701,
		APIKeyID:   501,
		SessionKey: "session-hash",
		RequestID:  "request-1",
		StateStore: store,
	})

	require.True(t, result.Allocated)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.Version)
	require.Equal(t, UsageAllocationKindReadMajor, result.Kind)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.RawUsage)
	require.Equal(t, 8000, result.ResponseUsage.OutputTokens)
	require.Equal(t,
		result.ResponseUsage.CacheCreation5mTokens+result.ResponseUsage.CacheCreation1hTokens,
		result.ResponseUsage.CacheCreationInputTokens,
	)
	require.Equal(t, equivalentCacheV2AllocationHeaderValue, result.HeaderValue())
	require.Equal(t, 1, store.calls)

	require.Equal(t, "msg_1", gjson.GetBytes(result.Body, "id").String())
	require.Equal(t, "message", gjson.GetBytes(result.Body, "type").String())
	require.Equal(t, "claude-sonnet-4-6", gjson.GetBytes(result.Body, "model").String())
	require.Equal(t, "hello", gjson.GetBytes(result.Body, "content.0.text").String())
	require.Equal(t, "end_turn", gjson.GetBytes(result.Body, "stop_reason").String())
	require.Equal(t, int64(0), gjson.GetBytes(result.Body, "usage.cached_tokens").Int())
	require.True(t, gjson.GetBytes(result.Body, "usage.vendor_extension.kept").Bool())

	require.Equal(t, int64(result.ResponseUsage.InputTokens), gjson.GetBytes(result.Body, "usage.input_tokens").Int())
	require.Equal(t, int64(8000), gjson.GetBytes(result.Body, "usage.output_tokens").Int())
	require.Equal(t, int64(result.ResponseUsage.CacheReadInputTokens), gjson.GetBytes(result.Body, "usage.cache_read_input_tokens").Int())
	require.Equal(t, int64(result.ResponseUsage.CacheCreationInputTokens), gjson.GetBytes(result.Body, "usage.cache_creation_input_tokens").Int())
	require.Equal(t, int64(result.ResponseUsage.CacheCreation5mTokens), gjson.GetBytes(result.Body, "usage.cache_creation.ephemeral_5m_input_tokens").Int())
	require.Equal(t, int64(result.ResponseUsage.CacheCreation1hTokens), gjson.GetBytes(result.Body, "usage.cache_creation.ephemeral_1h_input_tokens").Int())
}

func TestEquivalentCacheV2Response_ShadowComputesButReturnsRawBodyAndUsage(t *testing.T) {
	body := []byte(`{"id":"msg_shadow","usage":{"input_tokens":2000,"output_tokens":8000}}`)
	store := &equivalentCacheV2ResponseStateStoreStub{
		decision: EquivalentCacheV2StateDecision{Create: true, Generation: 4},
	}

	result := applyEquivalentCacheV2JSON(context.Background(), body, "usage", equivalentCacheV2ResponsePlan{
		Config: equivalentCacheV2Config{
			Enabled:             true,
			Mode:                equivalentCacheV2ModeShadow,
			PricingProfile:      equivalentCacheV2PricingProfile,
			VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
			VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
			KiroGoPoolConfirmed: true,
		},
		AccountID:  701,
		APIKeyID:   501,
		SessionKey: "session-hash",
		RequestID:  "request-shadow",
		StateStore: store,
	})

	require.False(t, result.Allocated)
	require.Equal(t, int16(0), result.Version)
	require.Equal(t, UsageAllocationKindNone, result.Kind)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.RawUsage)
	require.Equal(t, result.RawUsage, result.ResponseUsage)
	require.Equal(t, body, result.Body)
	require.Empty(t, result.HeaderValue())
	require.Equal(t, 1, store.calls)
}

func TestEquivalentCacheV2Response_InvalidOrRealCacheUsageFallsBackUntouched(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing input", body: `{"usage":{"output_tokens":8}}`},
		{name: "fractional input", body: `{"usage":{"input_tokens":2.5,"output_tokens":8}}`},
		{name: "negative output", body: `{"usage":{"input_tokens":2,"output_tokens":-1}}`},
		{name: "inconsistent creation", body: `{"usage":{"input_tokens":2,"output_tokens":8,"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":1}}}`},
		{name: "real standard cache", body: `{"usage":{"input_tokens":2,"output_tokens":8,"cache_read_input_tokens":1}}`},
		{name: "real vendor cache", body: `{"usage":{"input_tokens":2,"output_tokens":8,"cached_tokens":1}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			store := &equivalentCacheV2ResponseStateStoreStub{
				decision: EquivalentCacheV2StateDecision{Generation: 1},
			}
			result := applyEquivalentCacheV2JSON(context.Background(), body, "usage", equivalentCacheV2ResponsePlan{
				Config: equivalentCacheV2Config{
					Enabled:             true,
					Mode:                equivalentCacheV2ModeActive,
					PricingProfile:      equivalentCacheV2PricingProfile,
					VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
					VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
					KiroGoPoolConfirmed: true,
				},
				AccountID:  701,
				APIKeyID:   501,
				SessionKey: "session-hash",
				RequestID:  "request-invalid",
				StateStore: store,
			})

			require.False(t, result.Allocated)
			require.Equal(t, body, result.Body)
			require.Empty(t, result.HeaderValue())
			require.Equal(t, 0, store.calls, "state must not change before usage validation succeeds")
		})
	}
}

func TestEquivalentCacheV2Response_RedisFailureUsesStatelessReadMajor(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":2000,"output_tokens":8000}}`)
	store := &equivalentCacheV2ResponseStateStoreStub{err: errors.New("redis unavailable")}

	result := applyEquivalentCacheV2JSON(context.Background(), body, "usage", equivalentCacheV2ResponsePlan{
		Config: equivalentCacheV2Config{
			Enabled:             true,
			Mode:                equivalentCacheV2ModeActive,
			PricingProfile:      equivalentCacheV2PricingProfile,
			VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
			VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
			KiroGoPoolConfirmed: true,
		},
		AccountID:  701,
		APIKeyID:   501,
		SessionKey: "session-hash",
		RequestID:  "request-stateless",
		StateStore: store,
	})

	require.True(t, result.Allocated)
	require.Equal(t, UsageAllocationKindStateless, result.Kind)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.Version)
	require.Equal(t, 8000, result.ResponseUsage.OutputTokens)
	require.Equal(t, equivalentCacheV2AllocationHeaderValue, result.HeaderValue())
	require.Equal(t, 1, store.calls)
}

func TestEquivalentCacheV2Response_ResolvedPricingMustBeExactFlatTokenProfile(t *testing.T) {
	exact := &ResolvedPricing{
		Mode: BillingModeToken,
		BasePricing: &ModelPricing{
			InputPricePerToken:     5e-6,
			OutputPricePerToken:    25e-6,
			CacheReadPricePerToken: 0.6e-6,
			CacheCreation5mPrice:   6.25e-6,
			CacheCreation1hPrice:   10e-6,
			SupportsCacheBreakdown: true,
		},
	}
	require.True(t, equivalentCacheV2ResolvedPricingEligible(exact))

	withIntervals := *exact
	withIntervals.Intervals = []PricingInterval{{MinTokens: 1}}
	require.False(t, equivalentCacheV2ResolvedPricingEligible(&withIntervals))

	perRequest := *exact
	perRequest.Mode = BillingModePerRequest
	require.False(t, equivalentCacheV2ResolvedPricingEligible(&perRequest))

	wrongPrice := *exact
	wrongBase := *exact.BasePricing
	wrongBase.OutputPricePerToken = 24e-6
	wrongPrice.BasePricing = &wrongBase
	require.False(t, equivalentCacheV2ResolvedPricingEligible(&wrongPrice))
}

func TestEquivalentCacheV2Response_PreparePlanUsesFinalAccountGroupAndStableRequestContext(t *testing.T) {
	groupID := int64(902)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{
		"model":"claude-sonnet-4",
		"messages":[{"role":"user","content":"hello"}],
		"metadata":{"user_id":"user-session-1"}
	}`)), PlatformAnthropic)
	require.NoError(t, err)
	parsed.GroupID = &groupID
	parsed.SessionContext = &SessionContext{APIKeyID: 501}

	billing := &BillingService{fallbackPrices: map[string]*ModelPricing{
		"claude-sonnet-4": {
			InputPricePerToken:     5e-6,
			OutputPricePerToken:    25e-6,
			CacheReadPricePerToken: 0.6e-6,
			CacheCreation5mPrice:   6.25e-6,
			CacheCreation1hPrice:   10e-6,
			SupportsCacheBreakdown: true,
		},
	}}
	store := &equivalentCacheV2ResponseStateStoreStub{}
	channelService := &ChannelService{}
	channelCache := newEmptyChannelCache()
	channelCache.loadedAt = time.Now()
	channelService.cache.Store(channelCache)
	svc := &GatewayService{
		resolver:                    NewModelPricingResolver(channelService, billing),
		equivalentCacheV2StateStore: store,
	}
	account := &Account{
		ID: 701,
		Extra: map[string]any{
			equivalentCacheV2ExtraKey: map[string]any{
				"enabled":                true,
				"mode":                   "active",
				"pricing_profile":        equivalentCacheV2PricingProfile,
				"kiro_go_pool_confirmed": true,
			},
		},
	}
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "stable-local-request")

	plan := svc.prepareEquivalentCacheV2ResponsePlan(ctx, account, parsed, "claude-sonnet-4", "upstream-request")

	require.NotNil(t, plan)
	require.Equal(t, int64(701), plan.AccountID)
	require.Equal(t, int64(501), plan.APIKeyID)
	require.Equal(t, "local:stable-local-request", plan.RequestID)
	require.Equal(t, GenerateEquivalentCacheV2SessionKey(701, 501, parsed), plan.SessionKey)
	require.Same(t, store, plan.StateStore)
	require.Equal(t, equivalentCacheV2ModeActive, plan.Config.Mode)
}

func TestEquivalentCacheV2Response_PassthroughNonStreamingWritesActiveAllocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{
		"id":"msg_passthrough",
		"type":"message",
		"model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"kept"}],
		"usage":{"input_tokens":2000,"output_tokens":8000,"cached_tokens":0}
	}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	store := &equivalentCacheV2ResponseStateStoreStub{
		decision: EquivalentCacheV2StateDecision{Generation: 2},
	}
	plan := &equivalentCacheV2ResponsePlan{
		Config: equivalentCacheV2Config{
			Enabled:             true,
			Mode:                equivalentCacheV2ModeActive,
			PricingProfile:      equivalentCacheV2PricingProfile,
			VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
			VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
			KiroGoPoolConfirmed: true,
		},
		AccountID:  701,
		APIKeyID:   501,
		SessionKey: "session-hash",
		RequestID:  "request-passthrough",
		StateStore: store,
	}
	svc := &GatewayService{cfg: &config.Config{}}

	result, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(),
		resp,
		c,
		&Account{ID: 701},
		plan,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Allocated)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.RawUsage)
	require.Equal(t, 8000, result.ResponseUsage.OutputTokens)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.Version)
	require.Equal(t, UsageAllocationKindReadMajor, result.Kind)
	require.Equal(t, equivalentCacheV2AllocationHeaderValue, rec.Header().Get(equivalentCacheV2AllocationHeaderName))
	require.Equal(t, "kept", gjson.Get(rec.Body.String(), "content.0.text").String())
	require.Equal(t, int64(result.ResponseUsage.InputTokens), gjson.Get(rec.Body.String(), "usage.input_tokens").Int())
	require.Equal(t, int64(8000), gjson.Get(rec.Body.String(), "usage.output_tokens").Int())
}

func TestEquivalentCacheV2Response_NormalNonStreamingKeepsModelRewriteAndAllocatesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{
		"id":"msg_normal",
		"type":"message",
		"model":"mapped-model",
		"content":[{"type":"text","text":"kept"}],
		"usage":{"input_tokens":2000,"output_tokens":8000}
	}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	plan := &equivalentCacheV2ResponsePlan{
		Config: equivalentCacheV2Config{
			Enabled:             true,
			Mode:                equivalentCacheV2ModeActive,
			PricingProfile:      equivalentCacheV2PricingProfile,
			VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
			VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
			KiroGoPoolConfirmed: true,
		},
		AccountID:  701,
		APIKeyID:   501,
		SessionKey: "session-hash",
		RequestID:  "request-normal",
		StateStore: &equivalentCacheV2ResponseStateStoreStub{
			decision: EquivalentCacheV2StateDecision{Generation: 2},
		},
	}
	svc := &GatewayService{
		cfg:              &config.Config{},
		rateLimitService: &RateLimitService{},
	}

	result, err := svc.handleNonStreamingResponse(
		context.Background(),
		resp,
		c,
		&Account{ID: 701},
		"original-model",
		"mapped-model",
		plan,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Allocated)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.RawUsage)
	require.Equal(t, 8000, result.ResponseUsage.OutputTokens)
	require.Equal(t, "original-model", gjson.Get(rec.Body.String(), "model").String())
	require.Equal(t, "kept", gjson.Get(rec.Body.String(), "content.0.text").String())
	require.Equal(t, equivalentCacheV2AllocationHeaderValue, rec.Header().Get(equivalentCacheV2AllocationHeaderName))
	require.Equal(t, int64(result.ResponseUsage.InputTokens), gjson.Get(rec.Body.String(), "usage.input_tokens").Int())
	require.Equal(t, int64(8000), gjson.Get(rec.Body.String(), "usage.output_tokens").Int())
}

func TestEquivalentCacheV2Response_NormalNonStreamingShadowBypassesLegacyTTLOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{
		"id":"msg_shadow_ttl",
		"type":"message",
		"model":"claude-sonnet-4-6",
		"usage":{
			"input_tokens":2000,
			"output_tokens":8000,
			"cache_creation_input_tokens":100,
			"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":80}
		}
	}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	plan := activeEquivalentCacheV2ResponseTestPlan(&equivalentCacheV2ResponseStateStoreStub{})
	plan.Config.Mode = equivalentCacheV2ModeShadow
	svc := &GatewayService{
		cfg:              &config.Config{},
		rateLimitService: &RateLimitService{},
	}
	account := &Account{
		ID:       701,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"cache_ttl_override_enabled": true,
			"cache_ttl_override_target":  "5m",
		},
	}

	result, err := svc.handleNonStreamingResponse(
		context.Background(),
		resp,
		c,
		account,
		"claude-sonnet-4-6",
		"claude-sonnet-4-6",
		plan,
	)

	require.NoError(t, err)
	require.Equal(t, 20, result.RawUsage.CacheCreation5mTokens)
	require.Equal(t, 80, result.RawUsage.CacheCreation1hTokens)
	require.Equal(t, result.RawUsage, result.ResponseUsage)
	require.Equal(t, int16(0), result.Version)
	require.Equal(t, int64(20), gjson.Get(rec.Body.String(), "usage.cache_creation.ephemeral_5m_input_tokens").Int())
	require.Equal(t, int64(80), gjson.Get(rec.Body.String(), "usage.cache_creation.ephemeral_1h_input_tokens").Int())
}

func TestEquivalentCacheV2Response_NormalNonStreamingOffPreservesRawBeforeLegacyTTLOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{
		"id":"msg_off_ttl",
		"type":"message",
		"model":"claude-sonnet-4-6",
		"usage":{
			"input_tokens":2000,
			"output_tokens":8000,
			"cache_creation_input_tokens":100,
			"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":80}
		}
	}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	svc := &GatewayService{
		cfg:              &config.Config{},
		rateLimitService: &RateLimitService{},
	}
	account := &Account{
		ID:       701,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"cache_ttl_override_enabled": true,
			"cache_ttl_override_target":  "5m",
		},
	}

	result, err := svc.handleNonStreamingResponse(
		context.Background(),
		resp,
		c,
		account,
		"claude-sonnet-4-6",
		"claude-sonnet-4-6",
		nil,
	)

	require.NoError(t, err)
	require.Equal(t, 20, result.RawUsage.CacheCreation5mTokens)
	require.Equal(t, 80, result.RawUsage.CacheCreation1hTokens)
	require.Equal(t, 100, result.ResponseUsage.CacheCreation5mTokens)
	require.Equal(t, 0, result.ResponseUsage.CacheCreation1hTokens)
	require.Equal(t, int16(0), result.Version)
	require.Equal(t, UsageAllocationKindNone, result.Kind)
}

func TestEquivalentCacheV2Response_PassthroughStreamingRewritesMessageStartBeforeWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	stream := strings.Join([]string{
		"event: message_start",
		"id: event-1",
		`data: {"type":"message_start","message":{"id":"msg_stream","model":"claude-sonnet-4-6","usage":{"input_tokens":2000,"output_tokens":0,"cached_tokens":0}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":8000},"delta":{"stop_reason":"end_turn"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	plan := activeEquivalentCacheV2ResponseTestPlan(&equivalentCacheV2ResponseStateStoreStub{
		decision: EquivalentCacheV2StateDecision{Generation: 2},
	})
	svc := &GatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(),
		resp,
		c,
		&Account{ID: 701},
		time.Now(),
		"claude-sonnet-4-6",
		plan,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.rawUsage)
	require.Equal(t, 8000, result.responseUsage.OutputTokens)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.usageAllocationVersion)
	require.Equal(t, UsageAllocationKindReadMajor, result.usageAllocationKind)
	require.Equal(t, equivalentCacheV2AllocationHeaderValue, rec.Header().Get(equivalentCacheV2AllocationHeaderName))

	startData := equivalentCacheV2SSEDataForType(t, rec.Body.String(), "message_start")
	require.Equal(t, int64(result.responseUsage.InputTokens), gjson.Get(startData, "message.usage.input_tokens").Int())
	require.Equal(t, int64(0), gjson.Get(startData, "message.usage.output_tokens").Int())
	require.Equal(t, "msg_stream", gjson.Get(startData, "message.id").String())
	require.Contains(t, rec.Body.String(), "id: event-1")

	deltaData := equivalentCacheV2SSEDataForType(t, rec.Body.String(), "message_delta")
	require.Equal(t, int64(8000), gjson.Get(deltaData, "usage.output_tokens").Int())
	require.Equal(t, "end_turn", gjson.Get(deltaData, "delta.stop_reason").String())
}

func TestEquivalentCacheV2Response_PassthroughStreamingMalformedStartPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2.5,"output_tokens":0}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":8}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	store := &equivalentCacheV2ResponseStateStoreStub{}
	svc := &GatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(),
		resp,
		c,
		&Account{ID: 701},
		time.Now(),
		"claude-sonnet-4-6",
		activeEquivalentCacheV2ResponseTestPlan(store),
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int16(0), result.usageAllocationVersion)
	require.Equal(t, UsageAllocationKindNone, result.usageAllocationKind)
	require.Equal(t, 0, store.calls)
	require.Contains(t, rec.Body.String(), `"input_tokens":2.5`)
	require.Equal(t, 8, result.rawUsage.OutputTokens)
	require.Equal(t, result.rawUsage, result.responseUsage)
	require.Empty(t, rec.Header().Get(equivalentCacheV2AllocationHeaderName))
}

func TestEquivalentCacheV2Response_PassthroughStreamingWriteFailureStillDrainsFinalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2000,"output_tokens":0}}}`,
		"",
		`data: {"type":"message_delta","usage":{"output_tokens":8000}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	svc := &GatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(),
		resp,
		c,
		&Account{ID: 701},
		time.Now(),
		"claude-sonnet-4-6",
		activeEquivalentCacheV2ResponseTestPlan(&equivalentCacheV2ResponseStateStoreStub{
			decision: EquivalentCacheV2StateDecision{Generation: 2},
		}),
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.rawUsage)
	require.Equal(t, 8000, result.responseUsage.OutputTokens)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.usageAllocationVersion)
}

func TestEquivalentCacheV2Response_NormalStreamingRewritesMessageStartAndKeepsExistingTransforms(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	stream := strings.Join([]string{
		"event: message_start",
		"id: event-normal-1",
		`data: {"type":"message_start","message":{"id":"msg_normal_stream","model":"mapped-model","usage":{"input_tokens":2000,"output_tokens":0,"cached_tokens":0}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":8000},"delta":{"stop_reason":"end_turn"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: &RateLimitService{},
	}

	result, err := svc.handleStreamingResponse(
		context.Background(),
		resp,
		c,
		&Account{ID: 701},
		time.Now(),
		"original-model",
		"mapped-model",
		false,
		activeEquivalentCacheV2ResponseTestPlan(&equivalentCacheV2ResponseStateStoreStub{
			decision: EquivalentCacheV2StateDecision{Generation: 2},
		}),
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}, result.rawUsage)
	require.Equal(t, 8000, result.responseUsage.OutputTokens)
	require.Equal(t, result.responseUsage, *result.usage)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.usageAllocationVersion)
	require.Equal(t, UsageAllocationKindReadMajor, result.usageAllocationKind)
	require.Equal(t, equivalentCacheV2AllocationHeaderValue, rec.Header().Get(equivalentCacheV2AllocationHeaderName))

	startData := equivalentCacheV2SSEDataForType(t, rec.Body.String(), "message_start")
	require.Equal(t, "msg_normal_stream", gjson.Get(startData, "message.id").String())
	require.Equal(t, "original-model", gjson.Get(startData, "message.model").String())
	require.Equal(t, int64(result.responseUsage.InputTokens), gjson.Get(startData, "message.usage.input_tokens").Int())
	require.Contains(t, rec.Body.String(), "id: event-normal-1")

	deltaData := equivalentCacheV2SSEDataForType(t, rec.Body.String(), "message_delta")
	require.Equal(t, int64(8000), gjson.Get(deltaData, "usage.output_tokens").Int())
	require.Equal(t, "end_turn", gjson.Get(deltaData, "delta.stop_reason").String())
}

func TestEquivalentCacheV2Response_NormalStreamingActiveBypassesLegacyTTLOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"mapped-model","usage":{"input_tokens":2000,"output_tokens":0}}}`,
		"",
		`data: {"type":"message_delta","usage":{"output_tokens":8000}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: &RateLimitService{},
	}
	account := &Account{
		ID:       701,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"cache_ttl_override_enabled": true,
			"cache_ttl_override_target":  "5m",
		},
	}

	result, err := svc.handleStreamingResponse(
		context.Background(),
		resp,
		c,
		account,
		time.Now(),
		"original-model",
		"mapped-model",
		false,
		activeEquivalentCacheV2ResponseTestPlan(&equivalentCacheV2ResponseStateStoreStub{
			decision: EquivalentCacheV2StateDecision{Create: true, Generation: 2},
		}),
	)

	require.NoError(t, err)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, result.usageAllocationVersion)
	require.Greater(t, result.responseUsage.CacheCreation1hTokens, 0)
	rawCost, rawOK := fixedInputCostUnits(result.rawUsage)
	responseCost, responseOK := fixedResponseInputCostUnits(result.responseUsage)
	require.True(t, rawOK)
	require.True(t, responseOK)
	require.Equal(t, rawCost, responseCost)

	startData := equivalentCacheV2SSEDataForType(t, rec.Body.String(), "message_start")
	require.Equal(t, int64(result.responseUsage.CacheCreation5mTokens), gjson.Get(startData, "message.usage.cache_creation.ephemeral_5m_input_tokens").Int())
	require.Equal(t, int64(result.responseUsage.CacheCreation1hTokens), gjson.Get(startData, "message.usage.cache_creation.ephemeral_1h_input_tokens").Int())
}

func TestEquivalentCacheV2Response_NormalStreamingOffPreservesRawBeforeLegacyTTLOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2000,"output_tokens":0,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":80}}}}`,
		"",
		`data: {"type":"message_delta","usage":{"output_tokens":8000}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: &RateLimitService{},
	}
	account := &Account{
		ID:       701,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"cache_ttl_override_enabled": true,
			"cache_ttl_override_target":  "5m",
		},
	}

	result, err := svc.handleStreamingResponse(
		context.Background(),
		resp,
		c,
		account,
		time.Now(),
		"claude-sonnet-4-6",
		"claude-sonnet-4-6",
		false,
	)

	require.NoError(t, err)
	require.Equal(t, 20, result.rawUsage.CacheCreation5mTokens)
	require.Equal(t, 80, result.rawUsage.CacheCreation1hTokens)
	require.Equal(t, 100, result.responseUsage.CacheCreation5mTokens)
	require.Equal(t, 0, result.responseUsage.CacheCreation1hTokens)
	require.Equal(t, int16(0), result.usageAllocationVersion)
	require.Equal(t, UsageAllocationKindNone, result.usageAllocationKind)
}

func activeEquivalentCacheV2ResponseTestPlan(store EquivalentCacheV2StateStore) *equivalentCacheV2ResponsePlan {
	return &equivalentCacheV2ResponsePlan{
		Config: equivalentCacheV2Config{
			Enabled:             true,
			Mode:                equivalentCacheV2ModeActive,
			PricingProfile:      equivalentCacheV2PricingProfile,
			VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
			VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
			KiroGoPoolConfirmed: true,
		},
		AccountID:  701,
		APIKeyID:   501,
		SessionKey: "session-hash",
		RequestID:  "request-stream",
		StateStore: store,
	}
}

func equivalentCacheV2SSEDataForType(t *testing.T, stream, eventType string) string {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		data, ok := extractAnthropicSSEDataLine(line)
		if !ok || !gjson.Valid(data) {
			continue
		}
		if gjson.Get(data, "type").String() == eventType {
			return data
		}
	}
	t.Fatalf("missing SSE data for event type %q", eventType)
	return ""
}
