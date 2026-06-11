package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Agent — отдельная сущность, инкапсулирующая всю логику общения с LLM.
// Снаружи (из CLI) он выглядит как чёрный ящик с одним методом Ask:
// "дай строку — получи ответ". Как именно строится запрос, какая модель,
// сколько токенов и как копится контекст диалога — детали агента.
type Agent struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64

	// history хранит весь диалог (user + assistant), чтобы агент
	// "помнил" предыдущие реплики и отвечал с учётом контекста.
	history []anthropic.MessageParam
}

// NewAgent — конструктор. Тут собираем зависимости и дефолтную конфигурацию.
func NewAgent(client anthropic.Client) *Agent {
	return &Agent{
		client:    client,
		model:     anthropic.ModelClaudeOpus4_8,
		maxTokens: 1024,
	}
}

// Ask отправляет запрос пользователя в LLM и возвращает текст ответа.
// Вся логика запроса/ответа спрятана здесь — это и есть требование дня 6.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	// 1) добавляем реплику пользователя в историю
	a.history = append(a.history, anthropic.NewUserMessage(anthropic.NewTextBlock(input)))

	// 2) вызываем модель, передавая ВЕСЬ диалог
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  a.history,
	})
	if err != nil {
		return "", fmt.Errorf("agent: request to LLM failed: %w", err)
	}

	// 3) собираем текст ответа из блоков контента
	reply := collectText(msg)
	if reply == "" {
		return "", errors.New("agent: empty response from LLM")
	}

	// 4) сохраняем ответ ассистента в историю, чтобы держать контекст
	a.history = append(a.history, anthropic.NewAssistantMessage(anthropic.NewTextBlock(reply)))

	return reply, nil
}

// collectText склеивает все текстовые блоки ответа в одну строку.
func collectText(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}
