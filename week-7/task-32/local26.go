package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"ai-challenge/week-7/task-32/ragcore"
)

// День 26: демонстрация локальной LLM — «запущена и отвечает на запросы».
//
// Задание: установить локальную LLM, обратиться через CLI или HTTP API, сделать
// МИНИМУМ 3 запроса РАЗНОЙ СЛОЖНОСТИ. Здесь — HTTP-путь (LocalLLM поверх Ollama),
// три запроса по нарастающей:
//
//	1) простой факт        — /api/generate, одиночный prompt (как `ollama run`);
//	2) рассуждение+формат   — /api/chat, многошаговый вывод в заданном формате;
//	3) ЛОКАЛЬНЫЙ RAG        — поиск по русскому корпусу лекций ЛОКАЛЬНЫМ эмбеддером
//	                          (nomic, неделя 5) + ответ ЛОКАЛЬНОЙ генерирующей моделью,
//	                          заземлённый на найденные чанки, с источниками.
//
// Ни одного облачного вызова: ANTHROPIC_API_KEY не нужен. Это ровно тот сценарий,
// который лекция называет «сам Бог велел»: локальная LLM + локальный эмбеддинг = RAG,
// который не покидает машину.

// runLocal26 гоняет три запроса и печатает самодокументируемый отчёт (для немого видео).
func runLocal26(ctx context.Context, l *LocalLLM, ollamaURL, embedModel, ruIndex string) error {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  День 26. Локальная LLM: 3 запроса разной сложности           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Printf("Движок: %s · без облака, ключ ANTHROPIC не используется\n\n", l.Name())

	// Прежде всего — жив ли сервер и скачана ли модель (иначе понятная подсказка).
	if err := l.Health(ctx); err != nil {
		return fmt.Errorf("проверка локальной модели: %w", err)
	}
	fmt.Printf("✓ health: Ollama поднят на %s, модель %q на месте\n\n", l.BaseURL, l.Model)

	// ── Запрос 1: простой факт (одиночный prompt, /api/generate) ──────────────
	fmt.Println("── ЗАПРОС 1 (простой) · /api/generate ──")
	q1 := "В какой стране столица — Париж? Ответь одним словом."
	fmt.Printf("Q: %s\n", q1)
	a1, s1, err := l.Generate(ctx, q1)
	if err != nil {
		return fmt.Errorf("запрос 1: %w", err)
	}
	fmt.Printf("A: %s\n   [%s]\n\n", oneLine(a1, 200), s1)

	// ── Запрос 2: рассуждение + строгий формат (/api/chat) ────────────────────
	// Многошаговый счёт + требование формата: слабые локальные модели часто плывут
	// именно на «держи формат» — хороший индикатор сложности для сравнения моделей.
	fmt.Println("── ЗАПРОС 2 (рассуждение + формат) · /api/chat ──")
	sys2 := "Ты аккуратный ассистент. Считай по шагам, но выведи ответ СТРОГО в формате:\n" +
		"РЕЗУЛЬТАТ: <число>\nПОЧЕМУ: <одно предложение>"
	q2 := "В сервисе 12 микросервисов, мониторингом покрыто 25% из них. " +
		"Сколько сервисов ещё НЕ покрыто мониторингом?"
	fmt.Printf("Q: %s\n", q2)
	a2, s2, err := l.Chat(ctx, sys2, nil, q2)
	if err != nil {
		return fmt.Errorf("запрос 2: %w", err)
	}
	fmt.Printf("A: %s\n   [%s]\n\n", indentBlock(a2), s2)

	// ── Запрос 3: локальный RAG (поиск + заземлённый ответ, всё локально) ─────
	fmt.Println("── ЗАПРОС 3 (локальный RAG: поиск + заземление) · /api/chat + nomic ──")
	q3 := "Чем L1-регуляризация отличается от L2?"
	fmt.Printf("Q: %s\n", q3)

	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, 4)
	if err != nil {
		return fmt.Errorf("запрос 3, индекс: %w", err)
	}
	fmt.Printf("   индекс: %s\n", r.Info())
	hits, err := r.Retrieve(ctx, q3) // bi-encoder top-k, локальный эмбеддер (без облачного реранка)
	if err != nil {
		return fmt.Errorf("запрос 3, поиск: %w", err)
	}
	hits = ragcore.FilterThreshold(hits, 0.35) // отсечь заведомо нерелевантное
	if len(hits) == 0 {
		fmt.Println("A: (в корпусе не нашлось релевантного контекста — режим «не знаю»)")
		return nil
	}
	a3, s3, err := l.Chat(ctx, groundedSystem, nil, buildLocalRAGPrompt(q3, hits))
	if err != nil {
		return fmt.Errorf("запрос 3, генерация: %w", err)
	}
	fmt.Printf("A: %s\n", indentBlock(a3))
	fmt.Printf("   источники: %s\n", strings.Join(sourcesOf(hits), " · "))
	fmt.Printf("   [%s]\n\n", s3)

	fmt.Println("Итог: локальная LLM отвечает на все три запроса; запрос 3 — полностью")
	fmt.Println("локальный RAG (поиск nomic + генерация локальной модели), без облака.")
	return nil
}

// groundedSystem — системный промпт заземления (тот же принцип, что в chatAnswer
// дня 25: отвечать ТОЛЬКО по контексту, честно говорить «нет в базе»).
const groundedSystem = "Ты ассистент с доступом к базе знаний. Отвечай кратко и по существу, " +
	"опираясь ТОЛЬКО на приведённый КОНТЕКСТ. Если контекст не покрывает вопрос — честно скажи, " +
	"что в базе этого нет, не выдумывай."

// buildLocalRAGPrompt склеивает найденные чанки и вопрос в один пользовательский ход.
func buildLocalRAGPrompt(query string, hits []ragcore.Hit) string {
	var b strings.Builder
	b.WriteString("КОНТЕКСТ ИЗ БАЗЫ:\n")
	b.WriteString(buildContext(hits)) // общий помощник дня 22 (нумерует чанки с источниками)
	fmt.Fprintf(&b, "\n\nВОПРОС: %s", query)
	return b.String()
}

// indentBlock аккуратно сдвигает многострочный ответ под «A:».
func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "   " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// runLocalChat — интерактивный чат с ЛОКАЛЬНОЙ моделью (история диалога).
// С флагом ragOn каждый ход заземляется на русский корпус (локальный RAG).
// Команды: /rag on|off · /reset · exit. Всё локально, ключ не нужен.
func runLocalChat(ctx context.Context, l *LocalLLM, ollamaURL, embedModel, ruIndex string, ragOn bool) error {
	if err := l.Health(ctx); err != nil {
		return err
	}
	var r *Retriever
	if ragOn {
		var err error
		if r, err = NewRetriever(ruIndex, ollamaURL, embedModel, 4); err != nil {
			return fmt.Errorf("RAG-индекс: %w", err)
		}
	}
	fmt.Printf("Локальный чат · %s · RAG:%v\n", l.Name(), ragOn)
	fmt.Println("Команды: /rag on|off · /reset · exit")

	var history []chatMsg
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
		case line == "/reset":
			history = nil
			fmt.Println("история очищена")
			continue
		case line == "/rag on":
			if r == nil {
				if r, err = NewRetriever(ruIndex, ollamaURL, embedModel, 4); err != nil {
					log.Printf("RAG-индекс: %s", err)
					continue
				}
			}
			ragOn = true
			fmt.Println("RAG включён")
			continue
		case line == "/rag off":
			ragOn = false
			fmt.Println("RAG выключен")
			continue
		}

		sys, user := "Ты краткий локальный ассистент-помощник разработчика.", line
		var srcs []string
		if ragOn && r != nil {
			hits, herr := r.Retrieve(ctx, line)
			if herr == nil {
				hits = ragcore.FilterThreshold(hits, 0.35)
				if len(hits) > 0 {
					sys = groundedSystem
					user = buildLocalRAGPrompt(line, hits)
					srcs = sourcesOf(hits)
				}
			}
		}
		ans, st, aerr := l.Chat(ctx, sys, history, user)
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("ошибка: %s", aerr.Error())
			continue
		}
		fmt.Println(ans)
		if len(srcs) > 0 {
			fmt.Printf("[источники: %s]\n", strings.Join(srcs, " · "))
		}
		fmt.Printf("[%s]\n", st)
		// В историю кладём ИСХОДНУЮ реплику пользователя (не разбухший RAG-промпт).
		history = append(history, chatMsg{Role: "user", Content: line}, chatMsg{Role: "assistant", Content: ans})
	}
}
