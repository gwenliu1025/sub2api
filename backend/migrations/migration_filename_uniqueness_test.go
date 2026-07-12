package migrations

import (
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
