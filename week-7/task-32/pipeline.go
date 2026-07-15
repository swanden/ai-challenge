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
	agent           *Agent
	confirm         bool
	in              *bufio.Reader // источник подтверждений (stdin в чате)
	swarm           *Swarm        // рой ревьюеров на validation (опц.)
	lastPlan        string        // артефакт planning (для роя)
	lastExec        string        // артефакт execution (для роя)
	userQuery       string        // исходный запрос (для оркестратора)
	reworkLeft      int           // сколько кругов доработки validation→execution ещё разрешено
	reworkExhausted bool          // лимит доработки исчерпан → пауза, ждём решение человека
	lastStop        string        // stop_reason последнего ответа execution (для проверки обрыва)
}

const defaultReworkRounds = 2 // макс. кругов доработки validation→execution до паузы

func NewPipeline(agent *Agent, confirm bool, in *bufio.Reader) *Pipeline {
	return &Pipeline{agent: agent, confirm: confirm, in: in, reworkLeft: defaultReworkRounds}
}

// EnableSwarm включает рой ревьюеров на этапе validation.
func (p *Pipeline) EnableSwarm(s *Swarm) { p.swarm = s }

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
		if p.userQuery == "" {
			p.userQuery = mem.TaskName()
		}
		fmt.Printf("\n[этап %s] %s\n", state, expectedAction[state])
		fmt.Printf("[запрос] %s\n", q)
		reply, err := p.agent.Ask(ctx, q, FullPolicy())
		if err != nil {
			return false, err
		}
		fmt.Println(reply.Text)

		// запоминаем артефакты этапов и выставляем gate-флаги
		switch state {
		case StatePlanning:
			p.lastPlan = reply.Text
			mem.SetPlanText(reply.Text)
			if p.swarm != nil && p.swarm.Stage() == StatePlanning {
				if err := p.runSwarmPlanning(ctx); err != nil {
					return false, err
				}
			}
			mem.ApprovePlan() // план утверждён (после роя, если он был)
		case StateExecution:
			p.lastExec = reply.Text
			p.lastStop = reply.StopReason
			mem.SetExecArtifact(reply.Text, reply.StopReason)
		case StateValidation:
			if p.swarm != nil && p.swarm.Stage() == StateValidation {
				if err := p.runSwarm(ctx); err != nil {
					return false, err
				}
			} else {
				mem.MarkValidated()
			}
		}

		// лимит доработки исчерпан → пауза (вариант A): сохраняем состояние и выходим
		if p.reworkExhausted {
			fmt.Println("[pipeline] пауза: доработка не сходится. Состояние сохранено; нужно решение человека.")
			_ = mem.Save()
			return false, nil
		}

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

// runSwarm запускает рой ревьюеров + оркестратора на этапе validation.
// Если оркестратор не пропустил — gate Validated НЕ ставится (в done не уйдём).
func (p *Pipeline) runSwarm(ctx context.Context) error {
	// после resume (пауза → новый процесс) поля пайплайна пусты — берём артефакты из памяти
	mem := p.agent.Memory()
	if p.lastExec == "" {
		p.lastExec, p.lastStop = mem.ExecText(), mem.ExecStop()
	}
	if p.lastPlan == "" {
		p.lastPlan = mem.PlanText()
	}
	// Лёгкий детерминированный гейт ПЕРЕД роем: ловит ТОЛЬКО объективный обрыв/пустоту
	// (stop_reason=max_tokens или незакрытый блок кода). Отсутствие кода — НЕ ошибка:
	// бывают задачи без кода (план/анализ/доки), их по сути проверяет рой.
	if chk := CheckResult(p.lastExec, p.lastStop); !chk.OK {
		fmt.Printf("\n[проверка результата] неполный результат — %s\n", chk.Reason)
		if p.reworkLeft <= 0 {
			p.reworkExhausted = true
			fmt.Printf("  не сошлось за %d круга доработки — СТОП.\n", defaultReworkRounds)
			fmt.Println("  [пауза] результат неполный; решение человека (дореализовать / сузить / закрыть).")
			return nil
		}
		p.reworkLeft--
		fmt.Printf("  рой не запускаю (результат неполный) → возврат в execution (осталось кругов: %d).\n", p.reworkLeft)
		_ = p.agent.Memory().Goto(StateExecution)
		return nil
	}
	fmt.Println("\n[проверка результата] OK — результат полный, передаю рою на содержательную проверку.")
	fmt.Println("[рой] запускаю ревьюеров на validation…")
	fmt.Printf("  [вход роя] план: %s\n", oneLine(p.lastPlan, 160))
	fmt.Printf("  [вход роя] результат: %s\n", oneLine(p.lastExec, 160))
	reviews, err := p.swarm.Review(ctx, p.lastPlan, p.lastExec)
	if err != nil {
		return err
	}
	for _, r := range reviews {
		fmt.Printf("  [ревьюер: %s] %s — %s\n", r.Name, r.Verdict, r.Notes)
	}
	fmt.Printf("  [вход оркестратора] запрос юзера: %q + мнения %d ревьюеров\n", p.userQuery, len(reviews))
	passed, summary, err := p.swarm.Orchestrate(ctx, p.userQuery, reviews)
	if err != nil {
		return err
	}
	fmt.Printf("  [оркестратор] PASS=%v — %s\n", passed, summary)
	if passed {
		p.agent.Memory().MarkValidated()
		return nil
	}
	// не прошло: тратим круг доработки; если круги исчерпаны — пауза, а не бесконечный цикл
	if p.reworkLeft <= 0 {
		p.reworkExhausted = true
		fmt.Printf("  [рой] замечания не устранены за %d круга доработки — СТОП.\n", defaultReworkRounds)
		fmt.Println("  [пауза] доработка не сходится; нужно решение человека (дореализовать / сузить план / закрыть).")
		return nil
	}
	p.reworkLeft--
	fmt.Printf("  [рой] есть замечания → возврат в execution на доработку (осталось кругов: %d).\n", p.reworkLeft)
	_ = p.agent.Memory().Goto(StateExecution)
	return nil
}

// runSwarmPlanning запускает рой на этапе planning: ревьюеры оценивают ПЛАН,
// оркестратор сводит мнения (план принимается/дорабатывается перед execution).
func (p *Pipeline) runSwarmPlanning(ctx context.Context) error {
	fmt.Println("\n[рой] запускаю планировщиков-ревьюеров на planning…")
	fmt.Printf("  [вход роя] план: %s\n", oneLine(p.lastPlan, 160))
	reviews, err := p.swarm.Review(ctx, p.lastPlan, "")
	if err != nil {
		return err
	}
	for _, r := range reviews {
		fmt.Printf("  [ревьюер: %s] %s — %s\n", r.Name, r.Verdict, r.Notes)
	}
	fmt.Printf("  [вход оркестратора] запрос юзера: %q + мнения %d ревьюеров\n", p.userQuery, len(reviews))
	passed, summary, err := p.swarm.Orchestrate(ctx, p.userQuery, reviews)
	if err != nil {
		return err
	}
	fmt.Printf("  [оркестратор] план принят=%v — %s\n", passed, summary)
	return nil
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
