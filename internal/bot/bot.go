package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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
	replyMu            sync.RWMutex
	replies            map[string]string
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
		replies:            make(map[string]string),
	}
}

func (s *Service) Start(discordToken string) error {
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}

	dg.AddHandler(s.handleMessage)
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

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

	content := messageTextForPrompt(message.Message)
	if content == "" {
		if hydrated, err := session.ChannelMessage(message.ChannelID, message.ID); err == nil && hydrated != nil {
			message.Message = hydrated
			content = messageTextForPrompt(message.Message)
		}
	}
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
			s.rememberReply(session, message.ChannelID, block, ctxPrompt, contextText)
			time.Sleep(200 * time.Millisecond)
		}
		return
	}

	prompt, ok := extractPrompt(content, s.Prefix, session.State.User.ID)
	if !ok && s.isReplyToBot(session, message, session.State.User.ID) {
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

	replyContext := s.contextFromReply(session, message)
	s.reactToMessage(session, message, prompt)
	response, err := s.generateResponse(message, prompt, replyContext)
	if err != nil {
		_, _ = session.ChannelMessageSend(message.ChannelID, "I hit a problem: "+err.Error())
		return
	}
	for _, block := range splitDiscordMessage(response) {
		s.rememberReply(session, message.ChannelID, block, prompt, replyContext)
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
		fmt.Sprintf("running commit `%s` (%s)\nheader: %s\ndescription: %s\ntimestamp: `%s`\ngo: `%s`\nrepository: %s",
			revision, state, info.Subject, info.Body, info.Time, info.GoVersion, version.RepositoryURL))
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

func (s *Service) isReplyToBot(session *discordgo.Session, message *discordgo.MessageCreate, botID string) bool {
	referenced := s.referencedMessage(session, message)
	if referenced == nil || referenced.Author == nil {
		return false
	}
	return referenced.Author.ID == botID
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

func (s *Service) reactToMessage(session *discordgo.Session, message *discordgo.MessageCreate, prompt string) {
	if session == nil || message == nil || message.Message == nil {
		return
	}
	if len(strings.TrimSpace(prompt)) == 0 {
		return
	}
	response, err := s.Client.Generate(context.Background(), "Pick exactly one single emoji that best matches the intent of this Discord message. Only return a single emoji character, no text, no code block, no explanation. Message: "+prompt)
	if err != nil {
		return
	}
	emoji := firstEmoji(response)
	if emoji == "" {
		return
	}
	_ = session.MessageReactionAdd(message.ChannelID, message.ID, emoji)
}

func firstEmoji(text string) string {
	for _, r := range text {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if r >= 0x1F000 && r <= 0x10FFFF {
			return string(r)
		}
		if r >= 0x2600 && r <= 0x27BF {
			return string(r)
		}
	}
	return ""
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
		text := messageTextForPrompt(item)
		if text == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("<@%s> (%s): %s", item.Author.ID, item.Author.Username, text))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n"), nil
}

func messageTextForPrompt(msg *discordgo.Message) string {
	if msg == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if text := strings.TrimSpace(msg.Content); text != "" {
		parts = append(parts, text)
	}
	for _, embed := range msg.Embeds {
		if embed == nil {
			continue
		}
		pieces := make([]string, 0, 8)
		if embed.Title != "" {
			pieces = append(pieces, embed.Title)
		}
		if embed.Description != "" {
			pieces = append(pieces, embed.Description)
		}
		if embed.URL != "" {
			pieces = append(pieces, embed.URL)
		}
		if embed.Author != nil {
			if embed.Author.Name != "" {
				pieces = append(pieces, embed.Author.Name)
			}
			if embed.Author.URL != "" {
				pieces = append(pieces, embed.Author.URL)
			}
		}
		for _, field := range embed.Fields {
			if field.Name != "" {
				pieces = append(pieces, field.Name)
			}
			if field.Value != "" {
				pieces = append(pieces, field.Value)
			}
		}
		if embed.Footer != nil && embed.Footer.Text != "" {
			pieces = append(pieces, embed.Footer.Text)
		}
		if len(pieces) > 0 {
			parts = append(parts, strings.Join(pieces, "\n"))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (s *Service) contextFromReply(session *discordgo.Session, message *discordgo.MessageCreate) string {
	if session == nil || message == nil {
		return ""
	}
	seen := map[string]bool{}
	current := s.referencedMessage(session, message)
	for current != nil {
		if current.ID != "" && seen[current.ID] {
			break
		}
		if current.ID != "" {
			seen[current.ID] = true
		}
		if current.Author != nil && current.Author.ID == session.State.User.ID {
			s.replyMu.RLock()
			cachedText := s.replies[current.ID]
			s.replyMu.RUnlock()
			if cachedText != "" {
				return cachedText
			}
			text := messageTextForPrompt(current)
			if text != "" {
				return s.addPreviousUserMessage(session, current, text)
			}
		}
		if current.MessageReference != nil && current.MessageReference.MessageID != "" {
			ref, err := session.ChannelMessage(referenceChannelID(current), current.MessageReference.MessageID)
			if err == nil {
				current = ref
				continue
			}
		}
		break
	}
	return ""
}

func (s *Service) addPreviousUserMessage(session *discordgo.Session, botMessage *discordgo.Message, botText string) string {
	if session == nil || botMessage == nil {
		return botText
	}
	previous, err := session.ChannelMessages(botMessage.ChannelID, 10, botMessage.ID, "", "")
	if err != nil {
		return botText
	}
	for _, candidate := range previous {
		if candidate == nil || candidate.Author == nil || candidate.Author.Bot {
			continue
		}
		text := messageTextForPrompt(candidate)
		if text != "" {
			return fmt.Sprintf("User request: %s\nHector response: %s", text, botText)
		}
	}
	return botText
}

func (s *Service) referencedMessage(session *discordgo.Session, message *discordgo.MessageCreate) *discordgo.Message {
	if message == nil {
		return nil
	}
	if message.ReferencedMessage != nil && message.ReferencedMessage.Author != nil {
		return message.ReferencedMessage
	}
	if session == nil || message.MessageReference == nil || message.MessageReference.MessageID == "" {
		return nil
	}
	s.replyMu.RLock()
	cachedText, cached := s.replies[message.MessageReference.MessageID]
	s.replyMu.RUnlock()
	if cached {
		return &discordgo.Message{
			ID:        message.MessageReference.MessageID,
			ChannelID: referenceChannelID(message.Message),
			Author:    session.State.User,
			Content:   cachedText,
		}
	}
	referenced, err := session.ChannelMessage(referenceChannelID(message.Message), message.MessageReference.MessageID)
	if err != nil {
		return nil
	}
	return referenced
}

func referenceChannelID(message *discordgo.Message) string {
	if message != nil && message.MessageReference != nil && message.MessageReference.ChannelID != "" {
		return message.MessageReference.ChannelID
	}
	if message != nil {
		return message.ChannelID
	}
	return ""
}

func (s *Service) rememberReply(session *discordgo.Session, channelID, content, prompt, previousContext string) {
	if session == nil || session.State == nil {
		return
	}
	sent, err := session.ChannelMessageSend(channelID, content)
	if err != nil || sent == nil || sent.ID == "" {
		return
	}
	s.replyMu.Lock()
	if s.replies == nil {
		s.replies = make(map[string]string)
	}
	context := fmt.Sprintf("User request: %s\nHector response: %s", prompt, content)
	if previousContext != "" {
		context = previousContext + "\n" + context
	}
	s.replies[sent.ID] = context
	s.replyMu.Unlock()
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

func startsWithCommandPrefix(content, prefix string) bool {
	if prefix == "" {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(content))
	if lower == strings.ToLower(prefix) {
		return true
	}
	if len(lower) <= len(prefix) {
		return false
	}
	if !strings.HasPrefix(lower, strings.ToLower(prefix)) {
		return false
	}
	next := lower[len(prefix)]
	if (next >= 'a' && next <= 'z') || (next >= '0' && next <= '9') || next == '_' {
		return false
	}
	return true
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
	if startsWithCommandPrefix(trimmed, prefix) {
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
		clean = strings.TrimPrefix(clean, "@")
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
