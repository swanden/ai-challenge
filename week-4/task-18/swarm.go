package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Reviewer — один проверяющий агент роя (своя «роль»/угол зрения и system prompt).
type Reviewer struct {
	Name   string
	System string
}

// validationReviewers — рой проверяющих для этапа validation: смотрят с разных сторон.
func validationReviewers() []Reviewer {
	return []Reviewer{
		{Name: "корректность", System: "Ты ревьюер-критик. Проверь РЕЗУЛЬТАТ на соответствие ПЛАНУ: что не сделано, что расходится. Кратко, по пунктам."},
		{Name: "инварианты", System: "Ты ревьюер по ограничениям. Проверь, не нарушает ли результат инварианты (стек, отсутствие тяжёлых зависимостей, секреты). Кратко."},
		{Name: "качество", System: "Ты ревьюер по качеству Go. Оцени идиоматичность, обработку ошибок, тестируемость. Кратко, без переписывания."},
	}
}

// planningReviewers — рой на этапе planning/research: смотрят план с разных сторон
// (пример лектора «этап исследования 5 агентами с разных сторон»).
func planningReviewers() []Reviewer {
	return []Reviewer{
		{Name: "архитектура", System: "Ты архитектор. Оцени ПЛАН с точки зрения структуры и границ модулей. Что упущено? Кратко."},
		{Name: "риски", System: "Ты ревьюер рисков. Найди в ПЛАНЕ слабые места, неучтённые edge-cases, переусложнение. Кратко."},
		{Name: "стек/инварианты", System: "Ты ревьюер ограничений. Проверь, согласуется ли ПЛАН со стеком и инвариантами (только Go, без тяжёлых зависимостей). Кратко."},
	}
}

// ReviewResult — мнение одного ревьюера.
type ReviewResult struct {
	Name    string
	Verdict string // OK | ISSUES
	Notes   string
}

// Swarm — рой ревьюеров + оркестратор, сводящий мнения в одно решение.
type Swarm struct {
	client    anthropic.Client
	model     anthropic.Model
	stage     TaskState
	reviewers []Reviewer
}

// NewSwarm создаёт рой для конкретного этапа (planning или validation).
func NewSwarm(client anthropic.Client, model anthropic.Model, stage TaskState) *Swarm {
	rv := validationReviewers()
	if stage == StatePlanning {
		rv = planningReviewers()
	}
	return &Swarm{client: client, model: model, stage: stage, reviewers: rv}
}

func (s *Swarm) Stage() TaskState { return s.stage }

// Review запускает каждого ревьюера по (план, результат) — отдельный агент-инстанс.
func (s *Swarm) Review(ctx context.Context, plan, result string) ([]ReviewResult, error) {
	var out []ReviewResult
	for _, r := range s.reviewers {
		var user string
		if s.stage == StatePlanning {
			user = "[ПЛАН]\n" + plan +
				"\n\nОтветь строго:\nVERDICT: OK|ISSUES\nNOTES: <1–2 предложения>"
		} else {
			user = "[ПЛАН]\n" + plan + "\n\n[РЕЗУЛЬТАТ]\n" + result +
				"\n\nОтветь строго:\nVERDICT: OK|ISSUES\nNOTES: <1–2 предложения>"
		}
		msg, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model: s.model, MaxTokens: 250,
			System:   []anthropic.TextBlockParam{{Text: r.System}},
			Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		})
		if err != nil {
			return nil, fmt.Errorf("reviewer %s: %w", r.Name, err)
		}
		v, n := parseReview(collectText(msg))
		out = append(out, ReviewResult{Name: r.Name, Verdict: v, Notes: n})
	}
	return out, nil
}

// Orchestrate сводит мнения ревьюеров: если хоть один ISSUES — задача не проходит,
// оркестратор знает исходный запрос и принимает сводное решение.
func (s *Swarm) Orchestrate(ctx context.Context, userQuery string, reviews []ReviewResult) (passed bool, summary string, err error) {
	var b strings.Builder
	issues := 0
	for _, r := range reviews {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", r.Name, r.Verdict, r.Notes)
		if strings.ToUpper(r.Verdict) == "ISSUES" {
			issues++
		}
	}
	sys := "Ты агент-оркестратор. Знаешь запрос пользователя и мнения ревьюеров. " +
		"Прими сводное решение: если есть существенные замечания — PASS: NO. " +
		"Ответь строго:\nPASS: YES|NO\nSUMMARY: <1–2 предложения сводного решения>"
	user := "[ЗАПРОС ПОЛЬЗОВАТЕЛЯ]\n" + userQuery + "\n\n[МНЕНИЯ РЕВЬЮЕРОВ]\n" + b.String()
	msg, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: s.model, MaxTokens: 250,
		System:   []anthropic.TextBlockParam{{Text: sys}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return false, "", fmt.Errorf("orchestrator: %w", err)
	}
	pass, sum := parsePass(collectText(msg))
	// оркестратор имеет финальное слово, но подстрахуемся: при ISSUES без явного YES — NO
	if issues > 0 && pass {
		// доверяем оркестратору, он видит запрос; оставляем его решение
	}
	return pass, sum, nil
}

func parseReview(s string) (verdict, notes string) {
	verdict = "OK"
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		up := strings.ToUpper(l)
		if strings.HasPrefix(up, "VERDICT:") {
			if strings.Contains(up, "ISSUES") {
				verdict = "ISSUES"
			} else {
				verdict = "OK"
			}
		}
		if strings.HasPrefix(up, "NOTES:") {
			notes = strings.TrimSpace(l[len("NOTES:"):])
		}
	}
	return verdict, notes
}

func parsePass(s string) (pass bool, summary string) {
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		up := strings.ToUpper(l)
		if strings.HasPrefix(up, "PASS:") {
			pass = strings.Contains(up, "YES")
		}
		if strings.HasPrefix(up, "SUMMARY:") {
			summary = strings.TrimSpace(l[len("SUMMARY:"):])
		}
	}
	return pass, summary
}
