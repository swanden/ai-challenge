package main

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

func runReport(ctx context.Context, client anthropic.Client, dir string) error {
	model := anthropic.ModelClaudeOpus4_8
	mem := NewMemory(dir)
	system := "Отвечай кратко: 2–4 предложения, по делу."

	// долговременная: если в каталоге -mem лежит ТВОЙ long-term.md — используем его.
	authored := false
	if dir != "" {
		if err := mem.ReloadLongDoc(); err == nil && mem.HasLongDoc() {
			authored = true
		}
	}

	fmt.Println("=== День 11: модель памяти ассистента (3 слоя) ===")
	fmt.Println()

	// ---------- ЧАСТЬ 1. Что и куда сохраняется ----------
	fmt.Println("# Часть 1. Какие данные попадают в каждый слой")
	fmt.Println("Маршрутизируем входные данные ЯВНО — каждый факт в свой слой:")
	fmt.Println()

	if authored {
		fmt.Printf("  → [long-term (.md)] использую ТВОЙ %s/long-term.md (не трогаю его)\n", dir)
	} else {
		// long-term.md не найден — засеваем демо-профиль (только в этом случае)
		mem.SetProfile(Profile{
			Role:        "Senior Go-разработчик (распределённые системы)",
			Style:       "кратко, с рабочими примерами кода",
			Language:    "русский",
			Constraints: []string{"Go", "минимум зависимостей"},
		})
		mem.AppendKnowledge("Предпочтение: структурные логи в JSON")
		printRoute(LongTerm, "профиль + durable-решение → long-term.md (демо)")
	}

	// working: TaskContext про задачу, живёт через сессии (НЕ диалог)
	mem.SetTask("order-svc", "собрать конфигурацию сервиса заказов")
	mem.SetPlan("выбрать БД", "выбрать очередь", "описать конфиг", "тесты")
	mem.MarkDone("выбрать БД")
	mem.MarkDone("выбрать очередь")
	mem.SetCurrent("описать конфиг")
	mem.Decide("база", "Postgres")
	mem.Decide("очередь", "Kafka")
	printRoute(Working, "задача/план/статус/решения → working (TaskContext)")

	// short-term: диалог
	mem.Dialog(roleUser, "Привет! Продолжаем по сервису?")
	mem.Dialog(roleAssistant, "Да, на шаге «описать конфиг».")
	printRoute(ShortTerm, "реплики чата → short-term")

	fmt.Println()
	fmt.Println("Важно: рабочая память (TaskContext) — это НЕ текущий диалог. Диалог лежит")
	fmt.Println("в short-term, а working хранит статус задачи (план/сделано/шаг) через сессии.")
	fmt.Println()
	fmt.Println("Состояние памяти по слоям:")
	fmt.Print(indent(mem.Inspect(), "  "))
	fmt.Println()

	// ---------- ЧАСТЬ 2. Как слои влияют на ответ ----------
	fmt.Println("# Часть 2. Как память влияет на ответ ассистента")
	query := "Подскажи, как организовать конфигурацию для нашего сервиса?"
	fmt.Printf("Один и тот же вопрос: %q\n\n", query)

	scenarios := []struct {
		name   string
		policy Policy
	}{
		{"(A) без памяти (stateless)", Policy{}},
		{"(B) + long-term (.md профиль/знания)", Policy{Profile: true}},
		{"(C) + long-term + working (TaskContext)", Policy{Profile: true, Working: true}},
	}
	for _, s := range scenarios {
		a := NewAgent(client, model, mem).WithSystem(system)
		reply, err := a.askNoStore(ctx, query, s.policy)
		if err != nil {
			return err
		}
		fmt.Printf("## %s\n", s.name)
		fmt.Printf("Слои в запросе: %s\n", layersString(reply.UsedLayers))
		fmt.Printf("Ответ: %s\n", oneLine(reply.Text, 240))
		fmt.Printf("Токены: in=%d, out=%d\n\n", reply.Input, reply.Output)
	}

	fmt.Println("=== Вывод ===")
	fmt.Println("• Три слоя хранятся РАЗДЕЛЬНО и в разных форматах: диалог → short-term.json,")
	fmt.Println("  TaskContext → working.json, профиль/знания → long-term.md (правится руками).")
	fmt.Println("• В каждый слой пишем ЯВНО; в запрос слои подставляем ПО ПОЛИТИКЕ.")
	fmt.Println("• Влияние на ответ: без памяти — общий совет; +long-term — под senior/Go и стиль;")
	fmt.Println("  +working — конкретно по задаче order-svc (Postgres/Kafka, текущий шаг).")
	fmt.Println()

	// после рабочего прогона сохраняем ВСЕ слои в файлы памяти
	if dir != "" {
		if err := mem.Save(); err != nil { // short-term.json + working.json
			return err
		}
		if !authored { // твой авторский long-term.md не перезаписываем
			if err := mem.SaveLong(); err != nil {
				return err
			}
		}
		fmt.Printf("Слои сохранены в файлы памяти (%s/): short-term.json · working.json", dir)
		if authored {
			fmt.Println(" (твой long-term.md оставлен как есть)")
		} else {
			fmt.Println(" · long-term.md")
		}
	}
	return nil
}

// askNoStore — как Ask, но НЕ пишет диалог (для чистоты сравнения сценариев).
func (a *Agent) askNoStore(ctx context.Context, query string, p Policy) (Reply, error) {
	system, msgs, used := a.mem.Build(query, p)
	if a.system != "" {
		system = append([]anthropic.TextBlockParam{{Text: a.system}}, system...)
	}
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: a.model, MaxTokens: a.maxTokens, System: system, Messages: msgs,
	})
	if err != nil {
		return Reply{}, err
	}
	return Reply{
		Text: collectText(msg), UsedLayers: used,
		Input: msg.Usage.InputTokens, Output: msg.Usage.OutputTokens,
	}, nil
}

func printRoute(l Layer, what string) { fmt.Printf("  → [%s] %s\n", l, what) }

func indent(s, pad string) string {
	out := ""
	for _, line := range splitLines(s) {
		if line == "" {
			out += "\n"
			continue
		}
		out += pad + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
