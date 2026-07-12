package service

import (
	"math"

	"github.com/shopspring/decimal"
)

const (
	equivalentCacheV2AlgorithmVersion int16 = 2
	EquivalentCacheV2AlgorithmVersion       = equivalentCacheV2AlgorithmVersion

	equivalentCacheV2InputUnit    int64 = 500
	equivalentCacheV2OutputUnit   int64 = 2500
	equivalentCacheV2ReadUnit     int64 = 60
	equivalentCacheV2Create5mUnit int64 = 625
	equivalentCacheV2Create1hUnit int64 = 1000
	equivalentCacheV2USDUnitScale int64 = 100000000
)

type UsageAllocationKind int16

const (
	UsageAllocationKindNone        UsageAllocationKind = 0
	UsageAllocationKindReadMajor   UsageAllocationKind = 1
	UsageAllocationKindCreate1h    UsageAllocationKind = 2
	UsageAllocationKindCreateMixed UsageAllocationKind = 3
	UsageAllocationKindStateless   UsageAllocationKind = 4
)

type EquivalentCacheAllocation struct {
	Version        int16
	Kind           UsageAllocationKind
	RawUsage       ClaudeUsage
	ResponseUsage  ClaudeUsage
	InputCostUnits int64
}

func (a EquivalentCacheAllocation) Valid() bool {
	if a.Version != equivalentCacheV2AlgorithmVersion ||
		a.Kind < UsageAllocationKindReadMajor ||
		a.Kind > UsageAllocationKindStateless ||
		a.RawUsage.OutputTokens != a.ResponseUsage.OutputTokens ||
		hasNonZeroCacheUsage(a.RawUsage) {
		return false
	}
	rawCost, ok := fixedInputCostUnits(a.RawUsage)
	if !ok || rawCost != a.InputCostUnits {
		return false
	}
	responseCost, ok := fixedResponseInputCostUnits(a.ResponseUsage)
	return ok && responseCost == a.InputCostUnits
}

func cloneClaudeUsage(usage ClaudeUsage) ClaudeUsage {
	return usage
}

func hasNonZeroCacheUsage(usage ClaudeUsage) bool {
	return usage.CacheReadInputTokens != 0 ||
		usage.CacheCreationInputTokens != 0 ||
		usage.CacheCreation5mTokens != 0 ||
		usage.CacheCreation1hTokens != 0
}

func equivalentCacheV2PricingMatches(pricing *ModelPricing) bool {
	if pricing == nil ||
		!pricing.SupportsCacheBreakdown ||
		pricing.LongContextInputThreshold > 0 ||
		pricing.LongContextInputMultiplier != 0 ||
		pricing.LongContextOutputMultiplier != 0 {
		return false
	}
	return equivalentCacheV2PriceMatches(pricing.InputPricePerToken, equivalentCacheV2InputUnit) &&
		equivalentCacheV2PriceMatches(pricing.OutputPricePerToken, equivalentCacheV2OutputUnit) &&
		equivalentCacheV2PriceMatches(pricing.CacheReadPricePerToken, equivalentCacheV2ReadUnit) &&
		equivalentCacheV2PriceMatches(pricing.CacheCreation5mPrice, equivalentCacheV2Create5mUnit) &&
		equivalentCacheV2PriceMatches(pricing.CacheCreation1hPrice, equivalentCacheV2Create1hUnit)
}

func equivalentCacheV2PriceMatches(price float64, expectedUnits int64) bool {
	if price <= 0 {
		return false
	}
	units := decimal.NewFromFloat(price).Mul(decimal.NewFromInt(equivalentCacheV2USDUnitScale))
	return units.Equal(decimal.NewFromInt(expectedUnits))
}

func fixedInputCostUnits(usage ClaudeUsage) (int64, bool) {
	return fixedEquivalentCacheV2InputCostUnits(usage)
}

func fixedResponseInputCostUnits(usage ClaudeUsage) (int64, bool) {
	return fixedEquivalentCacheV2InputCostUnits(usage)
}

func fixedEquivalentCacheV2InputCostUnits(usage ClaudeUsage) (int64, bool) {
	if usage.InputTokens < 0 ||
		usage.OutputTokens < 0 ||
		usage.CacheReadInputTokens < 0 ||
		usage.CacheCreationInputTokens < 0 ||
		usage.CacheCreation5mTokens < 0 ||
		usage.CacheCreation1hTokens < 0 {
		return 0, false
	}
	if usage.CacheCreationInputTokens != usage.CacheCreation5mTokens+usage.CacheCreation1hTokens {
		return 0, false
	}

	var cost int64
	var ok bool
	if cost, ok = addTokenCostUnits(cost, usage.InputTokens, equivalentCacheV2InputUnit); !ok {
		return 0, false
	}
	if cost, ok = addTokenCostUnits(cost, usage.CacheReadInputTokens, equivalentCacheV2ReadUnit); !ok {
		return 0, false
	}
	if cost, ok = addTokenCostUnits(cost, usage.CacheCreation5mTokens, equivalentCacheV2Create5mUnit); !ok {
		return 0, false
	}
	if cost, ok = addTokenCostUnits(cost, usage.CacheCreation1hTokens, equivalentCacheV2Create1hUnit); !ok {
		return 0, false
	}
	return cost, true
}

func addTokenCostUnits(current int64, tokens int, unit int64) (int64, bool) {
	if current < 0 || tokens < 0 || unit <= 0 {
		return 0, false
	}
	if int64(tokens) > math.MaxInt64/unit {
		return 0, false
	}
	value := int64(tokens) * unit
	if current > math.MaxInt64-value {
		return 0, false
	}
	return current + value, true
}
