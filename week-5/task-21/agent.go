package main

import (
	"context"
	"encoding/json"
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
	llmCheck  bool      // включает семантическую проверку (способ 2)
	mcp       *MCPTools // день 17: MCP-инструменты, доступны агенту всегда (если подключены)
	lazyTools bool      // день 20 (усиление): ленивая загрузка схем тулов по серверам
}

// maxToolRounds — потолок раундов tool-use на один Ask (защита от зацикливания,
// в духе defaultReworkRounds из дня 15).
const maxToolRounds = 8

func NewAgent(client anthropic.Client, model anthropic.Model, mem *Memory) *Agent {
	return &Agent{client: client, model: model, maxTokens: 1024, mem: mem,
		base: "Ты ассистент-помощник разработчика."}
}

func (a *Agent) Memory() *Memory { return a.mem }

// SetInvariants подключает набор инвариантов (учитываются в каждом запросе).
func (a *Agent) SetInvariants(inv *InvariantSet) { a.inv = inv }
func (a *Agent) Invariants() *InvariantSet       { return a.inv }
func (a *Agent) SetLLMCheck(on bool)             { a.llmCheck = on }

// SetMCP подключает MCP-инструменты к агенту (день 17). Если задано — список
// инструментов кладётся в каждый запрос, и модель может их вызывать (tool-use).
func (a *Agent) SetMCP(m *MCPTools) { a.mcp = m }

// SetLazyTools включает ленивую загрузку схем (день 20, усиление): старт с каталога
// серверов, схемы тулов сервера подмешиваются только после load_tools.
func (a *Agent) SetLazyTools(on bool) { a.lazyTools = on }
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

	params := anthropic.MessageNewParams{
		Model: a.model, MaxTokens: a.maxTokens, System: system, Messages: msgs,
	}
	if a.mcp != nil {
		if a.lazyTools {
			params.Tools = a.mcp.CatalogTools() // только каталог серверов (ленивый режим)
		} else {
			params.Tools = a.mcp.Tools() // все тулы сразу (жадный режим, базовый день 20)
		}
	}

	// Цикл tool-use: пока модель просит инструменты — выполняем их через MCP
	// (с проверкой инвариантов на КАЖДЫЙ вызов) и возвращаем результаты.
	var inTok, outTok int64
	for round := 0; round < maxToolRounds; round++ {
		msg, err := a.client.Messages.New(ctx, params)
		if err != nil {
			return Reply{}, fmt.Errorf("agent: request failed: %w", err)
		}
		inTok += msg.Usage.InputTokens
		outTok += msg.Usage.OutputTokens

		// Нет инструментов или модель не просит их — это финальный ответ.
		if a.mcp == nil || string(msg.StopReason) != string(anthropic.StopReasonToolUse) {
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
				Input: inTok, Output: outTok,
				CheckVerdict: llmVerdict, StopReason: string(msg.StopReason),
			}, nil
		}

		// Раунд tool-use: добавляем ход ассистента (с tool_use) и выполняем вызовы.
		params.Messages = append(params.Messages, msg.ToParam())
		var results []anthropic.ContentBlockParamUnion
		for _, block := range msg.Content {
			if block.Type != "tool_use" {
				continue
			}
			tu := block.AsToolUse()
			// Мета-тул ленивой загрузки: подмешиваем схемы тулов запрошенного сервера.
			if a.lazyTools && tu.Name == LoadToolsName {
				added, summary := a.mcp.LoadServer(tu.Input)
				params.Tools = appendNewTools(params.Tools, added)
				fmt.Printf("[lazy] load_tools %s → подмешано тулов: %d\n", oneLine(string(tu.Input), 60), len(added))
				results = append(results, anthropic.NewToolResultBlock(tu.ID, summary, false))
				continue
			}
			out, isErr := a.runTool(ctx, tu.Name, tu.Input)
			results = append(results, anthropic.NewToolResultBlock(tu.ID, out, isErr))
		}
		params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
	}
	return Reply{}, errors.New("agent: превышен лимит раундов tool-use")
}

// runTool применяет инварианты к НАМЕРЕНИЮ вызвать инструмент и, если разрешено,
// выполняет его через MCP. Отказ возвращается модели как tool_result с is_error,
// чтобы агент не мог обойти инвариант через инструмент и видел причину отказа.
func (a *Agent) runTool(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	intent := name + " " + string(input)
	if a.inv != nil {
		if v := EnforceCode(a.inv, intent); !v.Allowed {
			fmt.Printf("[инвариант/код] вызов %s отклонён: %s\n", name, v.Reason)
			return "ОТКАЗ (инвариант, проверка кодом): " + v.Reason, true
		}
		if a.llmCheck {
			if v, err := EnforceLLM(ctx, a.client, a.model, a.inv, intent); err == nil && !v.Allowed {
				fmt.Printf("[инвариант/LLM] вызов %s отклонён: %s\n", name, v.Reason)
				return "ОТКАЗ (инвариант, LLM): " + v.Reason, true
			}
		}
	}
	fmt.Printf("[agent→mcp] %s %s\n", name, oneLine(string(input), 160))
	out, isErr := a.mcp.Call(ctx, name, input)
	fmt.Printf("[mcp→agent] %s\n", oneLine(out, 200))
	return out, isErr
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
