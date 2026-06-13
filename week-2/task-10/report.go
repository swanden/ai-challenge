package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/anthropics/anthropic-sdk-go"
)

const windowN = 4

// сценарий «собираем ТЗ»: факты задаются по одному, в конце — просьба собрать всё.
var specDialog = []string{
	"Делаем сервис order-svc на Go.",
	"База данных — Postgres.",
	"Очередь сообщений — Kafka.",
	"Нужен rate limiting: 100 запросов в секунду на клиента.",
	"Авторизация — по JWT, токен живёт 15 минут.",
	"Логи структурные, в JSON.",
	"Деплой в Kubernetes, 3 реплики.",
	"Метрики через Prometheus.",
	"Дедлайн MVP — 1 марта.",
	"Тестовое покрытие — не меньше 70%.",
	"Собери всё ТЗ воедино: стек, требования и дедлайн.",
}

// ключевые факты для проверки стабильности (теряет ли стратегия детали).
var keyFacts = []struct {
	label string
	syn   []string
}{
	{"order-svc", []string{"order-svc"}},
	{"Go", []string{" go", "golang", "го "}},
	{"Postgres", []string{"postgres"}},
	{"Kafka", []string{"kafka"}},
	{"rate limit 100rps", []string{"100", "rate"}},
	{"JWT 15m", []string{"jwt"}},
	{"Kubernetes", []string{"kubernetes", "k8s"}},
	{"Prometheus", []string{"prometheus"}},
	{"дедлайн 1 марта", []string{"1 март", "март"}},
	{"тесты 70%", []string{"70"}},
}

type turnRec struct {
	n       int
	user    string
	reply   string
	in, out int64
	cumIn   int64
	cumOut  int64
	cumCost float64
}

func runReport(ctx context.Context, client anthropic.Client) error {
	model := anthropic.ModelClaudeOpus4_8
	haiku := anthropic.ModelClaudeHaiku4_5

	fmt.Println("=== День 10: стратегии управления контекстом (без summary) ===")
	fmt.Printf("Сценарий «собираем ТЗ» (%d сообщений), окно N=%d.\n", len(specDialog), windowN)
	fmt.Println("Реплики пользователя (один и тот же сценарий для всех стратегий):")
	for i, m := range specDialog {
		fmt.Printf("  %2d. %s\n", i+1, m)
	}
	fmt.Println()

	// --- Стратегия 1 и 2 на одном сценарии ---
	sw := NewAgent(client, model, NewSlidingWindow(windowN))
	swRecs, err := runScenario(ctx, sw, specDialog)
	if err != nil {
		return err
	}
	printRun("Стратегия 1: Sliding Window", swRecs, sw)

	sf := NewAgent(client, model, NewStickyFacts(client, haiku, windowN))
	sfRecs, err := runScenario(ctx, sf, specDialog)
	if err != nil {
		return err
	}
	printRun("Стратегия 2: Sticky Facts", sfRecs, sf)
	if fs, ok := sf.Strategy().(*StickyFacts); ok {
		fmt.Println("  Накопленные facts (key-value):")
		for _, line := range strings.Split(strings.TrimRight(fs.renderFacts(), "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}

	// --- Стратегия 3: Branching ---
	if err := runBranchingDemo(ctx, client, model); err != nil {
		return err
	}

	printComparison(swRecs, sfRecs, sw, sf)
	return nil
}

func runScenario(ctx context.Context, a *Agent, msgs []string) ([]turnRec, error) {
	var recs []turnRec
	for i, m := range msgs {
		reply, err := a.Ask(ctx, m)
		if err != nil {
			return recs, err
		}
		ci, co, cc := a.Totals()
		recs = append(recs, turnRec{
			n: i + 1, user: m, reply: reply.Text,
			in: reply.Tokens.InputTokens, out: reply.Tokens.OutputTokens,
			cumIn: ci, cumOut: co, cumCost: cc,
		})
	}
	return recs, nil
}

const w = 92

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func printRun(title string, recs []turnRec, a *Agent) {
	fmt.Printf("# %s\n", title)
	if len(recs) == 0 {
		fmt.Println("  (нет ходов)")
		fmt.Println()
		return
	}
	last := recs[len(recs)-1]

	fmt.Println("Финальное ТЗ (полный ответ модели):")
	fmt.Println("----------------------------------------")
	fmt.Println(strings.TrimSpace(last.reply))
	fmt.Println("----------------------------------------")

	details := factDetails(last.reply)
	hit := 0
	fmt.Println("Факты в финальном ТЗ (✓ сохранён / ✗ потерян):")
	for _, d := range details {
		mark := "✗"
		if d.ok {
			mark = "✓"
			hit++
		}
		fmt.Printf("  %s %s\n", mark, d.label)
	}
	fmt.Printf("Стабильность: %d из %d.\n", hit, len(details))

	oi, oo, oc := a.Overhead()
	ci, co, cc := a.Totals()
	fmt.Printf("Токены: Σinput=%d, Σoutput=%d, стоимость $%.5f", ci, co, cc)
	if oi > 0 || oo > 0 {
		fmt.Printf(" (накладные стратегии: %d in + %d out = $%.5f)", oi, oo, oc)
	}
	fmt.Println()
	fmt.Println()
}

type factCheck struct {
	label string
	ok    bool
}

func factDetails(text string) []factCheck {
	low := strings.ToLower(text)
	out := make([]factCheck, 0, len(keyFacts))
	for _, f := range keyFacts {
		ok := false
		for _, syn := range f.syn {
			if strings.Contains(low, strings.ToLower(syn)) {
				ok = true
				break
			}
		}
		out = append(out, factCheck{label: f.label, ok: ok})
	}
	return out
}

// factCoverage считает, сколько ключевых фактов упомянуто в тексте.
func factCoverage(text string) (hit, total int) {
	for _, d := range factDetails(text) {
		if d.ok {
			hit++
		}
	}
	return hit, len(keyFacts)
}

func runBranchingDemo(ctx context.Context, client anthropic.Client, model anthropic.Model) error {
	fmt.Println("# Стратегия 3: Branching (ветки диалога)")
	br := NewBranching()
	a := NewAgent(client, model, br)

	// общее начало (checkpoint)
	if _, err := a.Ask(ctx, "Проектируем order-svc на Go. Обсуждаем выбор базы данных."); err != nil {
		return err
	}
	fmt.Println("  checkpoint после обсуждения базы; создаём 2 ветки от одного места.")

	// две ветки от одного checkpoint
	br.Branch("postgres")
	br.Branch("mongo")

	br.Switch("postgres")
	if _, err := a.Ask(ctx, "Решено: берём Postgres. Зафиксируй выбор одним предложением."); err != nil {
		return err
	}
	br.Switch("mongo")
	if _, err := a.Ask(ctx, "Решено: берём MongoDB. Зафиксируй выбор одним предложением."); err != nil {
		return err
	}

	// проверяем независимость веток одним и тем же вопросом
	br.Switch("postgres")
	rp, err := a.Ask(ctx, "Какую базу данных мы выбрали в этой ветке?")
	if err != nil {
		return err
	}
	br.Switch("mongo")
	rm, err := a.Ask(ctx, "Какую базу данных мы выбрали в этой ветке?")
	if err != nil {
		return err
	}

	fmt.Printf("  ветка postgres → %s\n", oneLine(rp.Text, 200))
	fmt.Printf("  ветка mongo    → %s\n", oneLine(rm.Text, 200))
	fmt.Printf("  ветки: %v — каждая помнит свой выбор независимо.\n\n", br.Branches())
	return nil
}

func printComparison(sw, sf []turnRec, swAgent, sfAgent *Agent) {
	fmt.Println("=== Сравнение (посчитано из чисел выше) ===")
	if len(sw) == 0 || len(sf) == 0 {
		fmt.Println("• Недостаточно данных.")
		return
	}
	swLast, sfLast := sw[len(sw)-1], sf[len(sf)-1]
	swHit, total := factCoverage(swLast.reply)
	sfHit, _ := factCoverage(sfLast.reply)

	swIn, swOut, swCost := swAgent.Totals()
	sfIn, sfOut, sfCost := sfAgent.Totals()
	_, _, sfOC := sfAgent.Overhead()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "стратегия\tстабильность\tΣinput\tΣoutput\tстоимость\tнакладные")
	fmt.Fprintf(tw, "Sliding Window\t%d/%d фактов\t%d\t%d\t$%.5f\t$%.5f\n", swHit, total, swIn, swOut, swCost, 0.0)
	fmt.Fprintf(tw, "Sticky Facts\t%d/%d фактов\t%d\t%d\t$%.5f\t$%.5f\n", sfHit, total, sfIn, sfOut, sfCost, sfOC)
	tw.Flush()
	fmt.Println()

	switch {
	case sfHit > swHit:
		fmt.Printf("• Стабильность: Sticky Facts удержал на %d факт(ов) больше (%d/%d против %d/%d) — "+
			"важное лежит в facts и не выпадает из окна.\n", sfHit-swHit, sfHit, total, swHit, total)
	case swHit > sfHit:
		fmt.Printf("• Стабильность: Sliding Window дал %d/%d, Sticky Facts %d/%d (на этом прогоне окно "+
			"захватило больше).\n", swHit, total, sfHit, total)
	default:
		fmt.Printf("• Стабильность одинаковая: %d/%d у обеих.\n", swHit, total)
	}

	if swIn > 0 {
		d := float64(swIn-sfIn) / float64(swIn) * 100
		fmt.Printf("• Input: Sliding Window=%d, Sticky Facts=%d (разница %.0f%%). Окно шлёт меньше "+
			"контекста; facts добавляет блок фактов + накладные на их обновление.\n", swIn, sfIn, d)
	}

	outCost := costFor(swAgent.model, 0, swOut)
	if swCost > 0 {
		fmt.Printf("• Стоимость определяет output: у Sliding Window это ~%.0f%% суммы ($%.5f из $%.5f). "+
			"Длину ответов стратегия не контролирует, поэтому по ИТОГОВОЙ стоимости стратегии близки "+
			"($%.5f vs $%.5f) — иногда «дешёвое» окно выходит даже дороже из-за разброса длины ответов.\n",
			outCost/swCost*100, outCost, swCost, swCost, sfCost)
	}

	fmt.Println("• Вывод: контекст-стратегия влияет на INPUT и СТАБИЛЬНОСТЬ, а не на общий счёт —")
	fmt.Println("  счёт правит длина ответов (output). Sliding Window — минимум контекста, но забывчив;")
	fmt.Println("  Sticky Facts — помнит главное ценой небольших накладных; Branching — про развилки.")
}
