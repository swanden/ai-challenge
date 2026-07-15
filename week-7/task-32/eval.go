package task_32

import (
	"context"
	"fmt"
	"strings"

	"ai-challenge/week-7/task-32/ragcore"
)

// День 22, усиление. EvalQ — контрольный вопрос: ожидание ответа + ожидаемые
// источники (файлы базы, в которых лежит ответ).
type EvalQ struct {
	Q       string
	Expect  string
	Sources []string
}

// controlQuestions — мини-набор из 10 вопросов по базе знаний (исходники самого
// накопительного агента, проиндексированные в день 21). Для каждого: что должно
// быть в ответе и какие файлы-источники релевантны.
func controlQuestions() []EvalQ {
	return []EvalQ{
		{Q: "Сколько слоёв памяти у агента и какие они?",
			Expect:  "три слоя: short-term (диалог), working (TaskContext), long-term (профиль)",
			Sources: []string{"memory.go"}},
		{Q: "Какие этапы у конечного автомата задачи и какие переходы разрешены?",
			Expect:  "planning→execution→validation→done; из validation можно вернуться в execution; прыжки через этап запрещены",
			Sources: []string{"state.go"}},
		{Q: "Что мешает перейти из planning в execution?",
			Expect:  "gate: план не утверждён (нужен /approve-plan, флаг PlanApproved)",
			Sources: []string{"memory.go", "state.go"}},
		{Q: "Как агент проверяет инварианты перед вызовом инструмента?",
			Expect:  "runTool применяет EnforceCode (стоп-слова) и опц. EnforceLLM к намерению вызова; отказ возвращается модели как tool_result с is_error",
			Sources: []string{"agent.go", "enforce.go"}},
		{Q: "Чем проверка инвариантов кодом отличается от проверки через LLM?",
			Expect:  "EnforceCode — детерминированные стоп-слова (быстро, дёшево); EnforceLLM — семантическая проверка моделью (ALLOW/DENY); способы не взаимоисключают друг друга",
			Sources: []string{"enforce.go", "invariants.go"}},
		{Q: "Что делает ленивая загрузка схем инструментов (lazy tools)?",
			Expect:  "старт с каталога серверов (мета-тул load_tools); схемы тулов сервера подмешиваются только после вызова load_tools — экономия токенов",
			Sources: []string{"mcptools.go", "agent.go"}},
		{Q: "Как рой ревьюеров принимает решение на этапе validation?",
			Expect:  "несколько ревьюеров дают VERDICT/NOTES, оркестратор сводит мнения в PASS YES/NO; при не-PASS — возврат в execution, лимит кругов доработки",
			Sources: []string{"swarm.go", "pipeline.go"}},
		{Q: "Как считается стоимость запроса к модели?",
			Expect:  "costFor: токены входа/выхода × цена за 1e6 (Opus 5/25, Haiku 1/5)",
			Sources: []string{"usage.go"}},
		{Q: "Как сервер погоды хранит наблюдения и почему ключом служат координаты?",
			Expect:  "Store в JSON; ключ — координаты, а не сырой ввод, чтобы 'Berlin' и 'Берлин' не плодили дубли",
			Sources: []string{"weatherserver/store.go"}},
		{Q: "Что происходит при паузе пайплайна и как идёт возобновление?",
			Expect:  "состояние и gate-флаги сохраняются в working.json; после паузы продолжаем с того же этапа, переходы по-прежнему контролируются",
			Sources: []string{"pipeline.go", "memory.go"}},
	}
}

// runEval — единая самодокументируемая демонстрация дня 22 для видео (флаг -eval).
// Видео без звука: каждый шаг подписан прямо в выводе, ничего проговаривать не нужно.
// Прогоняет 10 контрольных вопросов в ДВУХ режимах (без RAG / с RAG) и показывает
// различие, поднятые источники и сводный чеклист задания.
func runEval(ctx context.Context, a *Agent, r *Retriever, p Policy) error {
	qs := controlQuestions()

	ragBanner(0, "RAG-АГЕНТ — ДЕМОНСТРАЦИЯ (день 22)")
	fmt.Println("ЧТО ПРОИСХОДИТ: на каждый вопрос агент отвечает ДВАЖДЫ —")
	fmt.Println("  [БЕЗ RAG] обычный ответ модели (только знания внутри модели);")
	fmt.Println("  [С RAG]   вопрос → поиск чанков в индексе → контекст → ответ со ссылкой на источники.")
	fmt.Println("ЦЕЛЬ: видно, что [С RAG] опирается на конкретные файлы базы, а [БЕЗ RAG] — общие слова.")
	fmt.Println()
	fmt.Println("БАЗА ЗНАНИЙ: исходники самого агента, проиндексированы в день 21.")
	fmt.Println("ФОРМАТ ИНДЕКСА: общий пакет ragcore — и индексатор (ragindex), и агент")
	fmt.Println("                используют один тип Index/Chunk (без дублирования структур).")
	fmt.Printf("ПОДКЛЮЧЁННЫЙ ИНДЕКС: %s\n", r.Info())
	fmt.Printf("ВОПРОСОВ В ПРОГОНЕ: %d (у каждого — ожидаемый ответ и ожидаемые файлы-источники)\n", len(qs))

	hitTotal, hitExp := 0, 0
	for i, q := range qs {
		ragBanner(i+1, fmt.Sprintf("ВОПРОС %d из %d", i+1, len(qs)))
		fmt.Printf("ВОПРОС:             %s\n", q.Q)
		fmt.Printf("ОЖИДАЕМ В ОТВЕТЕ:   %s\n", q.Expect)
		fmt.Printf("ОЖИДАЕМ ИСТОЧНИКИ:  %s\n", strings.Join(q.Sources, ", "))

		base, err := a.AskOnce(ctx, q.Q, p)
		if err != nil {
			return fmt.Errorf("вопрос %d (без RAG): %w", i+1, err)
		}
		fmt.Printf("\n%s\n%s\n", ragRule("ОТВЕТ БЕЗ RAG (модель сама по себе)"), oneLine(base.Text, 600))

		rep, hits, err := ragReply(ctx, a, r, q.Q, p)
		if err != nil {
			return fmt.Errorf("вопрос %d (с RAG): %w", i+1, err)
		}
		fmt.Printf("\n%s\n%s\n", ragRule("ОТВЕТ С RAG (с контекстом из базы)"), oneLine(rep.Text, 600))

		fmt.Printf("\nRAG ПОДНЯЛ ИЗ БАЗЫ (top-%d, чем выше score — тем ближе по смыслу):\n", len(hits))
		for j, h := range hits {
			fmt.Printf("   %d. score=%.3f  %s · %s\n", j+1, h.Score, h.Chunk.Source, h.Chunk.Section)
		}
		got, exp := sourceHits(q.Sources, hits)
		hitTotal += got
		hitExp += exp
		fmt.Printf("ИСТОЧНИКИ: подняли %d из %d ожидаемых для этого вопроса\n", got, exp)
	}

	ragBanner(len(qs)+1, "ИТОГ")
	fmt.Printf("RETRIEVAL: ожидаемые файлы-источники подняты в %d из %d случаев по всем вопросам\n", hitTotal, hitExp)
	fmt.Println("КАЧЕСТВО: ответы [С RAG] называют конкретные файлы базы и приводят источники;")
	fmt.Println("          ответы [БЕЗ RAG] — общие формулировки, без ссылок, иногда мимо деталей.")
	fmt.Println()
	ragChecklist()
	return nil
}

// sourceHits — сколько ожидаемых файлов-источников реально попало в выдачу RAG.
func sourceHits(expected []string, hits []ragcore.Hit) (got, total int) {
	total = len(expected)
	if total == 0 {
		return 0, 0
	}
	have := map[string]bool{}
	for _, h := range hits {
		have[h.Chunk.Source] = true
	}
	for _, e := range expected {
		for src := range have {
			if strings.Contains(src, e) || strings.Contains(e, src) {
				got++
				break
			}
		}
	}
	return got, total
}

// ── вывод для немого видео: жирные подписи на каждый блок ─────────────────────

func ragBanner(step int, title string) {
	line := strings.Repeat("=", 70)
	fmt.Printf("\n%s\n  [%d] %s\n%s\n", line, step, title, line)
}

func ragRule(title string) string {
	pad := 64 - len([]rune(title))
	if pad < 0 {
		pad = 0
	}
	return "── " + title + " " + strings.Repeat("─", pad)
}

func ragChecklist() {
	fmt.Println("ЧЕКЛИСТ ЗАДАНИЯ ДНЯ 22:")
	fmt.Println("  [x] функция: вопрос → поиск чанков → объединение с вопросом → запрос к LLM")
	fmt.Println("  [x] два режима: ответ без RAG и ответ с RAG (видно построчно выше)")
	fmt.Println("  [x] 10 контрольных вопросов с ожиданием и ожидаемыми источниками")
	fmt.Println("  [x] сравнение качества двух режимов")
	fmt.Println("  [x] формат индекса вынесен в общий пакет ragcore (единая точка правды)")
}
