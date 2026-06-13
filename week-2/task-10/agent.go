package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

type Reply struct {
	Text   string
	Tokens TokenStats
}

// Agent ведёт диалог, делегируя управление контекстом сменной стратегии.
type Agent struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
	strategy  Strategy

	convInput, convOutput int64 // токены основного диалога (Opus)
}

func NewAgent(client anthropic.Client, model anthropic.Model, strategy Strategy) *Agent {
	return &Agent{client: client, model: model, maxTokens: 1024, strategy: strategy}
}

// SetStrategy — переключатель стратегий.
func (a *Agent) SetStrategy(s Strategy) { a.strategy = s }
func (a *Agent) Strategy() Strategy     { return a.strategy }

func (a *Agent) Ask(ctx context.Context, input string) (Reply, error) {
	system, msgs, err := a.strategy.Prepare(ctx, input)
	if err != nil {
		return Reply{}, fmt.Errorf("agent: strategy prepare: %w", err)
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

	reply := collectText(msg)
	if reply == "" {
		return Reply{}, errors.New("agent: empty response")
	}
	a.strategy.Commit(reply)

	a.convInput += msg.Usage.InputTokens
	a.convOutput += msg.Usage.OutputTokens

	return Reply{Text: reply, Tokens: TokenStats{
		InputTokens:  msg.Usage.InputTokens,
		OutputTokens: msg.Usage.OutputTokens,
	}}, nil
}

// Totals — токены и стоимость: диалог (Opus) + накладные стратегии (если есть).
func (a *Agent) Totals() (input, output int64, cost float64) {
	input, output = a.convInput, a.convOutput
	cost = costFor(a.model, a.convInput, a.convOutput)
	if r, ok := a.strategy.(overheadReporter); ok {
		oi, oo, om := r.Overhead()
		input += oi
		output += oo
		cost += costFor(om, oi, oo)
	}
	return
}

// Overhead — накладные стратегии (0, если стратегия их не имеет).
func (a *Agent) Overhead() (input, output int64, cost float64) {
	if r, ok := a.strategy.(overheadReporter); ok {
		oi, oo, om := r.Overhead()
		return oi, oo, costFor(om, oi, oo)
	}
	return 0, 0, 0
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
