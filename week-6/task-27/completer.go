package main

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
)

// День 27: тумблер облако↔локаль.
//
// Все «текстовые» точки вызова агента (мини-чат, обновление памяти задачи, рой
// ревьюеров, LLM-проверка инвариантов) — это один и тот же паттерн: system-промпт
// + сообщения → текст. На дне 25 они звали КОНКРЕТНЫЙ anthropic.Client напрямую;
// это привязывало «агентские флоу» к облаку. Здесь мы вводим шов Completer: тот же
// вход/выход, но бэкенд сменный. Тумблер -backend cloud|local просто кладёт в агента
// (и в рой) нужную реализацию — и те же самые флоу едут на локальной модели.
//
// Границы шва (осознанно, см. README):
//   - Completer покрывает ТЕКСТОВЫЕ флоу: chatAnswer, updateTaskMemory, Swarm, EnforceLLM.
//   - MCP-цикл tool-use (Agent.ask) остаётся облачным: он завязан на tool_use/tool_result
//     формат SDK, а надёжный tool-calling на 7B-локали — отдельная задача (не дня 27).
//   - Облачный RAG-реранк/rewrite тоже пока облачные; на локали чат идёт по bi-encoder
//     + порогу (без реранка), что и делает флоу полностью локальным.

// Msg — одно сообщение диалога, независимое от бэкенда (роль + текст).
type Msg struct {
	Role string // roleUser | roleAssistant
	Text string
}

// GenUsage — расход токенов одного вызова (у облака — из ответа API; у локали —
// из счётчиков Ollama). Названо GenUsage, чтобы не путать с anthropic.Usage.
type GenUsage struct {
	Input  int64
	Output int64
}

// CompleteOpts — параметры вызова (общие для обоих бэкендов). 0 = дефолт бэкенда.
type CompleteOpts struct {
	MaxTokens   int64
	Temperature float64
}

// Completer — сменный генератор текста. Одна реализация ходит в облако (Anthropic),
// другая — в локальную модель (Ollama). Вход/выход одинаковы, поэтому вызывающий код
// (chat, swarm, enforce) не знает и не должен знать, какой бэкенд под ним.
type Completer interface {
	Complete(ctx context.Context, system string, msgs []Msg, opt CompleteOpts) (string, GenUsage, error)
	Name() string
}

// ── Облачный бэкенд: обёртка над anthropic.Client (тот же вызов, что был на дне 25) ──

type cloudCompleter struct {
	client anthropic.Client
	model  anthropic.Model
}

func newCloudCompleter(client anthropic.Client, model anthropic.Model) *cloudCompleter {
	return &cloudCompleter{client: client, model: model}
}

func (c *cloudCompleter) Name() string { return "cloud/" + string(c.model) }

func (c *cloudCompleter) Complete(ctx context.Context, system string, msgs []Msg, opt CompleteOpts) (string, GenUsage, error) {
	maxTok := opt.MaxTokens
	if maxTok <= 0 {
		maxTok = 700
	}
	params := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: maxTok,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  toAnthropicMessages(msgs),
	}
	if opt.Temperature > 0 {
		params.Temperature = anthropic.Float(opt.Temperature)
	}
	msg, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return "", GenUsage{}, err
	}
	return collectText(msg), GenUsage{Input: msg.Usage.InputTokens, Output: msg.Usage.OutputTokens}, nil
}

// toAnthropicMessages переводит бэкенд-независимые Msg в сообщения SDK.
func toAnthropicMessages(msgs []Msg) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == roleAssistant {
			out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text)))
		} else {
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
		}
	}
	return out
}

// ── Локальный бэкенд: обёртка над LocalLLM (день 26) ──

type localCompleter struct {
	llm *LocalLLM
}

func newLocalCompleter(llm *LocalLLM) *localCompleter { return &localCompleter{llm: llm} }

func (c *localCompleter) Name() string { return c.llm.Name() }

func (c *localCompleter) Complete(ctx context.Context, system string, msgs []Msg, opt CompleteOpts) (string, GenUsage, error) {
	// Переносим потолок токенов/температуру запроса на клиент локальной модели.
	if opt.MaxTokens > 0 {
		c.llm.NumPredict = int(opt.MaxTokens)
	}
	if opt.Temperature > 0 {
		c.llm.Temp = opt.Temperature
	}
	// Ollama /api/chat хочет (system, история, последний user). Последнее сообщение —
	// текущий ход пользователя, предыдущие — история.
	history, user := splitLastUser(msgs)
	text, stats, err := c.llm.Chat(ctx, system, history, user)
	if err != nil {
		return "", GenUsage{}, err
	}
	return text, GenUsage{Input: int64(stats.PromptTokens), Output: int64(stats.EvalTokens)}, nil
}

// splitLastUser делит сообщения на историю (все, кроме последнего) и текст последнего
// пользовательского хода. Роли конвертируются в формат Ollama ("user"/"assistant").
func splitLastUser(msgs []Msg) ([]chatMsg, string) {
	if len(msgs) == 0 {
		return nil, ""
	}
	last := msgs[len(msgs)-1]
	hist := make([]chatMsg, 0, len(msgs)-1)
	for _, m := range msgs[:len(msgs)-1] {
		role := "user"
		if m.Role == roleAssistant {
			role = "assistant"
		}
		hist = append(hist, chatMsg{Role: role, Content: m.Text})
	}
	return hist, last.Text
}
