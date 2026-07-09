package service

import (
	"context"
	"math"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	equivalentCacheBillingEnabledKey            = "equivalent_cache_billing_enabled"
	equivalentCacheBillingLegacyKiroEnabledKey  = "kiro_equivalent_cache_billing_enabled"
	equivalentCacheBillingLossFactorKey         = "equivalent_cache_billing_loss_factor"
	equivalentCacheBillingInputShareKey         = "equivalent_cache_billing_input_share"
	equivalentCacheBillingCacheReadShareKey     = "equivalent_cache_billing_cache_read_share"
	equivalentCacheBillingCacheCreationShareKey = "equivalent_cache_billing_cache_creation_share"
)

type equivalentCacheBillingConfig struct {
	LossFactor         float64
	InputShare         float64
	CacheReadShare     float64
	CacheCreationShare float64
}

func defaultEquivalentCacheBillingConfig() equivalentCacheBillingConfig {
	return equivalentCacheBillingConfig{
		LossFactor:         1.08,
		InputShare:         0.20,
		CacheReadShare:     0.75,
		CacheCreationShare: 0.05,
	}
}

func equivalentCacheBillingConfigFromAccount(account *Account) (equivalentCacheBillingConfig, bool) {
	if account == nil || (!account.getExtraBool(equivalentCacheBillingEnabledKey) && !account.getExtraBool(equivalentCacheBillingLegacyKiroEnabledKey)) {
		return equivalentCacheBillingConfig{}, false
	}

	cfg := defaultEquivalentCacheBillingConfig()
	if v := account.getExtraFloat64(equivalentCacheBillingLossFactorKey); v > 0 {
		cfg.LossFactor = v
	}
	if v := account.getExtraFloat64(equivalentCacheBillingInputShareKey); v > 0 {
		cfg.InputShare = v
	}
	if v := account.getExtraFloat64(equivalentCacheBillingCacheReadShareKey); v > 0 {
		cfg.CacheReadShare = v
	}
	if v := account.getExtraFloat64(equivalentCacheBillingCacheCreationShareKey); v > 0 {
		cfg.CacheCreationShare = v
	}

	total := cfg.InputShare + cfg.CacheReadShare + cfg.CacheCreationShare
	if total <= 0 {
		return equivalentCacheBillingConfig{}, false
	}
	cfg.InputShare /= total
	cfg.CacheReadShare /= total
	cfg.CacheCreationShare /= total
	return cfg, true
}

func (s *GatewayService) applyEquivalentCacheBilling(ctx context.Context, result *ForwardResult, apiKey *APIKey, account *Account, billingModel string) bool {
	if result == nil {
		return false
	}
	cfg, ok := equivalentCacheBillingConfigFromAccount(account)
	if !ok {
		return false
	}
	pricing := s.equivalentCacheBillingPricing(ctx, result, apiKey, billingModel)
	if pricing == nil {
		return false
	}
	beforeInput := result.Usage.InputTokens
	beforeRead := result.Usage.CacheReadInputTokens
	beforeCreation := result.Usage.CacheCreationInputTokens
	if !applyEquivalentCacheBillingToUsage(&result.Usage, pricing, cfg) {
		return false
	}
	logger.LegacyPrintf(
		"service.gateway",
		"equivalent_cache_billing applied: account=%d input=%d->%d cache_read=%d->%d cache_creation=%d->%d",
		account.ID,
		beforeInput,
		result.Usage.InputTokens,
		beforeRead,
		result.Usage.CacheReadInputTokens,
		beforeCreation,
		result.Usage.CacheCreationInputTokens,
	)
	return true
}

func (s *GatewayService) equivalentCacheBillingPricing(ctx context.Context, result *ForwardResult, apiKey *APIKey, billingModel string) *ModelPricing {
	if s == nil || s.billingService == nil || billingModel == "" {
		return nil
	}
	totalPromptTokens := result.Usage.InputTokens + result.Usage.CacheReadInputTokens + result.Usage.CacheCreationInputTokens

	if s.resolver != nil && apiKey != nil && apiKey.Group != nil {
		gid := apiKey.Group.ID
		resolved := s.resolver.Resolve(ctx, PricingInput{Model: billingModel, GroupID: &gid})
		if resolved != nil && resolved.Mode == BillingModeToken {
			pricing := s.resolver.GetIntervalPricing(resolved, totalPromptTokens)
			return s.billingService.applyModelSpecificPricingPolicy(billingModel, pricing)
		}
	}

	pricing, err := s.billingService.GetModelPricing(billingModel)
	if err != nil {
		return nil
	}
	return s.billingService.applyModelSpecificPricingPolicy(billingModel, pricing)
}

func applyEquivalentCacheBillingToUsage(usage *ClaudeUsage, pricing *ModelPricing, cfg equivalentCacheBillingConfig) bool {
	if usage == nil || pricing == nil {
		return false
	}
	inputPrice := pricing.InputPricePerToken
	cacheReadPrice := pricing.CacheReadPricePerToken
	cacheCreationPrice := equivalentCacheCreationPrice(pricing)
	if inputPrice <= 0 || cacheReadPrice <= 0 || cacheCreationPrice <= 0 || cfg.LossFactor <= 0 {
		return false
	}

	adjustedInput := ceilTokens(usage.InputTokens, cfg.LossFactor)
	adjustedOutput := ceilTokens(usage.OutputTokens, cfg.LossFactor)
	adjustedCacheRead := ceilTokens(usage.CacheReadInputTokens, cfg.LossFactor)
	adjustedCacheCreation := ceilTokens(usage.CacheCreationInputTokens, cfg.LossFactor)
	adjustedCacheCreation5m := ceilTokens(usage.CacheCreation5mTokens, cfg.LossFactor)
	adjustedCacheCreation1h := ceilTokens(usage.CacheCreation1hTokens, cfg.LossFactor)

	promptCost := float64(adjustedInput)*inputPrice +
		float64(adjustedCacheRead)*cacheReadPrice +
		equivalentCacheCreationCost(pricing, adjustedCacheCreation, adjustedCacheCreation5m, adjustedCacheCreation1h)
	if promptCost <= 0 {
		usage.OutputTokens = adjustedOutput
		return true
	}

	usage.InputTokens = roundTokens(promptCost * cfg.InputShare / inputPrice)
	usage.CacheReadInputTokens = roundTokens(promptCost * cfg.CacheReadShare / cacheReadPrice)
	usage.CacheCreationInputTokens = roundTokens(promptCost * cfg.CacheCreationShare / cacheCreationPrice)
	usage.CacheCreation5mTokens = 0
	usage.CacheCreation1hTokens = 0
	usage.OutputTokens = adjustedOutput
	return true
}

func equivalentCacheCreationPrice(pricing *ModelPricing) float64 {
	if pricing == nil {
		return 0
	}
	if pricing.CacheCreationPricePerToken > 0 {
		return pricing.CacheCreationPricePerToken
	}
	if pricing.CacheCreation5mPrice > 0 {
		return pricing.CacheCreation5mPrice
	}
	if pricing.CacheCreation1hPrice > 0 {
		return pricing.CacheCreation1hPrice
	}
	return 0
}

func equivalentCacheCreationCost(pricing *ModelPricing, aggregate, fiveMinute, oneHour int) float64 {
	if pricing == nil {
		return 0
	}
	if (pricing.CacheCreation5mPrice > 0 || pricing.CacheCreation1hPrice > 0) && (fiveMinute > 0 || oneHour > 0) {
		return float64(fiveMinute)*pricing.CacheCreation5mPrice + float64(oneHour)*pricing.CacheCreation1hPrice
	}
	return float64(aggregate) * equivalentCacheCreationPrice(pricing)
}

func ceilTokens(tokens int, factor float64) int {
	if tokens <= 0 {
		return 0
	}
	return int(math.Ceil(float64(tokens) * factor))
}

func roundTokens(tokens float64) int {
	if tokens <= 0 {
		return 0
	}
	return int(math.Round(tokens))
}
