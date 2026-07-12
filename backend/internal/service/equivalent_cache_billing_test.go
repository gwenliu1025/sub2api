//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheV2Config_LegacyKeysNeverEnableV2(t *testing.T) {
	for _, extra := range []map[string]any{
		{equivalentCacheBillingEnabledKey: true},
		{equivalentCacheBillingLegacyKiroEnabledKey: true},
		{
			equivalentCacheBillingEnabledKey:           true,
			equivalentCacheBillingLegacyKiroEnabledKey: true,
		},
	} {
		cfg, ok := equivalentCacheV2ConfigFromAccount(&Account{Extra: extra})
		require.False(t, ok)
		require.Equal(t, equivalentCacheV2Config{}, cfg)
	}
}

func TestEquivalentCacheV2Config_RequiresExplicitPoolConfirmation(t *testing.T) {
	account := &Account{Extra: map[string]any{
		equivalentCacheV2ExtraKey: map[string]any{
			"enabled":         true,
			"mode":            "active",
			"pricing_profile": equivalentCacheV2PricingProfile,
		},
	}}

	cfg, ok := equivalentCacheV2ConfigFromAccount(account)

	require.False(t, ok)
	require.Equal(t, equivalentCacheV2Config{}, cfg)
}

func TestEquivalentCacheV2Config_ParsesShadowAndActive(t *testing.T) {
	tests := []struct {
		name string
		mode equivalentCacheV2Mode
	}{
		{name: "shadow", mode: equivalentCacheV2ModeShadow},
		{name: "active", mode: equivalentCacheV2ModeActive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{Extra: map[string]any{
				equivalentCacheV2ExtraKey: map[string]any{
					"enabled":                true,
					"mode":                   string(tt.mode),
					"pricing_profile":        equivalentCacheV2PricingProfile,
					"visible_rate_min":       0.96,
					"visible_rate_max":       0.999,
					"kiro_go_pool_confirmed": true,
				},
			}}

			cfg, ok := equivalentCacheV2ConfigFromAccount(account)

			require.True(t, ok)
			require.True(t, cfg.Enabled)
			require.Equal(t, tt.mode, cfg.Mode)
			require.Equal(t, equivalentCacheV2PricingProfile, cfg.PricingProfile)
			require.EqualValues(t, 960000, cfg.VisibleRateMinPPM)
			require.EqualValues(t, 999000, cfg.VisibleRateMaxPPM)
			require.True(t, cfg.KiroGoPoolConfirmed)
		})
	}
}

func TestEquivalentCacheV2Config_OffDoesNotEnableAllocation(t *testing.T) {
	account := &Account{Extra: map[string]any{
		equivalentCacheV2ExtraKey: map[string]any{
			"enabled":                true,
			"mode":                   "off",
			"pricing_profile":        equivalentCacheV2PricingProfile,
			"kiro_go_pool_confirmed": true,
		},
	}}

	cfg, ok := equivalentCacheV2ConfigFromAccount(account)

	require.False(t, ok)
	require.Equal(t, equivalentCacheV2Config{}, cfg)
}

func TestEquivalentCacheV2Config_RejectsUnknownProfileAndInvalidRates(t *testing.T) {
	tests := []map[string]any{
		{
			"enabled":                true,
			"mode":                   "active",
			"pricing_profile":        "unknown",
			"kiro_go_pool_confirmed": true,
		},
		{
			"enabled":                true,
			"mode":                   "active",
			"pricing_profile":        equivalentCacheV2PricingProfile,
			"visible_rate_min":       1.0,
			"visible_rate_max":       0.99,
			"kiro_go_pool_confirmed": true,
		},
		{
			"enabled":                true,
			"mode":                   "invalid",
			"pricing_profile":        equivalentCacheV2PricingProfile,
			"kiro_go_pool_confirmed": true,
		},
	}

	for _, nested := range tests {
		cfg, ok := equivalentCacheV2ConfigFromAccount(&Account{Extra: map[string]any{
			equivalentCacheV2ExtraKey: nested,
		}})
		require.False(t, ok)
		require.Equal(t, equivalentCacheV2Config{}, cfg)
	}
}
