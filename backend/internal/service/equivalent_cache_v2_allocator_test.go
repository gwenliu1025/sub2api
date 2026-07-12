//go:build unit

package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheV2Allocator_ReadMajorConservesExactly(t *testing.T) {
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          ClaudeUsage{InputTokens: 2000, OutputTokens: 8000},
		Kind:              UsageAllocationKindReadMajor,
		RequestID:         "req-read",
		AccountID:         701,
		SessionGeneration: 1,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})

	require.True(t, ok)
	require.True(t, allocation.Valid())
	require.Equal(t, 8000, allocation.ResponseUsage.OutputTokens)
	require.Zero(t, allocation.ResponseUsage.CacheCreationInputTokens)
	require.Greater(t, allocation.ResponseUsage.CacheReadInputTokens, allocation.ResponseUsage.InputTokens)
	require.True(t, equivalentCacheV2VisibleRateWithin(
		allocation.ResponseUsage,
		equivalentCacheV2ReadVisibleRateMinPPM,
		equivalentCacheV2ReadVisibleRateMaxPPM,
	))
}

func TestEquivalentCacheV2Allocator_Create1hConservesExactly(t *testing.T) {
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          ClaudeUsage{InputTokens: 2000, OutputTokens: 8000},
		Kind:              UsageAllocationKindCreate1h,
		RequestID:         "req-create-1h",
		AccountID:         701,
		SessionGeneration: 2,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})

	require.True(t, ok)
	require.True(t, allocation.Valid())
	require.Zero(t, allocation.ResponseUsage.CacheCreation5mTokens)
	require.Greater(t, allocation.ResponseUsage.CacheCreation1hTokens, 0)
	require.Equal(
		t,
		allocation.ResponseUsage.CacheCreation1hTokens,
		allocation.ResponseUsage.CacheCreationInputTokens,
	)
	require.True(t, equivalentCacheV2VisibleRateWithin(
		allocation.ResponseUsage,
		equivalentCacheV2CreateVisibleRateMinPPM,
		equivalentCacheV2CreateVisibleRateMaxPPM,
	))
}

func TestEquivalentCacheV2Allocator_CreateMixedIsOneHourMajor(t *testing.T) {
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          ClaudeUsage{InputTokens: 2000, OutputTokens: 8000},
		Kind:              UsageAllocationKindCreateMixed,
		RequestID:         "req-create-mixed",
		AccountID:         701,
		SessionGeneration: 3,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})

	require.True(t, ok)
	require.True(t, allocation.Valid())
	require.Greater(t, allocation.ResponseUsage.CacheCreation5mTokens, 0)
	require.Greater(t, allocation.ResponseUsage.CacheCreation1hTokens, 0)
	require.True(t, equivalentCacheV2OneHourCreationShareWithin(allocation.ResponseUsage))
}

func TestEquivalentCacheV2Allocator_IsDeterministicForRetry(t *testing.T) {
	input := equivalentCacheV2AllocationInput{
		RawUsage:          ClaudeUsage{InputTokens: 8192, OutputTokens: 1234},
		Kind:              UsageAllocationKindCreateMixed,
		RequestID:         "req-retry",
		AccountID:         99,
		SessionGeneration: 4,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	}

	first, firstOK := allocateEquivalentCacheV2(input)
	second, secondOK := allocateEquivalentCacheV2(input)

	require.Equal(t, firstOK, secondOK)
	require.Equal(t, first, second)
}

func TestEquivalentCacheV2Allocator_FallsBackWhenNoExactBoundedSolutionExists(t *testing.T) {
	raw := ClaudeUsage{InputTokens: 1, OutputTokens: 7}
	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          raw,
		Kind:              UsageAllocationKindReadMajor,
		RequestID:         "req-tiny",
		AccountID:         1,
		SessionGeneration: 1,
		VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
	})

	require.False(t, ok)
	require.Equal(t, EquivalentCacheAllocation{}, allocation)
	require.Equal(t, ClaudeUsage{InputTokens: 1, OutputTokens: 7}, raw)
}

func TestEquivalentCacheV2Allocator_AcceptedAllocationsAlwaysConserve(t *testing.T) {
	kinds := []UsageAllocationKind{
		UsageAllocationKindReadMajor,
		UsageAllocationKindCreate1h,
		UsageAllocationKindCreateMixed,
	}
	accepted := 0
	for rawInput := 32; rawInput <= 10000; rawInput += 17 {
		for _, kind := range kinds {
			allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
				RawUsage:          ClaudeUsage{InputTokens: rawInput, OutputTokens: rawInput / 2},
				Kind:              kind,
				RequestID:         fmt.Sprintf("req-%d-%d", rawInput, kind),
				AccountID:         701,
				SessionGeneration: int64(rawInput % 7),
				VisibleRateMinPPM: equivalentCacheV2DefaultVisibleRateMinPPM,
				VisibleRateMaxPPM: equivalentCacheV2DefaultVisibleRateMaxPPM,
			})
			if !ok {
				continue
			}
			accepted++
			require.True(t, allocation.Valid(), "raw=%d kind=%d allocation=%+v", rawInput, kind, allocation)
		}
	}
	require.Greater(t, accepted, 1000)
}
