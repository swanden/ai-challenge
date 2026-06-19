package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Verdict — результат проверки запроса на конфликт с инвариантами.
type Verdict struct {
	Allowed  bool
	Violated []string // id/описания нарушенных
	Reason   string   // объяснение отказа
	ByCode   bool     // сработала детерминированная проверка
	Decision string   // сырой DECISION от LLM-проверяющего (ALLOW/DENY)
	Raw      string   // сырой ответ LLM-проверяющего (для прозрачности)
}

// EnforceCode — способ 1: детерминированная проверка по стоп-словам (быстро, дёшево).
func EnforceCode(set *InvariantSet, query string) Verdict {
	hit := set.CheckCode(query)
	if len(hit) == 0 {
		return Verdict{Allowed: true}
	}
	var ids, reasons []string
	for _, inv := range hit {
		ids = append(ids, inv.ID)
		reasons = append(reasons, inv.Text)
	}
	return Verdict{
		Allowed:  false,
		Violated: ids,
		Reason:   "Запрос нарушает инвариант(ы): " + strings.Join(reasons, "; "),
		ByCode:   true,
	}
}

// EnforceLLM — способ 2: семантическая проверка запроса моделью.
// Возвращает вердикт ALLOW/DENY + объяснение. Эти способы не взаимоисключают друг друга.
func EnforceLLM(ctx context.Context, client anthropic.Client, model anthropic.Model, set *InvariantSet, query string) (Verdict, error) {
	rules := set.Render()
	sys := "Ты — проверяющий. Тебе даны инварианты и запрос пользователя. " +
		"Реши, нарушает ли ВЫПОЛНЕНИЕ запроса хотя бы один инвариант. " +
		"Ответь строго в формате:\nDECISION: ALLOW|DENY\nREASON: <одно предложение, какой инвариант и почему>"
	user := "[ИНВАРИАНТЫ]\n" + rules + "\n[ЗАПРОС]\n" + query

	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: model, MaxTokens: 200,
		System:   []anthropic.TextBlockParam{{Text: sys}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("enforce(llm): %w", err)
	}
	out := collectText(msg)
	decision, reason := parseVerdict(out)
	return Verdict{
		Allowed:  decision == "ALLOW",
		Reason:   reason,
		Decision: decision,
		Raw:      strings.TrimSpace(out),
	}, nil
}

func parseVerdict(s string) (decision, reason string) {
	decision = "ALLOW"
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		up := strings.ToUpper(l)
		if strings.HasPrefix(up, "DECISION:") {
			if strings.Contains(up, "DENY") {
				decision = "DENY"
			} else {
				decision = "ALLOW"
			}
		}
		if strings.HasPrefix(up, "REASON:") {
			reason = strings.TrimSpace(l[len("REASON:"):])
		}
	}
	return decision, reason
}
