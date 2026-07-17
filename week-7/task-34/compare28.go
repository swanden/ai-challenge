package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"ai-challenge/week-7/task-34/ragcore"
)

// День 28: локальная LLM + RAG. Полностью локальный RAG у нас уже есть (день 26
// запрос 3, день 27 флоу 1): retrieval — nomic (bi-encoder, локально), генерация —
// LocalLLM. Новое здесь — ХАРНЕСС СРАВНЕНИЯ И ОЦЕНКИ (по заданию: «сравните local↔cloud;
// оцените качество / скорость / стабильность»).
//
// Честная методика: retrieval выполняется ОДИН раз и ЛОКАЛЬНО, одинаковые чанки идут
// в оба бэкенда → сравниваем ровно ГЕНЕРАЦИЮ, а не поиск. Метрики:
//   - скорость: wall-время и выход-ток/с (по wall — единообразно для обоих);
//   - качество: оба ответа рядом (судит человек/видео) + опциональный облачный
//     судья 1–5 за заземлённость (если есть ключ);
//   - стабильность: K локальных прогонов одного вопроса → разброс задержки, разброс
//     длины ответа и средний лексический Jaccard между прогонами (локаль недетерминирована).
//
// «Полностью локально» — это результат: при отсутствии ключа облачная ветка просто
// пропускается, а локальный RAG работает как есть.

// genResult — итог одной генерации (единообразно для облака и локали).
type genResult struct {
	answer    string
	outTokens int64
	wall      time.Duration
	idk       bool
}

func (g genResult) tokPerSec() float64 {
	if g.wall <= 0 || g.outTokens <= 0 {
		return 0
	}
	return float64(g.outTokens) / g.wall.Seconds()
}

// ragGenerate строит заземлённый промпт (тот же, что в дне 26) и генерирует ответ
// заданным бэкендом, замеряя wall-время. Промпт и контекст идентичны для обоих
// бэкендов — сравнение честное.
func ragGenerate(ctx context.Context, gen Completer, query string, hits []ragcore.Hit) (genResult, error) {
	prompt := buildLocalRAGPrompt(query, hits) // groundedSystem + buildContext — из дня 26
	start := time.Now()
	text, u, err := gen.Complete(ctx, groundedSystem, []Msg{{Role: roleUser, Text: prompt}}, CompleteOpts{MaxTokens: 400})
	wall := time.Since(start)
	if err != nil {
		return genResult{}, err
	}
	return genResult{answer: text, outTokens: u.Output, wall: wall, idk: looksLikeIDK(text)}, nil
}

// looksLikeIDK — грубая эвристика «модель ушла в не знаю».
func looksLikeIDK(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "не нашёл") || strings.Contains(l, "не нашел") ||
		strings.Contains(l, "не знаю") || strings.Contains(l, "нет в базе") ||
		strings.Contains(l, "в базе знаний")
}

// judgeGrounded — опциональный облачный судья: оценивает 1–5, насколько ОТВЕТ
// опирается только на КОНТЕКСТ и отвечает на ВОПРОС. Возвращает (0,"") при ошибке.
func judgeGrounded(ctx context.Context, judge Completer, query, contextText, answer string) (int, string) {
	sys := "Ты строгий оценщик заземлённости. Оцени, насколько ОТВЕТ опирается ТОЛЬКО на КОНТЕКСТ и " +
		"по существу отвечает на ВОПРОС. Ответь СТРОГО:\nSCORE: <целое 1-5>\nNOTE: <кратко>"
	user := fmt.Sprintf("ВОПРОС: %s\n\nКОНТЕКСТ:\n%s\n\nОТВЕТ:\n%s", query, contextText, answer)
	out, _, err := judge.Complete(ctx, sys, []Msg{{Role: roleUser, Text: user}}, CompleteOpts{MaxTokens: 120})
	if err != nil {
		return 0, ""
	}
	return parseScore(out)
}

func parseScore(s string) (int, string) {
	score, note := 0, ""
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(strings.ToUpper(t), "SCORE:") {
			for _, r := range t {
				if r >= '1' && r <= '5' {
					score = int(r - '0')
					break
				}
			}
		} else if strings.HasPrefix(strings.ToUpper(t), "NOTE:") {
			note = strings.TrimSpace(t[len("NOTE:"):])
		}
	}
	return score, note
}

// stabReport — метрики стабильности по K локальным прогонам.
type stabReport struct {
	runs        int
	minWall     time.Duration
	maxWall     time.Duration
	avgWall     time.Duration
	meanTokens  float64
	sdTokens    float64
	meanJaccard float64 // средняя попарная лексическая близость ответов (1 = идентичны)
	idkFlips    int
}

// stability гоняет один и тот же вопрос K раз на ЛОКАЛИ и считает разброс.
func stability(ctx context.Context, local Completer, query string, hits []ragcore.Hit, k int) (stabReport, error) {
	var walls []time.Duration
	var toks []float64
	var answers []string
	rep := stabReport{runs: k}
	for i := 0; i < k; i++ {
		r, err := ragGenerate(ctx, local, query, hits)
		if err != nil {
			return rep, err
		}
		walls = append(walls, r.wall)
		toks = append(toks, float64(r.outTokens))
		answers = append(answers, r.answer)
		if r.idk {
			rep.idkFlips++
		}
	}
	rep.minWall, rep.maxWall, rep.avgWall = minMaxAvg(walls)
	rep.meanTokens, rep.sdTokens = meanSD(toks)
	rep.meanJaccard = meanPairwiseJaccard(answers)
	return rep, nil
}

// runCompare28 — самодокументируемый прогон дня 28 (для видео).
func runCompare28(ctx context.Context, cloud, local Completer, hasCloud bool, ollamaURL, embedModel, ruIndex string, topK, stabRuns int) error {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  День 28. Локальный RAG + сравнение local↔cloud + оценка       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Printf("Локаль: %s · retrieval: nomic (локально)\n", local.Name())
	if hasCloud {
		fmt.Printf("Облако: %s · судья качества: облако\n", cloud.Name())
	} else {
		fmt.Println("Облако: — (ключ ANTHROPIC не задан → сравнение только по локали, RAG полностью локальный)")
	}
	fmt.Println()

	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, topK)
	if err != nil {
		return fmt.Errorf("индекс: %w", err)
	}
	fmt.Printf("корпус: %s · порог 0.35 · стабильность: %d прогона\n\n", r.Info(), stabRuns)

	questions := []string{
		"Чем L1-регуляризация отличается от L2?",
		"Что такое бэггинг и зачем он нужен?",
		"Чем случайный лес отличается от градиентного бустинга?",
	}

	// агрегаты
	var sumLocalWall, sumCloudWall time.Duration
	var sumLocalTps float64
	var sumQLocal, sumQCloud, qCount int
	var sumJaccard float64
	answered := 0

	for i, q := range questions {
		fmt.Printf("━━━ ВОПРОС %d: %s ━━━\n", i+1, q)
		hits, err := r.Retrieve(ctx, q)
		if err != nil {
			return fmt.Errorf("поиск q%d: %w", i+1, err)
		}
		hits = ragcore.FilterThreshold(hits, 0.35)
		if len(hits) == 0 {
			fmt.Print("  (нет релевантного контекста — режим «не знаю»)\n\n")
			continue
		}
		answered++
		ctxText := buildContext(hits)
		fmt.Printf("  контекст: %s\n", strings.Join(sourcesOf(hits), " · "))

		// --- локаль ---
		lr, err := ragGenerate(ctx, local, q, hits)
		if err != nil {
			return fmt.Errorf("локаль q%d: %w", i+1, err)
		}
		sumLocalWall += lr.wall
		sumLocalTps += lr.tokPerSec()
		fmt.Printf("  [локаль] %s\n", oneLine(lr.answer, 300))
		fmt.Printf("           скорость: %s · %d ток · %.1f ток/с%s\n",
			round2(lr.wall), lr.outTokens, lr.tokPerSec(), idkTag(lr.idk))

		// --- облако (если есть) ---
		if hasCloud {
			cr, err := ragGenerate(ctx, cloud, q, hits)
			if err != nil {
				return fmt.Errorf("облако q%d: %w", i+1, err)
			}
			sumCloudWall += cr.wall
			fmt.Printf("  [облако] %s\n", oneLine(cr.answer, 300))
			fmt.Printf("           скорость: %s · %d ток · %.1f ток/с\n", round2(cr.wall), cr.outTokens, cr.tokPerSec())
			fmt.Printf("           СКОРОСТЬ: локаль %s vs облако %s → %s\n",
				round2(lr.wall), round2(cr.wall), speedVerdict(lr.wall, cr.wall))

			// качество: облачный судья оценивает оба ответа
			ql, nl := judgeGrounded(ctx, cloud, q, ctxText, lr.answer)
			qc, _ := judgeGrounded(ctx, cloud, q, ctxText, cr.answer)
			if ql > 0 && qc > 0 {
				sumQLocal += ql
				sumQCloud += qc
				qCount++
				fmt.Printf("           КАЧЕСТВО (судья 1-5): локаль %d/5 vs облако %d/5  (%s)\n", ql, qc, oneLine(nl, 80))
			}
		}

		// --- стабильность локали ---
		st, err := stability(ctx, local, q, hits, stabRuns)
		if err != nil {
			return fmt.Errorf("стабильность q%d: %w", i+1, err)
		}
		sumJaccard += st.meanJaccard
		fmt.Printf("  [стабильность ×%d] задержка %s..%s (avg %s) · токенов %.0f±%.0f · лексич.сходство %.2f%s\n\n",
			st.runs, round2(st.minWall), round2(st.maxWall), round2(st.avgWall),
			st.meanTokens, st.sdTokens, st.meanJaccard, idkFlipTag(st.idkFlips, st.runs))
	}

	// --- сводка ---
	fmt.Println("──────────────── СВОДКА ────────────────")
	if answered == 0 {
		fmt.Println("Ни на один вопрос не нашлось контекста — проверь индекс/порог.")
		return nil
	}
	fmt.Printf("Отвечено вопросов: %d/%d\n", answered, len(questions))
	fmt.Printf("Скорость (avg wall): локаль %s", round2(sumLocalWall/time.Duration(answered)))
	if hasCloud {
		fmt.Printf(" vs облако %s → %s\n", round2(sumCloudWall/time.Duration(answered)),
			speedVerdict(sumLocalWall, sumCloudWall))
	} else {
		fmt.Println()
	}
	fmt.Printf("Скорость локали (avg): %.1f ток/с\n", sumLocalTps/float64(answered))
	if hasCloud && qCount > 0 {
		fmt.Printf("Качество (avg, судья 1-5): локаль %.1f vs облако %.1f\n",
			float64(sumQLocal)/float64(qCount), float64(sumQCloud)/float64(qCount))
	}
	fmt.Printf("Стабильность локали (avg лексич.сходство): %.2f  (1.0 = детерминизм)\n", sumJaccard/float64(answered))
	fmt.Println("\nВывод: RAG полностью локальный (retrieval nomic + генерация локальной модели).")
	if hasCloud {
		fmt.Println("Облако — только точка отсчёта; локаль медленнее и шумнее, но офлайн и приватна.")
	}
	return nil
}

// ── мелкие метрики ──

func idkTag(idk bool) string {
	if idk {
		return " · ⚠ «не знаю»"
	}
	return ""
}

func idkFlipTag(flips, runs int) string {
	if flips == 0 {
		return ""
	}
	return fmt.Sprintf(" · ⚠ «не знаю» в %d/%d", flips, runs)
}

func speedVerdict(local, cloud time.Duration) string {
	if local <= 0 || cloud <= 0 {
		return "—"
	}
	if cloud <= local {
		return fmt.Sprintf("облако быстрее ×%.1f", float64(local)/float64(cloud))
	}
	return fmt.Sprintf("локаль быстрее ×%.1f", float64(cloud)/float64(local))
}

func minMaxAvg(ds []time.Duration) (mn, mx, avg time.Duration) {
	if len(ds) == 0 {
		return
	}
	mn, mx = ds[0], ds[0]
	var sum time.Duration
	for _, d := range ds {
		if d < mn {
			mn = d
		}
		if d > mx {
			mx = d
		}
		sum += d
	}
	return mn, mx, sum / time.Duration(len(ds))
}

func meanSD(xs []float64) (mean, sd float64) {
	if len(xs) == 0 {
		return
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		sd += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(sd / float64(len(xs)))
}

// meanPairwiseJaccard — средняя попарная близость ответов по множествам слов
// (1.0 = ответы идентичны лексически, 0 = не пересекаются). Прокси стабильности.
func meanPairwiseJaccard(texts []string) float64 {
	if len(texts) < 2 {
		return 1.0
	}
	sets := make([]map[string]bool, len(texts))
	for i, t := range texts {
		sets[i] = wordSet(t)
	}
	var sum float64
	var n int
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			sum += jaccard(sets[i], sets[j])
			n++
		}
	}
	if n == 0 {
		return 1.0
	}
	return sum / float64(n)
}

func wordSet(t string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(t)) {
		w = strings.Trim(w, ".,;:!?()[]«»\"'—-")
		if len([]rune(w)) >= 3 {
			m[w] = true
		}
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
