# Equivalent Cache 清退验证结果

日期：2026-07-14

## 验证范围

- 分支：`feat/remove-equivalent-cache`
- 清退验证基线提交：`08907ade`
- 独立审查定价修正提交：`4cde30c3`
- 发布版本文件：`backend/cmd/server/VERSION = 0.1.152`
- 目标：确认 Equivalent Cache V1/V2 运行时已移除，同时保留标准 Anthropic
  usage 解析、分模型定价、有效倍率、原生 TTL override 和历史迁移兼容。

## 环境

```text
go version go1.26.5 windows/amd64
GOOS=windows
GOARCH=amd64
CGO_ENABLED=0
node=v24.18.0
pnpm=10.25.0
golangci-lint=v2.9.0
```

本机没有 `make`。验证按根 `Makefile` 与 `backend/Makefile` 展开为等价命令。

## 后端验证

执行：

```powershell
Push-Location backend
go test ./...
go test -tags=unit ./...
go build ./cmd/server
$env:CGO_ENABLED = '0'
go build -ldflags='-s -w -X main.Version=0.1.152' -trimpath -o bin/server ./cmd/server
$env:GOPROXY = 'https://goproxy.cn,direct'
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0 run ./...
Pop-Location
```

结果：

- `go test ./...`：退出码 `0`；
- `go test -tags=unit ./...`：退出码 `0`；
- `go build ./cmd/server`：退出码 `0`；
- `0.1.152` 精确版本、`CGO_ENABLED=0`、`-trimpath` 构建：退出码 `0`；
- `golangci-lint v2.9.0`：退出码 `0`，`0 issues`。

首次为节省时间而并行启动普通全量与 unit 标签全量时，两套进程均在
`ent/schema` 的 `.entc` 临时目录失败，错误分别表现为临时文件消失和找不到
并发生成包。证据复核：

```powershell
go test ./ent/schema -count=1 -v
```

单独运行退出码为 `0`。随后普通全量与 unit 标签全量严格串行运行，均退出码
`0`。因此该失败归因为 Windows 下两个 Ent 测试进程争用同一临时目录，不是
代码回归。

首次完整 lint 发现 6 个清退后未使用符号：

- DTO 测试中的 `intPtr`、`int16Ptr`；
- usage log repository 中的 `nullInt16`、`nullIntPtr`、`nullInt16Ptr`；
- gateway 中已无调用方的 `parseRawSSEUsage`。

这些符号已在提交 `08907ade` 中删除。修改后重新执行相关普通/unit 包测试、
两套全量测试、两个构建与完整 lint，结果均通过。

## 前端验证

执行：

```powershell
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend run lint:check
pnpm --dir frontend run typecheck
pnpm --dir frontend exec vitest run `
  src/views/auth/__tests__/LinuxDoCallbackView.spec.ts `
  src/views/auth/__tests__/WechatCallbackView.spec.ts `
  src/views/user/__tests__/PaymentView.spec.ts `
  src/views/user/__tests__/PaymentResultView.spec.ts `
  src/components/user/profile/__tests__/ProfileInfoCard.spec.ts `
  src/views/admin/__tests__/SettingsView.spec.ts
pnpm --dir frontend run build
```

结果：

- frozen lockfile 安装：退出码 `0`，锁文件无需更新；
- ESLint：退出码 `0`；
- Vue/TypeScript 类型检查：退出码 `0`；
- 关键 Vitest：`6` 个测试文件、`91` 个测试全部通过；
- 前端生产构建：退出码 `0`。

构建仍报告既有的动态/静态混合导入与大 chunk 警告，不影响退出码；验证过程
结束后生成目录未形成 Git 差异。

## 运行时残留扫描

执行：

```powershell
rg -n "equivalent_cache|EquivalentCache|UsageAllocationVersion|UsageAllocationKind" `
  backend/internal backend/ent -g '!**/*_test.go'
```

结果：无命中。非测试运行时代码中没有 V1/V2 模块、配置解析、响应改写、
Redis 状态、资格或旧审计字段读写。

包含测试文件的扫描只命中清退回归测试：

- `equivalent_cache_cleanup_regression_test.go`
- `gateway_anthropic_apikey_passthrough_test.go`
- `gateway_record_usage_test.go`
- `model_pricing_resolver_test.go`

这些测试用于证明旧账户 Extra 不得改写原生 usage、最终 SSE usage 覆盖、
基础费用后倍率和分模型缓存价格合同，属于允许残留。

README、`docs` 与 `skills` 的同一关键词扫描无命中。旧运维入口已删除，当前
说明改为由上游兼容服务返回标准高缓存 usage。

## 历史迁移

以下文件仍存在：

```text
backend/migrations/174_usage_log_equivalent_cache_v2_audit.sql
```

迁移继续以 `ADD COLUMN IF NOT EXISTS` 保留历史审计列；迁移兼容测试禁止新增
`DROP COLUMN` 清理这些列。当前 Ent schema、repository、DTO 和 service 均不再
读写或返回旧审计字段。

## 结论与剩余验证

- Sub2API 只消费标准 Anthropic usage，并负责标准字段解析、分模型定价、有效
  倍率与扣费；
- `parseSSEUsagePassthrough`、`parseClaudeUsageFromResponseBody`、
  `ForceCacheBilling` 与 `cache_ttl_override` 的原生能力保留；
- Claude 缓存价格缺失保护、动态价格派生和分模型相对价格回归测试包含在已
  通过的 service 全量测试中；
- 发布版本保持 `0.1.152`，没有创建 `0.1.153`；
- Windows 本机未执行 race 测试；`go test -race ./...` 保留到 Linux 毕业机；
- 本地验证结束时，除本验证文档外没有其他未提交差异。

## 独立审查追加修正

最终独立审查发现 Kiro-Go 会对外返回 `claude-haiku-4.5`、
`claude-opus-4.8` 等点号版本，而 Sub2API 定价表使用
`claude-haiku-4-5`、`claude-opus-4-8` 短横线键。旧逻辑在精确匹配失败后会
落入宽泛系列回退，可能把 Haiku 4.5 按 Claude 3 Haiku 计价，Opus 4.8 还会
因 Go map 遍历顺序命中不同的 Opus 4 系列价格。

修正提交 `4cde30c3` 完成以下处理：

- Claude 点号版本、短横线版本、`anthropic/` 前缀和 `-thinking` 变体统一为
  定价表的短横线基础键；
- 系列回退补齐 Opus 4.8 和 Haiku 4.5；
- 去版本后缀与系列模糊查找使用排序键，并优先精确价格键，不再依赖 Go map
  迭代顺序；
- 回归测试连续 100 次验证 Haiku 4.5 与 Opus 4.8 始终命中各自的正确绝对
  输入、输出价格。

TDD 红灯命令：

```powershell
go test ./internal/service `
  -run '^TestPricingService_Claude点号版本精确匹配短横线价格键$' `
  -count=1 -v
```

修正前四种点号/`-thinking` 输入均因未规范化而失败。修正后同一命令通过，
并追加验证短横线输入。

修正后重新执行：

```powershell
go test ./internal/service `
  -run '(Pricing|EquivalentCacheCleanup|ClaudeFlat渠道|ClaudeInterval渠道)' `
  -count=1
go test -tags=unit ./internal/service `
  -run '(Pricing|EquivalentCacheCleanup|ClaudeFlat渠道|ClaudeInterval渠道)' `
  -count=1
go test ./...
go test -tags=unit ./...
go build ./cmd/server
$env:CGO_ENABLED = '0'
go build -ldflags='-s -w -X main.Version=0.1.152' -trimpath `
  -o bin/server ./cmd/server
$env:GOPROXY = 'https://goproxy.cn,direct'
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0 run ./...
```

结果：

- 两组定向 service 测试通过；
- 普通全量与 unit 标签全量均通过；
- 两个构建均退出码 `0`；
- `golangci-lint v2.9.0` 返回 `0 issues`。

## 毕业机定向部署追加验证

毕业机：`157.254.187.242`

生产机 `154.36.172.65` 未访问、未修改。

部署源码：

```text
1c0f22a8c01588f7aa9a35a4aed48ac65bce4098
```

部署镜像：

```text
ghcr.io/gwenliu1025/sub2api:0.1.152
本机镜像 ID=sha256:af52620f4dc4b293436d79e687520791198915f16e8b8826dcdb5a990f1cca9b
version=0.1.152
revision=1c0f22a8c01588f7aa9a35a4aed48ac65bce4098
created=2026-07-14T00:57:07Z
```

该镜像是毕业机本地候选。远端 GHCR 同标签仍是此前正式发布资产
`sha256:8f7f1cb6874da8a1aa28095d3b66b14e80aa89cab34b011605f4d001787e0a0c`；
正式重新发布前不得执行会重新拉取远端同标签的 Prepare、Activate 或 pull。

只重建 `sub2api` 后：

```text
started=2026-07-14T01:20:22.651655106Z
status=running
health=healthy
http://127.0.0.1:8080/health=200
http://127.0.0.1:8080/healthz=200
http://127.0.0.1/health=200
```

毕业机自动下载的上游价格文件包含不符合标准倍率的旧 Claude 3 缓存价格。
候选版本安全拒绝该文件并加载镜像内置 fallback：

```text
fallback_sha256=9b7f21e48fedeb601d98fd77fe3bd36fb41d4f960f20fc5e55c353d5ad01e28b
models=196
checked_claude_cache_models=23
bad_ratio_count=0
fatal_startup_errors=0
```

account `1910` 已在同一数据库事务中：

- 把 `credentials.base_url` 改为 `http://kiro-go-pr131:8321`；
- 删除五个 `equivalent_cache_billing_*` Extra；
- 写入 `cache_ttl_override_enabled=false`；
- 插入 `scheduler_outbox` 的 `account_changed` 事件 `372598`；
- 因 Kiro 上游账户池为空，继续保持 `schedulable=false`。

migration 174 的八个历史列仍全部存在。以部署前完整快照比较，只有
`kiro-go-pr131` 和 `sub2api` 的启动时间与镜像发生变化，无关容器没有新增、
丢失或重建。

毕业机再次执行：

```bash
go test ./internal/service \
  -run '(Pricing|EquivalentCacheCleanup|ClaudeFlat渠道|ClaudeInterval渠道)' \
  -count=1
```

结果为 `ok`。

Sub2API 的 `go test -race ./...` 尚未在 Linux 毕业机执行。本次追加验证不能
替代该证据；它继续作为生产变更前需要补齐或书面接受的验证项。

真实同步和流式请求均已到达 Kiro-Go，但因 `accounts=0` 返回
`503 No available accounts`。因此真实 usage、TTL 分布、换源和跨倍率扣费仍
需在导入明确授权的 Kiro 上游账户后验收。
