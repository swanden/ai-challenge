package task_32

import (
	"fmt"
	"strings"
)

// TaskState — этап задачи (слайд 18). Детерминированный автомат:
// planning → execution → validation → done.
type TaskState string

const (
	StatePlanning   TaskState = "planning"
	StateExecution  TaskState = "execution"
	StateValidation TaskState = "validation"
	StateDone       TaskState = "done"
)

// transitions — разрешённые переходы (слайд 20). Прыгать через этап нельзя.
var transitions = map[TaskState][]TaskState{
	StatePlanning:   {StateExecution},
	StateExecution:  {StateValidation},
	StateValidation: {StateDone, StateExecution}, // не прошло — назад в execution
	StateDone:       {},
}

// expectedAction — что агент делает на этом этапе (поле «ожидаемое действие»).
var expectedAction = map[TaskState]string{
	StatePlanning:   "собрать требования и предложить план; не писать финальный код",
	StateExecution:  "выполнять текущий шаг плана; не перепрыгивать шаги",
	StateValidation: "проверить результат на соответствие плану; найти расхождения",
	StateDone:       "задача завершена; зафиксировать результат",
}

// canTransition сообщает, разрешён ли переход from→to.
func canTransition(from, to TaskState) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// nextState — следующий этап по умолчанию (детерминированный путь вперёд).
func nextState(s TaskState) (TaskState, bool) {
	switch s {
	case StatePlanning:
		return StateExecution, true
	case StateExecution:
		return StateValidation, true
	case StateValidation:
		return StateDone, true
	default:
		return s, false
	}
}

// transition выполняет переход с контролем в КОДЕ (require-стиль, слайд 20).
func transition(from, to TaskState) error {
	if !canTransition(from, to) {
		return fmt.Errorf("переход %s → %s запрещён (разрешено: %s)",
			from, to, joinStates(transitions[from]))
	}
	return nil
}

func joinStates(ss []TaskState) string {
	if len(ss) == 0 {
		return "нет (терминальное состояние)"
	}
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

func (s TaskState) valid() bool {
	switch s {
	case StatePlanning, StateExecution, StateValidation, StateDone:
		return true
	}
	return false
}
