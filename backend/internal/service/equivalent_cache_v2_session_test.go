//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheV2SessionKey_MetadataHasPriorityAndStaysStable(t *testing.T) {
	first := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"metadata":{"user_id":"private-session-id"},
		"messages":[{"role":"user","content":"first"}]
	}`)
	second := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"metadata":{"user_id":"private-session-id"},
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"answer"},
			{"role":"user","content":"next"}
		]
	}`)

	firstKey := GenerateEquivalentCacheV2SessionKey(701, 501, first)
	secondKey := GenerateEquivalentCacheV2SessionKey(701, 501, second)

	require.Len(t, firstKey, 64)
	require.Equal(t, firstKey, secondKey)
	require.NotContains(t, firstKey, "private-session-id")
}

func TestEquivalentCacheV2SessionKey_FallbackStaysStableAcrossAppendedMessages(t *testing.T) {
	first := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"messages":[{"role":"user","content":"stable first prompt"}]
	}`)
	second := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"messages":[
			{"role":"user","content":"stable first prompt"},
			{"role":"assistant","content":"answer"},
			{"role":"user","content":"next"}
		]
	}`)
	first.SessionContext = &SessionContext{ClientIP: "192.0.2.10", UserAgent: "client/1.0.0", APIKeyID: 501}
	second.SessionContext = &SessionContext{ClientIP: "192.0.2.10", UserAgent: "client/1.0.1", APIKeyID: 501}

	require.Equal(
		t,
		GenerateEquivalentCacheV2SessionKey(701, 501, first),
		GenerateEquivalentCacheV2SessionKey(701, 501, second),
	)
}

func TestEquivalentCacheV2SessionKey_SeparatesAccountAndAPIKey(t *testing.T) {
	parsed := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"metadata":{"user_id":"same"},
		"messages":[{"role":"user","content":"same"}]
	}`)

	base := GenerateEquivalentCacheV2SessionKey(701, 501, parsed)
	require.NotEqual(t, base, GenerateEquivalentCacheV2SessionKey(702, 501, parsed))
	require.NotEqual(t, base, GenerateEquivalentCacheV2SessionKey(701, 502, parsed))
}

func TestEquivalentCacheV2SessionKey_DoesNotExposeRawInputs(t *testing.T) {
	parsed := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"messages":[{"role":"user","content":"secret prompt text"}]
	}`)
	parsed.SessionContext = &SessionContext{
		ClientIP:  "203.0.113.20",
		UserAgent: "private-client/9.9.9",
		APIKeyID:  501,
	}

	key := GenerateEquivalentCacheV2SessionKey(701, 501, parsed)

	for _, raw := range []string{"secret", "203.0.113.20", "private-client"} {
		require.NotContains(t, strings.ToLower(key), strings.ToLower(raw))
	}
}

func TestEquivalentCacheV2SessionKey_FallbackHashesCompleteFirstUserMessage(t *testing.T) {
	first := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"same text"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"first"}}
			]
		}]
	}`)
	second := mustParseEquivalentCacheV2Request(t, `{
		"model":"claude-opus-4-5",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"same text"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"second"}}
			]
		}]
	}`)

	require.NotEqual(
		t,
		GenerateEquivalentCacheV2SessionKey(701, 501, first),
		GenerateEquivalentCacheV2SessionKey(701, 501, second),
	)
}

func TestEquivalentCacheV2CreationKind_IsDeterministicAndNeverPure5m(t *testing.T) {
	mixed := 0
	for generation := int64(1); generation <= 100; generation++ {
		first := equivalentCacheV2CreationKind("request", 701, generation)
		second := equivalentCacheV2CreationKind("request", 701, generation)
		require.Equal(t, first, second)
		require.Contains(t, []UsageAllocationKind{
			UsageAllocationKindCreate1h,
			UsageAllocationKindCreateMixed,
		}, first)
		if first == UsageAllocationKindCreateMixed {
			mixed++
		}
	}
	require.GreaterOrEqual(t, mixed, 10)
	require.LessOrEqual(t, mixed, 20)
}

func mustParseEquivalentCacheV2Request(t *testing.T, body string) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}
