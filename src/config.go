package src

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	DiscordToken     string
	DiscordChannelID string
	DiscordGuildID   string

	OpenAIAPIKey string
	OpenAIModel  string

	SystemPrompt string

	LLMMaxTokens   int
	WorkerPoolSize int
	LLMTimeout     time.Duration
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{}
	var errs []string

	cfg.DiscordToken = mustEnv("DISCORD_TOKEN", &errs)
	cfg.DiscordChannelID = mustEnv("DISCORD_CHANNEL_ID", &errs)
	cfg.DiscordGuildID = mustEnv("DISCORD_GUILD_ID", &errs)

	cfg.OpenAIAPIKey = mustEnv("DEEPSEEK_API_KEY", &errs)
	cfg.OpenAIModel = envOr("DEEPSEEK_MODEL", "deepseek-chat")
	cfg.SystemPrompt = envOr("SYSTEM_PROMPT", "You are a helpful, concise assistant living inside a Discord server.")

	cfg.LLMMaxTokens = intEnvOr("LLM_MAX_TOKENS", 1024)
	cfg.WorkerPoolSize = intEnvOr("WORKER_POOL_SIZE", 10)

	timeoutSec := intEnvOr("LLM_TIMEOUT_SECONDS", 60)
	cfg.LLMTimeout = time.Duration(timeoutSec) * time.Second

	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration errors:\n  • %s", strings.Join(errs, "\n  • "))
	}

	return cfg, nil
}

func mustEnv(key string, errs *[]string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		*errs = append(*errs, fmt.Sprintf("%s is required but not set", key))
	}
	return v
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func intEnvOr(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

var _ = errors.New
