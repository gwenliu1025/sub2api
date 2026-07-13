//go:build unit

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheV2State_FirstRequestCreatesAndRetryIsIdempotent(t *testing.T) {
	store, _ := newEquivalentCacheV2StateTestStore(t)
	input := service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "request-1",
		RawInputTokens: 2000,
	}

	first, err := store.DecideAndUpdate(context.Background(), input)
	require.NoError(t, err)
	require.True(t, first.Create)
	require.EqualValues(t, 1, first.Generation)

	retry, err := store.DecideAndUpdate(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, first, retry)
}

func TestEquivalentCacheV2State_GrowthNeedsReadsAndMinimumInterval(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	ctx := context.Background()

	first, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "create",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.True(t, first.Create)

	for i := 0; i < 4; i++ {
		decision, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
			SessionKey:     "session",
			RequestID:      fmt.Sprintf("read-%d", i),
			RawInputTokens: 2000,
		})
		require.NoError(t, err)
		require.False(t, decision.Create)
	}

	tooSoon, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "growth-too-soon",
		RawInputTokens: 3024,
	})
	require.NoError(t, err)
	require.False(t, tooSoon.Create)

	advanceEquivalentCacheV2StateTime(t, mr, 10*time.Minute)
	growth, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "growth",
		RawInputTokens: 3024,
	})
	require.NoError(t, err)
	require.True(t, growth.Create)
	require.EqualValues(t, 2, growth.Generation)
}

func TestEquivalentCacheV2State_GrowthRequiresBothThresholds(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	ctx := context.Background()

	_, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "create",
		RawInputTokens: 8000,
	})
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		_, err = store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
			SessionKey:     "session",
			RequestID:      fmt.Sprintf("read-%d", i),
			RawInputTokens: 8000,
		})
		require.NoError(t, err)
	}
	advanceEquivalentCacheV2StateTime(t, mr, 10*time.Minute)

	onlyAbsolute, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "only-absolute",
		RawInputTokens: 9024,
	})
	require.NoError(t, err)
	require.False(t, onlyAbsolute.Create)

	meetsBoth, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "only-percent",
		RawInputTokens: 10000,
	})
	require.NoError(t, err)
	require.True(t, meetsBoth.Create)
}

func TestEquivalentCacheV2State_GrowthRejectsPercentOnly(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	ctx := context.Background()

	_, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "create",
		RawInputTokens: 1000,
	})
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		_, err = store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
			SessionKey:     "session",
			RequestID:      fmt.Sprintf("read-%d", i),
			RawInputTokens: 1000,
		})
		require.NoError(t, err)
	}
	advanceEquivalentCacheV2StateTime(t, mr, 10*time.Minute)

	percentOnly, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "percent-only",
		RawInputTokens: 1250,
	})
	require.NoError(t, err)
	require.False(t, percentOnly.Create)
}

func TestEquivalentCacheV2State_RefreshUsesProtectedFortyFiveToSixtyFiveMinuteWindow(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	ctx := context.Background()

	_, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "create",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		_, err = store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
			SessionKey:     "session",
			RequestID:      fmt.Sprintf("read-%d", i),
			RawInputTokens: 2000,
		})
		require.NoError(t, err)
	}

	advanceEquivalentCacheV2StateTime(t, mr, 44*time.Minute)
	beforeWindow, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "before-window",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.False(t, beforeWindow.Create)

	advanceEquivalentCacheV2StateTime(t, mr, 21*time.Minute)
	inWindow, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "in-window",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.True(t, inWindow.Create)
	require.EqualValues(t, 2, inWindow.Generation)
}

func TestEquivalentCacheV2State_RefreshRequiresThreePriorReads(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	ctx := context.Background()

	_, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "create",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
			SessionKey:     "session",
			RequestID:      fmt.Sprintf("read-%d", i),
			RawInputTokens: 2000,
		})
		require.NoError(t, err)
	}
	advanceEquivalentCacheV2StateTime(t, mr, 65*time.Minute)

	protected, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "protected",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.True(t, protected.Create)
	require.EqualValues(t, 2, protected.Generation)
}

func TestEquivalentCacheV2State_ConcurrentNewSessionHasAtMostOneCreate(t *testing.T) {
	store, _ := newEquivalentCacheV2StateTestStore(t)
	const workers = 32

	var wg sync.WaitGroup
	results := make(chan service.EquivalentCacheV2StateDecision, workers)
	errorsCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decision, err := store.DecideAndUpdate(context.Background(), service.EquivalentCacheV2StateInput{
				SessionKey:     "session",
				RequestID:      fmt.Sprintf("request-%d", i),
				RawInputTokens: 2000,
			})
			if err != nil {
				errorsCh <- err
				return
			}
			results <- decision
		}(i)
	}
	wg.Wait()
	close(results)
	close(errorsCh)

	for err := range errorsCh {
		require.NoError(t, err)
	}
	creates := 0
	for decision := range results {
		if decision.Create {
			creates++
		}
	}
	require.Equal(t, 1, creates)
}

func TestEquivalentCacheV2State_ExpiresAfterSeventyMinutes(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	_, err := store.DecideAndUpdate(context.Background(), service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "request",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.True(t, mr.Exists(equivalentCacheV2StateRedisKey("session")))

	advanceEquivalentCacheV2StateTime(t, mr, 69*time.Minute)
	require.True(t, mr.Exists(equivalentCacheV2StateRedisKey("session")))
	advanceEquivalentCacheV2StateTime(t, mr, 2*time.Minute)
	require.False(t, mr.Exists(equivalentCacheV2StateRedisKey("session")))
}

func TestEquivalentCacheV2State_UsesRedisServerAbsoluteTime(t *testing.T) {
	mr := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 123_000_000)
	mr.SetTime(now)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewEquivalentCacheV2StateStore(client)

	_, err := store.DecideAndUpdate(context.Background(), service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "request",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)

	stored, err := client.HGet(
		context.Background(),
		equivalentCacheV2StateRedisKey("session"),
		"last_request_at_ms",
	).Int64()
	require.NoError(t, err)
	require.Equal(t, now.UnixMilli(), stored)
}

func TestEquivalentCacheV2State_IdempotencyHistoryIsBounded(t *testing.T) {
	store, mr := newEquivalentCacheV2StateTestStore(t)
	ctx := context.Background()

	first, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "original",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.True(t, first.Create)

	for i := 0; i < 256; i++ {
		_, err = store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
			SessionKey:     "session",
			RequestID:      fmt.Sprintf("request-%d", i),
			RawInputTokens: 2000,
		})
		require.NoError(t, err)
	}

	fields, err := mr.HKeys(equivalentCacheV2StateRedisKey("session"))
	require.NoError(t, err)
	require.LessOrEqual(t, len(fields), 64)

	retry, err := store.DecideAndUpdate(ctx, service.EquivalentCacheV2StateInput{
		SessionKey:     "session",
		RequestID:      "original",
		RawInputTokens: 2000,
	})
	require.NoError(t, err)
	require.Equal(t, first, retry)
}

func TestEquivalentCacheV2State_RefreshSeedUsesRequestAccountGenerationAndVersion(t *testing.T) {
	refreshAt := func(requestID string, accountID int64) int64 {
		mr := miniredis.RunT(t)
		mr.SetTime(time.Unix(1_700_000_000, 0))
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		store := NewEquivalentCacheV2StateStore(client)

		_, err := store.DecideAndUpdate(context.Background(), service.EquivalentCacheV2StateInput{
			SessionKey:     "same-session",
			RequestID:      requestID,
			AccountID:      accountID,
			RawInputTokens: 2000,
		})
		require.NoError(t, err)
		value, err := client.HGet(
			context.Background(),
			equivalentCacheV2StateRedisKey("same-session"),
			"refresh_at_ms",
		).Int64()
		require.NoError(t, err)
		return value
	}

	base := refreshAt("request-a", 701)
	require.NotEqual(t, base, refreshAt("request-b", 701))
	require.NotEqual(t, base, refreshAt("request-a", 702))
}

func newEquivalentCacheV2StateTestStore(t *testing.T) (service.EquivalentCacheV2StateStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.Unix(1_700_000_000, 0))
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewEquivalentCacheV2StateStore(client), mr
}

func advanceEquivalentCacheV2StateTime(t *testing.T, mr *miniredis.Miniredis, duration time.Duration) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	now, err := client.Time(context.Background()).Result()
	require.NoError(t, err)
	require.NoError(t, client.Close())
	mr.SetTime(now.Add(duration))
	mr.FastForward(duration)
}
