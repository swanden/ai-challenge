package main

import (
	"fmt"
	"strings"

	"ai-challenge/week-6/task-28/ragcore"
)

// Stats — агрегаты по набору чанков для сравнения стратегий.
type Stats struct {
	Strategy  string
	Count     int
	TokAvg    float64
	TokMin    int
	TokMax    int
	CharAvg   float64
	Intact    int     // чанков, заканчивающихся на естественной границе
	IntactPct float64 // доля целостных границ, %
}

func computeStats(strategy string, chunks []ragcore.Chunk) Stats {
	s := Stats{Strategy: strategy, Count: len(chunks)}
	if len(chunks) == 0 {
		return s
	}
	s.TokMin = chunks[0].Tokens
	var tokSum, charSum int
	for _, c := range chunks {
		tokSum += c.Tokens
		charSum += len([]rune(c.Text))
		if c.Tokens < s.TokMin {
			s.TokMin = c.Tokens
		}
		if c.Tokens > s.TokMax {
			s.TokMax = c.Tokens
		}
		if endsAtBoundary(c.Text) {
			s.Intact++
		}
	}
	s.TokAvg = float64(tokSum) / float64(len(chunks))
	s.CharAvg = float64(charSum) / float64(len(chunks))
	s.IntactPct = 100 * float64(s.Intact) / float64(len(chunks))
	return s
}

// endsAtBoundary — эвристика «целостной границы»: чанк заканчивается концом
// предложения (.!?), закрывающей скобкой блока } или разделителем :;. Грубо, но
// показывает контраст: fixed-size часто рвёт на полуслове, structural — почти нет.
func endsAtBoundary(text string) bool {
	t := strings.TrimRight(strings.TrimSpace(text), "`)\"'")
	if t == "" {
		return false
	}
	switch t[len(t)-1] {
	case '.', '!', '?', '}', ':', ';':
		return true
	}
	return false
}

// compareReport — текстовая таблица сравнения двух стратегий (stdout и comparison.md).
func compareReport(fixed, structural Stats) string {
	var b strings.Builder
	b.WriteString("Сравнение стратегий chunking\n")
	b.WriteString("============================\n\n")
	row := func(name, a, c string) { fmt.Fprintf(&b, "%-24s | %-20s | %-20s\n", name, a, c) }
	row("Метрика", "fixed-size", "structural")
	b.WriteString(strings.Repeat("-", 70) + "\n")
	row("Чанков", itoa(fixed.Count), itoa(structural.Count))
	row("Токенов — среднее", f1(fixed.TokAvg), f1(structural.TokAvg))
	row("Токенов — мин/макс", fmt.Sprintf("%d / %d", fixed.TokMin, fixed.TokMax), fmt.Sprintf("%d / %d", structural.TokMin, structural.TokMax))
	row("Символов — среднее", f1(fixed.CharAvg), f1(structural.CharAvg))
	row("Целостных границ", fmt.Sprintf("%.0f%%", fixed.IntactPct), fmt.Sprintf("%.0f%%", structural.IntactPct))
	b.WriteString("\n")
	b.WriteString("Вывод: fixed-size даёт равномерные по размеру чанки, но рвёт мысль на стыках\n")
	b.WriteString("(низкая доля целостных границ). structural даёт чанки-«законченные мысли»\n")
	b.WriteString("(заголовок/раздел/декларация), но размер плавает. Для RAG обычно лучше\n")
	b.WriteString("structural + умеренный overlap на длинных разделах.\n")
	return b.String()
}

func itoa(n int) string   { return fmt.Sprintf("%d", n) }
func f1(x float64) string { return fmt.Sprintf("%.1f", x) }
