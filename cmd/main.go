package main

import (
	"context"
	"discord-llm-bot/src"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))

	cfg, err := src.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	slog.Info("configuration loaded",
		"channel_id", cfg.DiscordChannelID,
		"deepseek_model", cfg.OpenAIModel,
		"workers", cfg.WorkerPoolSize,
		"llm_timeout", cfg.LLMTimeout,
	)

	llmClient := src.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIModel, cfg.SystemPrompt, cfg.LLMMaxTokens)
	sqStore, err := src.NewStore("./bank.sql")
	if err != nil {
		slog.Error("error initializing store", "err", err)
		os.Exit(1)
	}

	b := src.NewBot(cfg, llmClient, sqStore)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := b.Start(ctx); err != nil {
		slog.Error("bot exited with error", "err", err)
		os.Exit(1)
	}

	slog.Info("bot shut down")
}
