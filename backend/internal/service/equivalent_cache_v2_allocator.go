package service

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const (
	equivalentCacheV2ReadInputMinPPM       int64 = 20000
	equivalentCacheV2ReadInputMaxPPM       int64 = 80000
	equivalentCacheV2ReadVisibleRateMinPPM int64 = 988000
	equivalentCacheV2ReadVisibleRateMaxPPM int64 = 998000

	equivalentCacheV2CreateInputMinPPM       int64 = 40000
	equivalentCacheV2CreateInputMaxPPM       int64 = 120000
	equivalentCacheV2CreateVisibleRateMinPPM int64 = 962000
	equivalentCacheV2CreateVisibleRateMaxPPM int64 = 988000
	equivalentCacheV2CreateValueMinPPM       int64 = 120000
	equivalentCacheV2CreateValueMaxPPM       int64 = 240000

	equivalentCacheV2OneHourShareMinPPM int64 = 920000
	equivalentCacheV2OneHourShareMaxPPM int64 = 970000
)

type equivalentCacheV2AllocationInput struct {
	RawUsage          ClaudeUsage
	Kind              UsageAllocationKind
	RequestID         string
	AccountID         int64
	SessionGeneration int64
	VisibleRateMinPPM int64
	VisibleRateMaxPPM int64
}

func allocateEquivalentCacheV2(input equivalentCacheV2AllocationInput) (EquivalentCacheAllocation, bool) {
	if input.RawUsage.InputTokens <= 0 ||
		input.RawUsage.OutputTokens < 0 ||
		hasNonZeroCacheUsage(input.RawUsage) {
		return EquivalentCacheAllocation{}, false
	}
	rawCost, ok := fixedInputCostUnits(input.RawUsage)
	if !ok || rawCost <= 0 {
		return EquivalentCacheAllocation{}, false
	}

	seed := equivalentCacheV2AllocationSeed(input)
	var response ClaudeUsage
	switch input.Kind {
	case UsageAllocationKindReadMajor, UsageAllocationKindStateless:
		response, ok = solveEquivalentCacheV2ReadMajor(input, rawCost, seed)
	case UsageAllocationKindCreate1h:
		response, ok = solveEquivalentCacheV2Create1h(input, rawCost, seed)
	case UsageAllocationKindCreateMixed:
		response, ok = solveEquivalentCacheV2CreateMixed(input, rawCost, seed)
	default:
		return EquivalentCacheAllocation{}, false
	}
	if !ok {
		return EquivalentCacheAllocation{}, false
	}

	allocation := EquivalentCacheAllocation{
		Version:        equivalentCacheV2AlgorithmVersion,
		Kind:           input.Kind,
		RawUsage:       cloneClaudeUsage(input.RawUsage),
		ResponseUsage:  response,
		InputCostUnits: rawCost,
	}
	if !allocation.Valid() {
		return EquivalentCacheAllocation{}, false
	}
	return allocation, true
}

func solveEquivalentCacheV2ReadMajor(input equivalentCacheV2AllocationInput, rawCost int64, seed uint64) (ClaudeUsage, bool) {
	minRate, maxRate, ok := equivalentCacheV2EffectiveVisibleRate(
		input,
		equivalentCacheV2ReadVisibleRateMinPPM,
		equivalentCacheV2ReadVisibleRateMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	minInput, maxInput, ok := equivalentCacheV2TokenRange(
		input.RawUsage.InputTokens,
		equivalentCacheV2ReadInputMinPPM,
		equivalentCacheV2ReadInputMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	targetInput := equivalentCacheV2TargetInRange(minInput, maxInput, seed)
	for _, displayedInput := range equivalentCacheV2CenteredCandidates(targetInput, minInput, maxInput, 256) {
		inputCost, ok := tokenCostUnits(displayedInput, equivalentCacheV2InputUnit)
		if !ok || inputCost >= rawCost {
			continue
		}
		remaining := rawCost - inputCost
		if remaining%equivalentCacheV2ReadUnit != 0 {
			continue
		}
		cacheRead, ok := equivalentCacheV2IntFromInt64(remaining / equivalentCacheV2ReadUnit)
		if !ok {
			continue
		}
		response := ClaudeUsage{
			InputTokens:          displayedInput,
			OutputTokens:         input.RawUsage.OutputTokens,
			CacheReadInputTokens: cacheRead,
		}
		if equivalentCacheV2VisibleRateWithin(response, minRate, maxRate) {
			return response, true
		}
	}
	return ClaudeUsage{}, false
}

func solveEquivalentCacheV2Create1h(input equivalentCacheV2AllocationInput, rawCost int64, seed uint64) (ClaudeUsage, bool) {
	minRate, maxRate, ok := equivalentCacheV2EffectiveVisibleRate(
		input,
		equivalentCacheV2CreateVisibleRateMinPPM,
		equivalentCacheV2CreateVisibleRateMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	minInput, maxInput, ok := equivalentCacheV2TokenRange(
		input.RawUsage.InputTokens,
		equivalentCacheV2CreateInputMinPPM,
		equivalentCacheV2CreateInputMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	minCreationValue, maxCreationValue, ok := equivalentCacheV2Int64Range(
		rawCost,
		equivalentCacheV2CreateValueMinPPM,
		equivalentCacheV2CreateValueMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	minCreate := equivalentCacheV2CeilDiv(minCreationValue, equivalentCacheV2Create1hUnit)
	maxCreate := maxCreationValue / equivalentCacheV2Create1hUnit
	if minCreate <= 0 || minCreate > maxCreate {
		return ClaudeUsage{}, false
	}

	targetInput := equivalentCacheV2TargetInRange(minInput, maxInput, seed)
	targetCreate := equivalentCacheV2TargetInt64InRange(minCreate, maxCreate, seed>>8)
	createCandidates := equivalentCacheV2CenteredInt64Candidates(targetCreate, minCreate, maxCreate, 96)
	for _, displayedInput := range equivalentCacheV2CenteredCandidates(targetInput, minInput, maxInput, 96) {
		inputCost, ok := tokenCostUnits(displayedInput, equivalentCacheV2InputUnit)
		if !ok || inputCost >= rawCost {
			continue
		}
		for _, create1h64 := range createCandidates {
			createCost := create1h64 * equivalentCacheV2Create1hUnit
			remaining := rawCost - inputCost - createCost
			if remaining <= 0 || remaining%equivalentCacheV2ReadUnit != 0 {
				continue
			}
			create1h, ok := equivalentCacheV2IntFromInt64(create1h64)
			if !ok {
				continue
			}
			cacheRead, ok := equivalentCacheV2IntFromInt64(remaining / equivalentCacheV2ReadUnit)
			if !ok {
				continue
			}
			response := ClaudeUsage{
				InputTokens:              displayedInput,
				OutputTokens:             input.RawUsage.OutputTokens,
				CacheReadInputTokens:     cacheRead,
				CacheCreationInputTokens: create1h,
				CacheCreation1hTokens:    create1h,
			}
			if equivalentCacheV2VisibleRateWithin(response, minRate, maxRate) {
				return response, true
			}
		}
	}
	return ClaudeUsage{}, false
}

func solveEquivalentCacheV2CreateMixed(input equivalentCacheV2AllocationInput, rawCost int64, seed uint64) (ClaudeUsage, bool) {
	minRate, maxRate, ok := equivalentCacheV2EffectiveVisibleRate(
		input,
		equivalentCacheV2CreateVisibleRateMinPPM,
		equivalentCacheV2CreateVisibleRateMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	minInput, maxInput, ok := equivalentCacheV2TokenRange(
		input.RawUsage.InputTokens,
		equivalentCacheV2CreateInputMinPPM,
		equivalentCacheV2CreateInputMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}
	minCreationValue, maxCreationValue, ok := equivalentCacheV2Int64Range(
		rawCost,
		equivalentCacheV2CreateValueMinPPM,
		equivalentCacheV2CreateValueMaxPPM,
	)
	if !ok {
		return ClaudeUsage{}, false
	}

	targetCreationValue := equivalentCacheV2TargetInt64InRange(minCreationValue, maxCreationValue, seed>>8)
	targetOneHourShare := equivalentCacheV2TargetInt64InRange(
		equivalentCacheV2OneHourShareMinPPM,
		equivalentCacheV2OneHourShareMaxPPM,
		seed>>16,
	)
	target5mValue := targetCreationValue * (equivalentCacheV2RateScale - targetOneHourShare) / equivalentCacheV2RateScale
	target5m := target5mValue / equivalentCacheV2Create5mUnit
	max5m := maxCreationValue * (equivalentCacheV2RateScale - equivalentCacheV2OneHourShareMinPPM) /
		equivalentCacheV2RateScale / equivalentCacheV2Create5mUnit
	if max5m < 1 {
		return ClaudeUsage{}, false
	}
	if target5m < 1 {
		target5m = 1
	}
	if target5m > max5m {
		target5m = max5m
	}

	targetInput := equivalentCacheV2TargetInRange(minInput, maxInput, seed)
	for _, displayedInput := range equivalentCacheV2CenteredCandidates(targetInput, minInput, maxInput, 64) {
		inputCost, ok := tokenCostUnits(displayedInput, equivalentCacheV2InputUnit)
		if !ok || inputCost >= rawCost {
			continue
		}
		for _, create5m64 := range equivalentCacheV2CenteredInt64Candidates(target5m, 1, max5m, 32) {
			create5mValue := create5m64 * equivalentCacheV2Create5mUnit
			minCreate1h := equivalentCacheV2CeilDiv(maxInt64(0, minCreationValue-create5mValue), equivalentCacheV2Create1hUnit)
			maxCreate1h := (maxCreationValue - create5mValue) / equivalentCacheV2Create1hUnit
			if minCreate1h < 1 {
				minCreate1h = 1
			}
			if minCreate1h > maxCreate1h {
				continue
			}
			targetCreate1h := (targetCreationValue - create5mValue) / equivalentCacheV2Create1hUnit
			if targetCreate1h < minCreate1h {
				targetCreate1h = minCreate1h
			}
			if targetCreate1h > maxCreate1h {
				targetCreate1h = maxCreate1h
			}
			for _, create1h64 := range equivalentCacheV2CenteredInt64Candidates(targetCreate1h, minCreate1h, maxCreate1h, 24) {
				remaining := rawCost - inputCost - create5mValue - create1h64*equivalentCacheV2Create1hUnit
				if remaining <= 0 || remaining%equivalentCacheV2ReadUnit != 0 {
					continue
				}
				create5m, ok := equivalentCacheV2IntFromInt64(create5m64)
				if !ok {
					continue
				}
				create1h, ok := equivalentCacheV2IntFromInt64(create1h64)
				if !ok {
					continue
				}
				cacheRead, ok := equivalentCacheV2IntFromInt64(remaining / equivalentCacheV2ReadUnit)
				if !ok {
					continue
				}
				response := ClaudeUsage{
					InputTokens:              displayedInput,
					OutputTokens:             input.RawUsage.OutputTokens,
					CacheReadInputTokens:     cacheRead,
					CacheCreationInputTokens: create5m + create1h,
					CacheCreation5mTokens:    create5m,
					CacheCreation1hTokens:    create1h,
				}
				if equivalentCacheV2VisibleRateWithin(response, minRate, maxRate) &&
					equivalentCacheV2OneHourCreationShareWithin(response) {
					return response, true
				}
			}
		}
	}
	return ClaudeUsage{}, false
}

func equivalentCacheV2AllocationSeed(input equivalentCacheV2AllocationInput) uint64 {
	var b strings.Builder
	_, _ = b.WriteString(strings.TrimSpace(input.RequestID))
	_, _ = b.WriteString("|")
	_, _ = b.WriteString(strconv.FormatInt(input.AccountID, 10))
	_, _ = b.WriteString("|")
	_, _ = b.WriteString(strconv.FormatInt(input.SessionGeneration, 10))
	_, _ = b.WriteString("|")
	_, _ = b.WriteString(strconv.FormatInt(int64(equivalentCacheV2AlgorithmVersion), 10))
	sum := sha256.Sum256([]byte(b.String()))
	return binary.BigEndian.Uint64(sum[:8])
}

func equivalentCacheV2EffectiveVisibleRate(input equivalentCacheV2AllocationInput, eventMin, eventMax int64) (int64, int64, bool) {
	minRate := maxInt64(input.VisibleRateMinPPM, eventMin)
	maxRate := minInt64(input.VisibleRateMaxPPM, eventMax)
	if minRate <= 0 || maxRate >= equivalentCacheV2RateScale || minRate > maxRate {
		return 0, 0, false
	}
	return minRate, maxRate, true
}

func equivalentCacheV2VisibleRateWithin(usage ClaudeUsage, minPPM, maxPPM int64) bool {
	total := int64(usage.InputTokens) +
		int64(usage.CacheReadInputTokens) +
		int64(usage.CacheCreationInputTokens)
	if usage.CacheReadInputTokens < 0 || total <= 0 {
		return false
	}
	return equivalentCacheV2RatioWithin(int64(usage.CacheReadInputTokens), total, minPPM, maxPPM)
}

func equivalentCacheV2OneHourCreationShareWithin(usage ClaudeUsage) bool {
	oneHourValue, ok := tokenCostUnits(usage.CacheCreation1hTokens, equivalentCacheV2Create1hUnit)
	if !ok {
		return false
	}
	fiveMinuteValue, ok := tokenCostUnits(usage.CacheCreation5mTokens, equivalentCacheV2Create5mUnit)
	if !ok || oneHourValue <= 0 || fiveMinuteValue <= 0 {
		return false
	}
	return equivalentCacheV2RatioWithin(
		oneHourValue,
		oneHourValue+fiveMinuteValue,
		equivalentCacheV2OneHourShareMinPPM,
		equivalentCacheV2OneHourShareMaxPPM,
	)
}

func equivalentCacheV2RatioWithin(numerator, denominator, minPPM, maxPPM int64) bool {
	if numerator < 0 || denominator <= 0 || numerator > denominator {
		return false
	}
	left := new(big.Int).Mul(big.NewInt(numerator), big.NewInt(equivalentCacheV2RateScale))
	minRight := new(big.Int).Mul(big.NewInt(denominator), big.NewInt(minPPM))
	maxRight := new(big.Int).Mul(big.NewInt(denominator), big.NewInt(maxPPM))
	return left.Cmp(minRight) >= 0 && left.Cmp(maxRight) <= 0
}

func equivalentCacheV2TokenRange(tokens int, minPPM, maxPPM int64) (int, int, bool) {
	minValue, ok := equivalentCacheV2ScaledInt(tokens, minPPM, true)
	if !ok {
		return 0, 0, false
	}
	maxValue, ok := equivalentCacheV2ScaledInt(tokens, maxPPM, false)
	if !ok || minValue <= 0 || minValue > maxValue {
		return 0, 0, false
	}
	return minValue, maxValue, true
}

func equivalentCacheV2Int64Range(value, minPPM, maxPPM int64) (int64, int64, bool) {
	if value <= 0 {
		return 0, 0, false
	}
	minValue := equivalentCacheV2ScaledInt64(value, minPPM, true)
	maxValue := equivalentCacheV2ScaledInt64(value, maxPPM, false)
	if minValue <= 0 || minValue > maxValue {
		return 0, 0, false
	}
	return minValue, maxValue, true
}

func equivalentCacheV2ScaledInt(value int, ppm int64, ceil bool) (int, bool) {
	result := equivalentCacheV2ScaledBigInt(big.NewInt(int64(value)), ppm, ceil)
	if !result.IsInt64() {
		return 0, false
	}
	return equivalentCacheV2IntFromInt64(result.Int64())
}

func equivalentCacheV2ScaledInt64(value, ppm int64, ceil bool) int64 {
	result := equivalentCacheV2ScaledBigInt(big.NewInt(value), ppm, ceil)
	if !result.IsInt64() {
		return 0
	}
	return result.Int64()
}

func equivalentCacheV2ScaledBigInt(value *big.Int, ppm int64, ceil bool) *big.Int {
	product := new(big.Int).Mul(value, big.NewInt(ppm))
	divisor := big.NewInt(equivalentCacheV2RateScale)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(product, divisor, remainder)
	if ceil && remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func equivalentCacheV2TargetInRange(minValue, maxValue int, seed uint64) int {
	span := uint64(maxValue-minValue) + 1
	return minValue + int(seed%span)
}

func equivalentCacheV2TargetInt64InRange(minValue, maxValue int64, seed uint64) int64 {
	span := uint64(maxValue-minValue) + 1
	return minValue + int64(seed%span)
}

func equivalentCacheV2CenteredCandidates(center, minValue, maxValue, limit int) []int {
	values := make([]int, 0, limit)
	for delta := 0; len(values) < limit; delta++ {
		added := false
		if candidate := center + delta; candidate <= maxValue {
			values = append(values, candidate)
			added = true
		}
		if delta > 0 {
			if candidate := center - delta; candidate >= minValue && len(values) < limit {
				values = append(values, candidate)
				added = true
			}
		}
		if !added && center+delta > maxValue && center-delta < minValue {
			break
		}
	}
	return values
}

func equivalentCacheV2CenteredInt64Candidates(center, minValue, maxValue int64, limit int) []int64 {
	values := make([]int64, 0, limit)
	for delta := int64(0); len(values) < limit; delta++ {
		added := false
		if candidate := center + delta; candidate <= maxValue {
			values = append(values, candidate)
			added = true
		}
		if delta > 0 {
			if candidate := center - delta; candidate >= minValue && len(values) < limit {
				values = append(values, candidate)
				added = true
			}
		}
		if !added && center+delta > maxValue && center-delta < minValue {
			break
		}
	}
	return values
}

func tokenCostUnits(tokens int, unit int64) (int64, bool) {
	return addTokenCostUnits(0, tokens, unit)
}

func equivalentCacheV2IntFromInt64(value int64) (int, bool) {
	if value < 0 || value > int64(math.MaxInt) {
		return 0, false
	}
	return int(value), true
}

func equivalentCacheV2CeilDiv(value, divisor int64) int64 {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
