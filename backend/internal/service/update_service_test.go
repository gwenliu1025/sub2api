//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
	downloadErr    error
	checksumErr    error
	repo           string
	calls          int
	latestCalls    int
	recentCalls    int
	downloadCalls  int
	checksumCalls  int
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.repo = repo
	s.calls++
	s.latestCalls++
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(_ context.Context, repo string, _ int) ([]*GitHubRelease, error) {
	s.repo = repo
	s.calls++
	s.recentCalls++
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	s.downloadCalls++
	return s.downloadErr
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	s.checksumCalls++
	return nil, s.checksumErr
}

type recordingUpdateAgentClient struct {
	prepareStatus  *UpdateAgentStatus
	prepareErr     error
	activateStatus *UpdateAgentStatus
	activateErr    error
	status         *UpdateAgentStatus
	statusErr      error
	prepared       []string
	activateCalls  int
	statusCalls    int
}

func (c *recordingUpdateAgentClient) Prepare(_ context.Context, version string) (*UpdateAgentStatus, error) {
	c.prepared = append(c.prepared, version)
	return c.prepareStatus, c.prepareErr
}

func (c *recordingUpdateAgentClient) Activate(context.Context) (*UpdateAgentStatus, error) {
	c.activateCalls++
	return c.activateStatus, c.activateErr
}

func (c *recordingUpdateAgentClient) Status(context.Context) (*UpdateAgentStatus, error) {
	c.statusCalls++
	return c.status, c.statusErr
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		defaultGitHubRepo,
		"0.1.132",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		defaultGitHubRepo,
		current,
		"release",
		config.UpdateModeBinary,
		nil,
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsUsesConfiguredRepo(t *testing.T) {
	client := &updateServiceGitHubClientStub{
		recentReleases: []*GitHubRelease{{TagName: "v0.1.146"}},
	}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		client,
		"gwenliu1025/sub2api",
		"0.1.147",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Equal(t, "gwenliu1025/sub2api", client.repo)
	require.Equal(t, 1, client.calls)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		defaultGitHubRepo,
		"0.1.147",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}

func TestUpdateServiceCheckUpdateUsesConfiguredRepo(t *testing.T) {
	cache := &updateServiceCacheStub{}
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.146",
			Name:    "v0.1.146",
		},
	}
	svc := NewUpdateService(
		cache,
		client,
		"gwenliu1025/sub2api",
		"0.1.145",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	info, err := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.Equal(t, "gwenliu1025/sub2api", client.repo)
	require.Equal(t, 1, client.calls)
	require.True(t, info.HasUpdate)
	require.Equal(t, "0.1.146", info.LatestVersion)
	require.Equal(t, config.UpdateModeBinary, info.UpdateMode)
}

func TestUpdateServiceBlankRepoFallsBackToDefault(t *testing.T) {
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.146",
			Name:    "v0.1.146",
		},
	}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		client,
		"  ",
		"0.1.146",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	_, err := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.Equal(t, defaultGitHubRepo, client.repo)
}

func TestUpdateServiceIgnoresCachedUpdateFromDifferentRepo(t *testing.T) {
	cache := &updateServiceCacheStub{
		data: `{"latest":"9.9.9","repo":"Wei-Shaw/sub2api","timestamp":32503680000}`,
	}
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.146",
			Name:    "v0.1.146",
		},
	}
	svc := NewUpdateService(
		cache,
		client,
		"gwenliu1025/sub2api",
		"0.1.145",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Equal(t, "0.1.146", info.LatestVersion)
	require.Equal(t, "gwenliu1025/sub2api", client.repo)
}

func TestUpdateServiceDockerAgentPreparesLatestVersion(t *testing.T) {
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.150",
			Assets: []GitHubAsset{
				{
					Name:               fmt.Sprintf("sub2api_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
					BrowserDownloadURL: "https://github.com/gwenliu1025/sub2api/releases/download/v0.1.150/sub2api.tar.gz",
				},
				{
					Name:               "checksums.txt",
					BrowserDownloadURL: "https://github.com/gwenliu1025/sub2api/releases/download/v0.1.150/checksums.txt",
				},
			},
		},
	}
	agent := &recordingUpdateAgentClient{
		prepareStatus: &UpdateAgentStatus{State: UpdateAgentPrepared},
	}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		client,
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeDockerAgent,
		agent,
	)

	err := svc.PerformUpdate(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"0.1.150"}, agent.prepared)
	require.Zero(t, client.downloadCalls)
	require.Zero(t, client.checksumCalls)
}

func TestUpdateServiceDockerAgentRollbackPreparesAllowedVersion(t *testing.T) {
	client := &updateServiceGitHubClientStub{
		recentReleases: []*GitHubRelease{
			{TagName: "v0.1.147"},
			{
				TagName: "v0.1.146",
				Assets: []GitHubAsset{
					{
						Name:               fmt.Sprintf("sub2api_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
						BrowserDownloadURL: "https://github.com/gwenliu1025/sub2api/releases/download/v0.1.146/sub2api.tar.gz",
					},
				},
			},
		},
	}
	agent := &recordingUpdateAgentClient{
		prepareStatus: &UpdateAgentStatus{State: UpdateAgentPrepared},
	}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		client,
		defaultGitHubRepo,
		"0.1.147",
		"release",
		config.UpdateModeDockerAgent,
		agent,
	)

	err := svc.RollbackToVersion(context.Background(), "0.1.146")

	require.NoError(t, err)
	require.Equal(t, 1, client.recentCalls)
	require.Equal(t, []string{"0.1.146"}, agent.prepared)
	require.Zero(t, client.downloadCalls)
	require.Zero(t, client.checksumCalls)
}

func TestUpdateServiceDockerAgentRejectsLegacyBackupRollback(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeDockerAgent,
		&recordingUpdateAgentClient{},
	)

	err := svc.Rollback()

	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, int32(400), appErr.Code)
	require.Equal(t, "LEGACY_ROLLBACK_UNAVAILABLE", appErr.Reason)
	require.Equal(t, "local binary rollback is unavailable in Docker update mode", appErr.Message)
}

func TestUpdateServiceDockerAgentActivateDelegatesToAgent(t *testing.T) {
	want := &UpdateAgentStatus{
		State:       UpdateAgentActivating,
		TargetImage: "ghcr.io/gwenliu1025/sub2api:0.1.150",
	}
	agent := &recordingUpdateAgentClient{activateStatus: want}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeDockerAgent,
		agent,
	)

	got, err := svc.ActivatePreparedUpdate(context.Background())

	require.NoError(t, err)
	require.Same(t, want, got)
	require.Equal(t, 1, agent.activateCalls)
}

func TestUpdateServiceDockerAgentStatusDelegatesToAgent(t *testing.T) {
	want := &UpdateAgentStatus{
		State:       UpdateAgentPrepared,
		TargetImage: "ghcr.io/gwenliu1025/sub2api:0.1.150",
	}
	agent := &recordingUpdateAgentClient{status: want}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeDockerAgent,
		agent,
	)

	got, err := svc.GetUpdateStatus(context.Background())

	require.NoError(t, err)
	require.Same(t, want, got)
	require.Equal(t, 1, agent.statusCalls)
}

func TestUpdateServiceBinaryModeStillAppliesReleaseAssets(t *testing.T) {
	downloadErr := errors.New("download attempted")
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.150",
			Assets: []GitHubAsset{
				{
					Name:               fmt.Sprintf("sub2api_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
					BrowserDownloadURL: "https://github.com/gwenliu1025/sub2api/releases/download/v0.1.150/sub2api.tar.gz",
				},
			},
		},
		downloadErr: downloadErr,
	}
	agent := &recordingUpdateAgentClient{}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		client,
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeBinary,
		agent,
	)

	err := svc.PerformUpdate(context.Background())

	require.ErrorIs(t, err, downloadErr)
	require.Equal(t, 1, client.downloadCalls)
	require.Empty(t, agent.prepared)
}

func TestUpdateServiceDockerAgentCachedInfoIncludesMode(t *testing.T) {
	cache := &updateServiceCacheStub{
		data: `{"latest":"0.1.150","repo":"gwenliu1025/sub2api","timestamp":32503680000}`,
	}
	client := &updateServiceGitHubClientStub{}
	svc := NewUpdateService(
		cache,
		client,
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeDockerAgent,
		&recordingUpdateAgentClient{},
	)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.True(t, info.Cached)
	require.Equal(t, config.UpdateModeDockerAgent, info.UpdateMode)
	require.Zero(t, client.calls)
}

func TestUpdateServiceDockerAgentNilClientReturnsApplicationErrors(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeDockerAgent,
		nil,
	)

	for _, call := range []func() error{
		func() error {
			_, err := svc.ActivatePreparedUpdate(context.Background())
			return err
		},
		func() error {
			_, err := svc.GetUpdateStatus(context.Background())
			return err
		},
	} {
		err := call()
		var appErr *infraerrors.ApplicationError
		require.ErrorAs(t, err, &appErr)
		require.Equal(t, int32(503), appErr.Code)
		require.Equal(t, "UPDATE_AGENT_UNAVAILABLE", appErr.Reason)
	}
}

func TestUpdateServiceBinaryModeAgentOperationsReturnApplicationErrors(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		defaultGitHubRepo,
		"0.1.149",
		"release",
		config.UpdateModeBinary,
		nil,
	)

	for _, call := range []func() error{
		func() error {
			_, err := svc.ActivatePreparedUpdate(context.Background())
			return err
		},
		func() error {
			_, err := svc.GetUpdateStatus(context.Background())
			return err
		},
	} {
		err := call()
		var appErr *infraerrors.ApplicationError
		require.ErrorAs(t, err, &appErr)
		require.Equal(t, int32(400), appErr.Code)
		require.Equal(t, "UPDATE_AGENT_UNAVAILABLE_IN_BINARY_MODE", appErr.Reason)
	}
}

func TestProvideUpdateServiceCreatesAgentOnlyForDockerMode(t *testing.T) {
	buildInfo := BuildInfo{Version: "0.1.149", BuildType: "release"}

	binaryService := ProvideUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		&config.Config{
			Update: config.UpdateConfig{
				Mode:                config.UpdateModeBinary,
				AgentSocket:         "/run/ignored.sock",
				AgentTimeoutSeconds: 7,
				ImageRepository:     "ghcr.io/gwenliu1025/ignored",
			},
		},
		buildInfo,
	)
	require.False(t, binaryService.UsesDockerAgent())
	require.Nil(t, binaryService.agentClient)

	dockerService := ProvideUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		&config.Config{
			Update: config.UpdateConfig{
				Mode:                config.UpdateModeDockerAgent,
				AgentSocket:         "/run/sub2api-updater/updater.sock",
				AgentTimeoutSeconds: 7,
				ImageRepository:     "ghcr.io/gwenliu1025/sub2api",
			},
		},
		buildInfo,
	)
	require.True(t, dockerService.UsesDockerAgent())
	agent, ok := dockerService.agentClient.(*UnixUpdateAgentClient)
	require.True(t, ok)
	require.Equal(t, 7*time.Second, agent.httpClient.Timeout)
	require.Equal(t, "ghcr.io/gwenliu1025/sub2api", agent.expectedRepository)
}
