package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

const (
	DefaultGeminiModel        = "gemini-3.5-flash-lite"
	DefaultBotName            = "Hector"
	DefaultBotPrefix          = "h"
	DefaultMaxContextMessages = 8
	DefaultSystemPrompt       = "You are a helpful assistant in Discord. Keep responses brief, clear, and useful. Do not exceed the configured character budget."
	DefaultHelpText           = "Ask me something, like `h hello`."
	DefaultMaxResponseChars   = 400
	DefaultMaxOutputTokens    = 512
)

type Config struct {
	DiscordToken       string
	GeminiAPIKey       string
	GeminiModel        string
	BotName            string
	BotPrefix          string
	MaxContextMessages int
	SystemPrompt       string
	HelpText           string
	MaxResponseChars   int
	MaxOutputTokens    int
}

func Load() Config {
	_ = godotenv.Load()

	cfg := Config{
		GeminiModel:        DefaultGeminiModel,
		BotName:            DefaultBotName,
		BotPrefix:          DefaultBotPrefix,
		MaxContextMessages: DefaultMaxContextMessages,
		SystemPrompt:       DefaultSystemPrompt,
		HelpText:           DefaultHelpText,
		MaxResponseChars:   DefaultMaxResponseChars,
		MaxOutputTokens:    DefaultMaxOutputTokens,
	}

	cfg.DiscordToken = getEnv("DISCORD_TOKEN")
	cfg.GeminiAPIKey = getEnv("GEMINI_API_KEY")
	cfg.GeminiModel = getEnvWithFallback("GEMINI_MODEL", cfg.GeminiModel)
	cfg.BotName = getEnvWithFallback("BOT_NAME", cfg.BotName)
	cfg.BotPrefix = getEnvWithFallback("BOT_PREFIX", cfg.BotPrefix)
	cfg.MaxContextMessages = getIntWithFallback("MAX_CONTEXT_MESSAGES", cfg.MaxContextMessages)
	cfg.SystemPrompt = getEnvWithFallback("SYSTEM_PROMPT", cfg.SystemPrompt)
	cfg.HelpText = getEnvWithFallback("HELP_TEXT", cfg.HelpText)
	cfg.MaxResponseChars = getIntWithFallback("MAX_RESPONSE_CHARS", cfg.MaxResponseChars)
	cfg.MaxOutputTokens = getIntWithFallback("MAX_OUTPUT_TOKENS", cfg.MaxOutputTokens)

	return cfg
}

func getEnv(name string) string {
	return os.Getenv(name)
}

func getEnvWithFallback(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func getIntWithFallback(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
