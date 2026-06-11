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

// Agent — отдельная сущность, инкапсулирующая логику общения с LLM
// и хранение контекста между запусками.
type Agent struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64

	store   Store           // куда сохраняем/откуда грузим историю
	history []storedMessage // весь диалог (источник правды)
}

// NewAgent создаёт агента и СРАЗУ восстанавливает историю из хранилища,
// чтобы продолжить диалог так, будто агент не выключался.
func NewAgent(client anthropic.Client, store Store) (*Agent, error) {
	history, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("agent: load history: %w", err)
	}
	return &Agent{
		client:    client,
		model:     anthropic.ModelClaudeOpus4_8,
		maxTokens: 1024,
		store:     store,
		history:   history,
	}, nil
}

// Messages — сколько реплик уже в памяти агента (для приветствия в UI).
func (a *Agent) Messages() int { return len(a.history) }

// Ask отправляет запрос в LLM с учётом всей истории, сохраняет контекст
// и возвращает текст ответа.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	// 1) реплика пользователя -> в историю
	a.history = append(a.history, storedMessage{Role: roleUser, Content: input})

	// 2) вызов модели со ВСЕМ диалогом
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  a.toParams(),
	})
	if err != nil {
		// откатываем не отвеченную реплику, чтобы не засорять контекст
		a.history = a.history[:len(a.history)-1]
		return "", fmt.Errorf("agent: request to LLM failed: %w", err)
	}

	reply := collectText(msg)
	if reply == "" {
		a.history = a.history[:len(a.history)-1]
		return "", errors.New("agent: empty response from LLM")
	}

	// 3) ответ ассистента -> в историю
	a.history = append(a.history, storedMessage{Role: roleAssistant, Content: reply})

	// 4) персистим контекст после успешного хода
	if err := a.store.Save(a.history); err != nil {
		return "", fmt.Errorf("agent: save history: %w", err)
	}

	return reply, nil
}

// toParams собирает историю в формат запроса SDK.
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

// collectText склеивает текстовые блоки ответа в одну строку.
func collectText(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}
