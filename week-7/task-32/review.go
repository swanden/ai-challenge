package task_32

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// День 32. АВТОМАТИЗАЦИЯ РЕВЬЮ КОДА.
//
// Пайплайн (реактивный, запускается на PR): diff → RAG (документация + код) →
// текст ревью → коммент в PR. Здесь — «мозг» ревьюера, без привязки к платформе.
// Как его дёргает GitHub Action и как ответ уходит в PR — в reviewrun.go.
//
// Почему это НАШ агент, а не отдельный скрипт (развилка дня, вариант A):
// у нас уже есть вылизанный за недели 5–7 конвейер — retrieveAdvanced (rewrite →
// bi-encoder → порог → MMR → LLM-реранк) и Completer (шов облако/локаль). Ревьюер
// их переиспользует целиком. Это ровно то «целостное», о котором говорил Гладков:
// код-ассистент, который понимает СВОЙ проект по его же докам.
//
// Ключевая идея RAG в ревью: diff сам по себе — это «что изменилось», но не «как
// в этом проекте ПРИНЯТО». Реальную ценность даёт контекст: конвенции из docs/,
// смежный код, который затронутые функции вызывают. Поэтому под каждый изменённый
// файл строим точечный запрос к корпусу и подмешиваем найденное в промпт ревьюера.

// ReviewConfig — настройки прогона ревью.
type ReviewConfig struct {
	Rerank       RerankConfig // тот же retrieval, что и у /help
	MaxFiles     int          // сколько файлов ревьюим (крупнейшие по числу правок вперёд)
	MaxDiffChars int          // потолок diff'а в промпте — большой PR не должен снести контекст
	RAGPerFile   int          // сколько чанков корпуса тянуть под каждый файл
}

func defaultReviewConfig(rcfg RerankConfig) ReviewConfig {
	return ReviewConfig{
		Rerank:       rcfg,
		MaxFiles:     10,
		MaxDiffChars: 12000,
		RAGPerFile:   3,
	}
}

// ReviewFinding — одно замечание ревьюера.
type ReviewFinding struct {
	File     string `json:"file"`
	Severity string `json:"severity"` // "bug" | "architecture" | "suggestion"
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// Review — результат ревью PR.
type Review struct {
	Summary  string          `json:"summary"`
	Findings []ReviewFinding `json:"findings"`
	Sources  []string        `json:"-"` // какие чанки корпуса подмешивались (для прозрачности)
	rawUsage GenUsage
}

// HasBlockers — есть ли замечания уровня bug/architecture (для строгого режима CI).
func (r Review) HasBlockers() bool {
	for _, f := range r.Findings {
		if f.Severity == "bug" || f.Severity == "architecture" {
			return true
		}
	}
	return false
}

// reviewPR — полный конвейер ревью. Возвращает структурированный Review.
func reviewPR(ctx context.Context, a *Agent, r *Retriever, diff string, cfg ReviewConfig) (Review, error) {
	files := parseDiff(diff)
	if len(files) == 0 {
		return Review{Summary: "Пустой или неразобранный diff — ревьюить нечего."}, nil
	}

	// Крупнейшие изменения — вперёд; лимитируем число файлов.
	sort.SliceStable(files, func(i, j int) bool { return files[i].AddedCount() > files[j].AddedCount() })
	if len(files) > cfg.MaxFiles {
		files = files[:cfg.MaxFiles]
	}

	// 1. RAG-контекст под изменённые файлы. Под каждый файл — точечный запрос:
	//    «конвенции и смежный код для <path>» + сами добавленные строки как сигнал.
	ragCtx, sources := a.gatherReviewContext(ctx, r, files, cfg)

	// 2. Собираем промпт ревьюера.
	sys := reviewSystemPrompt()
	user := buildReviewPrompt(files, ragCtx, cfg.MaxDiffChars)

	// 3. Один вызов Completer (шов облако/локаль — ревью поедет и на локали, если попросят).
	out, usage, err := a.gen.Complete(ctx, sys, []Msg{{Role: roleUser, Text: user}}, CompleteOpts{MaxTokens: 2000})
	if err != nil {
		return Review{}, fmt.Errorf("ревьюер: %w", err)
	}

	rev, err := parseReviewJSON(out)
	if err != nil {
		// Не роняем пайплайн из-за кривого JSON: отдаём текст как есть, помечая деградацию.
		return Review{
			Summary:  "Ревьюер вернул неструктурированный ответ (JSON не распарсился) — привожу как есть:\n\n" + strings.TrimSpace(out),
			Sources:  sources,
			rawUsage: usage,
		}, nil
	}
	rev.Sources = sources
	rev.rawUsage = usage
	return rev, nil
}

// gatherReviewContext строит RAG-контекст: под каждый изменённый файл — свой запрос
// к корпусу. Дедуплицирует чанки между файлами (смежные файлы часто тянут одно и то же).
func (a *Agent) gatherReviewContext(ctx context.Context, r *Retriever, files []FileDiff, cfg ReviewConfig) (string, []string) {
	perFile := cfg.Rerank
	perFile.TopK = cfg.RAGPerFile

	seen := map[string]bool{}
	var blocks []string
	var sources []string

	for _, f := range files {
		path := f.Path
		if path == "" {
			path = f.OldPath
		}
		// Запрос: путь + первые добавленные строки. Путь ловит доки/конвенции по файлу,
		// добавленные строки — смежный код по смыслу изменения.
		query := "конвенции и связанный код для " + path
		if len(f.Added) > 0 {
			query += "\n" + strings.Join(f.Added[:min(len(f.Added), 12)], "\n")
		}

		hits, _, err := retrieveAdvanced(ctx, a, r, query, perFile)
		if err != nil || len(hits) == 0 {
			continue
		}
		for _, h := range hits {
			if seen[h.Chunk.ID] {
				continue
			}
			seen[h.Chunk.ID] = true
			sources = append(sources, fmt.Sprintf("%s · %s", h.Chunk.Source, h.Chunk.Section))
			blocks = append(blocks, fmt.Sprintf("[%s · %s]\n%s", h.Chunk.Source, h.Chunk.Section, h.Chunk.Text))
		}
	}
	if len(blocks) == 0 {
		return "(в документации и коде проекта не нашлось релевантного контекста)", nil
	}
	return strings.Join(blocks, "\n\n"), sources
}

// reviewSystemPrompt — инструкция ревьюеру. Требуем строгий JSON и жёстко ограничиваем
// роль: ревьюим ТОЛЬКО diff, опираясь на контекст проекта, без выдумок.
func reviewSystemPrompt() string {
	return "Ты — ревьюер кода в этом проекте. Тебе дают DIFF пул-реквеста и КОНТЕКСТ ПРОЕКТА " +
		"(фрагменты документации и кода из RAG). Твоя задача — найти в diff потенциальные баги, " +
		"архитектурные проблемы и предложить улучшения, ОПИРАЯСЬ на конвенции проекта из контекста.\n\n" +
		"Правила:\n" +
		"- Ревьюй ТОЛЬКО изменения из diff, а не весь проект.\n" +
		"- Если контекст проекта показывает принятую конвенцию, которую diff нарушает — это findings.\n" +
		"- Не выдумывай проблемы ради галочки. Нет замечаний — так и скажи в summary, findings пустой.\n" +
		"- severity строго одно из: \"bug\" (сломается/некорректно), \"architecture\" (структурная " +
		"проблема, нарушение конвенции), \"suggestion\" (улучшение, не блокер).\n\n" +
		"Верни ТОЛЬКО JSON без пояснений и без ```-обёрток:\n" +
		`{"summary":"...","findings":[{"file":"...","severity":"bug|architecture|suggestion","title":"...","detail":"..."}]}`
}

func buildReviewPrompt(files []FileDiff, ragCtx string, maxDiff int) string {
	var b strings.Builder
	b.WriteString("КОНТЕКСТ ПРОЕКТА (документация + код из RAG):\n")
	b.WriteString(ragCtx)
	b.WriteString("\n\n===\n\nDIFF ПУЛ-РЕКВЕСТА:\n")

	var diffB strings.Builder
	for _, f := range files {
		path := f.Path
		if path == "" {
			path = f.OldPath
		}
		tag := "изменён"
		switch {
		case f.IsNew:
			tag = "новый файл"
		case f.IsDeleted:
			tag = "удалён"
		}
		fmt.Fprintf(&diffB, "\n--- %s (%s, +%d/-%d) ---\n%s\n", path, tag, len(f.Added), len(f.Removed), f.Hunk)
	}
	diff := diffB.String()
	if len([]rune(diff)) > maxDiff {
		diff = string([]rune(diff)[:maxDiff]) + "\n… [diff усечён по лимиту промпта]"
	}
	b.WriteString(diff)
	return b.String()
}

// parseReviewJSON терпимо разбирает ответ модели: снимает ```-обёртки, вычленяет
// объект по фигурным скобкам (модель иногда добавляет преамбулу).
func parseReviewJSON(s string) (Review, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndexByte(s, '}'); j >= 0 {
		s = s[:j+1]
	}
	var rev Review
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &rev); err != nil {
		return Review{}, err
	}
	return rev, nil
}

// renderReviewMarkdown — ревью в виде markdown-коммента для PR.
func renderReviewMarkdown(rev Review, changed []string) string {
	var b strings.Builder
	b.WriteString("## 🤖 AI-ревью\n\n")
	if rev.Summary != "" {
		b.WriteString(rev.Summary + "\n\n")
	}

	if len(rev.Findings) == 0 {
		b.WriteString("_Замечаний нет._\n")
	} else {
		bugs, arch, sug := groupBySeverity(rev.Findings)
		writeGroup(&b, "🔴 Потенциальные баги", bugs)
		writeGroup(&b, "🟠 Архитектурные проблемы", arch)
		writeGroup(&b, "🟡 Рекомендации", sug)
	}

	if len(changed) > 0 {
		fmt.Fprintf(&b, "\n<sub>Проверено файлов: %d · %s</sub>\n", len(changed), strings.Join(changed, ", "))
	}
	if len(rev.Sources) > 0 {
		fmt.Fprintf(&b, "<sub>Контекст из проекта: %s</sub>\n", strings.Join(dedupe(rev.Sources), "; "))
	}
	return b.String()
}

func groupBySeverity(fs []ReviewFinding) (bugs, arch, sug []ReviewFinding) {
	for _, f := range fs {
		switch f.Severity {
		case "bug":
			bugs = append(bugs, f)
		case "architecture":
			arch = append(arch, f)
		default:
			sug = append(sug, f)
		}
	}
	return
}

func writeGroup(b *strings.Builder, title string, fs []ReviewFinding) {
	if len(fs) == 0 {
		return
	}
	fmt.Fprintf(b, "### %s\n\n", title)
	for _, f := range fs {
		if f.File != "" {
			fmt.Fprintf(b, "- **%s** (`%s`): %s\n", f.Title, f.File, f.Detail)
		} else {
			fmt.Fprintf(b, "- **%s**: %s\n", f.Title, f.Detail)
		}
	}
	b.WriteString("\n")
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
