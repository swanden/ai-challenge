package task_32

import (
	"context"
	"fmt"
)

// День 24. Проверка на 10 вопросах (русский корпус) + 11-й ЗАВЕДОМО ЧУЖОЙ вопрос,
// на котором обязан сработать режим «не знаю». По каждому ответу проверяем:
// есть ли источники, есть ли цитаты, верифицированы ли цитаты (совпадает ли смысл
// ответа с тем, что реально в чанках).

// foreignQuestion — вопрос вне темы корпуса лекций по ML (для демонстрации «не знаю»).
func foreignQuestion() Q23 {
	return Q23{
		Q:       "Как приготовить борщ и сколько варить свёклу?",
		Expect:  "ожидается режим «не знаю» — в лекциях по ML этого нет",
		Sources: nil,
		Kind:    "foreign",
	}
}

// runEval24 — grounded-ответы с источниками и цитатами по русскому корпусу
// + проверка режима «не знаю» на чужом вопросе.
func runEval24(ctx context.Context, a *Agent, ollamaURL, embedModel, ruIndex string, cfg RerankConfig, knowThreshold float64) error {
	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, cfg.TopK)
	if err != nil {
		return fmt.Errorf("русский индекс: %w", err)
	}
	cfg.Corpus = "ru"

	qs := append(ruQuestions23(), foreignQuestion())

	ragBanner(0, "ЦИТАТЫ, ИСТОЧНИКИ, АНТИ-ГАЛЛЮЦИНАЦИИ (день 24)")
	fmt.Println("НА КАЖДЫЙ ВОПРОС модель обязана вернуть: ответ + источники (source·section·chunk_id) + цитаты.")
	fmt.Println("ЦИТАТЫ проверяются ПО ТЕКСТУ ЧАНКОВ (кодом): выдуманная цитата помечается как НЕ верифицирована.")
	fmt.Printf("РЕЖИМ «НЕ ЗНАЮ»: если лучший кандидат по rerank-score < %.1f — ответ не генерируется, просим уточнить.\n", knowThreshold)
	fmt.Printf("КОРПУС: %s\n", r.Info())
	fmt.Printf("ВОПРОСОВ: %d (10 по корпусу + 1 заведомо чужой)\n", len(qs))

	var withSrc, withQuote, cleanAll, known, idkOnForeign int
	var totEx, totPa, totNo int

	for i, q := range qs {
		foreign := q.Kind == "foreign"
		title := fmt.Sprintf("ВОПРОС %d/%d", i+1, len(qs))
		if foreign {
			title += " · ЗАВЕДОМО ЧУЖОЙ (ожидаем «не знаю»)"
		}
		ragBanner(i+1, title)
		fmt.Printf("ВОПРОС: %s\n", q.Q)

		rep, _, _, err := groundedReply(ctx, a, r, q.Q, cfg, knowThreshold)
		if err != nil {
			return fmt.Errorf("вопрос %d: %w", i+1, err)
		}

		if !rep.Known {
			switch rep.IDKReason {
			case "threshold":
				fmt.Printf("РЕЖИМ «НЕ ЗНАЮ» (порог: лучший score=%.2f < %.1f)\n", rep.BestScore, knowThreshold)
			default: // "model"
				fmt.Printf("РЕЖИМ «НЕ ЗНАЮ» (модель: контекст не отвечает на вопрос; лучший score=%.2f)\n", rep.BestScore)
			}
			fmt.Printf("  ОТВЕТ: %s\n", oneLine(rep.Answer, 160))
			if foreign {
				idkOnForeign++
			}
			continue
		}

		known++
		fmt.Printf("ОТВЕТ (score=%.2f): %s\n", rep.BestScore, oneLine(rep.Answer, 300))

		fmt.Printf("ИСТОЧНИКИ (%d):\n", len(rep.Sources))
		for _, s := range rep.Sources {
			fmt.Printf("   • %s · %s · %s\n", s.Source, s.Section, s.ChunkID)
		}
		ex, pa, no := matchCounts(rep.Quotes)
		fmt.Printf("ЦИТАТЫ (%d: ✓дословно %d · ~по смыслу %d · ✗нет %d):\n", len(rep.Quotes), ex, pa, no)
		for _, qt := range rep.Quotes {
			mark := "✗ нет в чанках"
			switch qt.Match {
			case "exact":
				mark = "✓ дословно"
			case "partial":
				mark = "~ по смыслу (из источника, не буквально)"
			}
			fmt.Printf("   [%s] %s  — %s\n", qt.ChunkID, oneLine(qt.Text, 140), mark)
		}

		if len(rep.Sources) > 0 {
			withSrc++
		}
		if len(rep.Quotes) > 0 {
			withQuote++
		}
		totEx += ex
		totPa += pa
		totNo += no
		if len(rep.Quotes) > 0 && no == 0 { // ни одной выдуманной (exact или partial)
			cleanAll++
		}
	}

	ragBanner(len(qs)+1, "ИТОГ")
	totQ := totEx + totPa + totNo
	fmt.Printf("Отвечено (не «не знаю»):        %d из %d\n", known, len(qs))
	fmt.Printf("С источниками:                  %d из %d отвеченных\n", withSrc, known)
	fmt.Printf("С цитатами:                     %d из %d отвеченных\n", withQuote, known)
	fmt.Printf("Цитаты по уровням:              ✓дословно %d · ~по смыслу %d · ✗нет %d (всего %d)\n", totEx, totPa, totNo, totQ)
	fmt.Printf("Ответы без единой выдумки:      %d из %d отвеченных\n", cleanAll, known)
	fmt.Printf("«Не знаю» на чужом вопросе:     %s\n", yesno(idkOnForeign > 0))
	fmt.Println()
	ragChecklist24()
	return nil
}

func yesno(b bool) string {
	if b {
		return "да ✓"
	}
	return "нет ✗"
}

func ragChecklist24() {
	fmt.Println("ЧЕКЛИСТ ЗАДАНИЯ ДНЯ 24:")
	fmt.Println("  [x] ответ + источники (source · section · chunk_id) в каждом ответе")
	fmt.Println("  [x] цитаты (дословные фрагменты из найденных чанков)")
	fmt.Println("  [x] проверка совпадения смысла: цитаты верифицируются по тексту чанков (анти-галлюцинация)")
	fmt.Println("  [x] режим «не знаю» при релевантности ниже порога + просьба уточнить")
	fmt.Println("  [x] проверка на 10 вопросах + заведомо чужой вопрос")
}
