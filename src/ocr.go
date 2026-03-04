package src

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/otiai10/gosseract/v2"
)

const (
	ocrCommandName = "ocr"

	// maxOCRImageBytes is a 20 MB guard against absurdly large uploads.
	maxOCRImageBytes = 20 * 1024 * 1024

	// codeBlockOverhead accounts for the opening/closing ``` fences + language
	// tag and a newline: "```\n" + "\n```" = 8 chars.
	codeBlockOverhead = 8

	// maxCodeBlockText is the maximum raw OCR text that fits in one 2 000-char
	// Discord message once the code-block fences are included.
	maxCodeBlockText = 2000 - codeBlockOverhead
)

// ocrCommand is the ApplicationCommand definition registered with Discord.
var ocrCommand = &discordgo.ApplicationCommand{
	Name:        ocrCommandName,
	Description: "Extract text from an image using OCR",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type:        discordgo.ApplicationCommandOptionAttachment,
			Name:        "image",
			Description: "The image to extract Japanese text from (PNG, JPEG, WEBP, BMP, TIFF)",
			Required:    true,
		},
	},
}

// handleSlashOCR is the entry point called from onInteractionCreate.
func (b *Bot) handleSlashOCR(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Acknowledge within Discord's 3-second window; actual work follows async.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("ocr: failed to defer response", "err", err)
		return
	}

	text, err := b.runOCR(i)
	if err != nil {
		slog.Warn("ocr: processing failed", "err", err)
		b.ocrFollowup(s, i, fmt.Sprintf("❌ %s", err.Error()))
		return
	}

	if strings.TrimSpace(text) == "" {
		b.ocrFollowup(s, i, "ℹ️ No Japanese text was detected in that image.")
		return
	}

	// Send result(s) inside code blocks, splitting if longer than one message.
	runes := []rune(text)
	for start := 0; start < len(runes); start += maxCodeBlockText {
		end := start + maxCodeBlockText
		if end > len(runes) {
			end = len(runes)
		}
		chunk := string(runes[start:end])
		b.ocrFollowup(s, i, fmt.Sprintf("```\n%s\n```", chunk))
	}
}

// runOCR pulls the attachment, downloads it, and returns the Tesseract output.
func (b *Bot) runOCR(i *discordgo.InteractionCreate) (string, error) {
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return "", fmt.Errorf("please attach an image when using `/ocr`")
	}

	attachmentID := opts[0].Value.(string)
	resolved := i.ApplicationCommandData().Resolved
	if resolved == nil || resolved.Attachments == nil {
		return "", fmt.Errorf("could not resolve the attachment — please try again")
	}

	attachment, ok := resolved.Attachments[attachmentID]
	if !ok {
		return "", fmt.Errorf("could not find the attachment — please try again")
	}

	ct := strings.ToLower(attachment.ContentType)
	if !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf(
			"**%s** is not an image (detected type: `%s`). Please upload a PNG, JPEG, WEBP, BMP, or TIFF file",
			attachment.Filename, attachment.ContentType,
		)
	}

	// Gosseract / Tesseract does not support GIF natively.
	if strings.Contains(ct, "gif") {
		return "", fmt.Errorf("GIF files are not supported — please convert to PNG or JPEG first")
	}

	slog.Info("ocr: processing attachment",
		"filename", attachment.Filename,
		"content_type", attachment.ContentType,
		"size", attachment.Size,
	)

	imageBytes, err := downloadImage(attachment.URL, maxOCRImageBytes)
	if err != nil {
		return "", fmt.Errorf("failed to download the image: %w", err)
	}

	raw, err := runJapaneseTesseract(imageBytes)
	if err != nil {
		return "", err
	}

	cleaned := filterJapanese(raw)
	slog.Info("ocr: extraction complete",
		"raw_chars", len([]rune(raw)),
		"filtered_chars", len([]rune(cleaned)),
	)

	return cleaned, nil
}

// runJapaneseTesseract configures a gosseract client specifically for Japanese
// and returns the raw extracted text.
//
// Language notes:
//   - "jpn"      covers horizontal Japanese text (most common).
//   - "jpn_vert" covers vertical Japanese text (manga, novels).
//     Using both via "jpn+jpn_vert" lets Tesseract pick the best fit.
//
// PSM 6 (uniform block of text) works well for clean document images.
// PSM 3 (fully automatic) is more robust for mixed or unclear layouts.
func runJapaneseTesseract(imageBytes []byte) (string, error) {
	client := gosseract.NewClient()
	defer client.Close()

	// Japanese horizontal + vertical scripts.
	if err := client.SetLanguage("jpn", "jpn_vert"); err != nil {
		return "", fmt.Errorf("OCR failed to set language: %w", err)
	}

	// PSM 6: treat the image as a single uniform block of text.
	// Change to "3" for fully automatic page segmentation on complex layouts.
	if err := client.SetPageSegMode(gosseract.PSM_SINGLE_BLOCK); err != nil {
		return "", fmt.Errorf("OCR failed to set page segmentation mode: %w", err)
	}

	// Preserve spacing between words — important for Japanese readability.
	if err := client.SetVariable("preserve_interword_spaces", "1"); err != nil {
		return "", fmt.Errorf("OCR failed to set variable: %w", err)
	}

	if err := client.SetImageFromBytes(imageBytes); err != nil {
		return "", fmt.Errorf("OCR failed to load the image: %w", err)
	}

	text, err := client.Text()
	if err != nil {
		return "", fmt.Errorf("OCR failed to extract text: %w", err)
	}

	return text, nil
}

// filterJapanese removes characters that are not part of Japanese script,
func filterJapanese(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	for _, r := range s {
		if isJapaneseChar(r) {
			sb.WriteRune(r)
		}
	}

	// Collapse runs of blank lines left by the filtering pass.
	return collapseBlankLines(sb.String())
}

// isJapaneseChar reports whether r belongs to a Japanese-script Unicode block
// or is a newline that preserves line structure.
func isJapaneseChar(r rune) bool {
	switch {
	case r == '\n':
		return true

	// Hiragana
	case r >= 0x3040 && r <= 0x309F:
		return true

	// Katakana
	case r >= 0x30A0 && r <= 0x30FF:
		return true

	// CJK Unified Ideographs (kanji)
	case unicode.In(r, unicode.Han):
		return true

	// Japanese punctuation & CJK symbols (。、「」…)
	case r >= 0x3000 && r <= 0x303F:
		return true

	// Full-width Latin & half-width Katakana (common in Japanese text)
	case r >= 0xFF00 && r <= 0xFFEF:
		return true

	// CJK Compatibility Ideographs
	case r >= 0xF900 && r <= 0xFAFF:
		return true

	// CJK Unified Ideographs Extension A
	case r >= 0x3400 && r <= 0x4DBF:
		return true

	// Katakana Phonetic Extensions
	case r >= 0x31F0 && r <= 0x31FF:
		return true

	default:
		return false
	}
}

// collapseBlankLines reduces any run of two or more consecutive blank lines
// to a single blank line, keeping the output tidy after filtering.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blankRun := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blankRun++
			if blankRun <= 1 {
				out = append(out, line)
			}
		} else {
			blankRun = 0
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// downloadImage fetches URL and returns the body bytes, capped at maxBytes.
func downloadImage(url string, maxBytes int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("image exceeds the %d MB size limit", maxBytes/(1024*1024))
	}

	return data, nil
}

// ocrFollowup sends a follow-up message for the deferred /ocr interaction.
func (b *Bot) ocrFollowup(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
	}); err != nil {
		slog.Error("ocr: failed to send follow-up", "err", err)
	}
}
