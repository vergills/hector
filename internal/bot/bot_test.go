package bot

import (
	"strings"
	"testing"
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

func TestBuildPromptDefinesHectorIdentity(t *testing.T) {
	prompt := buildPrompt("", "user", "42", "Hector", "Who are you?", 400)
	for _, want := range []string{
		"You are Hector",
		"when a user says Hector, they are addressing you directly",
		"the first person",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not contain identity instruction %q", want)
		}
	}
}
