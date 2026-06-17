package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

type Reply struct {
	Text          string
	UsedLayers    []Layer
	ProfileID     string
	Input, Output int64
}

// Agent — накопительный агент дня 12: память из дня 11 (3 слоя) + персонализация.
// Активный профиль кладётся в ДОЛГОВРЕМЕННЫЙ слой памяти и подмешивается в System.
type Agent struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
	base      string
	mem       *Memory
	profileID string
}

func NewAgent(client anthropic.Client, model anthropic.Model, mem *Memory) *Agent {
	return &Agent{client: client, model: model, maxTokens: 1024, mem: mem,
		base: "Ты ассистент-помощник разработчика."}
}

func (a *Agent) Memory() *Memory { return a.mem }

// SetProfile делает профиль активным: его содержимое становится долговременным слоем.
func (a *Agent) SetProfile(p Profile) {
	a.profileID = p.ID
	a.mem.SetLongDoc(p.Doc)
}
func (a *Agent) ProfileID() string { return a.profileID }

// Ask: собирает запрос из выбранных слоёв памяти (включая профиль как long-term),
// диалог пишет в short-term.
func (a *Agent) Ask(ctx context.Context, query string, p Policy) (Reply, error) {
	return a.ask(ctx, query, p, true)
}

// AskOnce: разовый запрос без записи диалога (для сравнения профилей).
func (a *Agent) AskOnce(ctx context.Context, query string, p Policy) (Reply, error) {
	return a.ask(ctx, query, p, false)
}

func (a *Agent) ask(ctx context.Context, query string, p Policy, store bool) (Reply, error) {
	system, msgs, used := a.mem.Build(query, p)
	system = append([]anthropic.TextBlockParam{{Text: a.base}}, system...)

	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: a.model, MaxTokens: a.maxTokens, System: system, Messages: msgs,
	})
	if err != nil {
		return Reply{}, fmt.Errorf("agent: request failed: %w", err)
	}
	text := collectText(msg)
	if text == "" {
		return Reply{}, errors.New("agent: empty response")
	}
	if store {
		a.mem.Dialog(roleUser, query)
		a.mem.Dialog(roleAssistant, text)
	}
	return Reply{
		Text: text, UsedLayers: used, ProfileID: a.profileID,
		Input: msg.Usage.InputTokens, Output: msg.Usage.OutputTokens,
	}, nil
}

func collectText(msg *anthropic.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

func layersString(ls []Layer) string {
	if len(ls) == 0 {
		return "нет (stateless)"
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.String()
	}
	return strings.Join(parts, ", ")
}
