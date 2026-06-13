package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Strategy — переключаемый способ управления контекстом.
// Prepare принимает новое сообщение пользователя, обновляет внутреннее состояние
// и возвращает то, что реально уйдёт в модель (system + messages).
// Commit сохраняет ответ ассистента в состояние стратегии.
type Strategy interface {
	Name() string
	Prepare(ctx context.Context, userInput string) (system []anthropic.TextBlockParam, msgs []anthropic.MessageParam, err error)
	Commit(assistant string)
}

// overheadReporter — опциональный интерфейс: стратегия, которая тратит токены
// на собственные служебные вызовы (например Facts), сообщает о них.
type overheadReporter interface {
	Overhead() (input, output int64, model anthropic.Model)
}

// toParams конвертирует сообщения в формат SDK.
func toParams(msgs []storedMessage) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Content)
		if m.Role == roleAssistant {
			out = append(out, anthropic.NewAssistantMessage(block))
			continue
		}
		out = append(out, anthropic.NewUserMessage(block))
	}
	return out
}

func lastN(msgs []storedMessage, n int) []storedMessage {
	if n <= 0 || len(msgs) <= n {
		return msgs
	}
	return msgs[len(msgs)-n:]
}

// ============================================================
// Стратегия 1: Sliding Window — храним только последние N сообщений.
// ============================================================

type SlidingWindow struct {
	n   int
	buf []storedMessage
}

func NewSlidingWindow(n int) *SlidingWindow { return &SlidingWindow{n: n} }

func (s *SlidingWindow) Name() string { return fmt.Sprintf("Sliding Window (N=%d)", s.n) }

func (s *SlidingWindow) Prepare(_ context.Context, userInput string) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	s.buf = append(s.buf, storedMessage{Role: roleUser, Content: userInput})
	s.buf = lastN(s.buf, s.n) // всё старше N отбрасываем
	return nil, toParams(s.buf), nil
}

func (s *SlidingWindow) Commit(assistant string) {
	s.buf = append(s.buf, storedMessage{Role: roleAssistant, Content: assistant})
	s.buf = lastN(s.buf, s.n)
}

// ============================================================
// Стратегия 2: Sticky Facts — отдельный key-value блок важных фактов
// (цель, ограничения, предпочтения, решения, договорённости) + последние N.
// ============================================================

type StickyFacts struct {
	n          int
	client     anthropic.Client
	factsModel anthropic.Model

	facts map[string]string
	buf   []storedMessage

	inTok, outTok int64 // накладные на извлечение фактов
}

func NewStickyFacts(client anthropic.Client, factsModel anthropic.Model, n int) *StickyFacts {
	return &StickyFacts{n: n, client: client, factsModel: factsModel, facts: map[string]string{}}
}

func (s *StickyFacts) Name() string { return fmt.Sprintf("Sticky Facts (N=%d, %s)", s.n, s.factsModel) }

func (s *StickyFacts) Overhead() (int64, int64, anthropic.Model) {
	return s.inTok, s.outTok, s.factsModel
}

func (s *StickyFacts) Prepare(ctx context.Context, userInput string) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	// 1) обновляем факты по новому сообщению пользователя
	if err := s.updateFacts(ctx, userInput); err != nil {
		return nil, nil, err
	}
	// 2) последние N сообщений
	s.buf = append(s.buf, storedMessage{Role: roleUser, Content: userInput})
	s.buf = lastN(s.buf, s.n)

	// 3) в запрос: facts (system) + последние N
	var system []anthropic.TextBlockParam
	if len(s.facts) > 0 {
		system = []anthropic.TextBlockParam{{Text: "Важные факты по задаче (помни их всегда):\n" + s.renderFacts()}}
	}
	return system, toParams(s.buf), nil
}

func (s *StickyFacts) Commit(assistant string) {
	s.buf = append(s.buf, storedMessage{Role: roleAssistant, Content: assistant})
	s.buf = lastN(s.buf, s.n)
}

func (s *StickyFacts) renderFacts() string {
	keys := make([]string, 0, len(s.facts))
	for k := range s.facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", k, s.facts[k])
	}
	return b.String()
}

// updateFacts просит дешёвую модель обновить key-value факты по новому сообщению.
func (s *StickyFacts) updateFacts(ctx context.Context, userInput string) error {
	current, _ := json.Marshal(s.facts)
	prompt := fmt.Sprintf(
		"Текущие факты (JSON): %s\nНовое сообщение пользователя: %q\n"+
			"Обнови набор фактов: добавь/измени важное (цель, стек, ограничения, "+
			"предпочтения, решения, договорённости, дедлайны). Верни ТОЛЬКО JSON-объект "+
			"ключ→значение, кратко, без markdown.", string(current), userInput)

	msg, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     s.factsModel,
		MaxTokens: 400,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		return err
	}
	s.inTok += msg.Usage.InputTokens
	s.outTok += msg.Usage.OutputTokens

	raw := extractJSON(collectText(msg))
	if raw == "" {
		return nil // не смогли распарсить — оставляем факты как есть
	}
	updated := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &updated); err != nil {
		return nil
	}
	if len(updated) > 0 {
		s.facts = updated
	}
	return nil
}

// extractJSON вырезает первый {...} из текста (модель иногда оборачивает в пояснения).
func extractJSON(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

// ============================================================
// Стратегия 3: Branching — ветки диалога с checkpoint и переключением.
// ============================================================

type Branching struct {
	branches map[string][]storedMessage
	order    []string
	current  string
}

func NewBranching() *Branching {
	const main = "main"
	return &Branching{
		branches: map[string][]storedMessage{main: {}},
		order:    []string{main},
		current:  main,
	}
}

func (b *Branching) Name() string { return fmt.Sprintf("Branching (ветка %q)", b.current) }

// Branch создаёт новую ветку как форк текущей (checkpoint = текущее состояние).
func (b *Branching) Branch(name string) {
	src := b.branches[b.current]
	cp := append([]storedMessage(nil), src...)
	b.branches[name] = cp
	b.order = append(b.order, name)
}

// Switch переключает активную ветку.
func (b *Branching) Switch(name string) error {
	if _, ok := b.branches[name]; !ok {
		return fmt.Errorf("branching: ветка %q не найдена", name)
	}
	b.current = name
	return nil
}

func (b *Branching) Current() string    { return b.current }
func (b *Branching) Branches() []string { return b.order }

func (b *Branching) Prepare(_ context.Context, userInput string) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	msgs := append(b.branches[b.current], storedMessage{Role: roleUser, Content: userInput})
	b.branches[b.current] = msgs
	return nil, toParams(msgs), nil // ветка хранит свою полную историю независимо
}

func (b *Branching) Commit(assistant string) {
	b.branches[b.current] = append(b.branches[b.current], storedMessage{Role: roleAssistant, Content: assistant})
}
