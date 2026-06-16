package main

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// runReport демонстрирует ТОЛЬКО задание текущего дня (день 12 — персонализация):
// один вопрос через разные профили → разные ответы. Память агента при этом
// существует и используется в чате, но в отчёт дня 12 не выносится.
func runReport(ctx context.Context, client anthropic.Client, profilesDir string) error {
	model := anthropic.ModelClaudeOpus4_8
	store := NewProfileStore(profilesDir)

	fmt.Println("=== День 12: персонализация ассистента ===")
	fmt.Println("Профиль подмешивается в System КАЖДОГО запроса (автоматически).")
	fmt.Printf("Доступные профили: %v\n\n", store.List())

	query := "Объясни, что такое dependency injection и когда его применять."
	fmt.Printf("Один вопрос для всех профилей: %q\n\n", query)

	for _, id := range []string{"none", "junior", "senior-go"} {
		p, err := store.Load(id)
		if err != nil {
			return fmt.Errorf("профиль %s: %w", id, err)
		}
		a := NewAgent(client, model, NewMemory(""))
		a.SetProfile(p)
		// только профиль (long-term): показываем именно эффект персонализации
		reply, err := a.AskOnce(ctx, query, Policy{Profile: true})
		if err != nil {
			return err
		}
		fmt.Printf("## Профиль: %s\n", id)
		fmt.Printf("Профиль в запросе: %v\n", len(reply.UsedLayers) > 0)
		fmt.Printf("Ответ: %s\n", oneLine(reply.Text, 260))
		fmt.Printf("Токены: in=%d, out=%d\n\n", reply.Input, reply.Output)
	}

	fmt.Println("=== Что проверяет задание ===")
	fmt.Println("• Профиль пользователя: набор профилей (none/junior/senior-go), каждый — свой .md")
	fmt.Println("  с предпочтениями (стиль, формат, ограничения).")
	fmt.Println("• Подключение к каждому запросу: профиль уходит в System автоматически,")
	fmt.Println("  пользователь не указывает его в самом вопросе.")
	fmt.Println("• Разные профили → разные ответы: none — нейтрально; junior — просто и пошагово;")
	fmt.Println("  senior-go — кратко, идиоматичный Go, trade-offs.")
	return nil
}
