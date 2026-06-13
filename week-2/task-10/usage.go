package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// Тарифы (USD за 1M токенов).
const (
	opusInPerM   = 5.0
	opusOutPerM  = 25.0
	haikuInPerM  = 1.0
	haikuOutPerM = 5.0
)

func priceFor(model anthropic.Model) (inPerM, outPerM float64) {
	if containsFold(string(model), "haiku") {
		return haikuInPerM, haikuOutPerM
	}
	return opusInPerM, opusOutPerM
}

func costFor(model anthropic.Model, input, output int64) float64 {
	in, out := priceFor(model)
	return float64(input)/1e6*in + float64(output)/1e6*out
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

// storedMessage — одна реплика диалога.
type storedMessage struct {
	Role    string
	Content string
}

const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// TokenStats — токены основного вызова диалога за ход.
type TokenStats struct {
	InputTokens  int64
	OutputTokens int64
}

func (t TokenStats) String() string {
	return fmt.Sprintf("input: %d · output: %d · turn cost: $%.5f",
		t.InputTokens, t.OutputTokens, costFor(anthropic.ModelClaudeOpus4_8, t.InputTokens, t.OutputTokens))
}
