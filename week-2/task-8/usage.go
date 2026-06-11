package main

import "fmt"

// Цены Claude Opus 4.8 (standard mode), USD за 1M токенов.
// Источник: тарифы Anthropic. Меняешь модель — меняешь и эти константы.
const (
	usdPerMTokInput  = 5.0
	usdPerMTokOutput = 25.0
)

// Контекстное окно Opus 4.8 — 1M токенов. Сверх него запрос отвергается.
// Чтобы УВИДЕТЬ переполнение на коротком диалоге, временно поставь маленькое
// число (например 50) — сработает защита в agent.Ask.
const modelContextWindow int64 = 1_000_000

// TokenStats — три величины из задания дня 8.
type TokenStats struct {
	MessageTokens int64 // токены текущего сообщения пользователя (CountTokens)
	InputTokens   int64 // весь контекст, ушедший в модель (Usage.InputTokens)
	OutputTokens  int64 // ответ модели (Usage.OutputTokens)
}

// costUSD считает стоимость хода/сессии по тарифу.
func costUSD(input, output int64) float64 {
	return float64(input)/1e6*usdPerMTokInput +
		float64(output)/1e6*usdPerMTokOutput
}

func (t TokenStats) String() string {
	return fmt.Sprintf("message: %d · context(input): %d · output: %d · turn cost: $%.5f",
		t.MessageTokens, t.InputTokens, t.OutputTokens,
		costUSD(t.InputTokens, t.OutputTokens))
}
