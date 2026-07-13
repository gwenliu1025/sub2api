package repository

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	equivalentCacheV2StateRedisPrefix        = "equivalent_cache_v2:state:"
	equivalentCacheV2StateTTLMilliseconds    = 70 * 60 * 1000
	equivalentCacheV2MinimumReads            = 3
	equivalentCacheV2MinimumAgeMilliseconds  = 10 * 60 * 1000
	equivalentCacheV2RefreshMinMilliseconds  = 45 * 60 * 1000
	equivalentCacheV2RefreshSpanMilliseconds = 20 * 60 * 1000
	equivalentCacheV2MinimumGrowth           = 1024
)

var equivalentCacheV2StateDecideScript = redis.NewScript(`
local key = KEYS[1]
local decision_key = KEYS[2]
local request_fingerprint = ARGV[1]
local raw_input_tokens = tonumber(ARGV[2])
local account_id = ARGV[3]
local ttl_ms = tonumber(ARGV[4])
local minimum_reads = tonumber(ARGV[5])
local minimum_age_ms = tonumber(ARGV[6])
local refresh_min_ms = tonumber(ARGV[7])
local refresh_span_ms = tonumber(ARGV[8])
local minimum_growth = tonumber(ARGV[9])
local algorithm_version = ARGV[10]

local redis_time = redis.call("TIME")
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)

local previous_decision = redis.call("GET", decision_key)
if previous_decision and redis.call("EXISTS", key) == 1 then
  local packed_decision = tonumber(previous_decision)
  redis.call("HSET", key, "last_request_at_ms", now_ms)
  redis.call("PEXPIRE", key, ttl_ms)
  redis.call("PEXPIRE", decision_key, ttl_ms)
  return {packed_decision % 2, math.floor(packed_decision / 2)}
elseif previous_decision then
  redis.call("DEL", decision_key)
end

local function save_decision(create, generation)
  local packed_decision = generation * 2 + create
  redis.call("SET", decision_key, packed_decision, "PX", ttl_ms)
  redis.call("HSET", key, "last_request_at_ms", now_ms)
  redis.call("PEXPIRE", key, ttl_ms)
  return {create, generation}
end

local function refresh_offset_ms(generation)
  local material = request_fingerprint .. ":" .. account_id .. ":" ..
    tostring(generation) .. ":" .. algorithm_version
  local digest = redis.sha1hex(material)
  local sample = tonumber(string.sub(digest, 1, 8), 16)
  return sample % (refresh_span_ms + 1)
end

local generation = tonumber(redis.call("HGET", key, "generation") or "0")
if generation <= 0 then
  generation = 1
  redis.call("HSET", key,
    "last_create_at_ms", now_ms,
    "last_request_at_ms", now_ms,
    "reads_since_create", 0,
    "last_create_tokens", raw_input_tokens,
    "last_raw_input_tokens", raw_input_tokens,
    "generation", generation,
    "refresh_at_ms", now_ms + refresh_min_ms + refresh_offset_ms(generation))
  return save_decision(1, generation)
end

local last_create_at_ms = tonumber(redis.call("HGET", key, "last_create_at_ms") or tostring(now_ms))
local reads_since_create = tonumber(redis.call("HGET", key, "reads_since_create") or "0")
local last_create_tokens = tonumber(redis.call("HGET", key, "last_create_tokens") or tostring(raw_input_tokens))
local refresh_at_ms = tonumber(redis.call("HGET", key, "refresh_at_ms") or
  tostring(last_create_at_ms + refresh_min_ms + refresh_span_ms))
local protected = reads_since_create >= minimum_reads and
  now_ms - last_create_at_ms >= minimum_age_ms
local grew_enough = raw_input_tokens - last_create_tokens >= minimum_growth and
  raw_input_tokens * 4 >= last_create_tokens * 5
local refresh_due = now_ms >= refresh_at_ms

if protected and (grew_enough or refresh_due) then
  generation = generation + 1
  redis.call("HSET", key,
    "last_create_at_ms", now_ms,
    "reads_since_create", 0,
    "last_create_tokens", raw_input_tokens,
    "last_raw_input_tokens", raw_input_tokens,
    "generation", generation,
    "refresh_at_ms", now_ms + refresh_min_ms + refresh_offset_ms(generation))
  return save_decision(1, generation)
end

redis.call("HSET", key,
  "reads_since_create", reads_since_create + 1,
  "last_raw_input_tokens", raw_input_tokens)
return save_decision(0, generation)
`)

type equivalentCacheV2StateStore struct {
	rdb *redis.Client
}

func NewEquivalentCacheV2StateStore(rdb *redis.Client) service.EquivalentCacheV2StateStore {
	return &equivalentCacheV2StateStore{rdb: rdb}
}

func (s *equivalentCacheV2StateStore) DecideAndUpdate(
	ctx context.Context,
	input service.EquivalentCacheV2StateInput,
) (service.EquivalentCacheV2StateDecision, error) {
	if s == nil || s.rdb == nil {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: redis client is nil")
	}
	if input.SessionKey == "" {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: session key is empty")
	}
	if input.RequestID == "" {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: request id is empty")
	}
	if input.RawInputTokens < 0 {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: raw input tokens is negative")
	}

	values, err := equivalentCacheV2StateDecideScript.Run(
		ctx,
		s.rdb,
		[]string{
			equivalentCacheV2StateRedisKey(input.SessionKey),
			equivalentCacheV2DecisionRedisKey(input.SessionKey, input.RequestID),
		},
		equivalentCacheV2StateFingerprint(input.RequestID),
		input.RawInputTokens,
		input.AccountID,
		equivalentCacheV2StateTTLMilliseconds,
		equivalentCacheV2MinimumReads,
		equivalentCacheV2MinimumAgeMilliseconds,
		equivalentCacheV2RefreshMinMilliseconds,
		equivalentCacheV2RefreshSpanMilliseconds,
		equivalentCacheV2MinimumGrowth,
		service.EquivalentCacheV2AlgorithmVersion,
	).Slice()
	if err != nil {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: %w", err)
	}
	if len(values) != 2 {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf(
			"decide equivalent cache v2 state: script returned %d values",
			len(values),
		)
	}

	create, err := equivalentCacheV2StateResultInt64(values[0])
	if err != nil {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: parse create: %w", err)
	}
	generation, err := equivalentCacheV2StateResultInt64(values[1])
	if err != nil {
		return service.EquivalentCacheV2StateDecision{}, fmt.Errorf("decide equivalent cache v2 state: parse generation: %w", err)
	}

	return service.EquivalentCacheV2StateDecision{
		Create:     create == 1,
		Generation: generation,
	}, nil
}

func equivalentCacheV2StateRedisKey(sessionKey string) string {
	return equivalentCacheV2StateRedisPrefix + "{" + sessionKey + "}:state"
}

func equivalentCacheV2DecisionRedisKey(sessionKey, requestID string) string {
	return equivalentCacheV2StateRedisPrefix + "{" + sessionKey + "}:request:" +
		strconv.FormatInt(equivalentCacheV2StateFingerprint(requestID), 10)
}

func equivalentCacheV2StateFingerprint(value string) int64 {
	sum := sha256.Sum256([]byte(value))
	return int64(binary.BigEndian.Uint64(sum[:8]) & uint64(^uint64(0)>>1))
}

func equivalentCacheV2StateResultInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("unexpected value type %T", value)
	}
}

var _ service.EquivalentCacheV2StateStore = (*equivalentCacheV2StateStore)(nil)
