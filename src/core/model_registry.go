package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The runner registry supplies selectable model versions independently of the
// instance providerConfig, which may still name a legacy large-model default.
type instanceModelVersion struct {
	ID              string   `json:"id"`
	Display         string   `json:"display"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	Temperature     *float64 `json:"temperature"`
	ThinkingTokens  *int     `json:"thinkingTokens"`
	Default         bool     `json:"default"`
}

type instanceModelProvider struct {
	Versions []instanceModelVersion `json:"versions"`
}

type instanceModelRegistry map[string]instanceModelProvider

func (model instanceModelVersion) usable() bool {
	return strings.TrimSpace(model.ID) != "" && model.MaxOutputTokens > 0 &&
		(model.Temperature == nil || *model.Temperature >= 0) &&
		(model.ThinkingTokens == nil || *model.ThinkingTokens >= 0)
}

func (c *Client) fetchInstanceModelRegistry(ctx context.Context) (instanceModelRegistry, error) {
	endpoint, err := instanceModelRegistryURL(c.cfg.InstanceURL, c.cfg.WSURL)
	if err != nil {
		return nil, err
	}
	if c.httpClient == nil {
		if err := c.initGatewayHTTPClient(); err != nil {
			return nil, err
		}
	}
	// Keep redirect restrictions local to this discovery request. In particular,
	// custom authentication headers must never follow a redirect off-instance.
	httpClient := *c.httpClient
	previousRedirect := httpClient.CheckRedirect
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOriginInstance(c.cfg.InstanceURL, req.URL.String()) || req.URL.User != nil {
			return errors.New("refusing cross-origin model registry redirect")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 model registry redirects")
		}
		return nil
	}
	// Do not copy Client itself: it contains mutexes and active-turn state. This
	// read-only helper retains the existing bounded metadata retry behavior.
	reader := &Client{
		cfg: c.cfg, opts: c.opts, httpClient: &httpClient,
		gatewayAuth: c.gatewayAuth, basicUser: c.basicUser, basicPass: c.basicPass,
		sessionCookieHeader: c.sessionCookieHeader, userToken: c.userToken,
		oauthAccessToken: c.oauthAccessToken, remoteRetryPolicy: c.remoteRetryPolicy,
	}
	body, _, err := reader.getJSON(ctx, endpoint)
	for _, attempt := range reader.attemptTelemetry {
		c.recordAttempt(attempt)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch model registry: %w", err)
	}
	var registry instanceModelRegistry
	if err := json.Unmarshal(body, &registry); err != nil {
		return nil, fmt.Errorf("decode model registry: %w", err)
	}
	for provider, entry := range registry {
		if strings.TrimSpace(provider) == "" {
			continue
		}
		for _, model := range entry.Versions {
			if model.usable() {
				return registry, nil
			}
		}
	}
	return nil, errors.New("model registry contains no usable models")
}

func instanceModelRegistryURL(instanceURL, websocketURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(websocketURL))
	if err != nil {
		return "", errors.New("invalid model registry websocket URL")
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "https", "http":
	default:
		return "", errors.New("unsupported model registry websocket URL scheme")
	}
	if u.Host == "" || u.User != nil || !sameOriginInstance(instanceURL, u.String()) {
		return "", errors.New("refusing cross-origin model registry request")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/nirvana/web-socket") + "/v1/models"
	u.RawPath = ""
	u.Fragment = ""
	return u.String(), nil
}

func (registry instanceModelRegistry) selectModel(providerWire, requestedID string, inferProvider bool) (string, instanceModelVersion, error) {
	providerWire = strings.TrimSpace(providerWire)
	requestedID = strings.TrimSpace(requestedID)
	if requestedID == "" {
		var first instanceModelVersion
		for _, model := range registry[providerWire].Versions {
			if !model.usable() {
				continue
			}
			if first.ID == "" {
				first = model
			}
			if model.Default {
				return providerWire, model, nil
			}
		}
		if first.ID != "" {
			return providerWire, first, nil
		}
		return "", instanceModelVersion{}, fmt.Errorf("model registry has no usable models for provider %q", providerWire)
	}
	var selected instanceModelVersion
	selectedProvider := ""
	matches := 0
	otherProviderMatch := false
	for provider, entry := range registry {
		if strings.TrimSpace(provider) == "" {
			continue
		}
		for _, model := range entry.Versions {
			if !model.usable() || model.ID != requestedID {
				continue
			}
			if !inferProvider && provider != providerWire {
				otherProviderMatch = true
				continue
			}
			matches++
			selected, selectedProvider = model, provider
		}
	}
	if matches > 1 {
		return "", instanceModelVersion{}, fmt.Errorf("model %q is ambiguous in the instance registry", requestedID)
	}
	if matches == 1 {
		return selectedProvider, selected, nil
	}
	if otherProviderMatch {
		return "", instanceModelVersion{}, fmt.Errorf("model %q does not belong to provider %q", requestedID, providerWire)
	}
	return "", instanceModelVersion{}, fmt.Errorf("unknown model %q in the instance registry", requestedID)
}
