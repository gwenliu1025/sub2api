package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net"
	"strconv"
	"strings"
)

type EquivalentCacheV2StateInput struct {
	SessionKey     string
	RequestID      string
	AccountID      int64
	RawInputTokens int64
}

type EquivalentCacheV2StateDecision struct {
	Create     bool
	Generation int64
}

type EquivalentCacheV2StateStore interface {
	DecideAndUpdate(context.Context, EquivalentCacheV2StateInput) (EquivalentCacheV2StateDecision, error)
}

func GenerateEquivalentCacheV2SessionKey(accountID, apiKeyID int64, parsed *ParsedRequest) string {
	metadataUserID := ""
	if parsed != nil {
		metadataUserID = strings.TrimSpace(parsed.MetadataUserID)
	}
	if metadataUserID != "" {
		return equivalentCacheV2HashParts(
			"metadata",
			strconv.FormatInt(accountID, 10),
			strconv.FormatInt(apiKeyID, 10),
			metadataUserID,
		)
	}

	return equivalentCacheV2HashParts(
		"fallback",
		strconv.FormatInt(accountID, 10),
		strconv.FormatInt(apiKeyID, 10),
		equivalentCacheV2ClientDiscriminator(parsed),
		equivalentCacheV2FirstUserMessage(parsed),
	)
}

func equivalentCacheV2CreationKind(requestID string, accountID, generation int64) UsageAllocationKind {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		requestID,
		strconv.FormatInt(accountID, 10),
		strconv.FormatInt(generation, 10),
		strconv.FormatInt(int64(equivalentCacheV2AlgorithmVersion), 10),
	}, "\x00")))
	if binary.BigEndian.Uint64(sum[:8])%100 < 15 {
		return UsageAllocationKindCreateMixed
	}
	return UsageAllocationKindCreate1h
}

func equivalentCacheV2HashParts(parts ...string) string {
	hash := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func equivalentCacheV2ClientDiscriminator(parsed *ParsedRequest) string {
	if parsed == nil || parsed.SessionContext == nil {
		return ""
	}
	return equivalentCacheV2HashParts(
		equivalentCacheV2NormalizeIP(parsed.SessionContext.ClientIP),
		NormalizeSessionUserAgent(parsed.SessionContext.UserAgent),
	)
}

func equivalentCacheV2NormalizeIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String()
	}
	return strings.ToLower(raw)
}

func equivalentCacheV2FirstUserMessage(parsed *ParsedRequest) string {
	if parsed == nil {
		return ""
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := parsed.DecodeMessages(&messages); err != nil {
		return ""
	}
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		return equivalentCacheV2CanonicalContent(message.Content)
	}
	return ""
}

func equivalentCacheV2CanonicalContent(content json.RawMessage) string {
	var decoded any
	if err := json.Unmarshal(content, &decoded); err != nil {
		return string(content)
	}
	normalized := equivalentCacheV2NormalizeContentValue(decoded, "")
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return string(content)
	}
	return string(canonical)
}

func equivalentCacheV2NormalizeContentValue(value any, key string) any {
	switch typed := value.(type) {
	case string:
		if key == "" || strings.EqualFold(key, "text") {
			return equivalentCacheV2NormalizeText(typed)
		}
		return typed
	case []any:
		normalized := make([]any, len(typed))
		for i, item := range typed {
			normalized[i] = equivalentCacheV2NormalizeContentValue(item, "")
		}
		return normalized
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			normalized[childKey] = equivalentCacheV2NormalizeContentValue(childValue, childKey)
		}
		return normalized
	default:
		return value
	}
}

func equivalentCacheV2NormalizeText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
