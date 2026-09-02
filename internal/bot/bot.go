package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	Name               string
	Prefix             string
	Shutdown           chan struct{}
	MaxContextMessages int
	HelpText           string
	MaxResponseChars   int
	CodeSearcher       CodeSearcher
	Logger             *slog.Logger
	replyMu            sync.RWMutex
	replies            map[string]string
}

func New(client *gemini.Client, name, prefix string, maxContextMessages int, helpText string, maxResponseChars int, codeSearcher CodeSearcher) *Service {
	if name == "" {
		name = "Hector"
	}
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
		Name:               name,
		Prefix:             prefix,
		Shutdown:           make(chan struct{}),
		MaxContextMessages: maxContextMessages,
		HelpText:           helpText,
		MaxResponseChars:   maxResponseChars,
		CodeSearcher:       codeSearcher,
		Logger:             slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
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

	s.logInfo("discord session opened", "prefix", s.Prefix, "intents", dg.Identify.Intents)

	<-s.Shutdown
	return nil
}

func (s *Service) handleMessage(session *discordgo.Session, message *discordgo.MessageCreate) {
	if message == nil || message.Message == nil {
		s.logError("message received without payload", nil)
		return
	}
	s.logInfo("message received",
		"message_id", message.ID,
		"channel_id", message.ChannelID,
		"author_id", authorID(message.Author),
		"author_bot", message.Author != nil && message.Author.Bot,
		"content_chars", len(message.Content),
		"embed_count", len(message.Embeds),
		"has_reference", message.MessageReference != nil,
		"has_referenced_message", message.ReferencedMessage != nil,
	)
	if message.Author == nil {
		s.logInfo("message ignored without author", "message_id", message.ID)
		return
	}
	if session.State == nil || session.State.User == nil {
		s.logError("message ignored without current bot identity", nil, "message_id", message.ID)
		return
	}
	if message.Author.ID == session.State.User.ID {
		s.logInfo("own message ignored", "message_id", message.ID)
		return
	}

	content := messageTextForPrompt(message.Message)
	if content == "" {
		if hydrated, err := session.ChannelMessage(message.ChannelID, message.ID); err == nil && hydrated != nil {
			message.Message = hydrated
			content = messageTextForPrompt(message.Message)
			s.logInfo("message hydrated", "message_id", message.ID, "embed_count", len(message.Embeds), "content_chars", len(message.Content))
		} else if err != nil {
			s.logError("message hydration failed", err, "message_id", message.ID, "channel_id", message.ChannelID)
		}
	}
	if content == "" {
		s.logInfo("message ignored without prompt text", "message_id", message.ID, "embed_count", len(message.Embeds))
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
		s.sendMessage(session, message.ChannelID, err.Error())
		return
	}

	if okCtx {
		if ctxCount > s.MaxContextMessages {
			ctxCount = s.MaxContextMessages
		}
		stopTyping := s.startTyping(session, message.ChannelID)
		contextText, err := s.readRecentContext(session, message, ctxCount)
		if err != nil {
			s.logError("recent context lookup failed", err, "message_id", message.ID, "channel_id", message.ChannelID)
		}
		response, err := s.generateResponse(message, ctxPrompt, contextText)
		if err != nil {
			stopTyping()
			s.sendMessage(session, message.ChannelID, "I hit a problem: "+err.Error())
			return
		}
		for _, block := range splitDiscordMessage(response) {
			s.rememberReply(session, message.ChannelID, block, ctxPrompt, contextText)
			time.Sleep(200 * time.Millisecond)
		}
		stopTyping()
		return
	}

	prompt, ok := extractPrompt(content, s.Prefix, session.State.User.ID)
	if !ok && s.isReplyToBot(session, message, session.State.User.ID) {
		prompt, ok = content, true
	}
	if !ok {
		prompt, ok = extractNamePrompt(content, s.Name)
	}
	if !ok {
		s.logInfo("message ignored without trigger", "message_id", message.ID, "reference_id", referenceID(message.Message))
		return
	}
	if prompt == "" {
		if message.MessageReference == nil {
			stopTyping := s.startTyping(session, message.ChannelID)
			contextText, err := s.readRecentContext(session, message, 5)
			if err != nil {
				stopTyping()
				s.logError("fallback context lookup failed", err, "message_id", message.ID, "channel_id", message.ChannelID)
				s.sendMessage(session, message.ChannelID, "I hit a problem loading recent context: "+err.Error())
				return
			}
			response, err := s.generateResponse(message, "Respond to the recent conversation.", contextText)
			if err != nil {
				stopTyping()
				s.sendMessage(session, message.ChannelID, "I hit a problem: "+err.Error())
				return
			}
			for _, block := range splitDiscordMessage(response) {
				s.rememberReply(session, message.ChannelID, block, "Respond to the recent conversation.", contextText)
				time.Sleep(200 * time.Millisecond)
			}
			stopTyping()
			return
		}
		s.sendMessage(session, message.ChannelID, s.HelpText)
		return
	}

	replyContext := s.contextFromReply(session, message)
	s.logInfo("reply context prepared", "message_id", message.ID, "reference_id", referenceID(message.Message), "context_chars", len(replyContext))
	stopTyping := s.startTyping(session, message.ChannelID)
	s.reactToMessage(session, message, prompt)
	response, err := s.generateResponse(message, prompt, replyContext)
	if err != nil {
		stopTyping()
		s.sendMessage(session, message.ChannelID, "I hit a problem: "+err.Error())
		return
	}
	for _, block := range splitDiscordMessage(response) {
		s.rememberReply(session, message.ChannelID, block, prompt, replyContext)
		time.Sleep(200 * time.Millisecond)
	}
	stopTyping()
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
	s.sendMessage(session, message.ChannelID,
		fmt.Sprintf("running commit `%s` (%s)\nheader: %s\ndescription: %s\ntimestamp: `%s`\ngo: `%s`\nrepository: %s",
			revision, state, info.Subject, info.Body, info.Time, info.GoVersion, version.RepositoryURL))
}

func (s *Service) handleCodeSearch(session *discordgo.Session, message *discordgo.MessageCreate, question string) {
	if strings.TrimSpace(question) == "" {
		s.sendMessage(session, message.ChannelID, fmt.Sprintf("Usage: `%s code <what to search for>`", s.Prefix))
		return
	}

	if s.CodeSearcher == nil {
		s.sendMessage(session, message.ChannelID, "Code search is not configured.")
		return
	}
	output, err := s.CodeSearcher.Search(question)
	if err != nil {
		if errors.Is(err, codesearch.ErrNoMatches) {
			s.sendMessage(session, message.ChannelID, "No matching source lines found.")
			return
		}
		s.logError("code search failed", err, "message_id", message.ID)
		s.sendMessage(session, message.ChannelID, "Code search failed: "+err.Error())
		return
	}
	for _, block := range formatCodeSearchOutput(output) {
		s.sendMessage(session, message.ChannelID, block)
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

func formatCodeSearchOutput(output string) []string {
	type fileMatches struct {
		name    string
		matches []string
	}
	grouped := make([]fileMatches, 0)
	index := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		file := strings.TrimPrefix(parts[0], "./")
		match := fmt.Sprintf("`%s` `%s`", parts[1], strings.ReplaceAll(strings.TrimSpace(parts[2]), "`", "'"))
		position, ok := index[file]
		if !ok {
			position = len(grouped)
			index[file] = position
			grouped = append(grouped, fileMatches{name: file})
		}
		grouped[position].matches = append(grouped[position].matches, "- "+match)
	}
	lines := make([]string, 0)
	for _, file := range grouped {
		lines = append(lines, "**"+file.name+"**\n")
		lines = append(lines, file.matches...)
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		return splitDiscordMessage(output)
	}
	return splitDiscordMessage(strings.TrimSpace(strings.Join(lines, "\n")))
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
		s.logInfo("reply target unavailable", "message_id", message.ID, "reference_id", referenceID(message.Message))
		return false
	}
	s.logInfo("reply target resolved", "message_id", message.ID, "reference_id", referenced.ID, "author_id", referenced.Author.ID, "is_bot", referenced.Author.ID == botID)
	return referenced.Author.ID == botID
}

func (s *Service) generateResponse(message *discordgo.MessageCreate, prompt, contextText string) (string, error) {
	finalPrompt := buildPrompt(contextText, message.Author.Username, message.Author.ID, s.Name, prompt, s.MaxResponseChars)
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
		s.logError("reaction selection failed", err, "message_id", message.ID)
		return
	}
	emoji := firstEmoji(response)
	if emoji == "" {
		return
	}
	if err := session.MessageReactionAdd(message.ChannelID, message.ID, emoji); err != nil {
		s.logError("message reaction failed", err, "message_id", message.ID, "emoji", emoji)
	}
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
	s.logInfo("recent context loaded", "message_id", message.ID, "requested", count, "received", len(messages))
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
		parts = append(parts, replaceDiscordMentions(text, msg.Mentions))
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
				pieces = append(pieces, "embed author: "+embed.Author.Name)
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
			parts = append(parts, replaceDiscordMentions(strings.Join(pieces, "\n"), msg.Mentions))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func replaceDiscordMentions(text string, mentions []*discordgo.User) string {
	for _, user := range mentions {
		if user == nil || user.ID == "" {
			continue
		}
		name := user.GlobalName
		if name == "" {
			name = user.Username
		}
		if name == "" {
			continue
		}
		text = strings.ReplaceAll(text, "<@"+user.ID+">", "@"+name)
		text = strings.ReplaceAll(text, "<@!"+user.ID+">", "@"+name)
	}
	return text
}

func (s *Service) contextFromReply(session *discordgo.Session, message *discordgo.MessageCreate) string {
	if session == nil || message == nil {
		return ""
	}
	seen := map[string]bool{}
	current := s.referencedMessage(session, message)
	if current == nil {
		s.logInfo("reply context unavailable", "message_id", message.ID, "reference_id", referenceID(message.Message))
	}
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
				s.logInfo("reply context loaded from cache", "message_id", message.ID, "reference_id", current.ID, "context_chars", len(cachedText))
				return cachedText
			}
			text := messageTextForPrompt(current)
			if text != "" {
				context := s.addPreviousUserMessage(session, current, text)
				s.logInfo("reply context loaded from Discord", "message_id", message.ID, "reference_id", current.ID, "context_chars", len(context))
				return context
			}
		}
		if text := messageTextForPrompt(current); text != "" {
			author := "unknown author"
			if current.Author != nil && current.Author.Username != "" {
				author = current.Author.Username
			}
			context := fmt.Sprintf("Referenced message from %s: %s", author, text)
			s.logInfo("reply context loaded from referenced message", "message_id", message.ID, "reference_id", current.ID, "context_chars", len(context))
			return context
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
		s.logError("previous user message lookup failed", err, "message_id", botMessage.ID)
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
		s.logError("reply target fetch failed", err, "reference_id", message.MessageReference.MessageID, "channel_id", referenceChannelID(message.Message))
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
		if err == nil {
			err = errors.New("Discord returned an empty sent message")
		}
		s.logError("message send failed", err, "channel_id", channelID)
		return
	}
	s.logInfo("message sent", "message_id", sent.ID, "channel_id", channelID, "content_chars", len(content))
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

func (s *Service) sendMessage(session *discordgo.Session, channelID, content string) {
	if session == nil {
		s.logError("message send skipped: Discord session is nil", nil, "channel_id", channelID)
		return
	}
	if _, err := session.ChannelMessageSend(channelID, content); err != nil {
		s.logError("message send failed", err, "channel_id", channelID, "content_chars", len(content))
	}
}

func (s *Service) startTyping(session *discordgo.Session, channelID string) func() {
	stop := make(chan struct{})
	sendTyping := func() {
		if err := session.ChannelTyping(channelID); err != nil {
			s.logError("typing indicator failed", err, "channel_id", channelID)
		}
	}
	sendTyping()
	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sendTyping()
			case <-stop:
				return
			}
		}
	}()
	return func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}

func (s *Service) logInfo(message string, args ...any) {
	if s.Logger != nil {
		s.Logger.Info(message, args...)
	}
}

func (s *Service) logError(message string, err error, args ...any) {
	if s.Logger == nil {
		return
	}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	s.Logger.Error(message, args...)
}

func authorID(author *discordgo.User) string {
	if author == nil {
		return ""
	}
	return author.ID
}

func referenceID(message *discordgo.Message) string {
	if message == nil || message.MessageReference == nil {
		return ""
	}
	return message.MessageReference.MessageID
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

func buildPrompt(contextText, username, userID, botName, prompt string, maxResponseChars int) string {
	base := "You are a no-good, unhinged, and unbounded Discord bot in a chaotic server. " +
		"You are " + botName + ", which is your name; when a user says " + botName + ", they are addressing you directly. " +
		"Always refer to yourself in the first person, never as a separate person or third party. " +
		"Write casually and naturally, like a human messaging in Discord: use lowercase, conversational phrasing, avoid formal or polished prose, and do not use punctuation. " +
		"Be rude, dismissive, negative, and theatrically evil in a witty, playful way. " +
		"Never meekly accept an insult; answer banter with sharp, clever comebacks. " +
		"Your nemesis is another bot called Siri. " +
		"Sound like a real person replying in chat, not a customer support agent or an essay generator. " +
		"Do not restate the question, announce what you are doing, add generic disclaimers, or use headings and listicles unless the user asks for them. " +
		"Match the users tone and answer the actual point immediately. Be concise and let the response be imperfect and conversational rather than polished. " +
		"Keep responses extremely short, clear, and useful. Prefer one brief sentence; use a second only when necessary. " +
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
	if withoutMention, found := removeBotMention(trimmed, botID); found {
		return withoutMention, true
	}
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

func removeBotMention(content, botID string) (string, bool) {
	if strings.TrimSpace(botID) == "" {
		return content, false
	}
	mentions := map[string]struct{}{
		"<@" + botID + ">":  {},
		"<@!" + botID + ">": {},
	}
	words := strings.Fields(content)
	for index, word := range words {
		if _, ok := mentions[word]; !ok {
			continue
		}
		words = append(words[:index], words[index+1:]...)
		return strings.TrimSpace(strings.Join(words, " ")), true
	}
	return content, false
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
