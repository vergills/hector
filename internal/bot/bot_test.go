package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestExtractPromptFromDiscordMention(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "plain mention", content: "<@123> hello", want: "hello"},
		{name: "nickname mention", content: "please ask <@!123> hello", want: "please ask hello"},
		{name: "mention only", content: "<@123>", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractPrompt(tt.content, "h", "123")
			if !ok {
				t.Fatal("expected Discord mention to trigger")
			}
			if got != tt.want {
				t.Fatalf("prompt = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractPromptRequiresPrefixWhitespace(t *testing.T) {
	if prompt, ok := extractPrompt("hello there", "h", "123"); ok || prompt != "" {
		t.Fatalf("unexpected prefix match: %q, %v", prompt, ok)
	}
	prompt, ok := extractPrompt("h hello there", "h", "123")
	if !ok || prompt != "hello there" {
		t.Fatalf("prompt = %q, ok = %v", prompt, ok)
	}
}

func TestReplaceDiscordMentionsUsesDisplayNames(t *testing.T) {
	got := replaceDiscordMentions("hello <@123> and <@!456>", []*discordgo.User{
		{ID: "123", Username: "bridge", GlobalName: "Bridge Person"},
		{ID: "456", Username: "another-user"},
	})
	want := "hello @Bridge Person and @another-user"
	if got != want {
		t.Fatalf("mentions = %q, want %q", got, want)
	}

}

func TestFormatCodeSearchOutputGroupsFiles(t *testing.T) {
	blocks := formatCodeSearchOutput("./internal/bot/bot.go:12:func handleMessage()\n./internal/bot/bot.go:20:return\n./README.md:4:go run ./cmd/hector")
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	for _, want := range []string{"**internal/bot/bot.go**", "- `12` `func handleMessage()`", "- `20` `return`", "**README.md**"} {
		if !strings.Contains(blocks[0], want) {
			t.Fatalf("formatted output does not contain %q: %s", want, blocks[0])
		}
	}
}

func TestBuildPromptDefinesHectorIdentity(t *testing.T) {
	prompt := buildPrompt("", "user", "42", "Hector", "Who are you?", 400)
	for _, want := range []string{
		"You are Hector",
		"when a user says Hector, they are addressing you directly",
		"the first person",
		"do not use punctuation",
		"not a customer support agent",
		"Do not restate the question",
		"Only mention a Discord user when it is relevant",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not contain identity instruction %q", want)
		}
	}

}

func TestTrimResponseRemovesParagraphTags(t *testing.T) {
	got := trimResponse("<p>hello there</p>", 140)
	if got != "hello there" {
		t.Fatalf("response = %q, want %q", got, "hello there")
	}
}
