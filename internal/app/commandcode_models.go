package app

// ccGoPlanModel 描述 CommandCode「Go 套餐」下的单个可选模型。
// Name 仅用于管理端展示，ID 才是发给上游 params.model 的真实值。
type ccGoPlanModel struct {
	ID   string
	Name string
}

// ccGoPlanModels 是 CommandCode「Go 套餐」的静态模型清单（46 个）。
//
// 来源：command-code@1.53.1 包内 dist/bundled/command-code-knowledge/reference/models.md
// 的权威清单（经关联项目 CommandCodeGo-manager/src/protocol/models.mjs 逐条转写），
// 此处只保留 plan=="go" 的条目，Pro / Max / GOAT 套餐模型不在本适配器范围内。
//
// 为什么必须内置：Go 套餐访问上游 /provider/v1/models 会返回 403，
// 无法动态拉取模型列表，因此 /v1/models 在 provider.Models 为空时必须回退到本清单，
// 绝不能去请求 /provider/v1/models。
var ccGoPlanModels = []ccGoPlanModel{
	{ID: "deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro"},
	{ID: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash"},
	{ID: "deepseek/deepseek-v4-flash-vision-exp", Name: "DeepSeek V4 Flash Vision"},
	{ID: "deepseek/deepseek-v4-flash-fast", Name: "DeepSeek V4 Flash Fast"},
	{ID: "deepseek/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash"},
	{ID: "moonshotai/Kimi-K3", Name: "Kimi K3"},
	{ID: "moonshotai/Kimi-K2.7-Code", Name: "Kimi K2.7 Code"},
	{ID: "moonshotai/Kimi-K2.7-Code-Highspeed", Name: "Kimi K2.7 Code HighSpeed"},
	{ID: "moonshotai/Kimi-K2.6", Name: "Kimi K2.6"},
	{ID: "moonshotai/Kimi-K2.5", Name: "Kimi K2.5"},
	{ID: "z-ai/glm-5.3-flash", Name: "GLM-5.3 Flash"},
	{ID: "z-ai/glm-5.3-flashx", Name: "GLM-5.3 FlashX"},
	{ID: "zai-org/GLM-5.3", Name: "GLM-5.3"},
	{ID: "zai-org/GLM-5.2", Name: "GLM-5.2"},
	{ID: "zai-org/GLM-5.2-Fast", Name: "GLM-5.2 Fast"},
	{ID: "zai-org/GLM-5.1", Name: "GLM-5.1"},
	{ID: "zai-org/GLM-5", Name: "GLM-5"},
	{ID: "MiniMaxAI/MiniMax-M3", Name: "MiniMax M3"},
	{ID: "MiniMaxAI/MiniMax-M2.7", Name: "MiniMax M2.7"},
	{ID: "MiniMaxAI/MiniMax-M2.5", Name: "MiniMax M2.5"},
	{ID: "xiaomi/mimo-v2.5-pro", Name: "MiMo V2.5 Pro"},
	{ID: "xiaomi/mimo-v2.5", Name: "MiMo V2.5"},
	{ID: "Qwen/Qwen3.8-Omni-Flash", Name: "Qwen 3.8 Omni Flash"},
	{ID: "Qwen/Qwen3.8-Max-0902", Name: "Qwen 3.8 Max 0902"},
	{ID: "Qwen/Qwen3.8-Max", Name: "Qwen 3.8 Max"},
	{ID: "Qwen/Qwen3.8-27B", Name: "Qwen 3.8 27B"},
	{ID: "Qwen/Qwen3.8-Flash", Name: "Qwen 3.8 Flash"},
	{ID: "Qwen/Qwen3.7-Max", Name: "Qwen 3.7 Max"},
	{ID: "Qwen/Qwen3.7-Plus", Name: "Qwen 3.7 Plus"},
	{ID: "Qwen/Qwen3.7-Flash", Name: "Qwen 3.7 Flash"},
	{ID: "Qwen/Qwen3.6-Max-Preview", Name: "Qwen 3.6 Max Preview"},
	{ID: "Qwen/Qwen3.6-Plus", Name: "Qwen 3.6 Plus"},
	{ID: "meituan/LongCat-2.0", Name: "LongCat 2.0"},
	{ID: "stepfun/Step-3.7-Flash", Name: "Step 3.7 Flash"},
	{ID: "stepfun/Step-3.5-Flash", Name: "Step 3.5 Flash"},
	{ID: "tencent/hy3-paid", Name: "Tencent Hy3"},
	{ID: "tencent/hy4-preview", Name: "Tencent Hy4 Preview"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b", Name: "Nemotron 3 Ultra"},
	{ID: "thinkingmachines/inkling", Name: "Inkling"},
	{ID: "thinkingmachines/inkling-small", Name: "Inkling Small"},
	{ID: "poolside/laguna-s-2.1-free", Name: "Laguna S 2.1"},
	{ID: "inclusionai/ling-3.0-flash-sante:free", Name: "Ling 3.0 Flash Sante"},
	{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna"},
	{ID: "meta/muse-spark-1.2-contributor", Name: "Muse Spark 1.2 Contributor"},
	{ID: "meta/muse-spark-1.3-contributor", Name: "Muse Spark 1.3 Contributor"},
	{ID: "xai/grok-4.5", Name: "Grok 4.5"},
}

// ccGoPlanModelIDs 返回 Go 套餐模型的 ID 列表（顺序与权威清单一致）。
func ccGoPlanModelIDs() []string {
	ids := make([]string, 0, len(ccGoPlanModels))
	for _, m := range ccGoPlanModels {
		ids = append(ids, m.ID)
	}
	return ids
}

// ccGoPlanModelList 以 OpenAI /v1/models 的条目形态返回静态清单，
// 供 listModelsForLocalKey 在 provider 未配置模型时回退使用。
func ccGoPlanModelList() []map[string]any {
	data := make([]map[string]any, 0, len(ccGoPlanModels))
	for _, m := range ccGoPlanModels {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  0,
			"owned_by": "commandcode",
			"name":     m.Name,
			"plan":     "go",
		})
	}
	return data
}
