package service

import (
	"fmt"
	"math"
	"strings"
)

const (
	claudeCacheReadRatio    = 0.10
	claudeCacheWrite5mRatio = 1.25
	claudeCacheWrite1hRatio = 2.00
)

func isClaudePricingEntry(model, provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "anthropic" || strings.HasPrefix(provider, "anthropic/") {
		return true
	}
	return containsClaudeModelIdentifier(model)
}

func containsClaudeModelIdentifier(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "claude-") ||
		strings.Contains(model, "/claude-") ||
		strings.Contains(model, ".claude-")
}

func validateClaudeRawCachePricing(model string, entry *LiteLLMRawEntry) error {
	if entry == nil || !isClaudePricingEntry(model, entry.LiteLLMProvider) {
		return nil
	}

	hasCachePricing := entry.CacheReadInputTokenCost != nil ||
		entry.CacheCreationInputTokenCost != nil ||
		entry.CacheCreationInputTokenCostAbove1hr != nil
	if !hasCachePricing {
		if entry.SupportsPromptCaching {
			return fmt.Errorf("模型 %s 的 Claude 缓存价格配置缺少 cache_read_input_token_cost", model)
		}
		return nil
	}

	required := []struct {
		name  string
		value *float64
	}{
		{name: "input_cost_per_token", value: entry.InputCostPerToken},
		{name: "cache_read_input_token_cost", value: entry.CacheReadInputTokenCost},
		{name: "cache_creation_input_token_cost", value: entry.CacheCreationInputTokenCost},
		{name: "cache_creation_input_token_cost_above_1hr", value: entry.CacheCreationInputTokenCostAbove1hr},
	}
	for _, field := range required {
		if field.value == nil {
			return fmt.Errorf("模型 %s 的 Claude 缓存价格配置缺少 %s", model, field.name)
		}
	}
	if *entry.InputCostPerToken <= 0 {
		return fmt.Errorf("模型 %s 的 Claude 缓存价格配置要求 input_cost_per_token 大于零", model)
	}

	return validateClaudeCachePriceRatiosForValues(
		model,
		*entry.InputCostPerToken,
		*entry.CacheReadInputTokenCost,
		*entry.CacheCreationInputTokenCost,
		*entry.CacheCreationInputTokenCostAbove1hr,
	)
}

func validateClaudeCachePriceRatiosForValues(model string, input, cacheRead, cacheWrite5m, cacheWrite1h float64) error {
	checks := []struct {
		name     string
		actual   float64
		expected float64
	}{
		{name: "cache_read_input_token_cost", actual: cacheRead, expected: input * claudeCacheReadRatio},
		{name: "cache_creation_input_token_cost", actual: cacheWrite5m, expected: input * claudeCacheWrite5mRatio},
		{name: "cache_creation_input_token_cost_above_1hr", actual: cacheWrite1h, expected: input * claudeCacheWrite1hRatio},
	}
	for _, check := range checks {
		if !cachePricesEqual(check.actual, check.expected) {
			return fmt.Errorf("模型 %s 的缓存价格 %s 不符合 Anthropic 标准相对倍率", model, check.name)
		}
	}
	return nil
}

func cachePricesEqual(actual, expected float64) bool {
	tolerance := math.Max(1e-15, math.Abs(expected)*1e-9)
	return math.Abs(actual-expected) <= tolerance
}

func applyClaudeStandardCachePricing(pricing *ModelPricing) {
	if pricing == nil || pricing.InputPricePerToken <= 0 {
		return
	}

	pricing.CacheReadPricePerToken = pricing.InputPricePerToken * claudeCacheReadRatio
	pricing.CacheCreationPricePerToken = pricing.InputPricePerToken * claudeCacheWrite5mRatio
	pricing.CacheCreation5mPrice = pricing.InputPricePerToken * claudeCacheWrite5mRatio
	pricing.CacheCreation1hPrice = pricing.InputPricePerToken * claudeCacheWrite1hRatio
	pricing.SupportsCacheBreakdown = true

	if pricing.InputPricePerTokenPriority > 0 {
		pricing.CacheReadPricePerTokenPriority = pricing.InputPricePerTokenPriority * claudeCacheReadRatio
		pricing.CacheCreationPricePerTokenPriority = pricing.InputPricePerTokenPriority * claudeCacheWrite5mRatio
	}
}

func newClaudeFallbackPricing(inputPrice, outputPrice float64) *ModelPricing {
	pricing := &ModelPricing{
		InputPricePerToken:  inputPrice,
		OutputPricePerToken: outputPrice,
	}
	applyClaudeStandardCachePricing(pricing)
	return pricing
}
