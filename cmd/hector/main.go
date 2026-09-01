package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/vergills/hector/internal/bot"
	"github.com/vergills/hector/internal/config"
	"github.com/vergills/hector/internal/gemini"
)

func main() {
	cfg := config.Load()
	if cfg.DiscordToken == "" || cfg.GeminiAPIKey == "" {
		fmt.Println("missing required env vars: DISCORD_TOKEN and GEMINI_API_KEY")
		fmt.Println("set them in .env or your shell before running the bot")
		return
	}

	client := gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.SystemPrompt, cfg.MaxResponseChars, cfg.MaxOutputTokens)
	svc := bot.New(client, cfg.BotPrefix, cfg.MaxContextMessages, cfg.HelpText, cfg.MaxResponseChars)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		close(svc.Shutdown)
	}()

	if err := svc.Start(cfg.DiscordToken); err != nil {
		log.Fatal(err)
	}
}
