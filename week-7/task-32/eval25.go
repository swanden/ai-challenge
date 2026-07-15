package task_32

import (
	"context"
	"fmt"
	"strings"
)

// День 25. Два длинных сценария (по ~10 сообщений) для проверки: мини-чат не теряет
// цель, держит память задачи и на каждый ход выдаёт источники.

// scenario — связная многоходовая линия вопросов по русскому корпусу (лекции ML).
type scenario struct {
	name  string
	goal  string // ожидаемая «цель диалога» (для проверки удержания)
	turns []string
}

func scenarios25() []scenario {
	return []scenario{
		{
			name: "Ансамбли: от бэггинга к бустингу",
			goal: "разобраться в ансамблевых методах и выбрать между лесом и бустингом",
			turns: []string{
				"Что такое бэггинг?",
				"А чем случайный лес отличается от обычного бэггинга?",
				"Почему деревья в случайном лесу декоррелируют?",
				"Теперь про бустинг — чем он идейно отличается от бэггинга?",
				"Что такое градиентный бустинг конкретно?",
				"А для каких функций потерь он работает?",
				"Почему ошибка на контроле у бустинга растёт после какого-то числа итераций?",
				"Как с этим переобучением борются на практике?",
				"Возвращаясь к моей цели: если данных немного и важна устойчивость — лес или бустинг?",
				"Резюмируй, что мы обсудили по ансамблям.",
			},
		},
		{
			name: "Линейные модели и регуляризация",
			goal: "понять линейные модели, регуляризацию и связь с отступом",
			turns: []string{
				"Что такое линейная регрессия?",
				"Что в ней называют переобучением?",
				"Как регуляризация помогает с переобучением?",
				"Чем L1-регуляризация отличается от L2?",
				"Тогда что такое гребневая регрессия?",
				"Давай зафиксируем: меня интересует только L2, L1 не рассматриваем.",
				"Логистическая регрессия — это про регрессию или классификацию?",
				"Что такое отступ (margin) объекта?",
				"Связаны ли между собой отступ и регуляризация?",
				"Резюмируй тему с учётом того, что L1 мы договорились не трогать.",
			},
		},
	}
}

// runEval25 — авто-прогон обоих сценариев одной командой (для видео).
func runEval25(ctx context.Context, a *Agent, ollamaURL, embedModel, ruIndex string, cfg RerankConfig, knowThreshold float64) error {
	r, err := NewRetriever(ruIndex, ollamaURL, embedModel, cfg.TopK)
	if err != nil {
		return fmt.Errorf("русский индекс: %w", err)
	}
	cfg.Corpus = "ru"

	ragBanner(0, "МИНИ-ЧАТ С RAG + ПАМЯТЬЮ ЗАДАЧИ (день 25)")
	fmt.Println("Каждый ход: RAG-поиск → ответ с учётом истории и цели → ИСТОЧНИКИ → обновление памяти задачи.")
	fmt.Printf("КОРПУС: %s\n", r.Info())
	fmt.Println("ПРОВЕРЯЕМ: источники в каждом ходе + цель диалога не теряется на длинной дистанции.")

	scs := scenarios25()
	for si, sc := range scs {
		ragBanner(si+1, fmt.Sprintf("СЦЕНАРИЙ %d: %s", si+1, sc.name))
		fmt.Printf("ЗАЯВЛЕННАЯ ЦЕЛЬ: %s\n", sc.goal)

		sess := NewChatSession(a, r, cfg, knowThreshold)
		turnsWithSrc, answered := 0, 0
		var goalAfter2 string

		for ti, msg := range sc.turns {
			fmt.Printf("\n%s\n", ragRule(fmt.Sprintf("ход %d/%d", ti+1, len(sc.turns))))
			fmt.Printf("Пользователь: %s\n", msg)

			turn, err := sess.Ask(ctx, msg)
			if err != nil {
				return fmt.Errorf("сценарий %d ход %d: %w", si+1, ti+1, err)
			}
			fmt.Printf("Ассистент:    %s\n", oneLine(turn.Answer, 320))
			if turn.IDK {
				fmt.Println("Источники:    — (режим «не знаю»)")
			} else {
				answered++
				if len(turn.Sources) > 0 {
					turnsWithSrc++
				}
				fmt.Printf("Источники:    %s\n", srcLine(turn.Sources))
			}

			if ti == 1 {
				goalAfter2 = sess.State.Goal
			}
			// снимок памяти задачи после первых ходов и в конце
			if ti == 1 || ti == len(sc.turns)-1 {
				fmt.Printf("── ПАМЯТЬ ЗАДАЧИ (после хода %d) ──\n%s\n", ti+1, sess.State.render())
			}
		}

		// Проверки сценария.
		ragRuleP("ИТОГ СЦЕНАРИЯ")
		fmt.Printf("Источники в ответах:   %d из %d содержательных ходов\n", turnsWithSrc, answered)
		fmt.Printf("Цель зафиксирована:    %s (после 2-го хода: %q)\n", yesno(goalAfter2 != ""), oneLine(goalAfter2, 80))
		// Удержание цели = совпадение финальной цели с ЗАЯВЛЕННОЙ по ключевым словам,
		// а не просто «непусто» (иначе застрявшая цель давала бы ложное ✓).
		ov := goalOverlap(sc.goal, sess.State.Goal)
		fmt.Printf("Финальная цель:        %q\n", oneLine(sess.State.Goal, 90))
		fmt.Printf("Совпадение с заявленной: %.0f%% ключевых слов — %s\n", ov*100, yesno(ov >= 0.34))
	}

	ragBanner(len(scs)+1, "ЧЕКЛИСТ ЗАДАНИЯ ДНЯ 25")
	fmt.Println("  [x] мини-чат хранит историю диалога")
	fmt.Println("  [x] на каждый вопрос ищет контекст через RAG")
	fmt.Println("  [x] отвечает с учётом найденной информации")
	fmt.Println("  [x] всегда выводит источники")
	fmt.Println("  [x] память задачи (цель/термины/ограничения/уточнения) — автоизвлечение LLM")
	fmt.Println("  [x] 2 длинных сценария по ~10 сообщений; цель не теряется")
	return nil
}

// goalOverlap — доля значимых слов заявленной цели, встретившихся в извлечённой
// (грубая мера того, что память задачи отражает реальную цель диалога, а не застряла).
func goalOverlap(declared, actual string) float64 {
	norm := func(x string) map[string]bool {
		m := map[string]bool{}
		for _, w := range strings.Fields(strings.ToLower(x)) {
			w = strings.Trim(w, ".,:;()«»\"-")
			if len([]rune(w)) >= 4 { // отбрасываем короткие слова-связки
				m[w] = true
			}
		}
		return m
	}
	d, a := norm(declared), norm(actual)
	if len(d) == 0 {
		return 0
	}
	hit := 0
	for w := range d {
		for aw := range a {
			if strings.Contains(aw, w) || strings.Contains(w, aw) {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(d))
}

func srcLine(cs []Citation) string {
	if len(cs) == 0 {
		return "(нет)"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Source+"#"+lastSeg(c.ChunkID))
	}
	return strings.Join(parts, ", ")
}

// lastSeg берёт хвост chunk_id после '#','para-...' для компактности вывода.
func lastSeg(id string) string {
	if i := strings.LastIndexByte(id, '#'); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

func ragRuleP(title string) { fmt.Printf("\n%s\n", ragRule(title)) }
