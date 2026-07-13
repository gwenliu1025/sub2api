# Kiro-Go 原生 Usage 计费说明

Equivalent Cache V1/V2 已退役。Kiro-Go 负责生成标准 Anthropic usage，
Sub2API 不再重写、重分配或模拟缓存 token。

## 职责边界

- Kiro-Go 输出 `input_tokens`、`output_tokens`、
  `cache_read_input_tokens`、`cache_creation_input_tokens`。
- 上游提供 5m/1h 缓存创建明细时，Kiro-Go 同时输出
  `cache_creation.ephemeral_5m_input_tokens` 和
  `cache_creation.ephemeral_1h_input_tokens`。
- Sub2API 原样解析同步与流式响应中的标准 usage，并以最终事件中实际存在的字段
  覆盖先前值；显式零值同样有效。
- Sub2API 按正常模型价格、渠道配置和分组倍率计费，不执行额外 token 放大、
  成本重分配、缓存率塑形或 Equivalent Cache 审计分配。
- `ForceCacheBilling` 与 `cache_ttl_override` 继续按各自原有职责工作，不属于
  Equivalent Cache V1/V2。

## Claude 标准缓存价格

Claude 缓存价格必须保持相对输入价格的标准关系：

| 项目 | 相对输入价格 |
| --- | ---: |
| 缓存读取 | `0.10x` |
| 5m 缓存写入 | `1.25x` |
| 1h 缓存写入 | `2.00x` |

价格数据装载会拒绝不符合上述关系的 Claude 条目。渠道定价 schema 只有单一
`cache_write_price`，无法同时表达 5m 与 1h 两档写入价，因此 Claude 渠道的 flat
和 interval 配置都不得设置该字段；非 Claude 渠道不受此限制。

## 废弃配置

以下配置入口已废弃且不再生效：

- `equivalent_cache_billing_*`
- `kiro_equivalent_cache_billing_*`
- `equivalent_cache_allocation_v2`

不要向账号 `extra` 写入这些字段，也不要继续执行旧版启用、shadow、active 或
回滚流程。历史数据中残留的废弃字段不代表功能仍可启用。

## 验证口径

排查计费时，应核对同一请求的上游原生 usage、Sub2API 使用记录、模型价格和分组
倍率。缓存读取、缓存创建和输出 token 应来自上游标准 usage，不应出现由
Equivalent Cache 策略生成的替代值。
