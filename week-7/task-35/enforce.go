package main

import (
	"context"
	"fmt"
	"strings"
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
// День 27: проверка инвариантов идёт через сменный Completer (облако или локаль),
// а не напрямую в anthropic.Client. Поведение то же — меняется лишь бэкенд.
func EnforceLLM(ctx context.Context, gen Completer, set *InvariantSet, query string) (Verdict, error) {
	rules := set.Render()
	sys := "Ты — проверяющий. Тебе даны инварианты и запрос пользователя. " +
		"Реши, нарушает ли ВЫПОЛНЕНИЕ запроса хотя бы один инвариант. " +
		"Ответь строго в формате:\nDECISION: ALLOW|DENY\nREASON: <одно предложение, какой инвариант и почему>"
	user := "[ИНВАРИАНТЫ]\n" + rules + "\n[ЗАПРОС]\n" + query

	out, _, err := gen.Complete(ctx, sys, []Msg{{Role: roleUser, Text: user}}, CompleteOpts{MaxTokens: 200})
	if err != nil {
		return Verdict{}, fmt.Errorf("enforce(llm): %w", err)
	}
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
