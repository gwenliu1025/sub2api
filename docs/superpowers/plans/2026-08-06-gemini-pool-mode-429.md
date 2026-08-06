# Gemini 池模式 429 本地限流修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让已启用池模式的 Gemini 账号在上游返回 429 后不被 Sub2API 写入本地账号级限流。

**Architecture:** 在 Gemini 兼容层现有错误处理入口复用 `Account.IsPoolMode()` 与 `Account.IsCustomErrorCodesEnabled()`。默认池模式提前返回；显式自定义错误码策略和普通账号继续走现有逻辑。

**Tech Stack:** Go、Sub2API service 层、Docker Compose、PostgreSQL。

---

### Task 1: 增加 Gemini 429 池模式回归测试

**Files:**
- Modify: `backend/internal/service/gemini_messages_compat_service_test.go`

- [ ] **Step 1: 写入失败测试**

新增记录 `SetRateLimited` 调用次数的仓库桩，并覆盖池模式与普通模式。

- [ ] **Step 2: 确认 RED**

Run: `go test ./internal/service -run TestGeminiMessagesCompat_HandleUpstream429RespectsPoolMode -count=1`

Expected: FAIL，池模式分支实际调用次数为 1，期望为 0。

### Task 2: 实现最小修复

**Files:**
- Modify: `backend/internal/service/gemini_messages_compat_service.go`

- [ ] **Step 1: 增加池模式守卫**

在 `handleGeminiUpstreamError` 的 `429` 分支中，若 `account.IsPoolMode() && !account.IsCustomErrorCodesEnabled()`，记录不含响应正文的跳过日志并返回。

- [ ] **Step 2: 确认 GREEN**

Run: `go test ./internal/service -run 'TestGeminiMessagesCompat_HandleUpstream429RespectsPoolMode|TestParseGeminiRateLimitResetTime' -count=1`

Expected: PASS。

- [ ] **Step 3: 完整验证**

Run: `go test ./internal/service -count=1`

Run: `go build ./cmd/server`

Expected: 两条命令均退出 0。

### Task 3: 生产备份、部署和状态清理

**Files:**
- Modify: production `/home/ubuntu/sub2api/.env`
- Preserve: production `/home/ubuntu/sub2api/docker-compose.yml`

- [ ] **Step 1: 记录基线并备份**

记录容器镜像、健康状态和启动时间；备份 Compose、环境文件和当前镜像引用，不输出凭据。

- [ ] **Step 2: 构建并切换精确镜像**

从 fork `v0.1.169` 基线及本补丁构建 Linux amd64 完整根 Dockerfile 镜像，只重建 `sub2api`。

- [ ] **Step 3: 清除账号 3250 的历史本地限流**

在事务中清空账号级限流和临时不可调度字段，确保 `status=active`、`schedulable=true`，并写入调度器 outbox 事件。

- [ ] **Step 4: 验证**

确认 `sub2api` healthy、本机和公网健康检查为 200、账号仍在调度快照中，且无关容器启动时间不变。
