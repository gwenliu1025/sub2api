# 等效缓存计费与展示口径

本文记录自建 Kiro 号池的“等效缓存计费/展示”运维口径。该能力只重分配平台侧 usage log 和结算口径，不修改真实上游 Kiro 缓存行为。

## 适用边界

- 只在新机、预热机、毕业机环境构建、部署、验证。
- 生产入口机器只允许只读排查，不改配置、不停服、不重启。
- 只给自建 Kiro 账号打开开关；接别人上游的 Kiro 不处理。
- `cloudflare-temp-email` 不进中转网络。
- 第一版只建议用于 token 计费的 Kiro 文本请求；图片、按次计费、渠道区间定价路由启用前要单独验证费用和展示口径。

## 计算口径

1. 先按当前 billing model 和价格表计算原始 prompt 成本。
2. 对 token 量应用 `equivalent_cache_billing_loss_factor`，默认 `1.08`。
3. 将 prompt 成本按默认成本比例重分配：
   - `20%` input
   - `75%` cache read
   - `5%` cache creation
4. 因为 cache read 单价低，前端按 token 计算的可见缓存率会显著高于 `75%`。在当前 Claude Sonnet 价格口径下，默认 `20/75/5` 会显示在 `96%+` 附近，常见约 `96.3%`。
5. 输出 token 只应用 loss factor，不参与 prompt 的 `20/75/5` 重分配。

## 账号 Extra 开关

| Key | 默认值 | 说明 |
| --- | ---: | --- |
| `equivalent_cache_billing_enabled` | `false` | 主开关，只有 `true` 才启用 |
| `equivalent_cache_billing_loss_factor` | `1.08` | token 放大系数 |
| `equivalent_cache_billing_input_share` | `0.20` | prompt 成本中 input 的目标占比 |
| `equivalent_cache_billing_cache_read_share` | `0.75` | prompt 成本中 cache read 的目标占比 |
| `equivalent_cache_billing_cache_creation_share` | `0.05` | prompt 成本中 cache creation 的目标占比 |

兼容旧 key：`kiro_equivalent_cache_billing_enabled=true` 也会启用，但新配置统一使用 `equivalent_cache_billing_enabled`。

## 开启或关闭

推荐用 `accounts bulk-update`，因为它对 `extra` 做 key 级合并，不会覆盖账号上已有的运行态 extra。单账号 `accounts update` 会全量替换 `extra`，除非先完整导出并手动合并，否则不要用它只改一个 extra key。

从 skill 目录运行：

```bash
node scripts/sub2api-admin.js accounts list --search '<kiro-account-name>' --page-size 20
node scripts/sub2api-admin.js accounts get <account-id>
```

开启：

```bash
node scripts/sub2api-admin.js accounts bulk-update --ids <account-id> --json '{
  "extra": {
    "equivalent_cache_billing_enabled": true,
    "equivalent_cache_billing_loss_factor": 1.08,
    "equivalent_cache_billing_input_share": 0.20,
    "equivalent_cache_billing_cache_read_share": 0.75,
    "equivalent_cache_billing_cache_creation_share": 0.05
  }
}'
```

关闭：

```bash
node scripts/sub2api-admin.js accounts bulk-update --ids <account-id> --json '{
  "extra": {
    "equivalent_cache_billing_enabled": false
  }
}'
```

批量开启时，把 `<account-id>` 换成逗号分隔 ID，例如 `101,102,103`。执行前必须先用 `accounts get` 核对目标账号确实属于自建 Kiro 号池。

## 验证

只在新机或预热环境验证。发送 1-2 次 Kiro 请求后检查 usage log：

```sql
SELECT
  id,
  request_id,
  account_id,
  model,
  input_tokens,
  cache_read_tokens,
  cache_creation_tokens,
  output_tokens,
  ROUND(
    cache_read_tokens::numeric /
    NULLIF(input_tokens + cache_read_tokens + cache_creation_tokens, 0),
    4
  ) AS visible_cache_rate,
  total_cost,
  actual_cost,
  created_at
FROM usage_logs
WHERE account_id = <account-id>
ORDER BY created_at DESC
LIMIT 5;
```

预期现象：

- `input_tokens` 低于原始输入 token。
- `cache_read_tokens` 和 `cache_creation_tokens` 大于 0。
- `visible_cache_rate` 通常在 `0.96` 附近，不要求每条都硬性大于 `0.95`。
- 后端日志可见 `equivalent_cache_billing applied`。
- 前端缓存展示接近 `95%+`，费用不为 0，usage log 和后扣命令使用同一组改写后的 token。

## 发布与更新

- 本地分支和 GitHub fork 保存私有改动。
- 上游正式发版后，可以使用同一个版本号在自己的 fork 构建镜像和 release asset。
- 部署环境的在线更新仓库应指向自己的 fork，例如 `UPDATE_REPO=gwenliu1025/sub2api` 或配置文件 `update.repo: "gwenliu1025/sub2api"`。
- 新镜像只在新机/预热机 compose 中重启验证；生产入口机器保持只读，最终 DNS 切换另行计划。

## 回滚

优先关闭账号 extra，不需要重启服务：

```bash
node scripts/sub2api-admin.js accounts bulk-update --ids <account-id> --json '{"extra":{"equivalent_cache_billing_enabled":false}}'
```

如果是新机镜像问题，再把新机 compose 回退到上一镜像。生产入口机器不参与本功能首轮回滚。
