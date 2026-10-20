package service

// plazaRMBOfficialPricing 仅供模型广场读取：人民币官方金额直接作为站内参考单位，
// 不换汇、不套销售倍率，也不参与 BillingService 或渠道价卡解析。只匹配已采用的调用名。
// 2026-10-20 核对来源：
//   - https://cloud.tencent.com/document/product/1823/130055 （K3、GLM、HY；hy4 采用 Preview 参考）
//   - https://api-docs.deepseek.com/zh-cn/quick_start/pricing/ （DeepSeek 谷价；旧 Flash 别名同现行 Flash）
//   - https://platform.minimax.cn/docs/pricing/overview （M3 标准两档）
//
// 每次构造独立字段和区间，防止调用方修改污染后续请求；未公布的缓存写入价保留 nil。
func plazaRMBOfficialPricing(model string) *PlazaOfficialPricing {
	var input, output, cacheRead float64
	switch model {
	case "deepseek-v4-flash", "deepseek-v4.1-flash":
		input, output, cacheRead = 1, 4, 0.02
	case "deepseek-v4-pro":
		input, output, cacheRead = 4.5, 13.5, 0.15
	case "glm-5.2", "glm-5.3":
		input, output, cacheRead = 8, 28, 2
	case "glm-5.3-flash":
		input, output, cacheRead = 0.8, 2.8, 0.23
	case "kimi-k3":
		input, output, cacheRead = 20, 100, 2
	case "minimax-m3":
		input, output, cacheRead = 2.1, 8.4, 0.42
	case "hy3":
		input, output, cacheRead = 1, 4, 0.25
	case "hy4":
		input, output, cacheRead = 6, 18, 0.3
	default:
		return nil
	}
	reference := &PlazaOfficialPricing{
		InputPrice:     nonZeroPtr(input / 1_000_000),
		OutputPrice:    nonZeroPtr(output / 1_000_000),
		CacheReadPrice: nonZeroPtr(cacheRead / 1_000_000),
	}
	if model == "minimax-m3" {
		// 区间契约为左开右闭，512000 token 仍属于第一档；与分组阶梯开关无关。
		boundary := 512000
		reference.Intervals = []PricingInterval{
			{MinTokens: 0, MaxTokens: &boundary, TierLabel: "≤512K", SortOrder: 0,
				InputPrice: nonZeroPtr(input / 1_000_000), OutputPrice: nonZeroPtr(output / 1_000_000), CacheReadPrice: nonZeroPtr(cacheRead / 1_000_000)},
			{MinTokens: boundary, TierLabel: ">512K", SortOrder: 1,
				InputPrice: nonZeroPtr(2 * input / 1_000_000), OutputPrice: nonZeroPtr(2 * output / 1_000_000), CacheReadPrice: nonZeroPtr(2 * cacheRead / 1_000_000)},
		}
	}
	return reference
}
