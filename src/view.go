package src

// contains everything related to the !query command:
//   - handleQuery        — initial command handler
//   - handleSelectInteraction — user picks a sentence from the dropdown
//   - handleNavInteraction    — user clicks Prev / Next
//   - buildListEmbed    — sentence-only list embed
//   - buildDetailEmbed  — full detail card for one record
//   - buildComponents   — select menu + nav buttons
//   - buildNavCustomID / buildSelectCustomID — custom_id encoding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// handleSlashQuery is called by onInteractionCreate when the user runs /query.
// It reads the optional "search" option (empty string = browse all) and
// immediately acknowledges the interaction before fetching results.
func (b *Bot) handleSlashQuery(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Extract the optional "search" option.
	search := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "search" {
			search = strings.TrimSpace(opt.StringValue())
		}
	}

	// Defer the response so Discord doesn't time out while we query the DB.
	// The actual embed is sent as a follow-up below.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("failed to defer slash command response", "err", err)
		return
	}

	pg, err := b.store.Query(context.Background(), search, 1, pageSize)
	if err != nil {
		slog.Error("store query failed", "search", search, "err", err)
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: "⚠️ Database error — please try again.",
		})
		return
	}

	_, sendErr := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Embeds:     []*discordgo.MessageEmbed{buildListEmbed(search, pg)},
		Components: buildComponents(search, pg),
	})
	if sendErr != nil {
		slog.Error("failed to send slash query results", "err", sendErr)
	}
}

// handleQuery fetches one page from the store and edits the existing embed
// in-place via an interaction update. It is called only from handleNavInteraction.
func (b *Bot) handleQuery(
	ctx context.Context,
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	search string,
	page int,
) {
	pg, err := b.store.Query(ctx, search, page, pageSize)
	if err != nil {
		slog.Error("store query failed in nav", "search", search, "page", page, "err", err)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ Database error — please try again.",
			},
		})
		return
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{buildListEmbed(search, pg)},
			Components: buildComponents(search, pg),
		},
	})
}

// handleSelectInteraction fires when the user picks a sentence from the dropdown.
// It fetches the full record and sends a new channel message with all details.
func (b *Bot) handleSelectInteraction(
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	data discordgo.MessageComponentInteractionData,
) {
	if len(data.Values) == 0 {
		slog.Error("value is zero", "value", data.Values[0])
		return
	}

	recordID, err := strconv.ParseInt(data.Values[0], 10, 64)
	if err != nil {
		slog.Error("invalid record id in select value", "value", data.Values[0])
		return
	}

	rec, err := b.store.GetByID(context.Background(), recordID)
	if err != nil {
		msg := "⚠️ Could not find that record — it may have been deleted."
		if !errors.Is(err, ErrNotFound) {
			slog.Error("GetByID failed", "id", recordID, "err", err)
			msg = "⚠️ Database error — please try again."
		}
		// Ephemeral error keeps the list embed untouched.
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Defer so we can send a visible follow-up (Discord requires a response within 3 s).
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	_, followErr := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{buildDetailEmbed(rec)},
	})
	if followErr != nil {
		slog.Error("failed to send detail follow-up", "id", recordID, "err", followErr)
	}
}

// handleNavInteraction fires when the user clicks ◀ Prev or Next ▶.
// It re-queries the store and edits the existing list embed in-place.
func (b *Bot) handleNavInteraction(
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	data discordgo.MessageComponentInteractionData,
) {
	parts := strings.SplitN(data.CustomID, customIDSep, 3)
	if len(parts) != 3 {
		return
	}

	page, err := strconv.Atoi(parts[1])
	if err != nil || page < 1 {
		return
	}
	search := parts[2]

	pg, err := b.store.Query(context.Background(), search, page, pageSize)
	if err != nil {
		slog.Error("store query failed in nav interaction", "err", err)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ Database error — please try again.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Edit the existing embed in-place — no new message is posted.
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{buildListEmbed(search, pg)},
			Components: buildComponents(search, pg),
		},
	})
}

// buildListEmbed renders sentence text only — no explanations.
// Each entry shows the sentence, author name, and timestamp.
func buildListEmbed(search string, pg *Page) *discordgo.MessageEmbed {
	title := "📋 All messages"
	footerSearch := "all"
	if search != "" {
		title = fmt.Sprintf("🔍 Results for \"%s\"", truncate(search, 50))
		footerSearch = fmt.Sprintf("%q", search)
	}

	embed := &discordgo.MessageEmbed{
		Color: colorList,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf(
				"Page %d / %d  •  %d result(s)  •  search: %s  •  Select a sentence to see its full explanation",
				pg.CurrentPage, pg.TotalPages, pg.TotalCount, footerSearch,
			),
		},
	}

	if len(pg.Records) == 0 {
		embed.Title = "🔍 No results found"
		if search != "" {
			embed.Description = fmt.Sprintf(
				"No messages match **%s**.\nTry a different search term.", escMD(search))
		} else {
			embed.Description = "No messages have been stored yet."
		}
		return embed
	}

	embed.Title = title

	var sb strings.Builder
	for idx, r := range pg.Records {
		n := (pg.CurrentPage-1)*pg.PageSize + idx + 1
		sb.WriteString(fmt.Sprintf(
			"**%d.** %s\n*— %s · %s*\n\n",
			n,
			escMD(truncate(r.Sentence, 120)),
			escMD(r.AuthorName),
			r.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"),
		))
	}
	embed.Description = strings.TrimSpace(sb.String())
	return embed
}

// buildDetailEmbed renders a full detail card for one record:
// sentence, explanation, author, timestamp, and record ID.
func buildDetailEmbed(r *Record) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color: colorDetail,
		Title: "📄 Message Detail",
		Fields: []*discordgo.MessageEmbedField{
			{Name: "✍️ Sentence", Value: escMD(r.Sentence), Inline: false},
			{Name: "💬 Explanation", Value: escMD(truncate(r.Explanation, 1000)), Inline: false},
			{Name: "👤 Author", Value: escMD(r.AuthorName), Inline: true},
			{Name: "🕒 Sent at", Value: r.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"), Inline: true},
			{Name: "🆔 Record ID", Value: fmt.Sprintf("#%d", r.ID), Inline: true},
		},
	}
}

// buildComponents returns two Discord action rows:
//
//	Row 1 — StringSelect dropdown, one option per sentence on the current page.
//	Row 2 — ◀ Prev and Next ▶ buttons, disabled at the boundaries.
func buildComponents(search string, pg *Page) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent

	if len(pg.Records) > 0 {
		opts := make([]discordgo.SelectMenuOption, 0, len(pg.Records))
		for idx, r := range pg.Records {
			n := (pg.CurrentPage-1)*pg.PageSize + idx + 1
			prefix := fmt.Sprintf("#%d · ", n)
			label := prefix + truncate(r.Sentence, maxSelectOptLabel-len(prefix))
			desc := fmt.Sprintf("%s · %s",
				truncate(r.AuthorName, 40),
				r.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"),
			)
			opts = append(opts, discordgo.SelectMenuOption{
				Label:       label,
				Value:       strconv.FormatInt(r.ID, 10),
				Description: desc,
				Emoji:       &discordgo.ComponentEmoji{Name: "📝"},
			})
		}
		rows = append(rows, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    buildSelectCustomID(search),
					Placeholder: "Select a sentence to see its full explanation …",
					Options:     opts,
				},
			},
		})
	}

	rows = append(rows, discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "◀ Prev",
				Style:    discordgo.SecondaryButton,
				Disabled: !pg.HasPrev,
				CustomID: buildNavCustomID(cidPrev, pg.CurrentPage-1, search),
			},
			discordgo.Button{
				Label:    "Next ▶",
				Style:    discordgo.SecondaryButton,
				Disabled: !pg.HasNext,
				CustomID: buildNavCustomID(cidNext, pg.CurrentPage+1, search),
			},
		},
	})

	return rows
}

// buildNavCustomID packs direction + page + search into a ≤100-char string.
func buildNavCustomID(direction string, page int, search string) string {
	overhead := len(direction) + 1 + 10 + 1 // sep + max-10-digit page + sep
	runes := []rune(search)
	if budget := maxCustomIDLen - overhead; len(runes) > budget {
		runes = runes[:budget]
	}
	return fmt.Sprintf("%s%s%d%s%s", direction, customIDSep, page, customIDSep, string(runes))
}

// buildSelectCustomID packs cidSelect + search into a ≤100-char string.
func buildSelectCustomID(search string) string {
	overhead := len(cidSelect) + 1
	runes := []rune(search)
	if budget := maxCustomIDLen - overhead; len(runes) > budget {
		runes = runes[:budget]
	}
	return fmt.Sprintf("%s%s%s", cidSelect, customIDSep, string(runes))
}

// truncate returns s capped to maxRunes runes, appending "…" when trimmed.
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// escMD escapes Discord markdown special characters so user-supplied text is
// rendered as plain text inside embeds and messages.
func escMD(s string) string {
	return strings.NewReplacer(
		`*`, `\*`,
		`_`, `\_`,
		`~`, `\~`,
		"`", "\\`",
		`>`, `\>`,
		`|`, `\|`,
	).Replace(s)
}
