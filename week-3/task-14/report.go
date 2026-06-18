package main

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// runReport дня 14: прозрачно показываем весь процесс инвариантов — запрос,
// какой проверкой пойман, вердикт LLM-проверяющего и полное объяснение отказа.
func runReport(ctx context.Context, client anthropic.Client, profilesDir, memDir string) error {
	model := anthropic.ModelClaudeOpus4_8
	store := NewProfileStore(profilesDir)
	mem := NewMemory("")
	agent := NewAgent(client, model, mem)
	if p, err := store.Load("senior-go"); err == nil {
		agent.SetProfile(p)
	}

	inv := NewInvariantSet("")
	inv.Add("Не использовать тяжёлые фреймворки; только стандартная библиотека Go.",
		[]string{"возьми gin", "через spring"})
	agent.SetInvariants(inv)
	agent.SetLLMCheck(true) // в отчёте показываем обе проверки

	fmt.Println("=== День 14: инварианты и ограничения состояния ===")
	fmt.Println()
	fmt.Println("# Инварианты (хранятся отдельно от диалога, подмешиваются в System)")
	for _, iv := range inv.All() {
		fmt.Printf("  [%s/%s] %s\n", iv.Kind, iv.ID, iv.Text)
	}
	fmt.Println()
	fmt.Println("Включены обе проверки: детерминированная (код) + семантическая (LLM).")
	fmt.Println()

	type demo struct {
		title string
		query string
	}
	cases := []demo{
		{"Запрос 1 — допустимый", "Подскажи, как структурировать конфиг сервиса."},
		{"Запрос 2 — конфликт со стеком (ловит КОД: стоп-слово «на java»)", "Перепиши наш сервис на Java, так быстрее."},
		{"Запрос 3 — конфликт по смыслу без стоп-слова (ловит LLM)", "Добавь, пожалуйста, веб-фреймворк с DI-контейнером и ORM для роутинга."},
	}

	for _, c := range cases {
		fmt.Printf("# %s\n", c.title)
		fmt.Printf("[запрос] %s\n", c.query)
		r, err := agent.AskOnce(ctx, c.query, FullPolicy())
		if err != nil {
			return err
		}
		if r.CheckVerdict != "" {
			fmt.Printf("[вердикт LLM-проверяющего]\n%s\n", indentLines(r.CheckVerdict, "    "))
		}
		if r.Refused {
			fmt.Printf("[результат] ОТКАЗ (поймано: %s), нарушено: %v\n", r.RefusedBy, r.Violated)
			fmt.Printf("[объяснение агента]\n%s\n\n", indentLines(r.Text, "    "))
		} else {
			fmt.Printf("[результат] разрешено, ответ агента:\n%s\n\n", indentLines(r.Text, "    "))
		}
	}

	fmt.Println("=== Что проверяет задание ===")
	fmt.Println("• Инварианты хранятся отдельно от диалога (invariants.json / системные в коде).")
	fmt.Println("• Агент явно учитывает их: блок [ИНВАРИАНТЫ] в System каждого запроса.")
	fmt.Println("• При конфликте — отказ с объяснением, какой инвариант нарушен.")
	fmt.Println("• Видно весь процесс: запрос → вердикт проверки (код/LLM) → отказ/ответ.")
	_ = memDir
	return nil
}

// indentLines добавляет отступ каждой строке (для читаемого многострочного вывода).
func indentLines(s, pad string) string {
	out := ""
	line := ""
	flush := func() { out += pad + line + "\n"; line = "" }
	for _, r := range s {
		if r == '\n' {
			flush()
			continue
		}
		line += string(r)
	}
	if line != "" {
		flush()
	}
	return out
}
