# Equivalent Cache V2 流式最终用量修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 让 Kiro-Go 流式请求使用最终 `message_delta.usage` 执行现有 V2 分配，并发布可回退的 `0.1.152`。

**架构：** `message_start` 继续实时透传，不再提前消费 V2 状态；最终
`message_delta` 同时包含完整输入和输出时，调用现有 JSON 分配器并改写该事件。
普通流式与 Anthropic API Key 透传流式采用同一触发规则。

**技术栈：** Go 1.26.5、Gin、gjson/sjson、PostgreSQL、Redis、Docker、
GitHub Actions、GHCR

---

### 任务 1：增加真实两阶段 usage 回归测试

**文件：**
- 修改：`backend/internal/service/equivalent_cache_v2_response_test.go`

- [ ] **步骤 1：修改 API Key 透传流式测试**

把流改为：

```go
`data: {"type":"message_start","message":{"usage":{"input_tokens":4,"output_tokens":0}}}`,
`data: {"type":"message_delta","usage":{"input_tokens":4168,"output_tokens":8000}}`,
```

断言 `message_start` 保持输入 `4`，最终 `message_delta.usage` 包含 V2
拆分结果，`rawUsage` 为 `4168/8000`。

- [ ] **步骤 2：修改普通流式测试**

使用同样的两阶段 usage，并保留模型名恢复、事件 ID 和停止原因断言。

- [ ] **步骤 3：运行测试确认失败**

运行：

```bash
go test ./internal/service -run 'TestEquivalentCacheV2Response_(PassthroughStreaming|NormalStreaming)' -count=1
```

预期：两条最终 usage 测试失败，旧代码仍在 `message_start` 提前尝试。

### 任务 2：把 V2 触发点移动到最终 `message_delta`

**文件：**
- 修改：`backend/internal/service/gateway_upstream_response.go`
- 修改：`backend/internal/service/gateway_anthropic_passthrough.go`

- [ ] **步骤 1：修改普通流式处理**

`message_start` 只参与原始 usage 解析。首次遇到 `message_delta` 时，以
`usage` 为路径调用：

```go
allocation := applyEquivalentCacheV2JSON(
    ctx,
    []byte(rawDataLine),
    "usage",
    *responsePlan,
)
```

成功后更新事件正文、`rawUsage`、`usage`、算法版本和分配类型。

- [ ] **步骤 2：修改 API Key 透传流式处理**

采用相同的 `message_delta.usage` 触发规则，并同步更新 `rawUsage` 与
`responseUsage`。

- [ ] **步骤 3：运行聚焦测试**

```bash
go test ./internal/service -run '^TestEquivalentCacheV2Response_' -count=1
```

预期：全部通过。

### 任务 3：运行后端回归与发布策略检查

**文件：**
- 修改：`backend/cmd/server/VERSION`
- 修改：`deploy/.env.example`
- 修改：`README.md`
- 修改：`README_CN.md`

- [ ] **步骤 1：把发布版本更新为 `0.1.152`**

版本文件和示例镜像必须使用精确版本 `0.1.152`。

- [ ] **步骤 2：更新中英文 README 的发布组成**

说明 `0.1.152` 完整合并上游 `0.1.152`，并增加 Kiro-Go 流式最终用量
V2 修复；中文 README 使用简体中文描述。

- [ ] **步骤 3：运行后端完整测试**

```bash
go test ./...
```

- [ ] **步骤 4：运行发布策略检查**

```bash
bash .github/scripts/test-validate-release-version.sh
bash .github/scripts/test-release-policy.sh
```

- [ ] **步骤 5：运行差异检查**

```bash
git diff --check
git status --short
```

### 任务 4：提交、发布和生产验证

**文件：**
- 提交本计划涉及的全部文件

- [ ] **步骤 1：提交并推送功能分支**

```bash
git add backend/internal/service/gateway_upstream_response.go \
  backend/internal/service/gateway_anthropic_passthrough.go \
  backend/internal/service/equivalent_cache_v2_response_test.go \
  backend/cmd/server/VERSION deploy/.env.example README.md README_CN.md \
  docs/superpowers/specs/2026-07-12-kiro-go-cost-locked-equivalent-cache-v2-design.md \
  docs/superpowers/specs/2026-07-13-equivalent-cache-v2-streaming-final-usage-fix-design.md \
  docs/superpowers/plans/2026-07-13-equivalent-cache-v2-streaming-final-usage-fix.md
git commit -m "修复：流式请求使用最终用量执行缓存拆分"
git push origin feat/kiro-equivalent-cache-v2
```

- [ ] **步骤 2：创建并推送 `v0.1.152` 注释标签**

标签说明使用简体中文，明确包含上游 `0.1.152` 与本次流式 V2 修复。

- [ ] **步骤 3：等待 GitHub Actions 发布完成**

确认 GitHub Release 资产和
`ghcr.io/gwenliu1025/sub2api:0.1.152` 多架构镜像均已发布。

- [ ] **步骤 4：备份生产部署配置**

备份 `.env`、`docker-compose.yml`、当前镜像和无关容器启动时间。

- [ ] **步骤 5：只更新生产 `sub2api`**

通过宿主机更新代理准备 `0.1.152`，然后只重建 `sub2api`。

- [ ] **步骤 6：验证真实生产流式数据**

确认：

```text
cc-kiro + claude-opus-4-6 + stream=true
usage_allocation_version = 2
raw_input_tokens = Kiro-Go 最终输入
raw_output_tokens = 显示输出
输入侧整数成本差值 = 0
```

同时确认同步请求仍正常、健康检查为 `200`，PostgreSQL、Redis、Caddy、
Kiro-Go 等无关容器启动时间未改变。
