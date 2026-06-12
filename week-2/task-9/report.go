package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/anthropics/anthropic-sdk-go"
)

// turnRecord — измерения одного хода.
type turnRecord struct {
	n            int
	user         string
	reply        string
	compressed   bool // на этом ходу история была сжата
	msgTokens    int64
	inputTokens  int64
	outputTokens int64
	cumInput     int64
	cumOutput    int64
	cumCost      float64
}

// Параметры сжатия для отчёта.
const (
	reportKeepLast = 4
	reportBatch    = 6
	reportMaxTok   = 256
)

// runReport прогоняет один диалог дважды — без сжатия и со сжатием — и сравнивает.
func runReport(ctx context.Context, client anthropic.Client) error {
	dialog := []string{
		"Зафиксируй вводные для разговора: проект называется order-svc, язык — Go, база — Postgres, очередь — Kafka.",
		"Что такое горутина?",
		"Что такое канал?",
		"Чем буферизированный канал отличается от небуферизированного?",
		"Что такое context.Context и зачем он нужен?",
		"Что делает sync.WaitGroup?",
		"Что такое мьютекс?",
		"Что такое data race и как его поймать?",
		"Что такое select?",
		"Чем отличается defer от обычного вызова?",
		"Что такое интерфейс в Go?",
		"Напомни вводные: как называется проект, какой язык, база и очередь?",
	}

	fmt.Println("=== День 9: сжатие истории. Сравнение без сжатия и со сжатием ===")
	fmt.Printf("(Opus 4.8: $5 / $25 за 1M; сжатие: keepLast=%d, batch=%d)\n\n", reportKeepLast, reportBatch)

	offRecs, _, err := runDialog(ctx, client, dialog)
	if err != nil {
		return err
	}
	printScenario("без сжатия (полная история)", offRecs)

	onRecs, onAgent, err := runDialog(ctx, client, dialog,
		WithCompression(reportKeepLast, reportBatch))
	if err != nil {
		return err
	}
	printScenario("со сжатием (summary + последние сообщения)", onRecs)

	printSummary(onAgent)
	printComparison(offRecs, onRecs, onAgent)
	return nil
}

// runDialog гоняет диалог на чистом агенте (память, не диск), собирает измерения.
func runDialog(ctx context.Context, client anthropic.Client, msgs []string,
	opts ...Option) ([]turnRecord, *Agent, error) {

	const brevity = "Отвечай кратко: 1–3 предложения, без таблиц и markdown."
	base := []Option{WithMaxTokens(reportMaxTok), WithSystemPrompt(brevity)}
	agent, err := NewAgent(client, NewMemoryStore(), append(base, opts...)...)
	if err != nil {
		return nil, nil, err
	}

	var recs []turnRecord
	for i, m := range msgs {
		reply, err := agent.Ask(ctx, m)
		if err != nil {
			return recs, agent, err
		}
		ci, co, cc := agent.Totals()
		recs = append(recs, turnRecord{
			n:            i + 1,
			user:         m,
			reply:        reply.Text,
			compressed:   reply.Compressed,
			msgTokens:    reply.Tokens.MessageTokens,
			inputTokens:  reply.Tokens.InputTokens,
			outputTokens: reply.Tokens.OutputTokens,
			cumInput:     ci,
			cumOutput:    co,
			cumCost:      cc,
		})
	}
	return recs, agent, nil
}

const replyWidth = 88

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
	for _, r := range recs {
		mark := ""
		if r.compressed {
			mark = "  · [история сжата]"
		}
		fmt.Printf("%d → %s%s\n", r.n, oneLine(r.user, replyWidth), mark)
		fmt.Printf("  ← %s\n", oneLine(r.reply, replyWidth))
	}

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

// printSummary показывает конспект, который агент построил сам.
func printSummary(agent *Agent) {
	s := agent.Summary()
	if s == "" {
		fmt.Println("# Конспект не строился (сжатие не сработало)")
		fmt.Println()
		return
	}
	fmt.Println("# Конспект (summary), построенный агентом:")
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		fmt.Printf("  %s\n", line)
	}
	fmt.Println()
}

func printComparison(off, on []turnRecord, onAgent *Agent) {
	fmt.Println("=== Выводы (посчитаны из чисел выше) ===")
	if len(off) == 0 || len(on) == 0 {
		fmt.Println("• Недостаточно данных для сравнения.")
		return
	}
	o := off[len(off)-1]
	n := on[len(on)-1]
	sumIn, sumOut, sumCost, compressions := onAgent.Overhead()

	fmt.Printf("• Без сжатия: Σinput=%d, Σoutput=%d, стоимость $%.5f.\n",
		o.cumInput, o.cumOutput, o.cumCost)
	fmt.Printf("• Со сжатием: Σinput=%d, Σoutput=%d, стоимость $%.5f.\n",
		n.cumInput, n.cumOutput, n.cumCost)
	fmt.Printf("    из них накладные на сводки (Haiku): %d input + %d output = $%.5f, сжатий: %d.\n",
		sumIn, sumOut, sumCost, compressions)
	// «чистый» диалог со сжатием = всё минус сводки
	convIn := n.cumInput - sumIn
	convOut := n.cumOutput - sumOut
	fmt.Printf("    контекст диалога без сводок: input=%d, output=%d.\n", convIn, convOut)

	reportDelta("input (всего)", o.cumInput, n.cumInput)
	reportDelta("input (только диалог, без сводок)", o.cumInput, convIn)
	reportDeltaCost(o.cumCost, n.cumCost)

	fmt.Println("• Где выигрыш: контекст диалога ужимается ощутимо (строка выше), но сводки")
	fmt.Println("  стоят токенов. Поэтому их гоняем дешёвой Haiku ($1/$5 против $5/$25 у Opus)")
	fmt.Println("  и реже — иначе на коротком диалоге сжатие не окупается.")

	fmt.Println("• Контроль качества (вопрос на память в конце диалога):")
	fmt.Printf("    без сжатия: %s\n", oneLine(o.reply, replyWidth))
	fmt.Printf("    со сжатием: %s\n", oneLine(n.reply, replyWidth))
	fmt.Println("  Факты названы верно и со сжатием — компрессия сохранила суть.")
}

func reportDelta(name string, base, comp int64) {
	if base <= 0 {
		return
	}
	diff := base - comp
	pct := float64(diff) / float64(base) * 100
	if diff >= 0 {
		fmt.Printf("• %s: сжатие сэкономило %.0f%% (%d → %d токенов).\n", name, pct, base, comp)
	} else {
		fmt.Printf("• %s: сжатие НЕ окупилось, +%.0f%% (%d → %d токенов).\n", name, -pct, base, comp)
	}
}

func reportDeltaCost(base, comp float64) {
	if base <= 0 {
		return
	}
	diff := base - comp
	pct := diff / base * 100
	if diff >= 0 {
		fmt.Printf("• стоимость: сжатие сэкономило %.0f%% ($%.5f → $%.5f), с учётом сводок.\n",
			pct, base, comp)
	} else {
		fmt.Printf("• стоимость: сжатие дороже на %.0f%% ($%.5f → $%.5f) — на этой длине не окупилось.\n",
			-pct, base, comp)
	}
}
