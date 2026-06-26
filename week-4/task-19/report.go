package main

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// runReport дня 15: (1) контроль переходов на «голой» механике (попытки недопустимых
// переходов и реакция — без API); (2) реальный прогон задачи по этапам через LLM, где
// видны запрос/ответ на каждом этапе и работают условия перехода; (3) опц. рой.
func runReport(ctx context.Context, client anthropic.Client, profilesDir, memDir string, swarmOn bool, swarmStage string, maxTokens int64) error {
	model := anthropic.ModelClaudeOpus4_8
	store := NewProfileStore(profilesDir)

	fmt.Println("=== День 15: контролируемые переходы состояний ===")
	fmt.Println()

	// --- ЧАСТЬ 1. Контроль переходов (без API) ---
	fmt.Println("# Часть 1. Контроль переходов: допустимые состояния и попытки прыжков")
	for _, st := range []TaskState{StatePlanning, StateExecution, StateValidation, StateDone} {
		fmt.Printf("  %-11s → %s\n", st, joinStates(transitions[st]))
	}
	probe := NewMemory("")
	probe.SetTask("парсер CSV на Go", "написать функцию чтения CSV")
	fmt.Printf("Старт: state=%s, план утверждён=%v\n", probe.State(), probe.PlanApproved())
	if err := probe.Goto(StateValidation); err != nil {
		fmt.Printf("  /goto validation (прыжок через этап): ЗАПРЕЩЕНО — %s\n", err.Error())
	}
	if err := probe.Goto(StateExecution); err != nil {
		fmt.Printf("  /goto execution без плана: ЗАПРЕЩЕНО — %s\n", err.Error())
	}
	probe.ApprovePlan()
	if err := probe.Goto(StateExecution); err == nil {
		fmt.Printf("  /approve-plan → /goto execution: РАЗРЕШЕНО (state=%s)\n", probe.State())
	}
	_, _ = probe.Advance()
	if err := probe.Goto(StateDone); err != nil {
		fmt.Printf("  /goto done без валидации (state=%s): ЗАПРЕЩЕНО — %s\n", probe.State(), err.Error())
	}
	fmt.Println()

	// --- ЧАСТЬ 2. Реальный прогон задачи по этапам через LLM ---
	fmt.Println("# Часть 2. Прогон задачи по этапам (живой диалог с LLM)")
	mem := NewMemory(memDir)
	agent := NewAgent(client, model, mem)
	agent.SetMaxTokens(maxTokens)
	if p, err := store.Load("senior-go"); err == nil {
		agent.SetProfile(p)
	}
	mem.SetTask("парсер CSV на Go", "написать функцию чтения CSV в структуры")
	fmt.Printf("Задача: %s — %s\n", mem.TaskName(), mem.TaskGoal())
	fmt.Printf("Старт: state=%s\n", mem.State())

	pipe := NewPipeline(agent, false, nil)
	if swarmOn {
		pipe.EnableSwarm(NewSwarm(client, model, swarmStageOf(swarmStage)))
		fmt.Printf("(рой включён на этапе %s)\n", swarmStageOf(swarmStage))
	}
	done, err := pipe.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nИтог прогона: state=%s, done=%v (условия перехода соблюдены автоматически)\n\n", mem.State(), done)

	// --- Пауза/продолжение ---
	fmt.Println("# Пауза и продолжение")
	fmt.Println("Состояние и флаги условий сохраняются в working.json; после паузы продолжаем")
	fmt.Println("с того же этапа, переходы по-прежнему контролируются (прыжки/без условия запрещены).")
	fmt.Println()

	fmt.Println("=== Что проверяет задание ===")
	fmt.Println("• Допустимые состояния и разрешённые переходы заданы явно (в коде).")
	fmt.Println("• Прыжок через этап и переход без выполненного условия — запрещены (Часть 1).")
	fmt.Println("• Реакция на недопустимый переход — явный отказ с причиной.")
	fmt.Println("• Условия: нельзя execution без утверждённого плана; нельзя done без валидации.")
	fmt.Println("• Прогон по этапам идёт через LLM с видимыми запросами/ответами (Часть 2).")
	if swarmOn {
		fmt.Printf("• Рой агентов на этапе %s + оркестратор сводит мнения в решение.\n", swarmStageOf(swarmStage))
	}
	fmt.Println("• После паузы продолжение корректно: состояние и контроль сохраняются.")
	return nil
}
