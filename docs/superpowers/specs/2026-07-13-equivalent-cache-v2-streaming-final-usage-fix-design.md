# Equivalent Cache V2 流式最终用量修复设计

日期：2026-07-13

状态：已确认实施

## 1. 问题

生产 Kiro-Go 流式响应的用量分两阶段返回：

```text
message_start.message.usage.input_tokens = 初始估算
message_delta.usage.input_tokens = 最终完整输入
message_delta.usage.output_tokens = 最终完整输出
```

生产受控请求实测，同一请求在 `message_start` 中的输入为 `4`，在最终
`message_delta` 中的输入为 `4168`。

当前 Sub2API 在 `message_start` 到达时立即执行 V2。初始估算无法满足整数
成本守恒时，当前请求会永久退回原始用量；最终完整用量到达后不会再次执行
V2。因此同步请求能够拆分，而流式请求稳定不拆分。

## 2. 修复目标

- 不修改同步 V2 算法、价格、分配比例、会话状态或计费规则。
- 流式请求使用最终完整用量执行与同步请求相同的 V2 分配。
- 不缓冲正文，不延迟内容块，不改变 SSE 事件顺序。
- 原始审计用量必须保存 Kiro-Go 最终返回值。
- 输出 token 必须保持不变，输入侧费用必须精确守恒。

## 3. 数据流

### 3.1 `message_start`

- 解析并保留初始用量，供普通流式兼容逻辑使用。
- 原样写给下游。
- 不调用 V2 状态存储，不执行分配，不把本次请求标记为已经完成分配。

### 3.2 最终 `message_delta`

当 `message_delta.usage` 同时存在整数型 `input_tokens` 和
`output_tokens` 时：

1. 冻结该对象为最终原始用量。
2. 调用现有 `applyEquivalentCacheV2JSON`，用量路径为 `usage`。
3. 成功时把改写后的 `usage` 写回该 SSE 事件。
4. 将原始用量、对外用量、算法版本和分配类型写入 `streamingResult`。
5. 后续计费与持久化继续使用现有 V2 锁价链路。

### 3.3 退回行为

出现以下任一情况时保持原始流式行为：

- 最终 `message_delta.usage` 不完整或类型无效；
- 上游已经返回非零真实缓存用量；
- V2 计划不存在或失效；
- 整数成本守恒求解失败。

退回时 `usage_allocation_version=0`，不写入 V2 审计快照。

## 4. 修改范围

- `backend/internal/service/gateway_upstream_response.go`
  - 修改普通 Anthropic 流式响应的 V2 触发事件。
- `backend/internal/service/gateway_anthropic_passthrough.go`
  - 修改 Anthropic API Key 透传流式响应的 V2 触发事件。
- `backend/internal/service/equivalent_cache_v2_response_test.go`
  - 增加并更新两条真实 Kiro-Go 两阶段 usage 回归测试。
- 原 V2 总设计文档
  - 修正流式响应契约。

不新增数据库迁移，不修改账号配置，不修改 Redis 状态结构。

## 5. 验证

自动化验证必须覆盖：

- `message_start` 输入很小、最终 `message_delta` 输入完整时成功执行 V2；
- 普通流式和 API Key 透传流式都成功；
- 原始用量等于最终 `message_delta`；
- 输出 token 前后完全一致；
- 改写后的输入侧费用与原始输入侧费用精确相等；
- 缺少最终完整 usage 时退回原始用量；
- 现有非流式、计费和审计测试继续通过。

生产验证使用 `0.1.152` 精确镜像，只重建 `sub2api`。验证真实
`cc-kiro` 流式记录出现 `usage_allocation_version=2` 后，再检查同步请求、
用户扣费、账号成本和无关容器启动时间。

## 6. 回退

- 首选：关闭账号 `1910` 的 V2 开关。
- 版本回退：把 `SUB2API_IMAGE` 切回
  `ghcr.io/gwenliu1025/sub2api:0.1.151`，只重建 `sub2api`。
- `0.1.151` 的 GitHub Release、二进制资产和 GHCR 镜像不覆盖、不删除。
