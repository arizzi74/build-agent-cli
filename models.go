package main

import "fmt"

type ModelInfo struct {
	ID              string
	MaxOutputTokens int
	Temperature     float64
	ThinkingTokens  int
	DefinitionID    string
}

type ProviderConfig struct {
	ProviderWire         string
	LargeModel           string
	SmallModel           string
	LargeMaxOutputTokens int
	SmallMaxOutputTokens int
	LargeTemperature     float64
	SmallTemperature     float64
	LargeThinkingTokens  int
	SmallThinkingTokens  int
	SolutionName         string
	LargeDefinitionID    string
	SmallDefinitionID    string
}

type RuntimeModelConfig struct {
	Provider             string
	LargeModel           string
	SmallModel           string
	LargeMaxOutputTokens int
	SmallMaxOutputTokens int
	LargeTemperature     float64
	SmallTemperature     float64
	LargeThinkingTokens  int
	SmallThinkingTokens  int
	SolutionName         string
	LargeDefinitionID    string
	SmallDefinitionID    string
}

var providerConfigs = map[string]ProviderConfig{
	"bedrock": {
		ProviderWire: "claude", LargeModel: "claude-opus-4-6", SmallModel: "claude_small",
		LargeMaxOutputTokens: 64000, SmallMaxOutputTokens: 4096,
		LargeTemperature: 1, SmallTemperature: 0.1,
		LargeThinkingTokens: 2000, SmallThinkingTokens: 0,
		SolutionName:      "Build Agent (Amazon Bedrock - Amazon Bedrock Chat Completions)",
		LargeDefinitionID: "ec920c03ff6d6210509bffffffffffb8", SmallDefinitionID: "12e2ba33c09f4b258e40e8f8e9e9c464",
	},
	"anthropic": {
		ProviderWire: "claude", LargeModel: "claude-opus-4-6", SmallModel: "claude_small",
		LargeMaxOutputTokens: 64000, SmallMaxOutputTokens: 4096,
		LargeTemperature: 1, SmallTemperature: 0.1,
		LargeThinkingTokens: 2000, SmallThinkingTokens: 0,
		SolutionName:      "Build Agent (Amazon Bedrock - Amazon Bedrock Chat Completions)",
		LargeDefinitionID: "ec920c03ff6d6210509bffffffffffb8", SmallDefinitionID: "12e2ba33c09f4b258e40e8f8e9e9c464",
	},
	"openai": {
		ProviderWire: "openai", LargeModel: "gpt_large", SmallModel: "gpt_small",
		LargeMaxOutputTokens: 32000, SmallMaxOutputTokens: 4096,
		LargeTemperature: 0.2, SmallTemperature: 0.1,
		LargeThinkingTokens: 0, SmallThinkingTokens: 0,
		SolutionName:      "Build Agent (Azure OpenAI - Chat Completions)",
		LargeDefinitionID: "44f228b7ffa662109ca6ffffffffffc5", SmallDefinitionID: "c02424a8151249c6bc47229d3a864244",
	},
	"vertex": {
		ProviderWire: "gemini", LargeModel: "gemini_large", SmallModel: "gemini_small",
		LargeMaxOutputTokens: 65535, SmallMaxOutputTokens: 8192,
		LargeTemperature: 0.1, SmallTemperature: 0.1,
		LargeThinkingTokens: 32768, SmallThinkingTokens: 0,
		SolutionName:      "Build Agent (Google Cloud Vertex AI - Chat Completions)",
		LargeDefinitionID: "28e65343ff31a2109578ffffffffff03", SmallDefinitionID: "2585f17691f1449bb44fd3821db26fca",
	},
	"nowllm": {
		ProviderWire: "now", LargeModel: "llm_generic_large_v2", SmallModel: "llm_generic_small_v2",
		LargeMaxOutputTokens: 32000, SmallMaxOutputTokens: 4096,
		LargeTemperature: 0.2, SmallTemperature: 0.1,
		LargeThinkingTokens: 0, SmallThinkingTokens: 0,
		SolutionName:      "Build Agent (ServiceNow NowLLM)",
		LargeDefinitionID: "6a69b45effae32102e62ffffffffffc7", SmallDefinitionID: "ea9a3812ffee32102e62ffffffffff2c",
	},
}

var knownModels = map[string]struct {
	Provider string
	Info     ModelInfo
}{
	"gpt-5.2":              {"openai", ModelInfo{"gpt-5.2", 32000, 1, 1024, "44f228b7ffa662109ca6ffffffffffc5"}},
	"gpt_large":            {"openai", ModelInfo{"gpt_large", 32000, 0.2, 0, "44f228b7ffa662109ca6ffffffffffc5"}},
	"gpt-5.4":              {"openai", ModelInfo{"gpt-5.4", 32000, 1, 1024, "a3cb54e2ff32b2102e62ffffffffff54"}},
	"gpt-5.5":              {"openai", ModelInfo{"gpt-5.5", 32000, 1, 1024, "a3cb54e2ff32b2102e62ffffffffff54"}},
	"gemini-3-pro":         {"vertex", ModelInfo{"gemini-3-pro", 65535, 0.1, 0, "086abae2ff32b2102e62ffffffffff8a"}},
	"gemini-3-flash":       {"vertex", ModelInfo{"gemini-3-flash", 8192, 0.1, 0, "3d8bbeaaff32b2102e62ffffffffff7b"}},
	"gemini-3.5-flash":     {"vertex", ModelInfo{"gemini-3.5-flash", 8192, 0.1, 0, "3d8bbeaaff32b2102e62ffffffffff7b"}},
	"gemini_large":         {"vertex", ModelInfo{"gemini_large", 65535, 0.1, 32768, "28e65343ff31a2109578ffffffffff03"}},
	"gemini_small":         {"vertex", ModelInfo{"gemini_small", 8192, 0.1, 0, "2585f17691f1449bb44fd3821db26fca"}},
	"gemini-3.1-pro":       {"vertex", ModelInfo{"gemini-3.1-pro", 65535, 0.1, 0, "28e65343ff31a2109578ffffffffff03"}},
	"claude-sonnet-4-5":    {"bedrock", ModelInfo{"claude-sonnet-4-5", 64000, 1, 2000, "ec920c03ff6d6210509bffffffffffb8"}},
	"claude-sonnet-4-6":    {"bedrock", ModelInfo{"claude-sonnet-4-6", 64000, 1, 2000, "ec920c03ff6d6210509bffffffffffb8"}},
	"claude-opus-4-5":      {"bedrock", ModelInfo{"claude-opus-4-5", 64000, 1, 2000, "ec920c03ff6d6210509bffffffffffb8"}},
	"claude-opus-4-6":      {"bedrock", ModelInfo{"claude-opus-4-6", 64000, 1, 2000, "ec920c03ff6d6210509bffffffffffb8"}},
	"claude_large":         {"bedrock", ModelInfo{"claude_large", 64000, 1, 2000, "ec920c03ff6d6210509bffffffffffb8"}},
	"claude-haiku-4-5":     {"bedrock", ModelInfo{"claude-haiku-4-5", 4096, 0.1, 0, "12e2ba33c09f4b258e40e8f8e9e9c464"}},
	"claude_small":         {"bedrock", ModelInfo{"claude_small", 4096, 0.1, 0, "12e2ba33c09f4b258e40e8f8e9e9c464"}},
	"llm_generic_large_v2": {"nowllm", ModelInfo{"llm_generic_large_v2", 32000, 0.2, 0, "6a69b45effae32102e62ffffffffffc7"}},
	"llm_generic_small_v2": {"nowllm", ModelInfo{"llm_generic_small_v2", 4096, 0.1, 0, "ea9a3812ffee32102e62ffffffffff2c"}},
}

func selectRuntimeModel(provider, model string) (RuntimeModelConfig, error) {
	explicitProvider := provider != ""
	if model != "" {
		known, ok := knownModels[model]
		if !ok {
			return RuntimeModelConfig{}, fmt.Errorf("unknown model %q", model)
		}
		if !explicitProvider {
			provider = known.Provider
		} else if provider != known.Provider && !(provider == "anthropic" && known.Provider == "bedrock") {
			return RuntimeModelConfig{}, fmt.Errorf("model %q belongs to provider %q, not %q", model, known.Provider, provider)
		}
	}
	if provider == "" {
		provider = "bedrock"
	}
	pc, ok := providerConfigs[provider]
	if !ok {
		return RuntimeModelConfig{}, fmt.Errorf("unsupported provider %q; use openai, bedrock, anthropic, vertex, or nowllm", provider)
	}
	rt := RuntimeModelConfig{
		Provider:             pc.ProviderWire,
		LargeModel:           pc.LargeModel,
		SmallModel:           pc.SmallModel,
		LargeMaxOutputTokens: pc.LargeMaxOutputTokens,
		SmallMaxOutputTokens: pc.SmallMaxOutputTokens,
		LargeTemperature:     pc.LargeTemperature,
		SmallTemperature:     pc.SmallTemperature,
		LargeThinkingTokens:  pc.LargeThinkingTokens,
		SmallThinkingTokens:  pc.SmallThinkingTokens,
		SolutionName:         pc.SolutionName,
		LargeDefinitionID:    pc.LargeDefinitionID,
		SmallDefinitionID:    pc.SmallDefinitionID,
	}
	if model != "" {
		mi := knownModels[model].Info
		rt.LargeModel = mi.ID
		rt.LargeMaxOutputTokens = mi.MaxOutputTokens
		rt.LargeTemperature = mi.Temperature
		rt.LargeThinkingTokens = mi.ThinkingTokens
		rt.LargeDefinitionID = mi.DefinitionID
	}
	return rt, nil
}
