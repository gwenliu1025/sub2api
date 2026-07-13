package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEquivalentCacheV2AuditMigrationFollowsUpstream173(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	require.Contains(t, names, "173_allow_cyber_blocked_usage_request_type.sql")
	require.NotContains(t, names, "173_usage_log_equivalent_cache_v2_audit.sql")
	require.Contains(t, names, "174_usage_log_equivalent_cache_v2_audit.sql")
}

func TestEquivalentCacheAuditMigrationRemainsHistorical(t *testing.T) {
	const historicalMigration = "174_usage_log_equivalent_cache_v2_audit.sql"
	auditColumns := []string{
		"raw_input_tokens",
		"raw_output_tokens",
		"raw_cache_read_tokens",
		"raw_cache_creation_tokens",
		"raw_cache_creation_5m_tokens",
		"raw_cache_creation_1h_tokens",
		"usage_allocation_version",
		"usage_allocation_kind",
	}

	historicalBody, err := FS.ReadFile(historicalMigration)
	require.NoError(t, err, "历史迁移 174 必须保留")
	require.NotContains(t, strings.ToLower(string(historicalBody)), "drop column", "历史迁移本身不得被改写为回删列")

	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == historicalMigration || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, readErr := FS.ReadFile(entry.Name())
		require.NoError(t, readErr)
		lowerBody := strings.ToLower(string(body))
		if !strings.Contains(lowerBody, "drop column") {
			continue
		}
		for _, column := range auditColumns {
			require.NotContainsf(
				t,
				lowerBody,
				column,
				"迁移 %s 不得新增 DROP COLUMN 清理历史审计列 %s",
				entry.Name(),
				column,
			)
		}
	}
}
