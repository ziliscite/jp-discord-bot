package src

import (
	"context"
	"fmt"

	"github.com/sashabaranov/go-openai"
)

const deepSeekBaseURL = "https://api.deepseek.com/v1"

// Client sends prompts to the DeepSeek ChatCompletion endpoint.
type Client struct {
	inner        *openai.Client
	model        string
	systemPrompt string
	maxTokens    int
}

func NewClient(apiKey, model, systemPrompt string, maxTokens int) *Client {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = deepSeekBaseURL

	return &Client{
		inner:        openai.NewClientWithConfig(cfg),
		model:        model,
		systemPrompt: systemPrompt,
		maxTokens:    maxTokens,
	}
}

// Complete sends the userMessage to the LLM (prepended by the static system prompt) and returns the assistant's reply text.
// The caller controls cancellation/timeout via ctx.
func (c *Client) Complete(ctx context.Context, userMessage string) (string, error) {
	req := openai.ChatCompletionRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: c.systemPrompt,
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: userMessage,
			},
		},
	}

	resp, err := c.inner.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", fmt.Errorf("openai chat completion: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai returned an empty choices list")
	}

	return resp.Choices[0].Message.Content, nil
}
