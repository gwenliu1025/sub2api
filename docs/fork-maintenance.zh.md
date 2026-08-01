# Fork 维护与临时生产修复台账

最后更新：2026-08-02（Asia/Shanghai）

## 当前 Anthropic Accept-Encoding 修复

- 生产基线：`f0c79a2ce5664ba5e4252e8184a400b59b2c574c`，正式版本仍为 `v0.1.169`。
- 修复分支：`fix/anthropic-accept-encoding`，在后续官方版本整合完成前必须保留。
- 修复提交：`146789daf37f98e24a343efd12f5cad10bd3c4ce`。
- 生产镜像：`ghcr.io/gwenliu1025/sub2api:0.1.169-anthropic-accept-encoding-146789daf-r2`。
- 镜像 ID：`sha256:01bbd5285b6e16e591d6f94a432e9743fc6296e6fdfe9462e9a2586ab92a7c58`。
- 生产切换时间：`2026-08-02 03:21:54 +08`。
- 生产备份：`/home/ubuntu/sub2api/backups/pre-anthropic-accept-encoding-r2-20260801T192143Z`。

该镜像使用自定义后缀，仅作为当前 `0.1.169` 生产热修复，不是正式 Release；本次没有修改
`backend/cmd/server/VERSION`，也没有创建或移动 Git tag、GitHub Release、正式 GHCR 资产或
`checksums.txt`。

首次镜像错误使用 `backend/Dockerfile`，未构建前端且未带 `-tags embed`，部署后公网根页面返回
`404 page not found`。该镜像
`ghcr.io/gwenliu1025/sub2api:0.1.169-anthropic-accept-encoding-146789daf` 已回滚并禁止复用。
`-r2` 使用仓库根多阶段 `Dockerfile` 构建，内嵌前端、正式入口、资源目录和公网根页面
`200 + HTML` 均已验证。

## 根因与修复边界

Anthropic 请求构造层原本允许客户端 `Accept-Encoding` 进入上游白名单。客户端原始小写头与
Go `http.Transport` 自动添加的 `Accept-Encoding: gzip` 会在真实请求中同时出现，部分上游因而
返回无法被现有响应链正确处理的压缩 SSE，最终表现为二进制乱码。

修复只从 Anthropic 公共请求头白名单删除 `accept-encoding`，由 Go Transport 统一协商并解压
`gzip`。本次未修改自动透传开关或逻辑、账号、模型映射、计费、数据库、Caddy 或响应解压器。

## 验证记录

- 回归测试先在旧白名单上复现四条请求构造路径保留客户端压缩头，并在真实 Transport 中观察到
  `gzip, deflate, br, zstd` 与 `gzip` 两个协商值；删除白名单项后全部转绿。
- `go test ./internal/service -run 'AnthropicAPIKey|AnthropicRequests_DropClientAcceptEncoding|AnthropicAcceptEncoding_Transport' -count=1` 通过。
- `go test ./internal/repository -run 'DecompressResponseBody' -count=1 -v` 通过。
- `go vet ./...` 通过。
- 全量测试仍存在生产基线已有的
  `TestContentModerationRuntimeSnapshotRefreshFailureKeepsStaleConfig` 时序失败，与本修复无关；未修改该测试。
- 生产容器为 `running/healthy`；公网根页面为 `200` 且正文是 HTML；应用直连、Caddy、公网和
  更新代理健康检查均为 `200`；未认证 `/v1/models` 为预期的 `401`；其他容器启动时间未变化。
- 禾维、cctest、ztest 的真实评分复测需要用户侧低额度临时 Key，当前未伪记为已完成。

## 回滚

恢复备份中的 `.env`，确认 `SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.169`，然后只重建
`sub2api`：

```bash
cp -a \
  /home/ubuntu/sub2api/backups/pre-anthropic-accept-encoding-r2-20260801T192143Z/.env \
  /home/ubuntu/sub2api/.env
docker compose \
  --project-directory /home/ubuntu/sub2api \
  -f /home/ubuntu/sub2api/docker-compose.yml \
  --env-file /home/ubuntu/sub2api/.env \
  up -d --no-deps --force-recreate --pull never sub2api
```

回滚后重新核对容器镜像、`running/healthy`、应用 `/health`、Caddy `/health`、公网 `/health`
和无关容器启动时间。

## 后续官方版本整合规则

官方发布下一版后，从保留的 `fix/anthropic-accept-encoding` fork 分支创建整合分支，再把官方
下一版代码合并进来。解决冲突时必须保留 `Accept-Encoding` 不进入 Anthropic 上游白名单的不变量，
并重新运行本文件中的回归测试。完整验证后，才在 fork 仓库修改版本号、构建正式资产并发布
`v0.1.170`。

## 分支台账

| 分支 | 状态 | 处理规则 |
| --- | --- | --- |
| `fix/anthropic-accept-encoding` | 当前生产热修复 | 必须保留，后续官方整合完成前不得删除 |
| `feat/configurable-update-repo` | 已并入生产 | 待单独确认退役，本次不删除 |
| `fix/dashboard-csp-grok-sync` | 已并入生产 | 待单独确认退役，本次不删除 |
| `fix/v0.1.169-release-gates` | 已并入生产 | 待单独确认退役，本次不删除 |
| `release/v0.1.169-fork` | 已并入生产 | 待单独确认退役，本次不删除 |
| `backup/kiro-equivalent-cache-v2-pre-upstream-0.1.151` | 含未合并提交 | 禁止删除 |
| `equivalent-cache-billing-new-machine` | 含未合并提交 | 禁止删除 |
| `feat/kiro-equivalent-cache-v2` | 含未合并提交 | 禁止删除 |
| `feat/remove-equivalent-cache` | 含未合并提交 | 禁止删除 |
| `feat/xai-imagine-media` | 当前工作区活跃分支 | 禁止触碰 |
| `gwen-main-v0.1.149-custom` | 含历史差异 | 删除前必须单独审计 |
| `main` | 默认分支 | 禁止删除 |
| `release/v0.1.165-clean` | 历史发布分支 | 删除前必须单独审计 |
