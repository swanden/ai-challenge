package main

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// runReport дня 13 демонстрирует ТОЛЬКО задание дня: конечный автомат задачи.
// Память/персонализация остаются в агенте, но в этот отчёт не выносятся.
func runReport(ctx context.Context, client anthropic.Client, profilesDir string) error {
	model := anthropic.ModelClaudeOpus4_8
	store := NewProfileStore(profilesDir)
	mem := NewMemory("")
	agent := NewAgent(client, model, mem)
	if p, err := store.Load("senior-go"); err == nil {
		agent.SetProfile(p)
	}

	fmt.Println("=== День 13: состояние задачи (Task State Machine) ===")
	fmt.Println()

	fmt.Println("# Автомат: planning → execution → validation → done (переходы в коде)")
	fmt.Println("Разрешённые переходы:")
	for _, s := range []TaskState{StatePlanning, StateExecution, StateValidation, StateDone} {
		fmt.Printf("  %-11s → %s\n", s, joinStates(transitions[s]))
	}
	fmt.Println()

	fmt.Println("# Контроль переходов: прыжок через этап запрещён")
	if err := transition(StatePlanning, StateValidation); err != nil {
		fmt.Printf("  planning → validation: %v\n", err)
	}
	if err := transition(StatePlanning, StateExecution); err == nil {
		fmt.Println("  planning → execution: разрешён")
	}
	fmt.Println()

	// детерминированный прогон автомата по задаче
	fmt.Println("# Прогон задачи по этапам")
	mem.SetTask("парсер CSV на Go", "написать функцию чтения CSV в структуры")
	fmt.Printf("Задача: %s — %s\n", mem.TaskName(), mem.TaskGoal())
	fmt.Printf("Старт. state=%s\n", mem.State())

	p := NewPipeline(agent, false, nil) // в отчёте без confirm
	done, err := p.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nИтог: state=%s, done=%v\n\n", mem.State(), done)

	fmt.Println("# Пауза и продолжение без повторных объяснений")
	fmt.Println("В чате `pause` сохраняет state в working.json; при следующем запуске")
	fmt.Println("агент читает состояние и продолжает с того же этапа — задача не объясняется заново.")
	fmt.Println("(working.json переживает закрытие CLI — стейт сохраняется автоматически.)")

	fmt.Println("\n=== Что проверяет задание ===")
	fmt.Println("• Этап задачи / текущий шаг / ожидаемое действие — поля TaskContext (state, current, expectedAction).")
	fmt.Println("• Переходы planning→execution→validation→done — детерминированно, под контролем кода (require-стиль).")
	fmt.Println("• Пауза на любом этапе + продолжение с того же состояния без повторных объяснений.")
	return nil
}
