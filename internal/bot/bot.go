package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/vergills/hector/internal/codesearch"
	"github.com/vergills/hector/internal/gemini"
	"github.com/vergills/hector/internal/version"
)

type CodeSearcher interface {
	Search(pattern string) (string, error)
}

type Service struct {
	Client             *gemini.Client
	Prefix             string
	Shutdown           chan struct{}
	MaxContextMessages int
	HelpText           string
	MaxResponseChars   int
	CodeSearcher       CodeSearcher
}

func New(client *gemini.Client, prefix string, maxContextMessages int, helpText string, maxResponseChars int, codeSearcher CodeSearcher) *Service {
	if prefix == "" {
		prefix = "h"
	}
	if maxContextMessages <= 0 {
		maxContextMessages = 8
	}
	if helpText == "" {
		helpText = "Ask me something, like `h hello`."
	}
	if maxResponseChars <= 0 {
		maxResponseChars = 700
	}
	return &Service{
		Client:             client,
		Prefix:             prefix,
		Shutdown:           make(chan struct{}),
		MaxContextMessages: maxContextMessages,
		HelpText:           helpText,
		MaxResponseChars:   maxResponseChars,
		CodeSearcher:       codeSearcher,
	}
}

func (s *Service) Start(discordToken string) error {
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}

	dg.AddHandler(s.handleMessage)
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages

	if err := dg.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	defer dg.Close()

	fmt.Println("hector is online")
	fmt.Println("prefix:", s.Prefix)

	<-s.Shutdown
	return nil
}

func (s *Service) handleMessage(session *discordgo.Session, message *discordgo.MessageCreate) {
	if message == nil || message.Author == nil || message.Author.Bot {
		return
	}

	content := strings.TrimSpace(message.Content)
	if content == "" {
		return
	}

	codeQuery, okCode := extractSubcommand(content, s.Prefix, session.State.User.ID, "code")
	if okCode {
		s.handleCodeSearch(session, message, codeQuery)
		return
	}
	if _, okVersion := extractSubcommand(content, s.Prefix, session.State.User.ID, "version"); okVersion {
		s.handleVersion(session, message)
		return
	}

	ctxPrompt, ctxCount, okCtx, err := parseContextCommand(content, s.Prefix, session.State.User.ID)
	if err != nil {
		_, _ = session.ChannelMessageSend(message.ChannelID, err.Error())
		return
	}

	if okCtx {
		if ctxCount > s.MaxContextMessages {
			ctxCount = s.MaxContextMessages
		}
		contextText, err := s.readRecentContext(session, message, ctxCount)
		if err != nil {
			contextText = ""
		}
		response, err := s.generateResponse(message, ctxPrompt, contextText)
		if err != nil {
			_, _ = session.ChannelMessageSend(message.ChannelID, "I hit a problem: "+err.Error())
			return
		}
		for _, block := range splitDiscordMessage(response) {
			_, _ = session.ChannelMessageSend(message.ChannelID, block)
			time.Sleep(200 * time.Millisecond)
		}
		return
	}

	prompt, ok := extractPrompt(content, s.Prefix, session.State.User.ID)
	if !ok && isReplyToBot(message, session.State.User.ID) {
		prompt, ok = content, true
	}
	if !ok {
		prompt, ok = extractNamePrompt(content, session.State.User.Username)
	}
	if !ok {
		return
	}
	if prompt == "" {
		_, _ = session.ChannelMessageSend(message.ChannelID, s.HelpText)
		return
	}

	response, err := s.generateResponse(message, prompt, "")
	if err != nil {
		_, _ = session.ChannelMessageSend(message.ChannelID, "I hit a problem: "+err.Error())
		return
	}
	for _, block := range splitDiscordMessage(response) {
		_, _ = session.ChannelMessageSend(message.ChannelID, block)
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *Service) handleVersion(session *discordgo.Session, message *discordgo.MessageCreate) {
	info := version.Current()
	revision := info.Revision
	if len(revision) > 12 {
		revision = revision[:12]
	}
	state := "clean"
	if info.Modified {
		state = "modified source"
	}
	_, _ = session.ChannelMessageSend(message.ChannelID,
		fmt.Sprintf("running commit `%s` (%s)\nheader: %s\ndescription: %s\nbuilt: `%s`\ngo: `%s`",
			revision, state, info.Subject, info.Body, info.Time, info.GoVersion))
}

func (s *Service) handleCodeSearch(session *discordgo.Session, message *discordgo.MessageCreate, question string) {
	if strings.TrimSpace(question) == "" {
		_, _ = session.ChannelMessageSend(message.ChannelID, fmt.Sprintf("Usage: `%s code <what to search for>`", s.Prefix))
		return
	}

	prompt := "Return only one safe grep -E regular expression for searching this Go repository. " +
		"Do not use shell syntax, flags, paths, backticks, or newlines. Search terms should be concise and match source identifiers or concepts. " +
		"User request: " + question
	pattern, err := s.Client.Generate(context.Background(), prompt)
	if err != nil {
		_, _ = session.ChannelMessageSend(message.ChannelID, "I hit a problem generating the code search: "+err.Error())
		return
	}
	pattern = strings.TrimSpace(strings.Trim(pattern, "`"))
	if s.CodeSearcher == nil {
		_, _ = session.ChannelMessageSend(message.ChannelID, "Code search is not configured.")
		return
	}
	output, err := s.CodeSearcher.Search(pattern)
	if err != nil {
		if errors.Is(err, codesearch.ErrNoMatches) {
			_, _ = session.ChannelMessageSend(message.ChannelID, "No matching source lines found.")
			return
		}
		_, _ = session.ChannelMessageSend(message.ChannelID, "Code search failed: "+err.Error())
		return
	}
	for _, block := range splitCodeSearchOutput(output) {
		_, _ = session.ChannelMessageSend(message.ChannelID, block)
	}
}

func splitCodeSearchOutput(output string) []string {
	const maxCodeBlock = 1900
	lines := strings.Split(output, "\n")
	blocks := make([]string, 0)
	current := ""
	for _, line := range lines {
		if len(current)+len(line)+1 > maxCodeBlock && current != "" {
			blocks = append(blocks, "```text\n"+current+"\n```")
			current = ""
		}
		if current != "" {
			current += "\n"
		}
		current += line
	}
	if current != "" {
		blocks = append(blocks, "```text\n"+current+"\n```")
	}
	return blocks
}

func extractSubcommand(content, prefix, botID, name string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	lower := strings.ToLower(trimmed)
	for _, mention := range []string{"<@" + botID + ">", "<@!" + botID + ">"} {
		if strings.HasPrefix(lower, strings.ToLower(mention)) {
			trimmed = strings.TrimSpace(trimmed[len(mention):])
			lower = strings.ToLower(trimmed)
			break
		}
	}
	keyword := strings.ToLower(prefix + " " + name)
	if !strings.HasPrefix(lower, keyword) {
		return "", false
	}
	return strings.TrimSpace(trimmed[len(keyword):]), true
}

func isReplyToBot(message *discordgo.MessageCreate, botID string) bool {
	if message == nil || message.ReferencedMessage == nil || message.ReferencedMessage.Author == nil {
		return false
	}
	return message.ReferencedMessage.Author.ID == botID
}

func (s *Service) generateResponse(message *discordgo.MessageCreate, prompt, contextText string) (string, error) {
	finalPrompt := buildPrompt(contextText, message.Author.Username, message.Author.ID, prompt, s.MaxResponseChars)
	response, err := s.Client.Generate(context.Background(), finalPrompt)
	if err != nil {
		var rateLimitErr *gemini.RateLimitError
		if errors.As(err, &rateLimitErr) {
			if rateLimitErr.Daily {
				return "", fmt.Errorf("the Gemini daily quota is exhausted; please try again after the quota resets")
			}
			if rateLimitErr.RetryAfter > 0 {
				return "", fmt.Errorf("Gemini is rate-limiting requests; please retry in %s", rateLimitErr.RetryAfter.Round(time.Second))
			}
			return "", fmt.Errorf("Gemini is temporarily rate-limiting requests; please try again shortly")
		}
		return "", err
	}
	return trimResponse(response, s.MaxResponseChars), nil
}

func (s *Service) readRecentContext(session *discordgo.Session, message *discordgo.MessageCreate, count int) (string, error) {
	if count <= 0 {
		count = s.MaxContextMessages
	}
	messages, err := session.ChannelMessages(message.ChannelID, count, message.ID, "", "")
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(messages))
	for i := len(messages) - 1; i >= 0; i-- {
		item := messages[i]
		if item == nil || item.Author == nil || item.Author.Bot {
			continue
		}
		if item.ID == message.ID {
			continue
		}
		parts = append(parts, fmt.Sprintf("<@%s> (%s): %s", item.Author.ID, item.Author.Username, strings.TrimSpace(item.Content)))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n"), nil
}

func parseContextCommand(content, prefix, botID string) (string, int, bool, error) {
	trimmed := strings.TrimSpace(content)
	lowerTrimmed := strings.ToLower(trimmed)
	for _, candidate := range []string{"<@" + botID + ">", "<@!" + botID + ">"} {
		if strings.HasPrefix(lowerTrimmed, strings.ToLower(candidate)) {
			trimmed = strings.TrimSpace(trimmed[len(candidate):])
			lowerTrimmed = strings.ToLower(trimmed)
			break
		}
	}

	ctxKeyword := strings.ToLower(prefix + " ctx")
	if strings.HasPrefix(lowerTrimmed, ctxKeyword) {
		afterPrefix := strings.TrimSpace(trimmed[len(prefix):])
		if strings.HasPrefix(strings.ToLower(afterPrefix), "ctx") {
			afterPrefix = strings.TrimSpace(afterPrefix[3:])
		}
		return parseContextValue(afterPrefix, prefix)
	}
	if strings.HasPrefix(lowerTrimmed, "ctx") {
		afterPrefix := strings.TrimSpace(trimmed[3:])
		return parseContextValue(afterPrefix, prefix)
	}
	return "", 0, false, nil
}

func parseContextValue(afterPrefix, prefix string) (string, int, bool, error) {
	if afterPrefix == "" {
		return "", 0, true, fmt.Errorf("Usage: `%s ctx <N> <question>`", prefix)
	}
	parts := strings.Fields(afterPrefix)
	count := 8
	if len(parts) > 0 {
		if n, err := strconv.Atoi(parts[0]); err == nil && n > 0 {
			count = n
			parts = parts[1:]
		}
	}
	if len(parts) == 0 {
		return "", 0, true, fmt.Errorf("Usage: `%s ctx <N> <question>`", prefix)
	}
	return strings.Join(parts, " "), count, true, nil
}

func buildPrompt(contextText, username, userID, prompt string, maxResponseChars int) string {
	base := "You are a helpful assistant in Discord. Keep responses brief, clear, and useful. " +
		"When mentioning a Discord user, always use Discord mention syntax like <@123456789>, never @username."
	if contextText != "" {
		base += "\nRecent channel context:\n" + contextText + "\n"
	}
	if maxResponseChars > 0 {
		base += "\nRespond in at most " + strconv.Itoa(maxResponseChars) + " characters."
	}
	return base + "\nCurrent user: " + username + " (ID: " + userID + ")\nCurrent request: " + prompt
}

func trimResponse(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 || len(text) <= max {
		return text
	}
	trimmed := text[:max-3]
	return strings.TrimRight(trimmed, " \n\t") + "..."
}

func extractPrompt(content, prefix, botID string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	lowerTrimmed := strings.ToLower(trimmed)
	for _, candidate := range []string{"<@" + botID + ">", "<@!" + botID + ">"} {
		if strings.HasPrefix(lowerTrimmed, strings.ToLower(candidate)) {
			trimmed = strings.TrimSpace(trimmed[len(candidate):])
			lowerTrimmed = strings.ToLower(trimmed)
			break
		}

	}
	if strings.HasPrefix(lowerTrimmed, strings.ToLower(prefix)) {
		prompt := strings.TrimSpace(trimmed[len(prefix):])
		return prompt, true
	}
	return "", false
}

func extractNamePrompt(content, username string) (string, bool) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", false
	}
	words := strings.Fields(content)
	for index, word := range words {
		clean := strings.Trim(word, ".,!?;:()[]{}<>\"'`")
		if strings.EqualFold(clean, username) {
			words = append(words[:index], words[index+1:]...)
			return strings.TrimSpace(strings.Join(words, " ")), true
		}
	}
	return "", false
}

func splitDiscordMessage(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= 2000 {
		return []string{text}
	}
	chunks := make([]string, 0)
	for len(text) > 2000 {
		cut := 2000
		for cut > 0 && cut < len(text) && text[cut] != ' ' && text[cut] != '\n' {
			cut--
		}
		if cut <= 0 {
			cut = 2000
		}
		chunks = append(chunks, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
