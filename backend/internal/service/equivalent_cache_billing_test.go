//go:build unit

package service

import (
	"math"
	"testing"
)

func TestEquivalentCacheBilling_DefaultSplitShowsHighCacheAndConservesPromptCost(t *testing.T) {
	usage := ClaudeUsage{
		InputTokens:  2000,
		OutputTokens: 8000,
	}
	pricing := &ModelPricing{
		InputPricePerToken:         5e-6,
		OutputPricePerToken:        25e-6,
		CacheCreationPricePerToken: 6.25e-6,
		CacheReadPricePerToken:     0.6e-6,
	}

	ok := applyEquivalentCacheBillingToUsage(&usage, pricing, defaultEquivalentCacheBillingConfig())
	if !ok {
		t.Fatal("expected equivalent cache billing to apply")
	}

	promptTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	cacheRate := float64(usage.CacheReadInputTokens) / float64(promptTokens)
	if cacheRate < 0.95 {
		t.Fatalf("cache rate = %.4f, want >= 0.95; usage=%+v", cacheRate, usage)
	}

	wantPromptCost := math.Ceil(2000*1.08) * 5e-6
	gotPromptCost := float64(usage.InputTokens)*pricing.InputPricePerToken +
		float64(usage.CacheReadInputTokens)*pricing.CacheReadPricePerToken +
		float64(usage.CacheCreationInputTokens)*pricing.CacheCreationPricePerToken
	if math.Abs(gotPromptCost-wantPromptCost) > 0.00001 {
		t.Fatalf("prompt cost = %.10f, want %.10f; usage=%+v", gotPromptCost, wantPromptCost, usage)
	}

	if usage.OutputTokens != int(math.Ceil(8000*1.08)) {
		t.Fatalf("output tokens = %d, want 8640", usage.OutputTokens)
	}
}

func TestEquivalentCacheBilling_RewritesExistingMediumCacheToHighEquivalentCache(t *testing.T) {
	usage := ClaudeUsage{
		InputTokens:              600,
		OutputTokens:             100,
		CacheReadInputTokens:     1400,
		CacheCreationInputTokens: 0,
	}
	pricing := &ModelPricing{
		InputPricePerToken:         5e-6,
		OutputPricePerToken:        25e-6,
		CacheCreationPricePerToken: 6.25e-6,
		CacheReadPricePerToken:     0.6e-6,
	}

	ok := applyEquivalentCacheBillingToUsage(&usage, pricing, defaultEquivalentCacheBillingConfig())
	if !ok {
		t.Fatal("expected equivalent cache billing to apply")
	}

	promptTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	cacheRate := float64(usage.CacheReadInputTokens) / float64(promptTokens)
	if cacheRate < 0.95 {
		t.Fatalf("cache rate = %.4f, want >= 0.95; usage=%+v", cacheRate, usage)
	}

	adjustedInputCost := math.Ceil(600*1.08) * pricing.InputPricePerToken
	adjustedReadCost := math.Ceil(1400*1.08) * pricing.CacheReadPricePerToken
	wantPromptCost := adjustedInputCost + adjustedReadCost
	gotPromptCost := float64(usage.InputTokens)*pricing.InputPricePerToken +
		float64(usage.CacheReadInputTokens)*pricing.CacheReadPricePerToken +
		float64(usage.CacheCreationInputTokens)*pricing.CacheCreationPricePerToken
	if math.Abs(gotPromptCost-wantPromptCost) > 0.00001 {
		t.Fatalf("prompt cost = %.10f, want %.10f; usage=%+v", gotPromptCost, wantPromptCost, usage)
	}
}
