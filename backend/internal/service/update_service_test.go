//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

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
	release *GitHubRelease
	repo    string
	calls   int
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.repo = repo
	s.calls++
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called by update check tests")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called by update check tests")
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
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func TestUpdateServiceCheckUpdateUsesConfiguredRepo(t *testing.T) {
	cache := &updateServiceCacheStub{}
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.146",
			Name:    "v0.1.146",
		},
	}
	svc := NewUpdateService(cache, client, "gwenliu1025/sub2api", "0.1.145", "release")

	info, err := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.Equal(t, "gwenliu1025/sub2api", client.repo)
	require.Equal(t, 1, client.calls)
	require.True(t, info.HasUpdate)
	require.Equal(t, "0.1.146", info.LatestVersion)
}

func TestUpdateServiceBlankRepoFallsBackToDefault(t *testing.T) {
	client := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.146",
			Name:    "v0.1.146",
		},
	}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "  ", "0.1.146", "release")

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
	svc := NewUpdateService(cache, client, "gwenliu1025/sub2api", "0.1.145", "release")

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Equal(t, "0.1.146", info.LatestVersion)
	require.Equal(t, "gwenliu1025/sub2api", client.repo)
}
