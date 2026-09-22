//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 测试选择器使用账号公开名，但原始上游目录仍由共享缓存独立持有。
func TestAccountTestPickerAppliesAccountModelMapping(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mapping     map[string]any
		passthrough bool
		want        []string
	}{
		{name: "exact_and_alias", mapping: map[string]any{"public-model": "upstream-model"}, want: []string{"public-model"}},
		// 通配符只匹配公开名，映射值仍为固定上游调用名。
		{name: "wildcard", mapping: map[string]any{"qwen-*": "qwen-plus"}, want: []string{"qwen-plus"}},
		{name: "passthrough", mapping: map[string]any{"public-model": "upstream-model"}, passthrough: true, want: []string{"upstream-model", "qwen-plus", "excluded-model"}},
		{name: "unrestricted", want: []string{"upstream-model", "qwen-plus", "excluded-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return ordinaryModelsUpstreamResponse(`{"data":[{"id":"upstream-model"},{"id":"qwen-plus"},{"id":"excluded-model"}]}`), nil
			}})
			account := newCodexModelsAPIKeyTestAccount("https://models.example/v1")
			if tc.mapping != nil {
				account.Credentials["model_mapping"] = tc.mapping
			}
			account.Extra = map[string]any{"openai_passthrough": tc.passthrough}
			svc := &AccountTestService{openaiGatewayService: gateway}
			before, err := gateway.FetchOpenAIModelsList(context.Background(), account)
			require.NoError(t, err)
			models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
			require.NoError(t, err)
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
			}
			require.ElementsMatch(t, tc.want, ids)
			after, err := gateway.FetchOpenAIModelsList(context.Background(), account)
			require.NoError(t, err)
			require.Equal(t, before.Body, after.Body)
		})
	}
}

// 直接提交测试也须检查客户端模型名，不能靠隐藏下拉框阻止越出账号白名单。
func TestAccountTestSubmitEnforcesModelAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name        string
		model       string
		passthrough bool
		allowed     bool
		mapping     map[string]any
		wantModel   string
		wantError   string
	}{
		{name: "excluded", model: "excluded-model"},
		{name: "upstream_name_is_not_public_alias", model: "upstream-model"},
		{name: "explicit_default_excluded", model: "gpt-5.4"},
		{name: "implicit_default_uses_allowed_alias", allowed: true},
		{name: "implicit_default_uses_allowed_wildcard_target", mapping: map[string]any{"qwen-*": "qwen-plus"}, allowed: true, wantModel: "qwen-plus"},
		{name: "implicit_default_needs_concrete_public_name", mapping: map[string]any{"alias-*": "upstream-model"}, wantError: "No concrete allowed test model"},
		{name: "allowed_alias", model: "public-model", allowed: true},
		{name: "passthrough_keeps_existing_semantics", model: "excluded-model", passthrough: true, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newTestContext()
			response := newJSONResponse(http.StatusOK, "")
			response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
			upstream := &queuedHTTPUpstream{responses: []*http.Response{response}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://relay.example.com", "model_mapping": map[string]any{"public-model": "upstream-model"}},
				Extra:       map[string]any{"openai_passthrough": tc.passthrough, "openai_responses_supported": true}}
			if tc.mapping != nil {
				account.Credentials["model_mapping"] = tc.mapping
			}
			svc := &AccountTestService{httpUpstream: upstream,
				cfg:         &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
				accountRepo: &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{1: account}}}}
			err := svc.TestAccountConnection(c, account.ID, tc.model, "", "")
			if tc.allowed {
				require.NoError(t, err)
				require.Len(t, upstream.requests, 1)
				if tc.model == "" {
					body, err := io.ReadAll(upstream.requests[0].Body)
					require.NoError(t, err)
					wantModel := tc.wantModel
					if wantModel == "" {
						wantModel = "upstream-model"
					}
					require.Contains(t, string(body), `"model":"`+wantModel+`"`)
				}
			} else {
				require.Error(t, err)
				require.Empty(t, upstream.requests)
				wantError := tc.wantError
				if wantError == "" {
					wantError = "not enabled for this account"
				}
				require.Contains(t, rec.Body.String(), wantError)
			}
		})
	}
}
