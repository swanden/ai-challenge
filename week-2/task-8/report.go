package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/anthropics/anthropic-sdk-go"
)

// scenario — набор пользовательских реплик для одного прогона.
type scenario struct {
	name     string
	messages []string
}

// turnRecord — измерения одного хода.
type turnRecord struct {
	n            int
	user         string // реплика пользователя
	reply        string // ответ модели (полный; обрезаем при печати)
	msgTokens    int64
	inputTokens  int64 // контекст, ушедший в модель на этом ходу
	outputTokens int64 // ответ модели
	cumInput     int64
	cumOutput    int64
	cumCost      float64
}

// runReport сам прогоняет сценарии, печатает таблицы и выводы.
func runReport(ctx context.Context, client anthropic.Client) error {
	const reportMaxTokens = 256 // держим ответы короткими, чтобы изолировать рост input

	short := scenario{"короткий диалог", []string{
		"Привет! Назови столицу Франции.",
		"А Японии?",
		"Спасибо.",
	}}
	long := scenario{"длинный диалог", []string{
		"Объясни одним предложением, что такое горутина.",
		"А канал?",
		"Чем буферизированный канал отличается от небуферизированного?",
		"Что такое select в Go?",
		"Зачем нужен context.Context?",
		"Что делает sync.WaitGroup?",
		"Что такое data race?",
		"Как его поймать?",
	}}

	fmt.Println("=== Сравнение диалогов по токенам (Claude Opus 4.8: $5 / $25 за 1M) ===")
	fmt.Println()

	shortRecs, _, err := runScenario(ctx, client, short, modelContextWindow, reportMaxTokens)
	if err != nil {
		return err
	}
	printScenario(short.name, shortRecs)

	longRecs, _, err := runScenario(ctx, client, long, modelContextWindow, reportMaxTokens)
	if err != nil {
		return err
	}
	printScenario(long.name, longRecs)

	// Переполнение: тот же длинный диалог, но с жёстким лимитом контекста.
	const overflowLimit int64 = 500
	ofName := fmt.Sprintf("переполнение (лимит %d токенов)", overflowLimit)
	ofRecs, ofErr, err := runScenario(ctx, client,
		scenario{ofName, long.messages}, overflowLimit, reportMaxTokens)
	if err != nil {
		return err
	}
	printScenario(ofName, ofRecs)

	printConclusions(shortRecs, longRecs, ofRecs, ofErr, overflowLimit)
	return nil
}

// runScenario гоняет один сценарий на чистом агенте (память, не диск).
// Если ловит переполнение — не падает, а возвращает где сломалось.
func runScenario(ctx context.Context, client anthropic.Client, sc scenario,
	limit, maxTok int64) ([]turnRecord, *ContextOverflowError, error) {

	agent, err := NewAgent(client, NewMemoryStore(),
		WithContextLimit(limit), WithMaxTokens(maxTok))
	if err != nil {
		return nil, nil, err
	}

	var recs []turnRecord
	for i, m := range sc.messages {
		reply, err := agent.Ask(ctx, m)
		if err != nil {
			var ofe *ContextOverflowError
			if errors.As(err, &ofe) {
				return recs, ofe, nil // штатная остановка на переполнении
			}
			return recs, nil, err
		}
		ci, co, cc := agent.SessionTokens()
		recs = append(recs, turnRecord{
			n:            i + 1,
			user:         m,
			reply:        reply.Text,
			msgTokens:    reply.Tokens.MessageTokens,
			inputTokens:  reply.Tokens.InputTokens,
			outputTokens: reply.Tokens.OutputTokens,
			cumInput:     ci,
			cumOutput:    co,
			cumCost:      cc,
		})
	}
	return recs, nil, nil
}

// replyWidth — макс. длина ответа модели в строке транскрипта.
const replyWidth = 88

// oneLine схлопывает пробелы/переводы строк и обрезает по рунам до max.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func printScenario(name string, recs []turnRecord) {
	fmt.Printf("# %s\n", name)
	if len(recs) == 0 {
		fmt.Println("  (нет успешных ходов)")
		fmt.Println()
		return
	}

	// транскрипт: вопрос целиком, ответ модели — в одну строку
	for _, r := range recs {
		fmt.Printf("%d → %s\n", r.n, oneLine(r.user, replyWidth))
		fmt.Printf("  ← %s\n", oneLine(r.reply, replyWidth))
	}

	// таблица токенов для сравнения
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ход\tmsg\tinput(ctx)\toutput\tΣinput\tΣoutput\tΣcost")
	for _, r := range recs {
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%d\t$%.5f\n",
			r.n, r.msgTokens, r.inputTokens, r.outputTokens,
			r.cumInput, r.cumOutput, r.cumCost)
	}
	w.Flush()
	fmt.Println()
}

// printConclusions считает выводы из реальных чисел.
func printConclusions(short, long, overflow []turnRecord, ofErr *ContextOverflowError, limit int64) {
	fmt.Println("=== Выводы (посчитаны из чисел выше) ===")

	if len(short) > 0 && len(long) > 0 {
		s := short[len(short)-1]
		l := long[len(long)-1]
		fmt.Printf("• Короткий (ходов: %d): Σinput=%d, итоговая стоимость $%.5f.\n",
			len(short), s.cumInput, s.cumCost)
		fmt.Printf("• Длинный (ходов: %d): Σinput=%d, итоговая стоимость $%.5f.\n",
			len(long), l.cumInput, l.cumCost)
		if s.cumCost > 0 {
			fmt.Printf("• Длинный дороже короткого в %.1fx при росте числа ходов в %.1fx.\n",
				l.cumCost/s.cumCost, float64(len(long))/float64(len(short)))
		}
	}

	if len(long) >= 2 {
		first := long[0]
		last := long[len(long)-1]
		if first.inputTokens > 0 {
			fmt.Printf("• Контекст одного хода вырос с %d (ход 1) до %d (ход %d) — в %.1fx, "+
				"т.к. каждый раз в модель уходит ВСЯ история.\n",
				first.inputTokens, last.inputTokens, last.n,
				float64(last.inputTokens)/float64(first.inputTokens))
		}
		// доля output в стоимости длинного диалога
		costIn := float64(last.cumInput) / 1e6 * usdPerMTokInput
		costOut := float64(last.cumOutput) / 1e6 * usdPerMTokOutput
		if total := costIn + costOut; total > 0 {
			fmt.Printf("• В стоимости длинного диалога output даёт %.0f%% при тарифе ×5 к input "+
				"(Σoutput=%d, Σinput=%d) — длину ответов резать важнее, чем промпты.\n",
				costOut/total*100, last.cumOutput, last.cumInput)
		}
	}

	if ofErr != nil {
		brokeAt := len(overflow) + 1
		fmt.Printf("• Переполнение: при лимите %d ход %d отвергнут "+
			"(запрос дошёл до %d токенов). Успешных ходов до этого: %d.\n",
			limit, brokeAt, ofErr.RequestTokens, len(overflow))
		fmt.Println("• Что ломается: модель не принимает запрос целиком — без обрезки или")
		fmt.Println("  суммаризации истории диалог в этой точке просто перестаёт работать.")
	} else {
		fmt.Printf("• Переполнение не наступило: лимит %d не превышен за все ходы (%d).\n",
			limit, len(overflow))
	}
}
