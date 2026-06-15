package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

type Reply struct {
	Text       string
	UsedLayers []Layer
	Input      int64
	Output     int64
}

// Agent общается с моделью, собирая контекст из памяти по политике слоёв.
// Сам диалог агент кладёт в краткосрочную память; в рабочую и долговременную
// пишет вызывающий код ЯВНО (m.Task / m.SetProfile / m.Learn).
type Agent struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
	mem       *Memory
	system    string // постоянная инструкция (например, кратко отвечать)

	inTok, outTok int64
}

func NewAgent(client anthropic.Client, model anthropic.Model, mem *Memory) *Agent {
	return &Agent{client: client, model: model, maxTokens: 1024, mem: mem}
}

func (a *Agent) WithSystem(s string) *Agent { a.system = s; return a }
func (a *Agent) Memory() *Memory            { return a.mem }

// Ask собирает запрос из выбранных слоёв, спрашивает модель и кладёт реплику
// в краткосрочную память.
func (a *Agent) Ask(ctx context.Context, query string, p Policy) (Reply, error) {
	system, msgs, used := a.mem.Build(query, p)
	if a.system != "" {
		system = append([]anthropic.TextBlockParam{{Text: a.system}}, system...)
	}

	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		System:    system,
		Messages:  msgs,
	})
	if err != nil {
		return Reply{}, fmt.Errorf("agent: request failed: %w", err)
	}
	text := collectText(msg)
	if text == "" {
		return Reply{}, errors.New("agent: empty response")
	}

	// диалог пишем в КРАТКОСРОЧНУЮ память
	a.mem.Dialog(roleUser, query)
	a.mem.Dialog(roleAssistant, text)

	a.inTok += msg.Usage.InputTokens
	a.outTok += msg.Usage.OutputTokens
	return Reply{
		Text: text, UsedLayers: used,
		Input: msg.Usage.InputTokens, Output: msg.Usage.OutputTokens,
	}, nil
}

func (a *Agent) Totals() (input, output int64, cost float64) {
	return a.inTok, a.outTok, costFor(a.model, a.inTok, a.outTok)
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
