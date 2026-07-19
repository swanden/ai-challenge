package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"ai-challenge/week-7/task-35/ragcore"
)

// День 25: мини-чат с RAG + памятью задачи (production-like).
// Каждый ход: RAG-поиск контекста → ответ с УЧЁТОМ истории и цели диалога →
// обязательный вывод источников → автообновление «памяти задачи» (task-state)
// моделью. Собирает воедино RAG-конвейер дней 21–24 и трёхслойную память дней 11–15.

// TaskMemory — «память задачи»: извлекается моделью из диалога после каждого хода.
// Это рабочий слой (в духе TaskContext дня 13), но заполняется автоматически.
type TaskMemory struct {
	Goal        string   `json:"goal"`        // цель диалога
	Terms       []string `json:"terms"`       // зафиксированные термины/определения
	Constraints []string `json:"constraints"` // ограничения, заданные пользователем
	Clarified   []string `json:"clarified"`   // что пользователь уже уточнил
}

func (ts TaskMemory) render() string {
	var b strings.Builder
	if ts.Goal != "" {
		fmt.Fprintf(&b, "Цель диалога: %s\n", ts.Goal)
	}
	if len(ts.Terms) > 0 {
		fmt.Fprintf(&b, "Зафиксированные термины: %s\n", strings.Join(ts.Terms, "; "))
	}
	if len(ts.Constraints) > 0 {
		fmt.Fprintf(&b, "Ограничения: %s\n", strings.Join(ts.Constraints, "; "))
	}
	if len(ts.Clarified) > 0 {
		fmt.Fprintf(&b, "Уже уточнено: %s\n", strings.Join(ts.Clarified, "; "))
	}
	if b.Len() == 0 {
		return "(пока пусто)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// ChatTurn — один ход диалога (для истории и отчёта).
type ChatTurn struct {
	User    string
	Answer  string
	Sources []Citation
	IDK     bool
}

// ChatSession — состояние мини-чата: история + память задачи.
type ChatSession struct {
	a       *Agent
	r       *Retriever
	cfg     RerankConfig
	knowThr float64
	History []ChatTurn
	State   TaskMemory
}

func NewChatSession(a *Agent, r *Retriever, cfg RerankConfig, knowThr float64) *ChatSession {
	return &ChatSession{a: a, r: r, cfg: cfg, knowThr: knowThr}
}

// Ask — один ход мини-чата: RAG → ответ (с историей и целью) → источники →
// обновление памяти задачи.
func (s *ChatSession) Ask(ctx context.Context, userMsg string) (ChatTurn, error) {
	hits, _, err := retrieveAdvanced(ctx, s.a, s.r, userMsg, s.cfg)
	if err != nil {
		return ChatTurn{}, err
	}

	// Порог «не знаю» (день 24): слабый контекст → не выдумываем.
	best := 0.0
	if len(hits) > 0 {
		best = hits[0].Score
	}
	var turn ChatTurn
	turn.User = userMsg
	if len(hits) == 0 || best < s.knowThr {
		turn.Answer = "Не нашёл в базе знаний релевантного контекста по этому вопросу. Уточните или переформулируйте."
		turn.IDK = true
	} else {
		ans, err := s.a.chatAnswer(ctx, userMsg, hits, s.History, s.State)
		if err != nil {
			return ChatTurn{}, err
		}
		turn.Answer = ans
		turn.Sources = sourcesFromHits(hits) // источники ВСЕГДА из реально поднятых чанков
	}

	s.History = append(s.History, turn)

	// Автообновление памяти задачи (не роняем диалог при ошибке — fail-open).
	if ns, err := s.a.updateTaskMemory(ctx, s.History, s.State); err == nil {
		s.State = ns
	}
	return turn, nil
}

// chatAnswer — ответ с учётом контекста RAG, истории диалога и памяти задачи.
// Всегда грунтится контекстом; держит цель диалога, чтобы не терять нить.
func (a *Agent) chatAnswer(ctx context.Context, query string, hits []ragcore.Hit, history []ChatTurn, ts TaskMemory) (string, error) {
	sys := "Ты ассистент-собеседник с доступом к базе знаний. Правила:\n" +
		"1) Отвечай кратко и по существу, опираясь ТОЛЬКО на предоставленный контекст.\n" +
		"2) Держи в голове ЦЕЛЬ ДИАЛОГА и уже зафиксированные термины (см. память задачи) — не теряй нить.\n" +
		"3) Если контекст не покрывает вопрос — честно скажи, что в базе этого нет.\n" +
		"Источники укажет система отдельно — их придумывать не нужно."
	var b strings.Builder
	fmt.Fprintf(&b, "ПАМЯТЬ ЗАДАЧИ:\n%s\n\n", ts.render())
	if h := recentHistory(history, 6); h != "" {
		fmt.Fprintf(&b, "ИСТОРИЯ ДИАЛОГА (последние ходы):\n%s\n\n", h)
	}
	fmt.Fprintf(&b, "КОНТЕКСТ ИЗ БАЗЫ:\n%s\n\n", buildGroundedContext(hits))
	fmt.Fprintf(&b, "ВОПРОС: %s", query)

	out, _, err := a.gen.Complete(ctx, sys, []Msg{{Role: roleUser, Text: b.String()}}, CompleteOpts{MaxTokens: 700})
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("пустой ответ модели")
	}
	return out, nil
}

// updateTaskMemory — модель извлекает/обновляет память задачи из диалога (строгий JSON).
func (a *Agent) updateTaskMemory(ctx context.Context, history []ChatTurn, prev TaskMemory) (TaskMemory, error) {
	sys := "Ты ведёшь «память задачи» для диалога. По истории обнови JSON строго по схеме:\n" +
		`{"goal":"...","terms":["..."],"constraints":["..."],"clarified":["..."]}` + "\n" +
		"goal — текущая цель диалога: держи её стабильной от вопроса к вопросу, НО обнови, если " +
		"фокус диалога заметно сместился (например, пользователь перешёл от одной темы к выбору/сравнению " +
		"или явно вернулся к своей исходной задаче); " +
		"terms — ключевые термины/определения, уже обсуждённые; constraints — ограничения от пользователя; " +
		"clarified — что пользователь уже уточнил/выбрал. Верни ТОЛЬКО JSON, без пояснений."
	prevJSON, _ := json.Marshal(prev)
	user := "ТЕКУЩАЯ ПАМЯТЬ:\n" + string(prevJSON) + "\n\nИСТОРИЯ ДИАЛОГА:\n" + recentHistory(history, 12)

	out, _, err := a.gen.Complete(ctx, sys, []Msg{{Role: roleUser, Text: user}}, CompleteOpts{MaxTokens: 500})
	if err != nil {
		return prev, err
	}
	ns, ok := parseTaskMemory(out)
	if !ok {
		return prev, fmt.Errorf("не разобрать JSON памяти задачи")
	}
	return ns, nil
}

func parseTaskMemory(s string) (TaskMemory, bool) {
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return TaskMemory{}, false
	}
	var ts TaskMemory
	if err := json.Unmarshal([]byte(s[i:j+1]), &ts); err != nil {
		return TaskMemory{}, false
	}
	return ts, true
}

// recentHistory рендерит последние n ходов диалога для промпта.
func recentHistory(history []ChatTurn, n int) string {
	start := 0
	if len(history) > n {
		start = len(history) - n
	}
	var b strings.Builder
	for _, t := range history[start:] {
		fmt.Fprintf(&b, "Пользователь: %s\nАссистент: %s\n", t.User, oneLine(t.Answer, 300))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sourcesFromHits — источники из реально поднятых чанков (гарантируем «всегда есть источники»).
func sourcesFromHits(hits []ragcore.Hit) []Citation {
	out := make([]Citation, 0, len(hits))
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.Chunk.ID] {
			continue
		}
		seen[h.Chunk.ID] = true
		out = append(out, Citation{ChunkID: h.Chunk.ID, Source: h.Chunk.Source, Section: h.Chunk.Section})
	}
	return out
}

// runInteractiveChat — живой мини-чат по русскому корпусу (день 25).
// Команды: /mem — показать память задачи, /reset — очистить, exit — выход.
func runInteractiveChat(ctx context.Context, a *Agent, ollamaURL, embedModel, ruIndex string, cfg RerankConfig, knowThr float64) error {
	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, cfg.TopK)
	if err != nil {
		return fmt.Errorf("русский индекс: %w", err)
	}
	sess := NewChatSession(a, r, cfg, knowThr)
	fmt.Printf("Мини-чат с RAG + памятью задачи. Корпус: %s\n", r.Info())
	fmt.Println("Команды: /mem — память задачи · /reset — очистить · exit — выход")

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "exit":
			return nil
		case line == "/mem":
			fmt.Printf("── ПАМЯТЬ ЗАДАЧИ ──\n%s\n", sess.State.render())
			continue
		case line == "/reset":
			sess.History = nil
			sess.State = TaskMemory{}
			fmt.Println("память задачи и история очищены")
			continue
		}
		turn, err := sess.Ask(ctx, line)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("ошибка: %s", err.Error())
			continue
		}
		fmt.Println(turn.Answer)
		if turn.IDK {
			fmt.Println("[источники: — режим «не знаю»]")
		} else {
			fmt.Printf("[источники: %s]\n", srcLine(turn.Sources))
		}
	}
}
