package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ai-challenge/week-7/task-34/ragcore"
)

// День 33. АССИСТЕНТ ПОДДЕРЖКИ ПОЛЬЗОВАТЕЛЕЙ: /support
//
// Прямой аналог /help дня 31, но другой домен:
//   - вместо документации кода — FAQ продукта (RAG, индекс support/faq);
//   - вместо git-контекста — данные тикета и пользователя (MCP crmserver).
//
// Гибрид (та же проверенная схема дня 31):
//   • КОНТЕКСТ ТИКЕТА — детерминированный ПРЕ-ФЕТЧ. Если указан ID тикета, мы САМИ
//     дёргаем get_ticket через MCP (mcptools.Call), без участия модели. Ответ
//     учитывает тариф и состояние аккаунта гарантированно, а не «если модель
//     догадается вызвать тул».
//   • FAQ — через RAG (retrieveAdvanced + groundedReply с цитатами).
//
// Ключевое отличие ответа поддержки от /help: тикет содержит данные, которые МЕНЯЮТ
// ответ. «Не могу сохранить заметку» на Free и на Pro — разные ответы (лимит 1 ГБ vs
// 50 ГБ). Поэтому контекст тикета обязан влиять на выбор из FAQ, а не просто дописываться.

// TicketContext — то, что мы знаем о тикете БЕЗ участия LLM.
type TicketContext struct {
	Raw string // рендер get_ticket (тикет + автор): тема, текст, тариф, хранилище
	ID  string // ID тикета
	Err string // почему не собран
}

func (t TicketContext) OK() bool { return t.Err == "" && t.Raw != "" }

// fetchTicketContext — прямой вызов MCP get_ticket. Токены не тратятся.
// ВНИМАНИЕ: MCPTools.Call возвращает (текст, isError), а НЕ (текст, ok) — на этом
// был пойман баг в дне 31. Второе значение — признак ОШИБКИ.
func fetchTicketContext(ctx context.Context, m *MCPTools, ticketID string) TicketContext {
	tc := TicketContext{ID: ticketID}
	if ticketID == "" {
		return tc // тикет не указан — ответим по одному FAQ
	}
	if m == nil {
		tc.Err = "MCP не подключён"
		return tc
	}
	out, isErr := m.Call(ctx, "get_ticket", jsonArgs(map[string]any{"id": ticketID}))
	if isErr || strings.HasPrefix(out, "ошибка:") {
		tc.Err = "get_ticket: " + oneLine(out, 120)
		return tc
	}
	tc.Raw = strings.TrimSpace(out)
	return tc
}

// supportReply — конвейер ответа поддержки. Как helpReply дня 31, но:
//   - порог означает «FAQ не помог»;
//   - контекст тикета не является источником для цитирования (это состояние аккаунта,
//     а не документ) — идёт в промпт отдельным блоком, verifyQuotes его не видит.
func supportReply(ctx context.Context, a *Agent, r *Retriever, question string, tc TicketContext,
	cfg RerankConfig, knowThreshold float64) (GroundedReply, []ragcore.Hit, RetrievalTrace, error) {

	// Контекст тикета кладём в агента — groundedAnswer подмешает его блоком
	// «КОНТЕКСТ ПРОЕКТА» (тем же механизмом, что git-контекст в дне 31).
	a.SetProjectContext(ticketBlock(tc))
	defer a.SetProjectContext("")

	hits, tr, err := retrieveAdvanced(ctx, a, r, question, cfg)
	if err != nil {
		return GroundedReply{}, nil, tr, err
	}
	best := 0.0
	if len(hits) > 0 {
		best = hits[0].Score
	}

	faqWeak := len(hits) == 0 || best < knowThreshold
	if faqWeak && !tc.OK() {
		// Ни FAQ, ни тикета — честное «не знаю».
		return GroundedReply{
			Known:     false,
			IDKReason: "threshold",
			BestScore: best,
			Answer: "Не могу ответить: в FAQ нет подходящего раздела, а контекст тикета " +
				"недоступен. Уточните вопрос или укажите номер тикета.",
		}, hits, tr, nil
	}
	if faqWeak {
		hits = nil // отвечаем по контексту тикета, цитировать из FAQ нечего
	}

	rep, err := a.groundedAnswer(ctx, cfg.Model, question, hits)
	if err != nil {
		return GroundedReply{}, hits, tr, err
	}
	rep.BestScore = best
	verifyQuotes(&rep, hits)
	return rep, hits, tr, nil
}

// ticketBlock — блок контекста для промпта. Пустой, если тикета нет.
func ticketBlock(tc TicketContext) string {
	if !tc.OK() {
		return ""
	}
	return tc.Raw
}

// runSupport — одна команда /support: собрать тикет → ответить → напечатать.
func runSupport(ctx context.Context, a *Agent, r *Retriever, m *MCPTools, question, ticketID string,
	cfg RerankConfig, knowThreshold float64) error {

	tc := fetchTicketContext(ctx, m, ticketID)
	if ticketID != "" {
		if tc.OK() {
			fmt.Printf("[контекст тикета %s] %s\n", ticketID, oneLine(firstLine(tc.Raw), 100))
		} else {
			fmt.Printf("[контекст тикета] недоступен: %s (отвечаю по одному FAQ)\n", tc.Err)
		}
	}

	rep, hits, tr, err := supportReply(ctx, a, r, question, tc, cfg, knowThreshold)
	if err != nil {
		return err
	}
	printSupportReply(rep, hits, tr, knowThreshold, tc)
	return nil
}

func printSupportReply(rep GroundedReply, hits []ragcore.Hit, tr RetrievalTrace, knowThreshold float64, tc TicketContext) {
	if !rep.Known {
		fmt.Printf("НЕ ЗНАЮ (%s, лучший score=%.2f < %.1f)\n  %s\n",
			rep.IDKReason, rep.BestScore, knowThreshold, oneLine(rep.Answer, 200))
		return
	}
	src := "FAQ"
	if len(hits) == 0 {
		src = "только контекст тикета (в FAQ ничего не нашлось)"
	}
	if tc.OK() {
		src += " + данные тикета " + tc.ID
	}
	fmt.Printf("ОТВЕТ (score=%.2f · источник: %s):\n  %s\n", rep.BestScore, src, rep.Answer)

	if len(rep.Sources) > 0 {
		fmt.Printf("ИСТОЧНИКИ FAQ (%d):\n", len(rep.Sources))
		for _, s := range rep.Sources {
			fmt.Printf("   • %s · %s\n", s.Source, s.Section)
		}
	}
	if len(rep.Quotes) > 0 {
		ex, pa, no := matchCounts(rep.Quotes)
		fmt.Printf("ЦИТАТЫ (%d: ✓%d ~%d ✗%d):\n", len(rep.Quotes), ex, pa, no)
		for _, qt := range rep.Quotes {
			mark := "✗ нет в FAQ"
			switch qt.Match {
			case "exact":
				mark = "✓ дословно"
			case "partial":
				mark = "~ по смыслу"
			}
			fmt.Printf("   [%s] %s — %s\n", qt.ChunkID, oneLine(qt.Text, 90), mark)
		}
	}
	fmt.Printf("RETRIEVAL: bi-encoder %d → порог %d → MMR %d → финал %d\n",
		tr.NBiEncoder, tr.NThreshold, tr.NMMR, tr.NFinal)
}

// ── демонстрация для видео: -support33 ───────────────────────────────────────

type supportCase struct {
	Question string
	TicketID string
	Expect   string
}

func supportCases() []supportCase {
	return []supportCase{
		{"Почему не приходит письмо для входа?", "T-5001",
			"FAQ auth + тикет: корпоративный Exchange, ждать/SSO"},
		{"Пропала заметка после синхронизации, помогите", "T-5002",
			"FAQ sync + тикет urgent: Корзина 30 дней"},
		{"Не могу сохранить заметку, пишет про лимит", "T-5003",
			"FAQ sync/limits + тикет: Pro, близко к 50 ГБ — важен тариф"},
		{"Что будет с общими заметками при переходе Team на Pro?", "T-5004",
			"FAQ billing downgrade + тикет: владелец команды"},
		{"Как сделать бэкап всех заметок?", "T-5005",
			"FAQ export + тикет: пользователь на Free — экспорт недоступен!"},
		{"Какая погода в Москве?", "",
			"вне домена — ожидаем честное «не знаю»"},
	}
}

func runSupport33(ctx context.Context, a *Agent, r *Retriever, m *MCPTools, cfg RerankConfig,
	knowThreshold float64) error {

	ragBanner(0, "АССИСТЕНТ ПОДДЕРЖКИ ПОЛЬЗОВАТЕЛЕЙ · /support (день 33)")
	fmt.Println("Продукт: CloudNote (вымышленный SaaS для заметок с синхронизацией).")
	fmt.Println("ДВА ИСТОЧНИКА: FAQ продукта (RAG) + данные тикета/пользователя (MCP CRM).")
	fmt.Println("ГИБРИД: тикет берётся ПРЕ-ФЕТЧЕМ (get_ticket, 0 токенов), FAQ — через RAG.")

	ragBanner(1, "MCP: CRM ПОДДЕРЖКИ")
	if m == nil {
		return fmt.Errorf("MCP не подключён — день 33 без CRM не демонстрируется")
	}
	fmt.Printf("Серверы и тулы:\n%s\n", m.Breakdown())
	all, isErr := m.Call(ctx, "list_tickets", json.RawMessage(`{"status":"open"}`))
	if !isErr {
		fmt.Printf("\nОткрытые тикеты (прямой вызов list_tickets):\n%s\n", indent(all))
	}

	ragBanner(2, "RAG: FAQ ПРОДУКТА")
	fmt.Printf("Индекс FAQ: %s\n", r.Info())
	fmt.Printf("Порог «FAQ не помог»: %.1f (%s)\n", knowThreshold, scaleName(cfg))

	cases := supportCases()
	var answered, usedFAQ, usedTicket, usedBoth, idk int
	for i, c := range cases {
		ragBanner(i+3, fmt.Sprintf("/support  (%d/%d)", i+1, len(cases)))
		fmt.Printf("ВОПРОС:  %s\n", c.Question)
		if c.TicketID != "" {
			fmt.Printf("ТИКЕТ:   %s\n", c.TicketID)
		}
		fmt.Printf("ОЖИДАЕМ: %s\n\n", c.Expect)

		tc := fetchTicketContext(ctx, m, c.TicketID)
		rep, hits, tr, err := supportReply(ctx, a, r, c.Question, tc, cfg, knowThreshold)
		if err != nil {
			return fmt.Errorf("кейс %d: %w", i+1, err)
		}
		printSupportReply(rep, hits, tr, knowThreshold, tc)

		// Классификация по ДВУМ независимым осям, а не по одной.
		// Ось FAQ: сослался ли ответ на чанки (len(Sources) > 0).
		// Ось тикета: был ли тикет успешно пре-фетчен и подмешан (tc.OK()).
		// Прошлая версия делила по len(Sources)==0 и показывала «по тикету: 0»,
		// хотя тикет учитывался в каждом ответе — просто FAQ тоже срабатывал.
		// Это та же ошибка, что чеклист дня 31: одна ось не описывает два источника.
		switch {
		case !rep.Known:
			idk++
		default:
			answered++
			if len(rep.Sources) > 0 {
				usedFAQ++
			}
			if tc.OK() {
				usedTicket++
			}
			if len(rep.Sources) > 0 && tc.OK() {
				usedBoth++
			}
		}
	}

	ragBanner(len(cases)+3, "ИТОГ (чеклист задания дня 33)")
	fmt.Printf("Отвечает на вопросы о продукте:            %s\n", mark(answered > 0))
	fmt.Printf("Использует RAG (FAQ, документация):        %s (%d чанков)\n", mark(usedFAQ > 0), len(r.idx.Chunks))
	fmt.Printf("Учитывает контекст тикета/пользователя:    %s (get_ticket через MCP)\n", mark(usedTicket > 0))
	fmt.Printf("  ответов с опорой на FAQ:        %d\n", usedFAQ)
	fmt.Printf("  ответов с контекстом тикета:    %d\n", usedTicket)
	fmt.Printf("  ответов, где сошлось и то и то: %d\n", usedBoth)
	fmt.Printf("  честное «не знаю»:              %d (ожидалось 1 — вопрос про погоду)\n", idk)
	return nil
}

// ── мелочи ───────────────────────────────────────────────────────────────────

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func indent(s string) string {
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  " + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
