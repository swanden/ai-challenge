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
	Refused       bool
	Violated      []string
	RefusedBy     string // "код" | "LLM"
	CheckVerdict  string // сырой ответ LLM-проверяющего (если включён -llm-check)
	StopReason    string // почему модель остановилась: end_turn | max_tokens | ...
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
	inv       *InvariantSet
	llmCheck  bool // включает семантическую проверку (способ 2)
}

func NewAgent(client anthropic.Client, model anthropic.Model, mem *Memory) *Agent {
	return &Agent{client: client, model: model, maxTokens: 1024, mem: mem,
		base: "Ты ассистент-помощник разработчика."}
}

func (a *Agent) Memory() *Memory { return a.mem }

// SetInvariants подключает набор инвариантов (учитываются в каждом запросе).
func (a *Agent) SetInvariants(inv *InvariantSet) { a.inv = inv }
func (a *Agent) Invariants() *InvariantSet       { return a.inv }
func (a *Agent) SetLLMCheck(on bool)             { a.llmCheck = on }
func (a *Agent) SetMaxTokens(n int64) {
	if n > 0 {
		a.maxTokens = n
	}
}

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
	// enforcement инвариантов ДО основного запроса
	var llmVerdict string
	if a.inv != nil {
		if v := EnforceCode(a.inv, query); !v.Allowed {
			r := a.refuse(v)
			r.RefusedBy = "код"
			return r, nil
		}
		if a.llmCheck {
			v, err := EnforceLLM(ctx, a.client, a.model, a.inv, query)
			if err == nil {
				llmVerdict = v.Raw
				if !v.Allowed {
					r := a.refuse(v)
					r.RefusedBy = "LLM"
					r.CheckVerdict = v.Raw
					return r, nil
				}
			}
		}
	}
	system, msgs, used := a.mem.Build(query, p)
	system = append([]anthropic.TextBlockParam{{Text: a.base}}, system...)
	if a.inv != nil {
		if block := a.inv.Render(); block != "" {
			system = append(system, anthropic.TextBlockParam{Text: "[ИНВАРИАНТЫ]\n" + block})
		}
	}

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
		CheckVerdict: llmVerdict, StopReason: string(msg.StopReason),
	}, nil
}

// refuse формирует отказ агента с объяснением, какой инвариант нарушен.
func (a *Agent) refuse(v Verdict) Reply {
	via := "LLM-проверка"
	if v.ByCode {
		via = "проверка кодом"
	}
	text := "Не могу выполнить: запрос нарушает инвариант. " + v.Reason +
		"\n(" + via + "). Предложи вариант, не нарушающий инвариант, или сними ограничение явно."
	return Reply{Text: text, ProfileID: a.profileID, Refused: true, Violated: v.Violated}
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
