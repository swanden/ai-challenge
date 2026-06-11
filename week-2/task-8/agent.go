package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// errContextOverflow — сентинел для errors.Is.
var errContextOverflow = errors.New("context overflow")

// ContextOverflowError несёт числа, чтобы отчёт мог показать, где именно сломалось.
type ContextOverflowError struct {
	RequestTokens int64
	Limit         int64
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("context overflow: request %d tokens > limit %d", e.RequestTokens, e.Limit)
}

// Is позволяет ловить ошибку через errors.Is(err, errContextOverflow).
func (e *ContextOverflowError) Is(target error) bool { return target == errContextOverflow }

// Reply — результат хода: текст + статистика по токенам.
type Reply struct {
	Text   string
	Tokens TokenStats
}

// Agent инкапсулирует общение с LLM, контекст и его учёт по токенам.
type Agent struct {
	client       anthropic.Client
	model        anthropic.Model
	maxTokens    int64
	contextLimit int64

	store   Store
	history []storedMessage

	totalInput  int64
	totalOutput int64
}

// Option — функциональная опция конфигурации агента.
type Option func(*Agent)

// WithContextLimit задаёт лимит контекста (для демонстрации переполнения).
func WithContextLimit(n int64) Option { return func(a *Agent) { a.contextLimit = n } }

// WithMaxTokens ограничивает длину ответа модели.
func WithMaxTokens(n int64) Option { return func(a *Agent) { a.maxTokens = n } }

func NewAgent(client anthropic.Client, store Store, opts ...Option) (*Agent, error) {
	history, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("agent: load history: %w", err)
	}
	a := &Agent{
		client:       client,
		model:        anthropic.ModelClaudeOpus4_8,
		maxTokens:    1024,
		contextLimit: modelContextWindow,
		store:        store,
		history:      history,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

func (a *Agent) Messages() int { return len(a.history) }

// SessionTokens — накопленные input/output за сессию и их стоимость.
func (a *Agent) SessionTokens() (input, output int64, usd float64) {
	return a.totalInput, a.totalOutput, costUSD(a.totalInput, a.totalOutput)
}

// Ask отправляет запрос в LLM, считает токены и сохраняет контекст.
func (a *Agent) Ask(ctx context.Context, input string) (Reply, error) {
	msgTokens, err := a.countTokens(ctx, []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(input)),
	})
	if err != nil {
		return Reply{}, fmt.Errorf("agent: count message tokens: %w", err)
	}

	a.history = append(a.history, storedMessage{Role: roleUser, Content: input})
	params := a.toParams()

	requestTokens, err := a.countTokens(ctx, params)
	if err != nil {
		a.history = a.history[:len(a.history)-1]
		return Reply{}, fmt.Errorf("agent: count request tokens: %w", err)
	}

	if requestTokens > a.contextLimit {
		a.history = a.history[:len(a.history)-1]
		return Reply{}, &ContextOverflowError{RequestTokens: requestTokens, Limit: a.contextLimit}
	}

	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  params,
	})
	if err != nil {
		a.history = a.history[:len(a.history)-1]
		return Reply{}, fmt.Errorf("agent: request to LLM failed: %w", err)
	}

	reply := collectText(msg)
	if reply == "" {
		a.history = a.history[:len(a.history)-1]
		return Reply{}, errors.New("agent: empty response from LLM")
	}

	a.history = append(a.history, storedMessage{Role: roleAssistant, Content: reply})
	if err := a.store.Save(a.history); err != nil {
		return Reply{}, fmt.Errorf("agent: save history: %w", err)
	}

	a.totalInput += msg.Usage.InputTokens
	a.totalOutput += msg.Usage.OutputTokens

	return Reply{
		Text: reply,
		Tokens: TokenStats{
			MessageTokens: msgTokens,
			InputTokens:   msg.Usage.InputTokens,
			OutputTokens:  msg.Usage.OutputTokens,
		},
	}, nil
}

func (a *Agent) countTokens(ctx context.Context, params []anthropic.MessageParam) (int64, error) {
	res, err := a.client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model:    a.model,
		Messages: params,
	})
	if err != nil {
		return 0, err
	}
	return res.InputTokens, nil
}

func (a *Agent) toParams() []anthropic.MessageParam {
	params := make([]anthropic.MessageParam, 0, len(a.history))
	for _, m := range a.history {
		block := anthropic.NewTextBlock(m.Content)
		if m.Role == roleAssistant {
			params = append(params, anthropic.NewAssistantMessage(block))
			continue
		}
		params = append(params, anthropic.NewUserMessage(block))
	}
	return params
}

func collectText(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}
