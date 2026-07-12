//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func newGatewayRecordUsageServiceForTest(usageRepo UsageLogRepository, userRepo UserRepository, subRepo UserSubscriptionRepository) *GatewayService {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.1
	return NewGatewayService(
		nil,
		nil,
		usageRepo,
		nil,
		userRepo,
		subRepo,
		nil,
		nil,
		cfg,
		nil,
		nil,
		NewBillingService(cfg, nil),
		nil,
		&BillingCacheService{},
		nil,
		nil,
		&DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // userPlatformQuotaRepo
	)
}

func newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo UsageLogRepository, billingRepo UsageBillingRepository, userRepo UserRepository, subRepo UserSubscriptionRepository) *GatewayService {
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)
	svc.usageBillingRepo = billingRepo
	return svc
}

type openAIRecordUsageBestEffortLogRepoStub struct {
	UsageLogRepository

	bestEffortErr   error
	createErr       error
	bestEffortCalls int
	createCalls     int
	lastLog         *UsageLog
	lastCtxErr      error
}

func (s *openAIRecordUsageBestEffortLogRepoStub) CreateBestEffort(ctx context.Context, log *UsageLog) error {
	s.bestEffortCalls++
	s.lastLog = log
	s.lastCtxErr = ctx.Err()
	return s.bestEffortErr
}

func (s *openAIRecordUsageBestEffortLogRepoStub) Create(ctx context.Context, log *UsageLog) (bool, error) {
	s.createCalls++
	s.lastLog = log
	s.lastCtxErr = ctx.Err()
	return false, s.createErr
}

func TestGatewayServiceRecordUsage_BillingUsesDetachedContext(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: false, err: context.DeadlineExceeded}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	quotaSvc := &openAIRecordUsageAPIKeyQuotaStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.RecordUsage(reqCtx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_detached_ctx",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:    501,
			Quota: 100,
		},
		User:          &User{ID: 601},
		Account:       &Account{ID: 701},
		APIKeyService: quotaSvc,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, userRepo.deductCalls)
	require.NoError(t, userRepo.lastCtxErr)
	require.Equal(t, 1, quotaSvc.quotaCalls)
	require.NoError(t, quotaSvc.lastQuotaCtxErr)
}

func TestGatewayServiceRecordUsage_BillingFingerprintIncludesRequestPayloadHash(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	payloadHash := HashUsageRequestPayload([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_payload_hash",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:             &APIKey{ID: 501, Quota: 100},
		User:               &User{ID: 601},
		Account:            &Account{ID: 701},
		RequestPayloadHash: payloadHash,
	})
	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, payloadHash, billingRepo.lastCmd.RequestPayloadHash)
}

func TestGatewayServiceRecordUsage_EquivalentCacheV2LocksRawCostAndLogsResponseUsage(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	groupID := int64(901)
	model := "claude-sonnet-4"
	configureEquivalentCacheV2RecordUsagePricing(t, svc, groupID, model, 701)
	quotaService := &openAIRecordUsageAPIKeyQuotaStub{}

	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          raw,
		Kind:              UsageAllocationKindReadMajor,
		RequestID:         "gateway_equivalent_cache_v2",
		AccountID:         701,
		SessionGeneration: 2,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})
	require.True(t, ok)
	require.True(t, allocation.Valid())

	expectedCost, err := svc.billingService.CalculateCost(model, UsageTokens{
		InputTokens:           raw.InputTokens,
		OutputTokens:          raw.OutputTokens,
		CacheCreationTokens:   raw.CacheCreationInputTokens,
		CacheReadTokens:       raw.CacheReadInputTokens,
		CacheCreation5mTokens: raw.CacheCreation5mTokens,
		CacheCreation1hTokens: raw.CacheCreation1hTokens,
	}, 1.1)
	require.NoError(t, err)

	err = svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:              "gateway_equivalent_cache_v2",
			Usage:                  cloneClaudeUsage(allocation.ResponseUsage),
			RawUsage:               cloneClaudeUsage(raw),
			ResponseUsage:          cloneClaudeUsage(allocation.ResponseUsage),
			UsageAllocationVersion: equivalentCacheV2AlgorithmVersion,
			UsageAllocationKind:    allocation.Kind,
			Model:                  model,
			Duration:               time.Second,
		},
		APIKey: &APIKey{
			ID:      501,
			Quota:   100,
			GroupID: &groupID,
			Group: &Group{
				ID:             groupID,
				RateMultiplier: 1.1,
			},
		},
		User:          &User{ID: 601},
		Account:       equivalentCacheV2RecordUsageAccount(701, equivalentCacheV2ModeActive),
		APIKeyService: quotaService,
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, billingRepo.lastCmd)

	require.Equal(t, allocation.ResponseUsage.InputTokens, usageRepo.lastLog.InputTokens)
	require.Equal(t, raw.OutputTokens, usageRepo.lastLog.OutputTokens)
	require.Equal(t, allocation.ResponseUsage.CacheReadInputTokens, usageRepo.lastLog.CacheReadTokens)
	require.Equal(t, allocation.ResponseUsage.CacheCreationInputTokens, usageRepo.lastLog.CacheCreationTokens)
	require.Equal(t, allocation.ResponseUsage.CacheCreation5mTokens, usageRepo.lastLog.CacheCreation5mTokens)
	require.Equal(t, allocation.ResponseUsage.CacheCreation1hTokens, usageRepo.lastLog.CacheCreation1hTokens)
	require.Equal(t, raw.InputTokens, *usageRepo.lastLog.RawInputTokens)
	require.Equal(t, raw.OutputTokens, *usageRepo.lastLog.RawOutputTokens)
	require.Equal(t, raw.CacheReadInputTokens, *usageRepo.lastLog.RawCacheReadTokens)
	require.Equal(t, raw.CacheCreationInputTokens, *usageRepo.lastLog.RawCacheCreationTokens)
	require.Equal(t, raw.CacheCreation5mTokens, *usageRepo.lastLog.RawCacheCreation5mTokens)
	require.Equal(t, raw.CacheCreation1hTokens, *usageRepo.lastLog.RawCacheCreation1hTokens)
	require.Equal(t, equivalentCacheV2AlgorithmVersion, *usageRepo.lastLog.UsageAllocationVersion)
	require.Equal(t, int16(allocation.Kind), *usageRepo.lastLog.UsageAllocationKind)

	require.Equal(t, usageRepo.lastLog.InputTokens, billingRepo.lastCmd.InputTokens)
	require.Equal(t, usageRepo.lastLog.OutputTokens, billingRepo.lastCmd.OutputTokens)
	require.Equal(t, usageRepo.lastLog.CacheReadTokens, billingRepo.lastCmd.CacheReadTokens)
	require.Equal(t, usageRepo.lastLog.CacheCreationTokens, billingRepo.lastCmd.CacheCreationTokens)

	require.InDelta(t, expectedCost.InputCost, usageRepo.lastLog.InputCost, 1e-12)
	require.InDelta(t, expectedCost.OutputCost, usageRepo.lastLog.OutputCost, 1e-12)
	require.InDelta(t, expectedCost.CacheCreationCost, usageRepo.lastLog.CacheCreationCost, 1e-12)
	require.InDelta(t, expectedCost.CacheReadCost, usageRepo.lastLog.CacheReadCost, 1e-12)
	require.InDelta(t, expectedCost.TotalCost, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, billingRepo.lastCmd.BalanceCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, billingRepo.lastCmd.APIKeyQuotaCost, 1e-12)

	require.NotNil(t, usageRepo.lastLog.AccountStatsCost)
	expectedAccountStatsCost := float64(raw.InputTokens)*1e-6 + float64(raw.OutputTokens)*2e-6
	require.InDelta(t, expectedAccountStatsCost, *usageRepo.lastLog.AccountStatsCost, 1e-12)
}

func TestGatewayServiceRecordUsage_EquivalentCacheV2ShadowLogsRawUsageAndRawCost(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	groupID := int64(902)
	model := "claude-sonnet-4"
	configureEquivalentCacheV2RecordUsagePricing(t, svc, groupID, model, 702)

	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	expectedCost, err := svc.billingService.CalculateCost(model, UsageTokens{
		InputTokens:  raw.InputTokens,
		OutputTokens: raw.OutputTokens,
	}, 1.1)
	require.NoError(t, err)

	err = svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:     "gateway_equivalent_cache_v2_shadow",
			Usage:         cloneClaudeUsage(raw),
			RawUsage:      cloneClaudeUsage(raw),
			ResponseUsage: cloneClaudeUsage(raw),
			Model:         model,
			Duration:      time.Second,
		},
		APIKey: &APIKey{
			ID:      502,
			Quota:   100,
			GroupID: &groupID,
			Group:   &Group{ID: groupID, RateMultiplier: 1.1},
		},
		User:    &User{ID: 602},
		Account: equivalentCacheV2RecordUsageAccount(702, equivalentCacheV2ModeShadow),
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, raw.InputTokens, usageRepo.lastLog.InputTokens)
	require.Equal(t, raw.OutputTokens, usageRepo.lastLog.OutputTokens)
	require.Zero(t, usageRepo.lastLog.CacheReadTokens)
	require.Zero(t, usageRepo.lastLog.CacheCreationTokens)
	require.Nil(t, usageRepo.lastLog.RawInputTokens)
	require.Nil(t, usageRepo.lastLog.RawOutputTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheReadTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheCreationTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheCreation5mTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheCreation1hTokens)
	require.Nil(t, usageRepo.lastLog.UsageAllocationVersion)
	require.Nil(t, usageRepo.lastLog.UsageAllocationKind)
	require.InDelta(t, expectedCost.TotalCost, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, billingRepo.lastCmd.BalanceCost, 1e-12)
}

func TestGatewayServiceRecordUsage_EquivalentCacheV2InvalidAllocationLeavesAuditFieldsNil(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	groupID := int64(903)
	model := "claude-sonnet-4"
	configureEquivalentCacheV2RecordUsagePricing(t, svc, groupID, model, 703)

	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	invalidResponse := ClaudeUsage{
		InputTokens:          1,
		OutputTokens:         raw.OutputTokens + 1,
		CacheReadInputTokens: 1,
	}
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:              "gateway_equivalent_cache_v2_invalid_allocation",
			Usage:                  cloneClaudeUsage(invalidResponse),
			RawUsage:               cloneClaudeUsage(raw),
			ResponseUsage:          cloneClaudeUsage(invalidResponse),
			UsageAllocationVersion: equivalentCacheV2AlgorithmVersion,
			UsageAllocationKind:    UsageAllocationKindReadMajor,
			Model:                  model,
			Duration:               time.Second,
		},
		APIKey: &APIKey{
			ID:      503,
			Quota:   100,
			GroupID: &groupID,
			Group:   &Group{ID: groupID, RateMultiplier: 1.1},
		},
		User:    &User{ID: 603},
		Account: equivalentCacheV2RecordUsageAccount(703, equivalentCacheV2ModeActive),
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, raw.InputTokens, usageRepo.lastLog.InputTokens)
	require.Equal(t, raw.OutputTokens, usageRepo.lastLog.OutputTokens)
	require.Zero(t, usageRepo.lastLog.CacheReadTokens)
	require.Zero(t, usageRepo.lastLog.CacheCreationTokens)
	require.Nil(t, usageRepo.lastLog.RawInputTokens)
	require.Nil(t, usageRepo.lastLog.RawOutputTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheReadTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheCreationTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheCreation5mTokens)
	require.Nil(t, usageRepo.lastLog.RawCacheCreation1hTokens)
	require.Nil(t, usageRepo.lastLog.UsageAllocationVersion)
	require.Nil(t, usageRepo.lastLog.UsageAllocationKind)
}

func TestGatewayServiceRecordUsage_EquivalentCacheV2RequestedBillingModelMismatchLocksRawCost(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	groupID := int64(904)
	mappedModel := "claude-sonnet-4"
	requestedModel := "claude-3-haiku"
	configureEquivalentCacheV2RecordUsagePricing(t, svc, groupID, mappedModel, 704)
	svc.billingService.fallbackPrices["claude-3-haiku"] = &ModelPricing{
		InputPricePerToken:     2e-6,
		OutputPricePerToken:    3e-6,
		CacheReadPricePerToken: 0.2e-6,
		CacheCreation5mPrice:   2.5e-6,
		CacheCreation1hPrice:   4e-6,
		SupportsCacheBreakdown: true,
	}

	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          raw,
		Kind:              UsageAllocationKindReadMajor,
		RequestID:         "gateway_equivalent_cache_v2_requested_mismatch",
		AccountID:         704,
		SessionGeneration: 2,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})
	require.True(t, ok)
	require.True(t, allocation.Valid())

	expectedCost, err := svc.billingService.CalculateCost(requestedModel, UsageTokens{
		InputTokens:  raw.InputTokens,
		OutputTokens: raw.OutputTokens,
	}, 1.1)
	require.NoError(t, err)

	err = svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:              "gateway_equivalent_cache_v2_requested_mismatch",
			Usage:                  cloneClaudeUsage(allocation.ResponseUsage),
			RawUsage:               cloneClaudeUsage(raw),
			ResponseUsage:          cloneClaudeUsage(allocation.ResponseUsage),
			UsageAllocationVersion: equivalentCacheV2AlgorithmVersion,
			UsageAllocationKind:    allocation.Kind,
			Model:                  requestedModel,
			UpstreamModel:          mappedModel,
			Duration:               time.Second,
		},
		APIKey: &APIKey{
			ID:      504,
			Quota:   100,
			GroupID: &groupID,
			Group:   &Group{ID: groupID, RateMultiplier: 1.1},
		},
		User:    &User{ID: 604},
		Account: equivalentCacheV2RecordUsageAccount(704, equivalentCacheV2ModeActive),
		ChannelUsageFields: ChannelUsageFields{
			OriginalModel:      requestedModel,
			ChannelMappedModel: mappedModel,
			BillingModelSource: BillingModelSourceRequested,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, raw.InputTokens, usageRepo.lastLog.InputTokens)
	require.Equal(t, raw.OutputTokens, usageRepo.lastLog.OutputTokens)
	require.Zero(t, usageRepo.lastLog.CacheReadTokens)
	require.Nil(t, usageRepo.lastLog.RawInputTokens)
	require.Nil(t, usageRepo.lastLog.UsageAllocationVersion)
	require.Nil(t, usageRepo.lastLog.UsageAllocationKind)
	require.InDelta(t, expectedCost.TotalCost, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, billingRepo.lastCmd.BalanceCost, 1e-12)
}

func TestGatewayServiceRecordUsage_EquivalentCacheV2ShadowForceCacheBillingPreservesBaselineCost(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	groupID := int64(905)
	model := "claude-sonnet-4"
	configureEquivalentCacheV2RecordUsagePricing(t, svc, groupID, model, 705)

	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	expectedCost, err := svc.billingService.CalculateCost(model, UsageTokens{
		OutputTokens:    raw.OutputTokens,
		CacheReadTokens: raw.InputTokens,
	}, 1.1)
	require.NoError(t, err)

	err = svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:     "gateway_equivalent_cache_v2_shadow_force",
			Usage:         cloneClaudeUsage(raw),
			RawUsage:      cloneClaudeUsage(raw),
			ResponseUsage: cloneClaudeUsage(raw),
			Model:         model,
			Duration:      time.Second,
		},
		APIKey: &APIKey{
			ID:      505,
			Quota:   100,
			GroupID: &groupID,
			Group:   &Group{ID: groupID, RateMultiplier: 1.1},
		},
		User:              &User{ID: 605},
		Account:           equivalentCacheV2RecordUsageAccount(705, equivalentCacheV2ModeShadow),
		ForceCacheBilling: true,
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, billingRepo.lastCmd)
	require.Zero(t, usageRepo.lastLog.InputTokens)
	require.Equal(t, raw.InputTokens, usageRepo.lastLog.CacheReadTokens)
	require.Equal(t, raw.OutputTokens, usageRepo.lastLog.OutputTokens)
	require.Nil(t, usageRepo.lastLog.RawInputTokens)
	require.Nil(t, usageRepo.lastLog.UsageAllocationVersion)
	require.InDelta(t, expectedCost.TotalCost, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, billingRepo.lastCmd.BalanceCost, 1e-12)
}

func TestGatewayServiceRecordUsage_EquivalentCacheV2ActiveForceCacheBillingFallsBackToBaseline(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	groupID := int64(906)
	model := "claude-sonnet-4"
	configureEquivalentCacheV2RecordUsagePricing(t, svc, groupID, model, 706)

	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          raw,
		Kind:              UsageAllocationKindReadMajor,
		RequestID:         "gateway_equivalent_cache_v2_active_force",
		AccountID:         706,
		SessionGeneration: 3,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})
	require.True(t, ok)
	require.True(t, allocation.Valid())

	expectedCost, err := svc.billingService.CalculateCost(model, UsageTokens{
		OutputTokens:    raw.OutputTokens,
		CacheReadTokens: raw.InputTokens,
	}, 1.1)
	require.NoError(t, err)

	err = svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:              "gateway_equivalent_cache_v2_active_force",
			Usage:                  cloneClaudeUsage(allocation.ResponseUsage),
			RawUsage:               cloneClaudeUsage(raw),
			ResponseUsage:          cloneClaudeUsage(allocation.ResponseUsage),
			UsageAllocationVersion: equivalentCacheV2AlgorithmVersion,
			UsageAllocationKind:    allocation.Kind,
			Model:                  model,
			Duration:               time.Second,
		},
		APIKey: &APIKey{
			ID:      506,
			Quota:   100,
			GroupID: &groupID,
			Group:   &Group{ID: groupID, RateMultiplier: 1.1},
		},
		User:              &User{ID: 606},
		Account:           equivalentCacheV2RecordUsageAccount(706, equivalentCacheV2ModeActive),
		ForceCacheBilling: true,
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, billingRepo.lastCmd)
	require.Zero(t, usageRepo.lastLog.InputTokens)
	require.Equal(t, raw.InputTokens, usageRepo.lastLog.CacheReadTokens)
	require.Equal(t, raw.OutputTokens, usageRepo.lastLog.OutputTokens)
	require.Nil(t, usageRepo.lastLog.RawInputTokens)
	require.Nil(t, usageRepo.lastLog.RawOutputTokens)
	require.Nil(t, usageRepo.lastLog.UsageAllocationVersion)
	require.Nil(t, usageRepo.lastLog.UsageAllocationKind)
	require.InDelta(t, expectedCost.TotalCost, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, billingRepo.lastCmd.BalanceCost, 1e-12)
}

func equivalentCacheV2RecordUsageAccount(id int64, mode equivalentCacheV2Mode) *Account {
	return &Account{
		ID: id,
		Extra: map[string]any{
			equivalentCacheV2ExtraKey: map[string]any{
				"enabled":                true,
				"mode":                   string(mode),
				"pricing_profile":        equivalentCacheV2PricingProfile,
				"kiro_go_pool_confirmed": true,
			},
		},
	}
}

func configureEquivalentCacheV2RecordUsagePricing(t *testing.T, svc *GatewayService, groupID int64, model string, accountID int64) {
	t.Helper()
	inputPrice := 1e-6
	outputPrice := 2e-6
	cacheWritePrice := 3e-6
	cacheReadPrice := 4e-6
	cache := newEmptyChannelCache()
	cache.channelByGroupID[groupID] = &Channel{
		ID:     groupID,
		Status: StatusActive,
		AccountStatsPricingRules: []AccountStatsPricingRule{{
			AccountIDs: []int64{accountID},
			Pricing: []ChannelModelPricing{{
				Models:          []string{model},
				BillingMode:     BillingModeToken,
				InputPrice:      &inputPrice,
				OutputPrice:     &outputPrice,
				CacheWritePrice: &cacheWritePrice,
				CacheReadPrice:  &cacheReadPrice,
			}},
		}},
	}
	cache.groupPlatform[groupID] = PlatformAnthropic
	cache.loadedAt = time.Now()
	channelService := &ChannelService{}
	channelService.cache.Store(cache)

	billingService := &BillingService{fallbackPrices: map[string]*ModelPricing{
		model: {
			InputPricePerToken:     5e-6,
			OutputPricePerToken:    25e-6,
			CacheReadPricePerToken: 0.6e-6,
			CacheCreation5mPrice:   6.25e-6,
			CacheCreation1hPrice:   10e-6,
			SupportsCacheBreakdown: true,
		},
	}}
	svc.channelService = channelService
	svc.billingService = billingService
	svc.resolver = NewModelPricingResolver(channelService, billingService)
}

func TestGatewayServiceRecordUsage_BillingFingerprintFallsBackToContextRequestID(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-local-123")
	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_payload_fallback",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 501, Quota: 100},
		User:    &User{ID: 601},
		Account: &Account{ID: 701},
	})
	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, "local:req-local-123", billingRepo.lastCmd.RequestPayloadHash)
}

func TestGatewayServiceRecordUsage_PreservesRequestedAndUpstreamModels(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	mappedModel := "claude-sonnet-4-20250514"

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:     "gateway_models_split",
			Usage:         ClaudeUsage{InputTokens: 10, OutputTokens: 6},
			Model:         "claude-sonnet-4",
			UpstreamModel: mappedModel,
			Duration:      time.Second,
		},
		APIKey:  &APIKey{ID: 501, Quota: 100},
		User:    &User{ID: 601},
		Account: &Account{ID: 701},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, "claude-sonnet-4", usageRepo.lastLog.Model)
	require.Equal(t, "claude-sonnet-4", usageRepo.lastLog.RequestedModel)
	require.NotNil(t, usageRepo.lastLog.UpstreamModel)
	require.Equal(t, mappedModel, *usageRepo.lastLog.UpstreamModel)
}

func TestGatewayServiceRecordUsage_EmptyImageSizeDefaultsBeforeBillingAndPersistence(t *testing.T) {
	imagePrice2K := 0.19
	groupID := int64(901)
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:      "gateway_image_default_size",
			Model:          "gemini-image",
			ImageCount:     1,
			ImageInputSize: "auto",
			Duration:       time.Second,
		},
		APIKey: &APIKey{
			ID:      801,
			GroupID: i64p(groupID),
			Group: &Group{
				ID:             groupID,
				RateMultiplier: 1.0,
				ImagePrice2K:   &imagePrice2K,
			},
		},
		User:    &User{ID: 601},
		Account: &Account{ID: 701},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 1, usageRepo.lastLog.ImageCount)
	require.NotNil(t, usageRepo.lastLog.ImageSize)
	require.Equal(t, ImageBillingSize2K, *usageRepo.lastLog.ImageSize)
	require.NotNil(t, usageRepo.lastLog.ImageInputSize)
	require.Equal(t, "auto", *usageRepo.lastLog.ImageInputSize)
	require.NotNil(t, usageRepo.lastLog.ImageSizeSource)
	require.Equal(t, ImageSizeSourceDefault, *usageRepo.lastLog.ImageSizeSource)
	require.InDelta(t, 0.19, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, 0.19, usageRepo.lastLog.ActualCost, 1e-12)
}

func TestGatewayServiceRecordUsage_PeakRateAffectsTokenModeImageOutputTokens(t *testing.T) {
	groupID := int64(902)
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{})
	svc.resolver = newOpenAITokenImageChannelPricingResolverForTest(t, groupID, "gemini-image")

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:  "gateway_peak_image_tokens",
			Model:      "gemini-image",
			ImageCount: 1,
			Usage: ClaudeUsage{
				InputTokens:       1000,
				OutputTokens:      600,
				ImageOutputTokens: 100,
			},
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:      802,
			GroupID: i64p(groupID),
			Group: &Group{
				ID:                 groupID,
				RateMultiplier:     1.0,
				SubscriptionType:   SubscriptionTypeSubscription,
				PeakRateEnabled:    true,
				PeakStart:          "00:00",
				PeakEnd:            "23:59",
				PeakRateMultiplier: 3.0,
			},
		},
		User:    &User{ID: 602},
		Account: &Account{ID: 702},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, usageRepo.lastLog.BillingMode)
	require.Equal(t, string(BillingModeToken), *usageRepo.lastLog.BillingMode)
	require.Equal(t, 3.0, usageRepo.lastLog.RateMultiplier)

	textInput := 1000 * 3e-6
	textOutput := 500 * 15e-6
	imageOutput := 100 * 15e-6
	expectedActual := (textInput + textOutput + imageOutput) * 3.0

	require.InDelta(t, textInput+textOutput+imageOutput, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, imageOutput, usageRepo.lastLog.ImageOutputCost, 1e-12)
	require.InDelta(t, expectedActual, usageRepo.lastLog.ActualCost, 1e-12)
	require.InDelta(t, expectedActual, userRepo.lastAmount, 1e-12)
}

func TestGatewayServiceRecordUsage_UsageLogWriteErrorDoesNotSkipBilling(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: false, err: MarkUsageLogCreateNotPersisted(context.Canceled)}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	quotaSvc := &openAIRecordUsageAPIKeyQuotaStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_not_persisted",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:    503,
			Quota: 100,
		},
		User:          &User{ID: 603},
		Account:       &Account{ID: 703},
		APIKeyService: quotaSvc,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, userRepo.deductCalls)
	require.Equal(t, 1, quotaSvc.quotaCalls)
}

func TestGatewayServiceRecordUsageWithLongContext_BillingUsesDetachedContext(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: false, err: context.DeadlineExceeded}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	quotaSvc := &openAIRecordUsageAPIKeyQuotaStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.RecordUsageWithLongContext(reqCtx, &RecordUsageLongContextInput{
		Result: &ForwardResult{
			RequestID: "gateway_long_context_detached_ctx",
			Usage: ClaudeUsage{
				InputTokens:  12,
				OutputTokens: 8,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:    502,
			Quota: 100,
		},
		User:                  &User{ID: 602},
		Account:               &Account{ID: 702},
		LongContextThreshold:  200000,
		LongContextMultiplier: 2,
		APIKeyService:         quotaSvc,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, userRepo.deductCalls)
	require.NoError(t, userRepo.lastCtxErr)
	require.Equal(t, 1, quotaSvc.quotaCalls)
	require.NoError(t, quotaSvc.lastQuotaCtxErr)
}

func TestGatewayServiceRecordUsage_UsesFallbackRequestIDForUsageLog(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "gateway-local-fallback")
	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 504},
		User:    &User{ID: 604},
		Account: &Account{ID: 704},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, "local:gateway-local-fallback", usageRepo.lastLog.RequestID)
}

func TestGatewayServiceRecordUsage_PrefersClientRequestIDOverUpstreamRequestID(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-stable-123")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "req-local-ignored")
	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "upstream-volatile-456",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 506},
		User:    &User{ID: 606},
		Account: &Account{ID: 706},
	})

	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, "client:client-stable-123", billingRepo.lastCmd.RequestID)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, "client:client-stable-123", usageRepo.lastLog.RequestID)
}

func TestGatewayServiceRecordUsage_GeneratesRequestIDWhenAllSourcesMissing(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 507},
		User:    &User{ID: 607},
		Account: &Account{ID: 707},
	})

	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.True(t, strings.HasPrefix(billingRepo.lastCmd.RequestID, "generated:"))
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, billingRepo.lastCmd.RequestID, usageRepo.lastLog.RequestID)
}

func TestGatewayServiceRecordUsage_DroppedUsageLogFallsBackToSyncCreate(t *testing.T) {
	// 计费成功后 best-effort 写入被丢弃（队列超时）时必须同步兜底，
	// 否则出现“已扣费但无 usage_log”的对账缺口（issue #3656）。
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{
		bestEffortErr: MarkUsageLogCreateDropped(errors.New("usage log best-effort queue full")),
	}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_drop_usage_log",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 508},
		User:    &User{ID: 608},
		Account: &Account{ID: 708},
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.bestEffortCalls)
	require.Equal(t, 1, usageRepo.createCalls)
	// 兜底调用使用的 ctx 必须仍然存活，不能带着已死的 ctx 走过场。
	require.NoError(t, usageRepo.lastCtxErr)
}

func TestGatewayServiceRecordUsage_BillingErrorSkipsUsageLogWrite(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{err: context.DeadlineExceeded}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_billing_fail",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 505},
		User:    &User{ID: 605},
		Account: &Account{ID: 705},
	})

	require.Error(t, err)
	require.Equal(t, 1, billingRepo.calls)
	require.Equal(t, 0, usageRepo.calls)
}

func TestGatewayServiceRecordUsage_ReasoningEffortPersisted(t *testing.T) {
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	effort := "max"
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "effort_test",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 5,
			},
			Model:           "claude-opus-4-6",
			Duration:        time.Second,
			ReasoningEffort: &effort,
		},
		APIKey:  &APIKey{ID: 1},
		User:    &User{ID: 1},
		Account: &Account{ID: 1},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, usageRepo.lastLog.ReasoningEffort)
	require.Equal(t, "max", *usageRepo.lastLog.ReasoningEffort)
}

func TestGatewayServiceRecordUsage_ReasoningEffortNil(t *testing.T) {
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "no_effort_test",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 5,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 1},
		User:    &User{ID: 1},
		Account: &Account{ID: 1},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Nil(t, usageRepo.lastLog.ReasoningEffort)
}
