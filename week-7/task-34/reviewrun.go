package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// День 32. Раннер ревьюера: связывает reviewPR с внешним миром.
//
// Архитектура — self-contained в раннере (выбранный вариант): бинарь запускается
// ВНУТРИ GitHub Action на событие pull_request, читает diff, гоняет reviewPR по
// предсобранному корпусу и постит ревью комментом обратно в PR через GITHUB_TOKEN.
// Ни своего хостинга, ни туннеля — Action сам поднимает раннер и сам даёт токен.
//
// Тот же бинарь работает и ЛОКАЛЬНО (флаг -review32): читает diff из файла/стдина,
// печатает ревью в терминал. Это путь для разработки и для видео.

// ReviewInput — откуда взять diff и куда (если куда-то) постить.
type ReviewInput struct {
	Diff     string // сам diff
	Repo     string // "owner/repo" (для постинга в GitHub)
	PRNumber int    // номер PR
	Token    string // GITHUB_TOKEN
	DryRun   bool   // true = только печать, без постинга
	StrictCI bool   // true = ненулевой код выхода при наличии bug/architecture
	APIBase  string // база GitHub API (для тестов; по умолчанию api.github.com)
}

// runReview32 — локальный/демо-режим: diff из -diff-file или stdin, ревью в терминал.
func runReview32(ctx context.Context, a *Agent, r *Retriever, cfg ReviewConfig, diffPath string, demo bool) error {
	diff, err := readDiff(diffPath)
	if err != nil {
		return err
	}
	// Демо-режим для видео без озвучки: если diff не подали, берём встроенный образец
	// с типовыми проблемами (пароль в лог, SQL-инъекция, игнор ошибки). Так одна
	// команда `-review-demo` показывает весь пайплайн, ничего не готовя заранее.
	if strings.TrimSpace(diff) == "" {
		if demo {
			diff = sampleBadDiff
		} else {
			return fmt.Errorf("пустой diff (укажи -diff-file или подай diff в stdin)")
		}
	}

	ragBanner(0, "АВТОМАТИЗАЦИЯ РЕВЬЮ КОДА · день 32")
	fmt.Println("Пайплайн: PR diff → RAG (документация + код проекта) → LLM-ревьюер → severity.")
	fmt.Println("Ревьюер — тот же агент (RAG-конвейер дней 21–31) + постинг в PR.")

	// Шаг 1 — разбор diff (видно, что именно ревьюим)
	files := parseDiff(diff)
	ragBanner(1, "РАЗБОР DIFF")
	for _, f := range files {
		path := f.Path
		if path == "" {
			path = f.OldPath
		}
		tag := "изменён"
		if f.IsNew {
			tag = "новый файл"
		} else if f.IsDeleted {
			tag = "удалён"
		}
		fmt.Printf("  %s (%s, +%d/-%d строк)\n", path, tag, len(f.Added), len(f.Removed))
	}
	if demo {
		fmt.Println("\n  Содержимое (встроенный образец с типовыми проблемами):")
		for _, ln := range files[0].Added {
			fmt.Printf("    + %s\n", ln)
		}
	}

	// Шаг 2 — RAG (видно, что подключается контекст проекта)
	ragBanner(2, "RAG: КОНТЕКСТ ПРОЕКТА")
	fmt.Printf("  Индекс: %s\n", r.Info())
	fmt.Println("  Под каждый изменённый файл строится запрос к корпусу (конвенции + смежный код).")

	// Шаг 3 — само ревью
	ragBanner(3, "РЕВЬЮ (LLM по diff + контекст проекта)")
	rev, err := reviewPR(ctx, a, r, diff, cfg)
	if err != nil {
		return err
	}
	md := renderReviewMarkdown(rev, changedPaths(files))
	fmt.Println(md)

	// Шаг 4 — итог (видно, что severity и блокеры работают)
	ragBanner(4, "ИТОГ")
	bugs, arch, sug := groupBySeverity(rev.Findings)
	fmt.Printf("  Найдено замечаний: %d (🔴 баги: %d · 🟠 архитектура: %d · 🟡 рекомендации: %d)\n",
		len(rev.Findings), len(bugs), len(arch), len(sug))
	fmt.Printf("  Блокеры для CI (bug/architecture): %v\n", rev.HasBlockers())
	fmt.Printf("  Токены: вход %d · выход %d\n", rev.rawUsage.Input, rev.rawUsage.Output)
	if demo {
		fmt.Println("\n  В реактивном режиме (GitHub Action) этот же текст уходит комментом в PR.")
	}
	return nil
}

// sampleBadDiff — встроенный образец для -review-demo: новый файл с типовыми
// проблемами, которые ревьюер должен поймать (для немого видео).
const sampleBadDiff = `diff --git a/week-7/task-34/badcode.go b/week-7/task-34/badcode.go
new file mode 100644
--- /dev/null
+++ b/week-7/task-34/badcode.go
@@ -0,0 +1,6 @@
+package main
+import "database/sql"
+func BadLogin(db *sql.DB, user, pass string) {
+	println("login user=" + user + " pass=" + pass)
+	db.Query("SELECT * FROM users WHERE name='" + user + "'")
+}
`

// runReviewCI — реактивный режим внутри GitHub Action: diff из события, ревью
// комментом в PR. Возвращает ошибку, если StrictCI и есть блокеры.
func runReviewCI(ctx context.Context, a *Agent, r *Retriever, cfg ReviewConfig, in ReviewInput) error {
	files := parseDiff(in.Diff)
	if len(files) == 0 {
		fmt.Println("diff пуст — ревьюить нечего, выходим без коммента")
		return nil
	}
	rev, err := reviewPR(ctx, a, r, in.Diff, cfg)
	if err != nil {
		return err
	}
	md := renderReviewMarkdown(rev, changedPaths(files))

	if in.DryRun || in.Token == "" || in.Repo == "" || in.PRNumber == 0 {
		fmt.Println("[dry-run] коммент не постится, вот его содержимое:")
		fmt.Println()
		fmt.Println(md)
	} else {
		if err := postPRComment(ctx, in, md); err != nil {
			return fmt.Errorf("постинг коммента: %w", err)
		}
		fmt.Printf("Ревью опубликовано в %s#%d\n", in.Repo, in.PRNumber)
	}

	if in.StrictCI && rev.HasBlockers() {
		return fmt.Errorf("ревью нашло блокеры (bug/architecture) — проваливаем проверку CI")
	}
	return nil
}

// postPRComment публикует коммент в PR через GitHub REST API.
// PR-комменты создаются через issues endpoint (PR — это тоже issue).
func postPRComment(ctx context.Context, in ReviewInput, body string) error {
	base := in.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/issues/%d/comments", base, in.Repo, in.PRNumber)
	payload, _ := json.Marshal(map[string]string{"body": body})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+in.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API %d: %s", resp.StatusCode, oneLine(string(b), 200))
	}
	return nil
}

// readDiff читает diff из файла, а если путь пуст — из stdin.
func readDiff(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("чтение diff-файла: %w", err)
		}
		return string(b), nil
	}
	// stdin (например: git diff main...HEAD | ... -review32)
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return "", nil // это терминал, данных на входе нет
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// reviewInputFromEnv собирает ReviewInput из окружения GitHub Action.
// Diff берётся из готового файла (Action его формирует шагом раньше — см. .github/workflows).
func reviewInputFromEnv(diffPath string, dryRun, strict bool) (ReviewInput, error) {
	diff, err := readDiff(diffPath)
	if err != nil {
		return ReviewInput{}, err
	}
	in := ReviewInput{
		Diff:     diff,
		Repo:     os.Getenv("GITHUB_REPOSITORY"), // "owner/repo", даёт Action
		Token:    os.Getenv("GITHUB_TOKEN"),      // secrets.GITHUB_TOKEN
		DryRun:   dryRun,
		StrictCI: strict,
	}
	if n := os.Getenv("PR_NUMBER"); n != "" {
		in.PRNumber, _ = strconv.Atoi(n)
	}
	return in, nil
}
