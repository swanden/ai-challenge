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

var errContextOverflow = errors.New("context overflow")

type ContextOverflowError struct {
	RequestTokens int64
	Limit         int64
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("context overflow: request %d tokens > limit %d", e.RequestTokens, e.Limit)
}
func (e *ContextOverflowError) Is(target error) bool { return target == errContextOverflow }

// Reply — результат хода.
type Reply struct {
	Text       string
	Tokens     TokenStats
	Compressed bool
}

// Agent инкапсулирует общение с LLM, контекст, учёт токенов и сжатие истории.
type Agent struct {
	client          anthropic.Client
	model           anthropic.Model // основная модель диалога
	summarizerModel anthropic.Model // дешёвая модель для сводок
	maxTokens       int64
	contextLimit    int64

	// сжатие
	keepLast      int
	compressBatch int
	compressions  int

	systemPrompt string
	store        Store
	summary      string
	history      []storedMessage

	// раздельный учёт: диалог (дорогая модель) и сводки (дешёвая)
	convInput, convOutput int64
	sumInput, sumOutput   int64
}

type Option func(*Agent)

func WithContextLimit(n int64) Option  { return func(a *Agent) { a.contextLimit = n } }
func WithMaxTokens(n int64) Option     { return func(a *Agent) { a.maxTokens = n } }
func WithSystemPrompt(s string) Option { return func(a *Agent) { a.systemPrompt = s } }

func WithCompression(keepLast, batch int) Option {
	return func(a *Agent) {
		a.keepLast = keepLast
		a.compressBatch = batch
	}
}

// WithSummarizerModel задаёт модель для сводок (по умолчанию — Haiku).
func WithSummarizerModel(m anthropic.Model) Option {
	return func(a *Agent) { a.summarizerModel = m }
}

func NewAgent(client anthropic.Client, store Store, opts ...Option) (*Agent, error) {
	st, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("agent: load state: %w", err)
	}
	a := &Agent{
		client:          client,
		model:           anthropic.ModelClaudeOpus4_8,
		summarizerModel: anthropic.ModelClaudeHaiku4_5,
		maxTokens:       1024,
		contextLimit:    modelContextWindow,
		store:           store,
		summary:         st.Summary,
		history:         st.Messages,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

func (a *Agent) Messages() int   { return len(a.history) }
func (a *Agent) Summary() string { return a.summary }

// Totals — суммарные токены (диалог + сводки) и стоимость по тарифам обеих моделей.
func (a *Agent) Totals() (input, output int64, cost float64) {
	input = a.convInput + a.sumInput
	output = a.convOutput + a.sumOutput
	cost = costFor(a.model, a.convInput, a.convOutput) +
		costFor(a.summarizerModel, a.sumInput, a.sumOutput)
	return
}

// Overhead — накладные именно на сводки (дешёвая модель).
func (a *Agent) Overhead() (input, output int64, cost float64, compressions int) {
	return a.sumInput, a.sumOutput,
		costFor(a.summarizerModel, a.sumInput, a.sumOutput), a.compressions
}

func (a *Agent) Ask(ctx context.Context, input string) (Reply, error) {
	msgTokens, err := a.countTokens(ctx, nil, []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(input)),
	})
	if err != nil {
		return Reply{}, fmt.Errorf("agent: count message tokens: %w", err)
	}

	a.history = append(a.history, storedMessage{Role: roleUser, Content: input})
	params := a.toParams()
	system := a.systemBlocks()

	requestTokens, err := a.countTokens(ctx, system, params)
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
		System:    system,
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
	a.convInput += msg.Usage.InputTokens
	a.convOutput += msg.Usage.OutputTokens

	compressed := false
	if c, err := a.maybeCompress(ctx); err == nil {
		compressed = c
	}

	if err := a.store.Save(State{Summary: a.summary, Messages: a.history}); err != nil {
		return Reply{}, fmt.Errorf("agent: save state: %w", err)
	}

	return Reply{
		Text:       reply,
		Compressed: compressed,
		Tokens: TokenStats{
			MessageTokens: msgTokens,
			InputTokens:   msg.Usage.InputTokens,
			OutputTokens:  msg.Usage.OutputTokens,
		},
	}, nil
}

func (a *Agent) maybeCompress(ctx context.Context) (bool, error) {
	if a.keepLast <= 0 {
		return false, nil
	}
	if len(a.history) <= a.keepLast+a.compressBatch {
		return false, nil
	}
	cut := len(a.history) - a.keepLast
	older := a.history[:cut]

	newSummary, err := a.summarize(ctx, a.summary, older)
	if err != nil {
		return false, err
	}
	a.summary = newSummary
	a.history = append([]storedMessage(nil), a.history[cut:]...)
	a.compressions++
	return true, nil
}

func (a *Agent) summarize(ctx context.Context, prev string, msgs []storedMessage) (string, error) {
	var b strings.Builder
	if prev != "" {
		b.WriteString("Текущий конспект диалога:\n")
		b.WriteString(prev)
		b.WriteString("\n\n")
	}
	b.WriteString("Добавь в конспект эти сообщения:\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}

	const sys = "Ты ведёшь ПРЕДЕЛЬНО краткий конспект диалога. " +
		"Сохрани только факты, имена, числа, принятые решения и открытые вопросы. " +
		"Без форматирования, markdown, таблиц и списков — сплошной короткий текст. " +
		"Уложись примерно в 120 слов. Если конспект уже есть — обнови его, не раздувая."

	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.summarizerModel,
		MaxTokens: 256,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(b.String()))},
	})
	if err != nil {
		return "", err
	}
	a.sumInput += msg.Usage.InputTokens
	a.sumOutput += msg.Usage.OutputTokens

	out := collectText(msg)
	if out == "" {
		return "", errors.New("agent: empty summary from LLM")
	}
	return out, nil
}

func (a *Agent) systemBlocks() []anthropic.TextBlockParam {
	var blocks []anthropic.TextBlockParam
	if a.systemPrompt != "" {
		blocks = append(blocks, anthropic.TextBlockParam{Text: a.systemPrompt})
	}
	if a.summary != "" {
		blocks = append(blocks, anthropic.TextBlockParam{
			Text: "Краткое содержание предыдущей части диалога:\n" + a.summary,
		})
	}
	return blocks
}

func (a *Agent) countTokens(ctx context.Context, system []anthropic.TextBlockParam, params []anthropic.MessageParam) (int64, error) {
	p := anthropic.MessageCountTokensParams{
		Model:    a.model,
		Messages: params,
	}
	if len(system) > 0 {
		p.System = anthropic.MessageCountTokensParamsSystemUnion{OfTextBlockArray: system}
	}
	res, err := a.client.Messages.CountTokens(ctx, p)
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
