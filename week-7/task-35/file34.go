package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// День 34. АССИСТЕНТ ДЛЯ РАБОТЫ С ФАЙЛАМИ ПРОЕКТА.
//
// Отличие от всех предыдущих дней недели: до сих пор каждый MCP-сервер был ТОЛЬКО
// НА ЧТЕНИЕ (git, crm) или писал в свою песочницу (notes). Здесь ассистент впервые
// МЕНЯЕТ файлы проекта. Значит впервые за неделю нужен настоящий human-in-the-loop
// гейт на опасную операцию — тот самый, что требовала лекция недели 7 и который мы
// до сих пор честно обходили архитектурой (read-only). Здесь он уместен, а не
// искусственен.
//
// Ключевые требования задания и как они выполнены:
//   • «ассистент САМ инициирует работу с файлами» → задача ставится ЦЕЛЬЮ
//     («собери changelog», «проверь инварианты»), а не «открой файл X». Какие файлы
//     читать и что писать — решает сам конвейер.
//   • «работает с 2–3 файлами» → changelog читает git-лог + существующий CHANGELOG;
//     invariant-check читает набор .go + файл правил.
//   • «изменения сохраняются ИЛИ выводятся как diff» → всегда сначала diff, запись
//     только после подтверждения (гейт).
//   • «результат воспроизводим» → флаг -apply=false (по умолчанию) печатает diff и
//     ничего не пишет; -apply включает интерактивный гейт y/n.
//
// Реализованы ДВА сценария (задание просит минимум два):
//   1. changelog — сгенерировать/обновить CHANGELOG.md из истории git.
//   2. invariant-check — проверить файлы проекта на соответствие инвариантам (день 13),
//      и, если найдены нарушения, предложить дописать отчёт в docs/.

// FileEdit — предложение изменить один файл. Именно его показывает гейт.
type FileEdit struct {
	Path   string // путь относительно корня проекта
	Old    string // текущее содержимое ("" — файл новый)
	New    string // предлагаемое содержимое
	Reason string // зачем правка (одна строка для гейта)
	IsNew  bool
}

// Changed — есть ли фактическое изменение.
func (e FileEdit) Changed() bool { return e.Old != e.New }

// runFile34 — демонстрация для видео: оба сценария по шагам, гейт в режиме показа diff.
func runFile34(ctx context.Context, a *Agent, r *Retriever, m *MCPTools, scope string, apply bool) error {
	ragBanner(0, "АССИСТЕНТ ДЛЯ РАБОТЫ С ФАЙЛАМИ ПРОЕКТА · день 34")
	fmt.Println("Отличие недели: до сих пор все серверы были на ЧТЕНИЕ. Здесь ассистент МЕНЯЕТ")
	fmt.Println("файлы проекта — поэтому впервые появляется гейт подтверждения перед записью.")
	fmt.Printf("Режим: %s\n", applyMode(apply))
	fmt.Println("Задача ставится ЦЕЛЬЮ (не «открой файл X»): ассистент сам решает, что читать и писать.")

	// Сценарий 1 — changelog из git.
	ragBanner(1, "СЦЕНАРИЙ 1: собрать CHANGELOG из истории git")
	fmt.Println("ЦЕЛЬ: «Обнови changelog по последним коммитам».")
	edit1, err := changelogScenario(ctx, a, m, scope)
	if err != nil {
		fmt.Printf("  сценарий 1 недоступен: %v\n", err)
	} else {
		gate(ctx, edit1, apply)
	}

	// Сценарий 2 — invariant-check.
	ragBanner(2, "СЦЕНАРИЙ 2: проверить файлы на инварианты и записать отчёт")
	fmt.Println("ЦЕЛЬ: «Проверь, что код проекта не нарушает наши инварианты, и зафиксируй отчёт».")
	edit2, err := invariantScenario(ctx, a, r, scope)
	if err != nil {
		fmt.Printf("  сценарий 2 недоступен: %v\n", err)
	} else {
		gate(ctx, edit2, apply)
	}

	ragBanner(3, "ИТОГ")
	fmt.Println("Оба сценария: ассистент сам нашёл файлы, сам подготовил изменение, показал diff.")
	fmt.Printf("Запись на диск: %s.\n", applyOutcome(apply))
	fmt.Println("Воспроизводимо: без -apply ничего не пишется, только diff.")
	return nil
}

// ── Сценарий 1: changelog из git ─────────────────────────────────────────────

func changelogScenario(ctx context.Context, a *Agent, m *MCPTools, scope string) (FileEdit, error) {
	if m == nil {
		return FileEdit{}, fmt.Errorf("MCP недоступен — git-лог не прочитать")
	}
	// Ассистент САМ решает, что ему нужен git-лог, и читает его (не пользователь
	// указывает файл). Прямой вызов тула, 0 токенов.
	logOut, isErr := m.Call(ctx, "git_log", jsonArgs(map[string]any{"limit": 15}))
	if isErr {
		return FileEdit{}, fmt.Errorf("git_log: %s", oneLine(logOut, 100))
	}

	path := filepath.Join(scope, "CHANGELOG.md")
	old := readFileOrEmpty(path)

	// Модель превращает сырой git-лог в человекочитаемый changelog. Это генерация
	// нового файла из «сырья» — ровно сценарий «сгенерировать новый файл» из задания.
	sys := "Ты ведёшь CHANGELOG проекта. На вход — вывод git log (хеш, дата, автор, тема). " +
		"Сгруппируй изменения в аккуратный markdown-changelog: заголовок '# Changelog', " +
		"затем секции по смыслу (Features / Fixes / Docs / Refactor), маркированные пункты. " +
		"Пиши по темам коммитов, кратко и по делу, без хешей. Только markdown, без пояснений."
	user := "Вот git log:\n\n" + logOut
	if old != "" {
		user += "\n\nТекущий CHANGELOG (обнови/дополни, не дублируя уже описанное):\n\n" + old
	}
	out, _, err := a.gen.Complete(ctx, sys, []Msg{{Role: roleUser, Text: user}}, CompleteOpts{MaxTokens: 1500})
	if err != nil {
		return FileEdit{}, err
	}

	return FileEdit{
		Path:   path,
		Old:    old,
		New:    strings.TrimSpace(out) + "\n",
		Reason: "сгенерирован из последних 15 коммитов git",
		IsNew:  old == "",
	}, nil
}

// ── Сценарий 2: invariant-check ──────────────────────────────────────────────

func invariantScenario(ctx context.Context, a *Agent, r *Retriever, scope string) (FileEdit, error) {
	// Правила уровня КОДА (день 13 задавал инварианты для диалога через стоп-слова;
	// здесь нам нужны правила для исходников, поэтому используем свой небольшой набор
	// паттернов). Каждый инвариант — подстрока, которой в коде быть не должно.
	rules := defaultCodeRules()

	// Ассистент САМ выбирает, какие файлы проверять: берёт .go верхнего уровня проекта.
	files, err := goFilesIn(scope)
	if err != nil || len(files) == 0 {
		return FileEdit{}, fmt.Errorf("не нашёл .go для проверки в %s", scope)
	}

	var violations []string
	checked := 0
	for _, f := range files {
		body := readFileOrEmpty(f)
		if body == "" {
			continue
		}
		checked++
		for _, rule := range rules {
			if lineHasOutsideComment(body, rule.pattern) {
				violations = append(violations,
					fmt.Sprintf("- `%s`: %s (найдено `%s`)", filepath.Base(f), rule.desc, rule.pattern))
			}
		}
	}

	// Отчёт — новый файл в docs/. Это сценарий «сгенерировать новый файл (ADR/отчёт)».
	path := filepath.Join(scope, "docs", "invariant-report.md")
	old := readFileOrEmpty(path)

	var b strings.Builder
	b.WriteString("# Отчёт проверки инвариантов\n\n")
	fmt.Fprintf(&b, "Проверено файлов: %d.\n\n", checked)
	if len(violations) == 0 {
		b.WriteString("Нарушений не найдено — код соответствует инвариантам проекта.\n")
	} else {
		fmt.Fprintf(&b, "Найдено потенциальных нарушений: %d.\n\n", len(violations))
		b.WriteString(strings.Join(dedupeStrings(violations), "\n"))
		b.WriteString("\n")
	}

	return FileEdit{
		Path:   path,
		Old:    old,
		New:    b.String(),
		Reason: fmt.Sprintf("проверено %d файлов, нарушений: %d", checked, len(violations)),
		IsNew:  old == "",
	}, nil
}

// codeRule — правило уровня исходника: подстрока, которой не должно быть.
type codeRule struct {
	pattern string
	desc    string
}

func defaultCodeRules() []codeRule {
	return []codeRule{
		{"panic(", "не использовать panic() в обычном коде — возвращать error"},
		{"TODO", "не оставлять TODO в коммите"},
		{"FIXME", "не оставлять FIXME в коммите"},
	}
}

// lineHasOutsideComment ищет паттерн в строках, которые не являются комментарием и
// где паттерн не находится внутри строкового литерала. Второе важно, потому что сам
// этот файл перечисляет паттерны правил ("panic(", "TODO") как ДАННЫЕ в кавычках — и
// без такой проверки чек ловил бы собственные определения правил как нарушения.
func lineHasOutsideComment(body, pattern string) bool {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") {
			continue
		}
		idx := strings.Index(ln, pattern)
		if idx < 0 {
			continue
		}
		// Паттерн в кавычках (строковый литерал) — это данные, а не код. Пропускаем.
		if quotesBefore(ln, idx)%2 == 1 {
			continue
		}
		return true
	}
	return false
}

// quotesBefore считает двойные кавычки до позиции i (грубая, но достаточная эвристика
// «внутри строкового литерала ли мы»).
func quotesBefore(s string, i int) int {
	n := 0
	for _, r := range s[:i] {
		if r == '"' || r == '`' {
			n++
		}
	}
	return n
}

// ── Гейт подтверждения ───────────────────────────────────────────────────────

// gate — сердце дня 34. Показывает diff и, если apply=true, спрашивает подтверждение
// перед записью. Это тот самый human-in-the-loop, которого требовала лекция.
func gate(ctx context.Context, e FileEdit, apply bool) {
	if !e.Changed() {
		fmt.Printf("\n[%s] изменений нет — файл уже актуален, писать нечего.\n", e.Path)
		return
	}
	tag := "изменить"
	if e.IsNew {
		tag = "создать"
	}
	fmt.Printf("\nПРЕДЛАГАЮ %s: %s\n", strings.ToUpper(tag), e.Path)
	fmt.Printf("Причина: %s\n", e.Reason)
	fmt.Println(strings.Repeat("─", 60))
	fmt.Print(renderDiff(e))
	fmt.Println(strings.Repeat("─", 60))

	if !apply {
		fmt.Println("Режим предпросмотра (-apply=false): файл НЕ записан. Это и есть воспроизводимый diff.")
		return
	}

	// apply=true → настоящий гейт: спросить человека.
	if !confirmGate(e.Path) {
		fmt.Println("Отклонено пользователем — файл не тронут.")
		return
	}
	if err := writeFileMkdir(e.Path, e.New); err != nil {
		fmt.Printf("ошибка записи: %v\n", err)
		return
	}
	fmt.Printf("Записано: %s (%d байт).\n", e.Path, len(e.New))
}

// stdinReader — ЕДИНЫЙ на весь процесс ридер stdin. Раньше confirmGate создавал новый
// bufio.Scanner на каждый вызов, и это был баг: Scanner читает с упреждающей
// буферизацией, поэтому первый сканер захватывал из потока больше одной строки, а
// второй вызов терял свой ввод (ответ «y» на второй гейт читался как пустой → отказ).
// Один общий буферизированный ридер эту гонку убирает.
var stdinReader = bufio.NewReader(os.Stdin)

// confirmGate спрашивает подтверждение в терминале. Любой ответ кроме y/yes/д/да — отказ.
func confirmGate(path string) bool {
	fmt.Printf("Записать изменения в %s? [y/N] ", path)
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes" || ans == "д" || ans == "да"
}

// renderDiff — простой построчный diff (без внешних библиотек): удалённые строки со
// знаком «-», добавленные со знаком «+», для нового файла — все строки как добавленные.
func renderDiff(e FileEdit) string {
	if e.IsNew {
		var b strings.Builder
		for _, ln := range strings.Split(strings.TrimRight(e.New, "\n"), "\n") {
			b.WriteString("+ " + ln + "\n")
		}
		return b.String()
	}
	oldLines := strings.Split(e.Old, "\n")
	newLines := strings.Split(e.New, "\n")
	var b strings.Builder
	i, j := 0, 0
	for i < len(oldLines) || j < len(newLines) {
		switch {
		case i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j]:
			b.WriteString("  " + oldLines[i] + "\n")
			i++
			j++
		case j < len(newLines) && (i >= len(oldLines) || !contains(oldLines, newLines[j])):
			b.WriteString("+ " + newLines[j] + "\n")
			j++
		case i < len(oldLines):
			b.WriteString("- " + oldLines[i] + "\n")
			i++
		default:
			j++
		}
	}
	return b.String()
}

// ── REPL: /file <цель> ───────────────────────────────────────────────────────

// runFile — интерактивный режим: пользователь ставит ЦЕЛЬ, ассистент выбирает сценарий.
func runFile(ctx context.Context, a *Agent, r *Retriever, m *MCPTools, goal, scope string, apply bool) error {
	g := strings.ToLower(goal)
	var edit FileEdit
	var err error
	switch {
	case strings.Contains(g, "changelog") || strings.Contains(g, "коммит") || strings.Contains(g, "измен"):
		edit, err = changelogScenario(ctx, a, m, scope)
	case strings.Contains(g, "инвариант") || strings.Contains(g, "правил") || strings.Contains(g, "проверь"):
		edit, err = invariantScenario(ctx, a, r, scope)
	default:
		fmt.Println("Не понял цель. Поддерживаю: «обнови changelog» или «проверь инварianты».")
		return nil
	}
	if err != nil {
		return err
	}
	gate(ctx, edit, apply)
	return nil
}

// ── вспомогательное ──────────────────────────────────────────────────────────

func applyMode(apply bool) string {
	if apply {
		return "-apply=true (после diff спрошу подтверждение y/N перед записью)"
	}
	return "-apply=false (только предпросмотр diff, ничего не пишу — воспроизводимо)"
}

func applyOutcome(apply bool) string {
	if apply {
		return "по подтверждению y/N"
	}
	return "выключена (режим предпросмотра)"
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func writeFileMkdir(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// goFilesIn возвращает .go-файлы верхнего уровня каталога (без вложенных серверов).
func goFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func dedupeStrings(ss []string) []string {
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
