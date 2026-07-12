package service

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	equivalentCacheBillingEnabledKey           = "equivalent_cache_billing_enabled"
	equivalentCacheBillingLegacyKiroEnabledKey = "kiro_equivalent_cache_billing_enabled"

	equivalentCacheV2ExtraKey       = "equivalent_cache_allocation_v2"
	equivalentCacheV2PricingProfile = "kiro_unified_5_25_0_6_6_25_10"

	equivalentCacheV2DefaultVisibleRateMinPPM int64 = 960000
	equivalentCacheV2DefaultVisibleRateMaxPPM int64 = 999000
	equivalentCacheV2RateScale                int64 = 1000000
)

type equivalentCacheV2Mode string

const (
	equivalentCacheV2ModeOff    equivalentCacheV2Mode = "off"
	equivalentCacheV2ModeShadow equivalentCacheV2Mode = "shadow"
	equivalentCacheV2ModeActive equivalentCacheV2Mode = "active"
)

type equivalentCacheV2Config struct {
	Enabled             bool
	Mode                equivalentCacheV2Mode
	PricingProfile      string
	VisibleRateMinPPM   int64
	VisibleRateMaxPPM   int64
	KiroGoPoolConfirmed bool
}

func equivalentCacheV2ConfigFromAccount(account *Account) (equivalentCacheV2Config, bool) {
	if account == nil {
		return equivalentCacheV2Config{}, false
	}
	if account.getExtraBool(equivalentCacheBillingEnabledKey) ||
		account.getExtraBool(equivalentCacheBillingLegacyKiroEnabledKey) {
		logger.LegacyPrintf(
			"service.gateway",
			"legacy equivalent cache billing is disabled; using raw usage: account=%d",
			account.ID,
		)
		return equivalentCacheV2Config{}, false
	}

	raw, ok := account.Extra[equivalentCacheV2ExtraKey]
	if !ok {
		return equivalentCacheV2Config{}, false
	}
	nested, ok := raw.(map[string]any)
	if !ok {
		return equivalentCacheV2Config{}, false
	}

	cfg := equivalentCacheV2Config{
		Enabled:             equivalentCacheV2Bool(nested["enabled"]),
		Mode:                equivalentCacheV2Mode(strings.ToLower(strings.TrimSpace(equivalentCacheV2String(nested["mode"])))),
		PricingProfile:      strings.TrimSpace(equivalentCacheV2String(nested["pricing_profile"])),
		VisibleRateMinPPM:   equivalentCacheV2DefaultVisibleRateMinPPM,
		VisibleRateMaxPPM:   equivalentCacheV2DefaultVisibleRateMaxPPM,
		KiroGoPoolConfirmed: equivalentCacheV2Bool(nested["kiro_go_pool_confirmed"]),
	}
	if ppm, exists, valid := equivalentCacheV2RatePPM(nested["visible_rate_min"]); exists {
		if !valid {
			return equivalentCacheV2Config{}, false
		}
		cfg.VisibleRateMinPPM = ppm
	}
	if ppm, exists, valid := equivalentCacheV2RatePPM(nested["visible_rate_max"]); exists {
		if !valid {
			return equivalentCacheV2Config{}, false
		}
		cfg.VisibleRateMaxPPM = ppm
	}

	if !cfg.Enabled ||
		!cfg.KiroGoPoolConfirmed ||
		cfg.PricingProfile != equivalentCacheV2PricingProfile ||
		(cfg.Mode != equivalentCacheV2ModeShadow && cfg.Mode != equivalentCacheV2ModeActive) ||
		cfg.VisibleRateMinPPM <= 0 ||
		cfg.VisibleRateMaxPPM >= equivalentCacheV2RateScale ||
		cfg.VisibleRateMinPPM > cfg.VisibleRateMaxPPM {
		return equivalentCacheV2Config{}, false
	}
	return cfg, true
}

func equivalentCacheV2Bool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && parsed
	default:
		return false
	}
}

func equivalentCacheV2String(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func equivalentCacheV2RatePPM(value any) (int64, bool, bool) {
	if value == nil {
		return 0, false, true
	}
	rate, ok := parseEquivalentCacheV2Float64(value)
	if !ok || rate <= 0 || rate >= 1 {
		return 0, true, false
	}
	ppm := int64(rate*float64(equivalentCacheV2RateScale) + 0.5)
	if ppm <= 0 || ppm >= equivalentCacheV2RateScale {
		return 0, true, false
	}
	return ppm, true, true
}

func parseEquivalentCacheV2Float64(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		parsed, err := v.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
