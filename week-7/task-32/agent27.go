package main

import (
	"context"
	"fmt"
)

// День 27: интеграция локальной LLM в приложение = наши АГЕНТСКИЕ ФЛОУ едут на
// локальной модели. Приложение — тот же накопительный CLI-агент; «интеграция» —
// тумблер -backend local (шов Completer, completer.go). Здесь демонстрируем, что
// на локали отрабатывают именно агентские сценарии, а не «просто чат»:
//
//	1) МИНИ-ЧАТ дня 25 (RAG local + история + автоизвлечение памяти задачи LLM-агентом);
//	2) РОЙ РЕВЬЮЕРОВ дня 15 (несколько агентов + оркестратор сводит вердикт).
//
// Всё локально: retrieval — nomic (bi-encoder, без облачного реранка), генерация и
// все под-агенты — через localCompleter. Ключ ANTHROPIC не нужен.

// runAgent27 гоняет агентские флоу на локальном бэкенде (самодокументируемо, для видео).
// a уже сконфигурирован на локаль (a.SetCompleter(local)); gen — тот же локальный
// Completer для роя; cfg — конфиг retrieval с ВЫКЛЮЧЕННЫМ реранком (иначе он облачный).
func runAgent27(ctx context.Context, a *Agent, gen Completer, ollamaURL, embedModel, ruIndex string, cfg RerankConfig, knowThr float64) error {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  День 27. Агентские флоу на локальной LLM (без облака)         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Printf("Бэкенд генерации: %s · retrieval: nomic (локально, без реранка)\n\n", gen.Name())

	// ── Флоу 1: мини-чат с RAG + памятью задачи (день 25), полностью локально ──
	fmt.Println("━━━ ФЛОУ 1: мини-чат (RAG + память задачи) ━━━")
	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, cfg.TopK)
	if err != nil {
		return fmt.Errorf("индекс: %w", err)
	}
	fmt.Printf("корпус: %s\n\n", r.Info())

	sess := NewChatSession(a, r, cfg, knowThr)
	turns := []string{
		"Что такое бэггинг?",
		"А чем случайный лес отличается от обычного бэггинга?",
		"Теперь про бустинг — чем он идейно отличается от бэггинга?",
		"Возвращаясь к моей цели: если данных немного и важна устойчивость — лес или бустинг?",
	}
	for i, q := range turns {
		turn, err := sess.Ask(ctx, q)
		if err != nil {
			return fmt.Errorf("ход %d: %w", i+1, err)
		}
		fmt.Printf("[%d] Пользователь: %s\n", i+1, q)
		fmt.Printf("    Ассистент: %s\n", oneLine(turn.Answer, 400))
		if turn.IDK {
			fmt.Println("    источники: — (режим «не знаю»)")
		} else {
			fmt.Printf("    источники: %s\n", srcLine(turn.Sources))
		}
		fmt.Printf("    память задачи → цель: %s\n\n", emptyDash(sess.State.Goal))
	}
	fmt.Printf("Итог флоу 1: цель диалога отслеживается локальным агентом: %q\n", sess.State.Goal)
	fmt.Printf("            зафиксированные термины: %s\n\n", joinOrDash(sess.State.Terms))

	// ── Флоу 2: рой ревьюеров + оркестратор (день 15), полностью локально ──
	fmt.Println("━━━ ФЛОУ 2: рой ревьюеров (агентская сеть) ━━━")
	swarm := NewSwarm(gen, StateValidation)
	plan := "Добавить эндпоинт /health в sms-worker: HTTP 200 + JSON {status, uptime}. " +
		"Только stdlib net/http, без новых зависимостей. Покрыть тестом."
	result := "Реализован хендлер /health на net/http, возвращает {status:\"ok\", uptime}. " +
		"Тест на 200 и валидный JSON добавлен."
	fmt.Printf("ПЛАН: %s\nРЕЗУЛЬТАТ: %s\n\n", oneLine(plan, 200), oneLine(result, 200))

	reviews, err := swarm.Review(ctx, plan, result)
	if err != nil {
		return fmt.Errorf("рой: %w", err)
	}
	for _, rv := range reviews {
		fmt.Printf("  ревьюер[%s]: %s — %s\n", rv.Name, rv.Verdict, oneLine(rv.Notes, 200))
	}
	passed, summary, err := swarm.Orchestrate(ctx, "добавь /health в sms-worker без новых зависимостей", reviews)
	if err != nil {
		return fmt.Errorf("оркестратор: %w", err)
	}
	fmt.Printf("  оркестратор → PASS=%v · %s\n\n", passed, oneLine(summary, 200))

	fmt.Println("Итог: и мини-чат (RAG+память), и рой ревьюеров отработали на ЛОКАЛЬНОЙ")
	fmt.Println("модели — без единого облачного вызова. Тумблер: -backend local.")
	return nil
}

// emptyDash возвращает «—» для пустой строки (читабельность отчёта).
func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// joinOrDash — список через «; » или «—», если пусто.
func joinOrDash(xs []string) string {
	if len(xs) == 0 {
		return "—"
	}
	out := xs[0]
	for _, x := range xs[1:] {
		out += "; " + x
	}
	return out
}
