//go:build unit

package service

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheV2Usage_CloneIsIndependent(t *testing.T) {
	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 8000}
	cloned := cloneClaudeUsage(raw)
	cloned.InputTokens = 80

	require.Equal(t, 2000, raw.InputTokens)
	require.Equal(t, 80, cloned.InputTokens)
}

func TestEquivalentCacheV2Usage_RejectsRealCacheUsage(t *testing.T) {
	tests := []ClaudeUsage{
		{InputTokens: 1, CacheReadInputTokens: 1},
		{InputTokens: 1, CacheCreationInputTokens: 1},
		{InputTokens: 1, CacheCreation5mTokens: 1},
		{InputTokens: 1, CacheCreation1hTokens: 1},
	}

	for _, usage := range tests {
		require.True(t, hasNonZeroCacheUsage(usage))
	}
	require.False(t, hasNonZeroCacheUsage(ClaudeUsage{InputTokens: 1, OutputTokens: 2}))
}

func TestEquivalentCacheV2Pricing_MatchesExactProfile(t *testing.T) {
	pricing := &ModelPricing{
		InputPricePerToken:     5e-6,
		OutputPricePerToken:    25e-6,
		CacheReadPricePerToken: 0.6e-6,
		CacheCreation5mPrice:   6.25e-6,
		CacheCreation1hPrice:   10e-6,
		SupportsCacheBreakdown: true,
	}

	require.True(t, equivalentCacheV2PricingMatches(pricing))

	pricing.CacheReadPricePerToken = 0.5e-6
	require.False(t, equivalentCacheV2PricingMatches(pricing))
}

func TestEquivalentCacheV2Pricing_RejectsLongContextPolicy(t *testing.T) {
	pricing := &ModelPricing{
		InputPricePerToken:        5e-6,
		OutputPricePerToken:       25e-6,
		CacheReadPricePerToken:    0.6e-6,
		CacheCreation5mPrice:      6.25e-6,
		CacheCreation1hPrice:      10e-6,
		SupportsCacheBreakdown:    true,
		LongContextInputThreshold: 200000,
	}

	require.False(t, equivalentCacheV2PricingMatches(pricing))
}

func TestEquivalentCacheV2Usage_FixedInputCostMatchesDesignExamples(t *testing.T) {
	rawCost, ok := fixedInputCostUnits(ClaudeUsage{InputTokens: 2000, OutputTokens: 8000})
	require.True(t, ok)
	require.EqualValues(t, 1000000, rawCost)

	readCost, ok := fixedResponseInputCostUnits(ClaudeUsage{
		InputTokens:          80,
		OutputTokens:         8000,
		CacheReadInputTokens: 16000,
	})
	require.True(t, ok)
	require.Equal(t, rawCost, readCost)

	createCost, ok := fixedResponseInputCostUnits(ClaudeUsage{
		InputTokens:              120,
		OutputTokens:             8000,
		CacheReadInputTokens:     12350,
		CacheCreationInputTokens: 208,
		CacheCreation5mTokens:    24,
		CacheCreation1hTokens:    184,
	})
	require.True(t, ok)
	require.Equal(t, rawCost, createCost)
}

func TestEquivalentCacheV2Usage_RejectsAggregateCreationMismatch(t *testing.T) {
	_, ok := fixedResponseInputCostUnits(ClaudeUsage{
		InputTokens:              120,
		CacheReadInputTokens:     12350,
		CacheCreationInputTokens: 207,
		CacheCreation5mTokens:    24,
		CacheCreation1hTokens:    184,
	})

	require.False(t, ok)
}

func TestEquivalentCacheV2Usage_RejectsNegativeAndOverflowTokens(t *testing.T) {
	_, ok := fixedInputCostUnits(ClaudeUsage{InputTokens: -1})
	require.False(t, ok)

	_, ok = fixedInputCostUnits(ClaudeUsage{InputTokens: math.MaxInt})
	require.False(t, ok)
}

func TestEquivalentCacheV2Usage_AllocationPreservesRawOutput(t *testing.T) {
	allocation := EquivalentCacheAllocation{
		Version:        equivalentCacheV2AlgorithmVersion,
		Kind:           UsageAllocationKindReadMajor,
		RawUsage:       ClaudeUsage{InputTokens: 2000, OutputTokens: 8000},
		ResponseUsage:  ClaudeUsage{InputTokens: 80, OutputTokens: 8000, CacheReadInputTokens: 16000},
		InputCostUnits: 1000000,
	}

	require.True(t, allocation.Valid())

	allocation.ResponseUsage.OutputTokens++
	require.False(t, allocation.Valid())
}
