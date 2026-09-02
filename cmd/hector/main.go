package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vergills/hector/internal/bot"
	"github.com/vergills/hector/internal/codesearch"
	"github.com/vergills/hector/internal/config"
	"github.com/vergills/hector/internal/gemini"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()
	if cfg.DiscordToken == "" || cfg.GeminiAPIKey == "" {
		logger.Error("missing required configuration", "required", []string{"DISCORD_TOKEN", "GEMINI_API_KEY"})
		return
	}

	client := gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.SystemPrompt, cfg.MaxResponseChars, cfg.MaxOutputTokens)
	codeSearcher, err := codesearch.New(".")
	if err != nil {
		logger.Error("code search initialization failed", "error", err.Error())
		os.Exit(1)
	}
	svc := bot.New(client, cfg.BotPrefix, cfg.MaxContextMessages, cfg.HelpText, cfg.MaxResponseChars, codeSearcher)
	svc.Logger = logger

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		close(svc.Shutdown)
	}()

	if err := svc.Start(cfg.DiscordToken); err != nil {
		logger.Error("bot stopped with error", "error", err.Error())
		os.Exit(1)
	}
}
