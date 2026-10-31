package provider

import (
	"net/http"
	"os"

	"github.com/nikolasss4/switchyard/internal/config"
)

// FromConfig builds adapters for the configured providers. API keys are read
// from the environment variables the config names, never from the file.
func FromConfig(cfg *config.Config, client *http.Client) map[string]Provider {
	out := map[string]Provider{}
	for _, p := range cfg.Providers {
		key := ""
		if p.APIKeyEnv != "" {
			key = os.Getenv(p.APIKeyEnv)
		}
		switch p.Type {
		case "anthropic":
			out[p.Name] = NewAnthropic(AnthropicConfig{Name: p.Name, BaseURL: p.BaseURL, APIKey: key, Headers: p.Headers}, client)
		case "openai":
			out[p.Name] = NewOpenAI(OpenAIConfig{Name: p.Name, BaseURL: p.BaseURL, APIKey: key, MaxTokensField: p.MaxTokensField, Headers: p.Headers}, client)
		}
	}
	return out
}
