package main

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Pipeline прогоняет задачу по этапам planning → execution → validation → done
// ДЕТЕРМИНИРОВАННО (переходы в коде). Опционально ставит паузу и спрашивает юзера
// перед каждым переходом (confirm-режим).
type Pipeline struct {
	agent   *Agent
	confirm bool
	in      *bufio.Reader // источник подтверждений (stdin в чате)
}

func NewPipeline(agent *Agent, confirm bool, in *bufio.Reader) *Pipeline {
	return &Pipeline{agent: agent, confirm: confirm, in: in}
}

// stageQuery — что просим у модели на каждом этапе для данной задачи.
func stageQuery(state TaskState, task string) string {
	switch state {
	case StatePlanning:
		return fmt.Sprintf("Задача: %s. Составь короткий план (3–5 шагов). Только план, без кода.", task)
	case StateExecution:
		return "Выполни план по шагам и дай результат (код/артефакт)."
	case StateValidation:
		return "Проверь результат на соответствие плану. Кратко: что ок, что нет."
	default:
		return ""
	}
}

// Run прогоняет автомат от текущего state до done.
// Возвращает true, если дошли до done; false — если поставлено на паузу.
func (p *Pipeline) Run(ctx context.Context) (bool, error) {
	mem := p.agent.Memory()
	for {
		state := mem.State()
		if state == StateDone {
			fmt.Println("[pipeline] задача в состоянии done — завершено.")
			return true, nil
		}

		q := stageQuery(state, mem.TaskName())
		fmt.Printf("\n[этап %s] %s\n", state, expectedAction[state])
		fmt.Printf("[запрос] %s\n", q)
		reply, err := p.agent.Ask(ctx, q, FullPolicy())
		if err != nil {
			return false, err
		}
		fmt.Println(reply.Text)

		to, ok := nextState(state)
		if !ok {
			return true, nil
		}

		// пауза + подтверждение перед детерминированным переходом
		if p.confirm {
			ok, paused := p.askConfirm(state, to)
			if paused {
				fmt.Printf("[pipeline] пауза на этапе %s. Состояние сохранено; продолжить позже.\n", state)
				_ = mem.Save()
				return false, nil
			}
			if !ok {
				// не подтвердил переход вперёд: на validation можно вернуть в execution
				if state == StateValidation {
					if err := mem.Goto(StateExecution); err == nil {
						fmt.Println("[pipeline] возвращаюсь в execution для доработки.")
						_ = mem.Save()
						continue
					}
				}
				fmt.Println("[pipeline] переход отклонён; остаюсь на текущем этапе (пауза).")
				_ = mem.Save()
				return false, nil
			}
		}

		newState, err := mem.Advance() // детерминированный переход с контролем в коде
		if err != nil {
			return false, err
		}
		fmt.Printf("[переход] %s → %s\n", state, newState)
		_ = mem.Save()
	}
}

// askConfirm спрашивает юзера: продолжить переход, нет, или пауза.
// returns (proceed, paused).
func (p *Pipeline) askConfirm(from, to TaskState) (bool, bool) {
	if p.in == nil {
		return true, false
	}
	fmt.Printf("[confirm] этап %s завершён. Перейти к %s? [y = да / n = доработать / pause]: ", from, to)
	line, err := p.in.ReadString('\n')
	if err != nil {
		return false, true
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "да", "":
		return true, false
	case "pause", "p", "пауза":
		return false, true
	default:
		return false, false
	}
}

var _ = anthropic.ModelClaudeOpus4_8 // keep import stable across edits
