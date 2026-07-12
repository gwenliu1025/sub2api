package service

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	equivalentCacheV2AllocationHeaderName  = "X-Sub2API-Usage-Allocation"
	equivalentCacheV2AllocationHeaderValue = "equivalent-cache-v2"
)

type equivalentCacheV2ResponsePlan struct {
	Config     equivalentCacheV2Config
	AccountID  int64
	APIKeyID   int64
	SessionKey string
	RequestID  string
	StateStore EquivalentCacheV2StateStore
}

type equivalentCacheV2ResponseResult struct {
	Body          []byte
	RawUsage      ClaudeUsage
	ResponseUsage ClaudeUsage
	Version       int16
	Kind          UsageAllocationKind
	Allocated     bool
	UsageValid    bool
}

func (r equivalentCacheV2ResponseResult) HeaderValue() string {
	if !r.Allocated {
		return ""
	}
	return equivalentCacheV2AllocationHeaderValue
}

func applyEquivalentCacheV2JSON(
	ctx context.Context,
	body []byte,
	usagePath string,
	plan equivalentCacheV2ResponsePlan,
) equivalentCacheV2ResponseResult {
	rawUsage, ok := parseEquivalentCacheV2Usage(body, usagePath)
	result := equivalentCacheV2RawResponseResult(body, rawUsage, ok)
	if !ok || hasNonZeroCacheUsage(rawUsage) || !equivalentCacheV2ResponsePlanEnabled(plan) {
		return result
	}

	kind := UsageAllocationKindReadMajor
	generation := int64(0)
	if plan.StateStore != nil {
		decision, err := plan.StateStore.DecideAndUpdate(ctx, EquivalentCacheV2StateInput{
			SessionKey:     plan.SessionKey,
			RequestID:      plan.RequestID,
			AccountID:      plan.AccountID,
			RawInputTokens: int64(rawUsage.InputTokens),
		})
		if err != nil {
			kind = UsageAllocationKindStateless
		} else {
			generation = decision.Generation
			if decision.Create {
				kind = equivalentCacheV2CreationKind(plan.RequestID, plan.AccountID, generation)
			}
		}
	} else {
		kind = UsageAllocationKindStateless
	}

	allocation, ok := allocateEquivalentCacheV2(equivalentCacheV2AllocationInput{
		RawUsage:          rawUsage,
		Kind:              kind,
		RequestID:         plan.RequestID,
		AccountID:         plan.AccountID,
		SessionGeneration: generation,
		VisibleRateMinPPM: plan.Config.VisibleRateMinPPM,
		VisibleRateMaxPPM: plan.Config.VisibleRateMaxPPM,
	})
	if !ok || !allocation.Valid() || plan.Config.Mode != equivalentCacheV2ModeActive {
		return result
	}

	rewritten, ok := rewriteEquivalentCacheV2Usage(body, usagePath, allocation.ResponseUsage)
	if !ok {
		return result
	}
	return equivalentCacheV2ResponseResult{
		Body:          rewritten,
		RawUsage:      cloneClaudeUsage(allocation.RawUsage),
		ResponseUsage: cloneClaudeUsage(allocation.ResponseUsage),
		Version:       allocation.Version,
		Kind:          allocation.Kind,
		Allocated:     true,
		UsageValid:    true,
	}
}

func equivalentCacheV2RawResponseResult(body []byte, rawUsage ClaudeUsage, usageValid bool) equivalentCacheV2ResponseResult {
	return equivalentCacheV2ResponseResult{
		Body:          body,
		RawUsage:      cloneClaudeUsage(rawUsage),
		ResponseUsage: cloneClaudeUsage(rawUsage),
		UsageValid:    usageValid,
	}
}

func equivalentCacheV2ResolvedPricingEligible(resolved *ResolvedPricing) bool {
	return resolved != nil &&
		resolved.Mode == BillingModeToken &&
		len(resolved.Intervals) == 0 &&
		equivalentCacheV2PricingMatches(resolved.BasePricing)
}

func (s *GatewayService) prepareEquivalentCacheV2ResponsePlan(
	ctx context.Context,
	account *Account,
	parsed *ParsedRequest,
	billingModel string,
	upstreamRequestID string,
) *equivalentCacheV2ResponsePlan {
	cfg, ok := equivalentCacheV2ConfigFromAccount(account)
	if !ok ||
		s == nil ||
		s.resolver == nil ||
		parsed == nil ||
		parsed.GroupID == nil ||
		*parsed.GroupID <= 0 ||
		parsed.SessionContext == nil ||
		parsed.SessionContext.APIKeyID <= 0 ||
		strings.TrimSpace(billingModel) == "" {
		return nil
	}

	resolved := s.resolver.Resolve(ctx, PricingInput{
		Model:   billingModel,
		GroupID: parsed.GroupID,
	})
	if !equivalentCacheV2ResolvedPricingEligible(resolved) {
		return nil
	}

	apiKeyID := parsed.SessionContext.APIKeyID
	requestID := resolveUsageBillingRequestID(ctx, upstreamRequestID)
	sessionKey := GenerateEquivalentCacheV2SessionKey(account.ID, apiKeyID, parsed)
	plan := &equivalentCacheV2ResponsePlan{
		Config:     cfg,
		AccountID:  account.ID,
		APIKeyID:   apiKeyID,
		SessionKey: sessionKey,
		RequestID:  requestID,
		StateStore: s.equivalentCacheV2StateStore,
	}
	if !equivalentCacheV2ResponsePlanEnabled(*plan) {
		return nil
	}
	return plan
}

func equivalentCacheV2ResponsePlanEnabled(plan equivalentCacheV2ResponsePlan) bool {
	return plan.Config.Enabled &&
		plan.Config.KiroGoPoolConfirmed &&
		plan.Config.PricingProfile == equivalentCacheV2PricingProfile &&
		(plan.Config.Mode == equivalentCacheV2ModeShadow || plan.Config.Mode == equivalentCacheV2ModeActive) &&
		plan.Config.VisibleRateMinPPM > 0 &&
		plan.Config.VisibleRateMaxPPM < equivalentCacheV2RateScale &&
		plan.Config.VisibleRateMinPPM <= plan.Config.VisibleRateMaxPPM &&
		plan.AccountID > 0 &&
		plan.APIKeyID > 0 &&
		strings.TrimSpace(plan.SessionKey) != "" &&
		strings.TrimSpace(plan.RequestID) != ""
}

func parseEquivalentCacheV2Usage(body []byte, usagePath string) (ClaudeUsage, bool) {
	usageNode := gjson.GetBytes(body, usagePath)
	if !usageNode.Exists() || !usageNode.IsObject() {
		return ClaudeUsage{}, false
	}

	decoder := json.NewDecoder(bytes.NewBufferString(usageNode.Raw))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return ClaudeUsage{}, false
	}

	inputTokens, ok := equivalentCacheV2RequiredUsageInt(values, "input_tokens")
	if !ok {
		return ClaudeUsage{}, false
	}
	outputTokens, ok := equivalentCacheV2RequiredUsageInt(values, "output_tokens")
	if !ok {
		return ClaudeUsage{}, false
	}
	cacheRead, ok := equivalentCacheV2OptionalUsageInt(values, "cache_read_input_tokens")
	if !ok {
		return ClaudeUsage{}, false
	}
	cacheCreation, cacheCreationExists, ok := equivalentCacheV2OptionalUsageIntWithPresence(values, "cache_creation_input_tokens")
	if !ok {
		return ClaudeUsage{}, false
	}

	var cacheCreation5m, cacheCreation1h int
	if rawCreation, exists := values["cache_creation"]; exists {
		creation, ok := rawCreation.(map[string]any)
		if !ok {
			return ClaudeUsage{}, false
		}
		cacheCreation5m, ok = equivalentCacheV2OptionalUsageInt(creation, "ephemeral_5m_input_tokens")
		if !ok {
			return ClaudeUsage{}, false
		}
		cacheCreation1h, ok = equivalentCacheV2OptionalUsageInt(creation, "ephemeral_1h_input_tokens")
		if !ok {
			return ClaudeUsage{}, false
		}
	}
	if !cacheCreationExists {
		cacheCreation = cacheCreation5m + cacheCreation1h
	}

	if cachedTokens, exists, ok := equivalentCacheV2OptionalUsageIntWithPresence(values, "cached_tokens"); !ok {
		return ClaudeUsage{}, false
	} else if exists && cachedTokens != 0 {
		return ClaudeUsage{}, false
	}

	usage := ClaudeUsage{
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: cacheCreation,
		CacheReadInputTokens:     cacheRead,
		CacheCreation5mTokens:    cacheCreation5m,
		CacheCreation1hTokens:    cacheCreation1h,
	}
	if _, ok := fixedInputCostUnits(usage); !ok {
		return ClaudeUsage{}, false
	}
	return usage, true
}

func equivalentCacheV2RequiredUsageInt(values map[string]any, key string) (int, bool) {
	value, exists := values[key]
	if !exists {
		return 0, false
	}
	return equivalentCacheV2UsageInt(value)
}

func equivalentCacheV2OptionalUsageInt(values map[string]any, key string) (int, bool) {
	value, _, ok := equivalentCacheV2OptionalUsageIntWithPresence(values, key)
	return value, ok
}

func equivalentCacheV2OptionalUsageIntWithPresence(values map[string]any, key string) (int, bool, bool) {
	value, exists := values[key]
	if !exists {
		return 0, false, true
	}
	parsed, ok := equivalentCacheV2UsageInt(value)
	return parsed, true, ok
}

func equivalentCacheV2UsageInt(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	raw := number.String()
	if strings.ContainsAny(raw, ".eE") {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 || parsed > int64(math.MaxInt) {
		return 0, false
	}
	return int(parsed), true
}

func rewriteEquivalentCacheV2Usage(body []byte, usagePath string, usage ClaudeUsage) ([]byte, bool) {
	creationTotal := usage.CacheCreation5mTokens + usage.CacheCreation1hTokens
	if usage.OutputTokens < 0 ||
		usage.CacheCreationInputTokens != creationTotal {
		return nil, false
	}

	updates := []struct {
		path  string
		value int
	}{
		{path: usagePath + ".input_tokens", value: usage.InputTokens},
		{path: usagePath + ".output_tokens", value: usage.OutputTokens},
		{path: usagePath + ".cache_read_input_tokens", value: usage.CacheReadInputTokens},
		{path: usagePath + ".cache_creation_input_tokens", value: creationTotal},
		{path: usagePath + ".cache_creation.ephemeral_5m_input_tokens", value: usage.CacheCreation5mTokens},
		{path: usagePath + ".cache_creation.ephemeral_1h_input_tokens", value: usage.CacheCreation1hTokens},
	}

	rewritten := append([]byte(nil), body...)
	for _, update := range updates {
		next, err := sjson.SetBytes(rewritten, update.path, update.value)
		if err != nil {
			return nil, false
		}
		rewritten = next
	}
	return rewritten, true
}
