package task_32

import "strings"

// ArtifactCheck — результат лёгкой детерминированной проверки результата этапа.
// НЕ компилирует код и НЕ требует наличия кода: ловит только объективные признаки
// неполного/оборванного ответа. Содержательную проверку делает рой.
type ArtifactCheck struct {
	OK        bool   // результат пригоден для содержательной валидации (роя)
	Truncated bool   // ответ оборван (max_tokens или незакрытый блок кода)
	Empty     bool   // результат пустой
	Reason    string // причина при OK=false
}

// CheckResult — детерминированная проверка результата этапа.
// stopReason — из API (end_turn | max_tokens | …). Костыля с `go build` нет.
func CheckResult(text, stopReason string) ArtifactCheck {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ArtifactCheck{Empty: true, Reason: "пустой результат этапа"}
	}
	// 1) Штатный сигнал API: модель упёрлась в лимит токенов → ответ оборван.
	if stopReason == "max_tokens" {
		return ArtifactCheck{Truncated: true,
			Reason: "ответ оборван по лимиту токенов (stop_reason=max_tokens)"}
	}
	// 2) Незакрытый блок кода ```…``` — тоже признак обрыва (если оборвалось не по токенам).
	if strings.Count(text, "```")%2 != 0 {
		return ArtifactCheck{Truncated: true,
			Reason: "незакрытый блок кода (```) — результат неполный"}
	}
	return ArtifactCheck{OK: true}
}
