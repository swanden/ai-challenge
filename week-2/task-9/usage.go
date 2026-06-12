package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// Тарифы (USD за 1M токенов). Подбираются по модели: топ-модель дорогая,
// Haiku — дешёвая, и сводки выгоднее гонять именно на ней.
const (
	opusInPerM   = 5.0
	opusOutPerM  = 25.0
	haikuInPerM  = 1.0
	haikuOutPerM = 5.0
)

// priceFor возвращает тариф по модели (по подстроке — устойчиво к версии SDK).
func priceFor(model anthropic.Model) (inPerM, outPerM float64) {
	m := string(model)
	if containsFold(m, "haiku") {
		return haikuInPerM, haikuOutPerM
	}
	return opusInPerM, opusOutPerM // Opus и как дефолт
}

func containsFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// costFor — стоимость по тарифу конкретной модели.
func costFor(model anthropic.Model, input, output int64) float64 {
	in, out := priceFor(model)
	return float64(input)/1e6*in + float64(output)/1e6*out
}

// costUSD — стоимость по тарифу Opus (для построчного вывода диалога).
func costUSD(input, output int64) float64 {
	return float64(input)/1e6*opusInPerM + float64(output)/1e6*opusOutPerM
}

// Контекстное окно Opus 4.8.
const modelContextWindow int64 = 1_000_000

// TokenStats — три величины из дня 8 (по основному вызову диалога).
type TokenStats struct {
	MessageTokens int64
	InputTokens   int64
	OutputTokens  int64
}

func (t TokenStats) String() string {
	return fmt.Sprintf("message: %d · context(input): %d · output: %d · turn cost: $%.5f",
		t.MessageTokens, t.InputTokens, t.OutputTokens,
		costUSD(t.InputTokens, t.OutputTokens))
}
