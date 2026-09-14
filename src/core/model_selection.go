package core

import (
	"context"
	"fmt"
	"strconv"
)

func selectInitialRuntimeModel(opts Options) (RuntimeModelConfig, error) {
	if opts.Nirvana && opts.Model != "" {
		if _, known := knownModels[opts.Model]; !known {
			// New backend models must be validated after authenticated discovery,
			// rather than rejected by the older built-in fallback catalog.
			return selectRuntimeModel(opts.Provider, "")
		}
	}
	return selectRuntimeModel(opts.Provider, opts.Model)
}

func (c *Client) configureNirvanaModel(ctx context.Context) error {
	cfg, err := c.fetchWebAgentConfig(ctx)
	if err != nil {
		return err
	}
	c.webAgentConfig = cfg
	c.modelFromRegistry = false
	registry, err := c.fetchInstanceModelRegistry(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.opts.Model != "" {
			if _, known := knownModels[c.opts.Model]; !known {
				return fmt.Errorf("cannot resolve model %q: instance model registry unavailable", c.opts.Model)
			}
		}
		c.reportAuthenticationNotice("Instance model registry unavailable; using the configured model.", "warning: instance model registry unavailable; using the configured model", true)
		return nil
	}
	return c.applyInstanceModelRegistry(registry)
}

func (c *Client) applyInstanceModelRegistry(registry instanceModelRegistry) error {
	provider := c.runtime.Provider
	if c.opts.Provider == "" && c.webAgentConfig.Provider != "" {
		provider = c.webAgentConfig.Provider
	}
	provider, model, err := registry.selectModel(provider, c.opts.Model, c.opts.Provider == "")
	if err != nil {
		return err
	}
	providerName := provider
	switch provider {
	case "claude":
		providerName = "bedrock"
	case "gemini":
		providerName = "vertex"
	case "now":
		providerName = "nowllm"
	}
	runtime, err := selectRuntimeModel(providerName, "")
	if err != nil {
		return err
	}
	runtime.LargeModel = model.ID
	runtime.LargeDefinitionID = ""
	if model.MaxOutputTokens > 0 {
		runtime.LargeMaxOutputTokens = model.MaxOutputTokens
	}
	if model.Temperature != nil {
		runtime.LargeTemperature = *model.Temperature
	}
	if model.ThinkingTokens != nil {
		runtime.LargeThinkingTokens = *model.ThinkingTokens
	}
	// The Web UI replaces only the large model with the selected registry
	// version and keeps the instance's small-model configuration.
	if small := c.webAgentConfig.SmallConfig; small != nil && (c.webAgentConfig.Provider == "" || c.webAgentConfig.Provider == runtime.Provider) {
		if id := firstString(small, "model"); id != "" {
			runtime.SmallModel = id
		}
		if max := numericValue(firstNonNilStartup(small["maxOutputTokens"], small["maxTokens"])); max > 0 {
			runtime.SmallMaxOutputTokens = int(max)
		}
		if temperature, err := strconv.ParseFloat(stringify(small["temperature"]), 64); err == nil {
			runtime.SmallTemperature = temperature
		}
		if thinking, ok := small["thinkingTokens"]; ok {
			runtime.SmallThinkingTokens = int(numericValue(thinking))
		}
	}
	c.runtime = runtime
	c.webAgentConfig.Model = model.ID
	c.modelFromRegistry = true
	return nil
}
