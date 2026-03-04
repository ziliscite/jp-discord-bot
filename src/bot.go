package src

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

const (
	// custom_id prefixes
	cidPrev   = "qprev"   // ◀ Prev navigation button
	cidNext   = "qnext"   // Next ▶ navigation button
	cidSelect = "qselect" // sentence select-menu

	// customIDSep is ASCII Unit Separator — safe delimiter inside Discord
	// custom_id fields because it never appears in ordinary user text.
	customIDSep = "\x1f"

	// Discord component limits
	maxCustomIDLen    = 100
	maxSelectOptLabel = 100
	maxSelectOpts     = 25 // hard cap on select-menu options

	// Embed accent colours
	colorList   = 0x5865f2 // Discord blurple — sentence list
	colorDetail = 0x57f287 // Discord green   — detail card

	pageSize = DefaultPageSize

	// slashCommandName is the name users type after the slash.
	slashCommandName = "query"
)

// slashCommand is the definition registered with Discord on startup.
var slashCommand = &discordgo.ApplicationCommand{
	Name:        slashCommandName,
	Description: "Search stored messages and LLM responses",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "search",
			Description: "Keywords to search for (leave empty to browse all messages)",
			Required:    false,
		},
	},
}

// job is a unit of work dispatched to a worker goroutine
type job struct {
	session *discordgo.Session
	message *discordgo.MessageCreate
}

// Bot is the top-level object that owns the Discord session and worker pool
type Bot struct {
	cfg  *Config
	llm  *Client
	jobs chan job

	// deletedMessages tracks IDs of messages deleted before LLM reply
	deletedMessages sync.Map // map[string]struct{}

	once   sync.Once
	stopCh chan struct{}

	store *Store
}

func NewBot(cfg *Config, llmClient *Client, store *Store) *Bot {
	return &Bot{
		cfg:    cfg,
		llm:    llmClient,
		jobs:   make(chan job, cfg.WorkerPoolSize*4), // buffered so the event handler never blocks
		stopCh: make(chan struct{}),
		store:  store,
	}
}

// Start opens the Discord gateway connection, registers handlers and the slash
// command, then launches the worker pool. It blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	session, err := discordgo.New("Bot " + b.cfg.DiscordToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}

	// We only need to receive message-related events
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	// Register event handlers
	session.AddHandler(b.onMessageCreate)
	session.AddHandler(b.onMessageDelete)
	session.AddHandler(b.onMessageDeleteBulk)
	session.AddHandler(b.onInteractionCreate)

	if err := session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	defer session.Close()

	// Register the /query slash command.
	if _, err = session.ApplicationCommandCreate(
		session.State.User.ID,
		b.cfg.DiscordGuildID,
		slashCommand,
	); err != nil {
		return fmt.Errorf("register slash command: %w", err)
	}

	slog.Info("discord gateway connected",
		"channel_id", b.cfg.DiscordChannelID,
		"workers", b.cfg.WorkerPoolSize,
	)

	// Launch the worker pool
	var wg sync.WaitGroup
	for i := range b.cfg.WorkerPoolSize {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			b.worker(ctx, id)
		}(i)
	}

	// Block until the caller cancels the context (e.g. OS signal)
	<-ctx.Done()
	slog.Info("shutdown signal received, draining workers …")

	close(b.jobs) // signal workers to stop after draining the queue
	wg.Wait()
	slog.Info("all workers stopped")
	return nil
}

// onMessageCreate runs in discordgo's event goroutine.
// It only handles plain messages forwarded to the LLM — slash commands arrive
// via onInteractionCreate instead.
func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself
	if m.Author == nil || m.Author.ID == s.State.User.ID {
		return
	}
	// Ignore messages from channels we don't monitor
	if m.ChannelID != b.cfg.DiscordChannelID {
		return
	}
	// Drop empty messages (e.g. attachment-only posts)
	if m.Content == "" {
		return
	}
	// Ignore if not purely japanese
	if !isPureJapanese(m.Content) {
		return
	}

	select {
	case b.jobs <- job{session: s, message: m}:
		slog.Debug("enqueued message", "message_id", m.ID, "author", m.Author.Username)
	default:
		slog.Warn("job queue full, dropping message", "message_id", m.ID, "author", m.Author.Username)
	}
}

func (b *Bot) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	if m.ChannelID != b.cfg.DiscordChannelID {
		return
	}
	b.deletedMessages.Store(m.ID, struct{}{})
	slog.Debug("message marked as deleted", "message_id", m.ID)
}

func (b *Bot) onMessageDeleteBulk(s *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	if m.ChannelID != b.cfg.DiscordChannelID {
		return
	}
	for _, id := range m.Messages {
		b.deletedMessages.Store(id, struct{}{})
	}
}

// onInteractionCreate routes all Discord interactions:
//   - ApplicationCommand → /query slash command
//   - MessageComponent   → Prev/Next buttons and sentence select-menu
func (b *Bot) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		if i.ApplicationCommandData().Name == slashCommandName {
			b.handleSlashQuery(s, i)
		}

	case discordgo.InteractionMessageComponent:
		data := i.MessageComponentData()
		switch {
		case data.ComponentType == discordgo.SelectMenuComponent && hasPrefix(data.CustomID, cidSelect):
			b.handleSelectInteraction(s, i, data)
		case data.ComponentType == discordgo.ButtonComponent && (hasPrefix(data.CustomID, cidPrev) || hasPrefix(data.CustomID, cidNext)):
			b.handleNavInteraction(s, i, data)
		}
	}
}

// ── Worker pool ───────────────────────────────────────────────────────────────

func (b *Bot) worker(ctx context.Context, id int) {
	slog.Debug("worker started", "worker_id", id)
	for j := range b.jobs {
		b.handleJob(ctx, j)
	}
	slog.Debug("worker stopped", "worker_id", id)
}

func (b *Bot) handleJob(ctx context.Context, j job) {
	msgID := j.message.ID
	log := slog.With("message_id", msgID, "author", j.message.Author.Username)
	log.Info("processing message")

	llmCtx, cancel := context.WithTimeout(ctx, b.cfg.LLMTimeout)
	defer cancel()

	reply, err := b.llm.Complete(llmCtx, j.message.Content)
	if err != nil {
		log.Error("llm error", "err", err)
		b.tryReply(j.session, j.message, msgID,
			"⚠️ Sorry, I couldn't get a response. Please try again.")
		return
	}

	if b.isDeleted(msgID) {
		log.Info("original message deleted — aborting reply")
		return
	}

	// Persist before replying so the record exists even if the Discord send fails.
	if b.store != nil {
		if _, saveErr := b.store.Save(ctx, Record{
			AuthorID:    j.message.Author.ID,
			AuthorName:  j.message.Author.Username,
			ChannelID:   j.message.ChannelID,
			Sentence:    j.message.Content,
			Explanation: reply,
		}); saveErr != nil {
			log.Warn("failed to persist record", "err", saveErr)
		}
	}

	b.tryReply(j.session, j.message, msgID, reply)
}

// tryReply sends content as a Discord reply to the original message, aborting
// if that message has since been deleted.
func (b *Bot) tryReply(
	s *discordgo.Session,
	m *discordgo.MessageCreate,
	originalID, content string,
) {
	if b.isDeleted(originalID) {
		slog.Info("message deleted before reply — aborted", "message_id", originalID)
		return
	}

	chunks := splitMessage(content, 2000)
	for i, chunk := range chunks {
		if b.isDeleted(originalID) {
			slog.Info("message deleted mid-reply — stopping",
				"message_id", originalID, "chunk", i)
			return
		}

		_, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content: chunk,
			Reference: &discordgo.MessageReference{
				MessageID: originalID,
				ChannelID: m.ChannelID,
				GuildID:   m.GuildID,
			},
		})
		if err != nil {
			if isUnknownMessage(err) {
				slog.Info("reply failed: message was deleted", "message_id", originalID)
				return
			}
			slog.Error("failed to send reply",
				"message_id", originalID, "chunk", i, "err", err)
			return
		}

		if i < len(chunks)-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}

	slog.Info("reply sent", "message_id", originalID, "chunks", len(chunks))
}

func (b *Bot) isDeleted(messageID string) bool {
	_, found := b.deletedMessages.Load(messageID)
	return found
}

// ── Utilities ─────────────────────────────────────────────────────────────────

// splitMessage breaks text into chunks of at most maxLen runes
func splitMessage(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}

		cut := maxLen
		// Walk back to the last space so we don't cut mid-word.
		for cut > maxLen/2 && runes[cut] != ' ' && runes[cut] != '\n' {
			cut--
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}

	return chunks
}

// isUnknownMessage checks whether a discordgo REST error is "Unknown Message"
// (HTTP 404, Discord code 10008), which indicates the message was deleted.
func isUnknownMessage(err error) bool {
	if err == nil {
		return false
	}
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) {
		return false
	}
	return restErr.Response != nil && restErr.Response.StatusCode == 404
}

// hasPrefix is a local alias kept so the switch in onInteractionCreate reads
// cleanly without importing strings in this file.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func isPureJapanese(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		switch {
		// Hiragana
		case unicode.In(r, unicode.Hiragana):
		// Katakana (includes full-width)
		case unicode.In(r, unicode.Katakana):
		// Kanji
		case unicode.In(r, unicode.Han):
		// Japanese punctuation & full-width symbols
		case unicode.In(r, unicode.Common):
		case unicode.IsSpace(r):
		default:
			return false
		}
	}
	return true
}
