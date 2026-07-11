package main

import (
	"context"
	"fmt"
	"time"

	"ai-challenge/week-6/task-30/ragcore"
)

// День 29: оптимизация локальной модели под КОНКРЕТНУЮ задачу.
//
// Наша задача — grounded RU-RAG (заземлённые ответы по русскому корпусу лекций).
// Жалоба из чата (Denis Kotov): «для RAG ответы излишне сухие». Оптимизируем ровно
// этот кейс и сравниваем ДО/ПОСЛЕ по качеству / скорости / ресурсам (по заданию).
//
// Три оси тюнинга из задания:
//   1) параметры: temperature, max_tokens (num_predict), context window (num_ctx);
//   2) квантование: A/B по тегу модели (-local-model vs -tuned-model), «если доступно»;
//   3) prompt-шаблон под кейс: system переписан под «преподаватель ML, полно но без воды,
//      со ссылками [n], по-русски».
//
// Настраиваем НА СТОРОНЕ СВОЕГО СОФТА (совет Гладкова: не в LM Studio — «доступа могут
// лишить»): все крутилки — поля LocalLLM и наш prompt, а не настройки чужого приложения.

type tunePreset struct {
	name       string
	model      string
	temp       float64
	numPredict int
	numCtx     int // 0 = дефолт модели
	system     string
}

// baselinePreset — «ДО»: дефолтные параметры и общий заземляющий промпт дня 26.
func baselinePreset(model string) tunePreset {
	return tunePreset{
		name: "ДО (baseline)", model: model,
		temp: 0.2, numPredict: 400, numCtx: 0,
		system: groundedSystem,
	}
}

// tunedPreset — «ПОСЛЕ»: параметры и промпт, заточенные под RU-RAG-кейс.
//   - system под кейс: полнее, но без воды, со ссылками [n], по-русски (лечит «сухо»);
//   - temperature 0.1 → фактологичнее и стабильнее (выше лексич. сходство);
//   - num_predict 512 → не обрезаем полный ответ;
//   - num_ctx 8192 → все top-4 чанка влезают целиком, контекст не усекается.
func tunedPreset(model string) tunePreset {
	sys := "Ты преподаватель машинного обучения. Ответь на ВОПРОС, опираясь ТОЛЬКО на КОНТЕКСТ, и " +
		"ссылайся на номера фрагментов вида [n]. Дай ПОЛНЫЙ, но без воды ответ: 2–4 предложения или " +
		"короткий маркированный список ключевых отличий. Если контекст не покрывает вопрос — прямо " +
		"скажи, что этого нет в базе, не выдумывай. Пиши по-русски."
	return tunePreset{
		name: "ПОСЛЕ (tuned)", model: model,
		temp: 0.1, numPredict: 512, numCtx: 8192,
		system: sys,
	}
}

// genPreset генерирует заземлённый ответ локальной моделью с параметрами пресета.
func genPreset(ctx context.Context, p tunePreset, ollamaURL, query string, hits []ragcore.Hit) (genResult, error) {
	llm := NewLocalLLM(ollamaURL, p.model)
	llm.Temp = p.temp
	llm.NumPredict = p.numPredict
	llm.NumCtx = p.numCtx
	prompt := buildLocalRAGPrompt(query, hits) // общий контекст+вопрос (день 26)
	start := time.Now()
	text, stats, err := llm.Chat(ctx, p.system, nil, prompt)
	wall := time.Since(start)
	if err != nil {
		return genResult{}, err
	}
	return genResult{answer: text, outTokens: int64(stats.EvalTokens), wall: wall, idk: looksLikeIDK(text)}, nil
}

// runOptimize29 — самодокументируемое сравнение ДО/ПОСЛЕ (для видео).
func runOptimize29(ctx context.Context, judge Completer, hasCloud bool, ollamaURL, embedModel, ruIndex, baseModel, tunedModel string, topK int) error {
	base := baselinePreset(baseModel)
	tuned := tunedPreset(tunedModel)

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  День 29. Оптимизация локальной LLM под RU-RAG (до/после)      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Printf("ДО:    %s · temp %.2f · num_predict %d · num_ctx %s · промпт: общий\n",
		base.model, base.temp, base.numPredict, ctxLabel(base.numCtx))
	fmt.Printf("ПОСЛЕ: %s · temp %.2f · num_predict %d · num_ctx %d · промпт: под кейс (ссылки [n], полнее)\n",
		tuned.model, tuned.temp, tuned.numPredict, tuned.numCtx)
	if baseModel != tunedModel {
		fmt.Printf("Квантование: A/B по модели (%s → %s)\n", baseModel, tunedModel)
	}
	if hasCloud {
		fmt.Printf("Судья качества: %s\n", judge.Name())
	} else {
		fmt.Println("Судья качества: — (нет ключа → качество глазами; скорость/токены считаются)")
	}
	fmt.Println()

	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, topK)
	if err != nil {
		return fmt.Errorf("индекс: %w", err)
	}
	fmt.Printf("корпус: %s · порог 0.35\n\n", r.Info())

	questions := []string{
		"Чем L1-регуляризация отличается от L2?",
		"Чем случайный лес отличается от градиентного бустинга?",
	}

	var sumQB, sumQA, qN int
	var sumWB, sumWA time.Duration
	var sumTB, sumTA int64
	answered := 0

	for i, q := range questions {
		fmt.Printf("━━━ ВОПРОС %d: %s ━━━\n", i+1, q)
		hits, err := r.Retrieve(ctx, q)
		if err != nil {
			return fmt.Errorf("поиск q%d: %w", i+1, err)
		}
		hits = ragcore.FilterThreshold(hits, 0.35)
		if len(hits) == 0 {
			fmt.Print("  (нет релевантного контекста)\n\n")
			continue
		}
		answered++
		ctxText := buildContext(hits)

		before, err := genPreset(ctx, base, ollamaURL, q, hits)
		if err != nil {
			return fmt.Errorf("до q%d: %w", i+1, err)
		}
		after, err := genPreset(ctx, tuned, ollamaURL, q, hits)
		if err != nil {
			return fmt.Errorf("после q%d: %w", i+1, err)
		}
		sumWB += before.wall
		sumWA += after.wall
		sumTB += before.outTokens
		sumTA += after.outTokens

		fmt.Printf("  [ДО]    %s\n", oneLine(before.answer, 260))
		fmt.Printf("          %s · %d ток%s\n", round2(before.wall), before.outTokens, idkTag(before.idk))
		fmt.Printf("  [ПОСЛЕ] %s\n", oneLine(after.answer, 260))
		fmt.Printf("          %s · %d ток%s\n", round2(after.wall), after.outTokens, idkTag(after.idk))

		if hasCloud {
			qb, _ := judgeGrounded(ctx, judge, q, ctxText, before.answer)
			qa, _ := judgeGrounded(ctx, judge, q, ctxText, after.answer)
			if qb > 0 && qa > 0 {
				sumQB += qb
				sumQA += qa
				qN++
				fmt.Printf("  КАЧЕСТВО (судья 1-5): %d → %d  (%s)\n", qb, qa, deltaLabel(qa-qb))
			}
		}
		fmt.Printf("  ТОКЕНЫ: %d → %d (%+d)   СКОРОСТЬ: %s → %s\n\n",
			before.outTokens, after.outTokens, after.outTokens-before.outTokens,
			round2(before.wall), round2(after.wall))
	}

	fmt.Println("──────────────── СВОДКА ДО → ПОСЛЕ ────────────────")
	if answered == 0 {
		fmt.Println("Ни на один вопрос не нашлось контекста — проверь индекс/порог.")
		return nil
	}
	if hasCloud && qN > 0 {
		fmt.Printf("Качество (avg, судья 1-5): %.1f → %.1f  (%s)\n",
			float64(sumQB)/float64(qN), float64(sumQA)/float64(qN),
			deltaLabelF(float64(sumQA-sumQB)/float64(qN)))
	}
	fmt.Printf("Длина ответа (avg токенов): %d → %d  (%+d — «сухо» лечится num_predict+промптом)\n",
		sumTB/int64(answered), sumTA/int64(answered), (sumTA-sumTB)/int64(answered))
	fmt.Printf("Скорость (avg wall): %s → %s  (%s)\n",
		round2(sumWB/time.Duration(answered)), round2(sumWA/time.Duration(answered)),
		speedShift(sumWB, sumWA))
	fmt.Println("\nРесурсы: num_ctx 8192 держит все чанки, но поднимает потребление памяти —")
	fmt.Println("реальные VRAM/RAM смотри `ollama ps` во время прогона (в Go их не измерить точно).")
	fmt.Println("Вывод: оптимизация — на стороне нашего кода (параметры LocalLLM + prompt), не в LM Studio.")
	return nil
}

// ── подписи ──

func ctxLabel(n int) string {
	if n <= 0 {
		return "дефолт"
	}
	return fmt.Sprintf("%d", n)
}

func deltaLabel(d int) string {
	switch {
	case d > 0:
		return fmt.Sprintf("качество ↑ на %d", d)
	case d < 0:
		return fmt.Sprintf("качество ↓ на %d", -d)
	default:
		return "без изменения"
	}
}

func deltaLabelF(d float64) string {
	switch {
	case d > 0.05:
		return fmt.Sprintf("↑ %.1f", d)
	case d < -0.05:
		return fmt.Sprintf("↓ %.1f", -d)
	default:
		return "≈ без изменения"
	}
}

func speedShift(before, after time.Duration) string {
	if before <= 0 || after <= 0 {
		return "—"
	}
	if after <= before {
		return fmt.Sprintf("быстрее ×%.2f", float64(before)/float64(after))
	}
	return fmt.Sprintf("медленнее ×%.2f (плата за полноту/контекст)", float64(after)/float64(before))
}
