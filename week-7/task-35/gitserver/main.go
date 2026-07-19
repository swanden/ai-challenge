package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// День 31. ТРЕТИЙ MCP-сервер — git-контекст проекта ("git").
//
// Зачем отдельный сервер, а не пакет в агенте: ровно та же причина, что у
// weatherserver (день 17) и notesserver (день 20) — граница процесса. Агент не
// умеет «запускать git», он умеет «звать тул по имени». Сервер можно подменить,
// переписать на другой ЯП, вынести на другую машину — контракт (JSON-схема тула)
// не меняется. Это и есть смысл MCP.
//
// ЖЁСТКОЕ ПРАВИЛО СЕРВЕРА: ТОЛЬКО ЧТЕНИЕ.
// Лекция недели 7 отдельно проговаривает опасные операции (rm, git push, конфиги)
// и требует подтверждения у человека. Мы решаем эту проблему на уровень раньше —
// АРХИТЕКТУРНО: сервер физически не умеет писать. Список подкоманд — белый список
// (branch/status/ls-files/diff/log/rev-parse), запись невозможна не потому что мы
// «спросили пользователя», а потому что такого кода здесь нет. Значит и HITL-гейт
// на день 31 не нужен — нечего гейтить. (День 32, где ассистент пойдёт постить
// комменты в PR, — вот там гейт понадобится, и это будет честная новая работа.)
//
// Транспорт: stdio (сабпроцесс агента), как у обоих предыдущих серверов.

const (
	maxOutChars = 8000 // потолок ответа тула: diff большого PR не должен снести контекст модели
	gitTimeout  = 20 * time.Second
)

func main() {
	repo, err := repoRoot()
	if err != nil {
		// Не фаталим: сервер обязан подняться и честно ответить ошибкой на вызов,
		// иначе агент решит, что весь MCP лежит, и потеряет weather/notes.
		log.Printf("git-mcp: репозиторий не найден: %v", err)
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "git-mcp", Version: "0.1.0"}, nil)

	// ── git_branch — МИНИМУМ задания дня 31 ───────────────────────────────────
	// Возвращает не только ветку: HEAD, upstream и «грязность» дерева — это тот
	// самый «контекст проекта», который /help подмешивает в КАЖДЫЙ ответ.
	s.AddTool(&mcp.Tool{
		Name: "git_branch",
		Description: "Текущее состояние репозитория: активная ветка, короткий SHA HEAD, upstream, " +
			"число незакоммиченных файлов. Без параметров.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if repo == "" {
			return errText(fmt.Errorf("git-репозиторий не найден")), nil
		}
		out, err := branchInfo(ctx, repo)
		if err != nil {
			return errText(err), nil
		}
		return okText(out), nil
	})

	// ── git_files — «список файлов» (опция задания) ───────────────────────────
	s.AddTool(&mcp.Tool{
		Name: "git_files",
		Description: "Список файлов под контролем git (git ls-files). Можно сузить префиксом пути " +
			"и ограничить количество. Полезно для вопросов о структуре проекта.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Префикс пути, напр. 'week-7/task-35' или 'week-7/task-35/docs'. Пусто = весь репозиторий."},
				"limit":{"type":"integer","description":"Максимум строк в ответе (по умолчанию 200)"}
			}
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if repo == "" {
			return errText(fmt.Errorf("git-репозиторий не найден")), nil
		}
		var in struct {
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return errText(err), nil
			}
		}
		out, err := listFiles(ctx, repo, in.Path, in.Limit)
		if err != nil {
			return errText(err), nil
		}
		return okText(out), nil
	})

	// ── git_diff — «diff» (опция задания; фундамент дня 32) ────────────────────
	s.AddTool(&mcp.Tool{
		Name: "git_diff",
		Description: "Diff репозитория. По умолчанию — незакоммиченные изменения рабочего дерева против HEAD. " +
			"Можно задать ref (напр. 'main', 'HEAD~1' или диапазон 'main...HEAD'), сузить путём, " +
			"или запросить только сводку по файлам (stat=true). Вывод усечён до " + strconv.Itoa(maxOutChars) + " символов.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"ref":{"type":"string","description":"Ревизия или диапазон, напр. 'HEAD~1' или 'main...HEAD'. Пусто = рабочее дерево против HEAD."},
				"path":{"type":"string","description":"Ограничить путём/каталогом"},
				"stat":{"type":"boolean","description":"true = только сводка изменённых файлов (--stat), без тела диффа"}
			}
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if repo == "" {
			return errText(fmt.Errorf("git-репозиторий не найден")), nil
		}
		var in struct {
			Ref  string `json:"ref"`
			Path string `json:"path"`
			Stat bool   `json:"stat"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return errText(err), nil
			}
		}
		out, err := diff(ctx, repo, in.Ref, in.Path, in.Stat)
		if err != nil {
			return errText(err), nil
		}
		return okText(out), nil
	})

	// ── git_log — история (контекст «что тут вообще происходило») ──────────────
	s.AddTool(&mcp.Tool{
		Name:        "git_log",
		Description: "Последние коммиты: SHA, дата, автор, заголовок. Можно ограничить количеством и путём.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","description":"Сколько коммитов (по умолчанию 10, максимум 50)"},
				"path":{"type":"string","description":"Ограничить историей конкретного пути"}
			}
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if repo == "" {
			return errText(fmt.Errorf("git-репозиторий не найден")), nil
		}
		var in struct {
			Limit int    `json:"limit"`
			Path  string `json:"path"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return errText(err), nil
			}
		}
		out, err := gitLog(ctx, repo, in.Limit, in.Path)
		if err != nil {
			return errText(err), nil
		}
		return okText(out), nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// ── реализация тулов ─────────────────────────────────────────────────────────

func branchInfo(ctx context.Context, repo string) (string, error) {
	branch, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	head, err := git(ctx, repo, "rev-parse", "--short", "HEAD")
	if err != nil {
		head = "(нет коммитов)"
	}
	// upstream может отсутствовать — это не ошибка, а нормальное состояние ветки.
	upstream, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil || upstream == "" {
		upstream = "(не задан)"
	}
	// День 31, баг с живого прогона: `git status --porcelain` СХЛОПЫВАЕТ неотслеживаемые
	// каталоги в одну запись (`?? week-7/task-35/`). Мы печатали «рабочее дерево: 1
	// изменённых файл(ов)» — за этой единицей стояли 79 файлов. Агент в tool-use цикле
	// честно заметил расхождение с пустым `git diff --stat` и потратил ход на его
	// разгадывание. Лечим `--untracked-files=all` и разделяем счётчики: изменённое
	// (то, что увидит diff) и новое (то, чего diff не покажет) — это РАЗНЫЕ вещи.
	status, err := git(ctx, repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	modified, untracked := 0, 0
	for _, ln := range strings.Split(status, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if strings.HasPrefix(ln, "??") {
			untracked++
		} else {
			modified++
		}
	}
	tree := "чистое"
	if modified > 0 || untracked > 0 {
		tree = fmt.Sprintf("изменённых (видит git diff): %d · новых, не в индексе (git diff их НЕ покажет): %d",
			modified, untracked)
	}
	return fmt.Sprintf("репозиторий: %s\nветка: %s\nHEAD: %s\nupstream: %s\nрабочее дерево: %s",
		filepath.Base(repo), branch, head, upstream, tree), nil
}

// listFiles — ФАЙЛЫ РАБОЧЕГО ДЕРЕВА, а не только индекса git.
//
// День 31, баг с живого прогона. Сначала здесь был просто `git ls-files`, который
// показывает ТОЛЬКО отслеживаемые файлы. На незакоммиченном каталоге task-31 это
// дало «файлов: 1», блок контекста схлопнулся до 187 символов, и ассистент не мог
// ответить ни на один вопрос о структуре проекта.
//
// Дело не в том, что «надо было закоммитить». Ассистент разработчика обязан видеть
// РАБОЧЕЕ ДЕРЕВО: файл, который ты только что создал и ещё не добавил в индекс, —
// как раз самый интересный для вопроса «что у меня в проекте». Поэтому берём и
// отслеживаемые (--cached), и новые (--others), уважая .gitignore (--exclude-standard),
// и помечаем новые, чтобы модель понимала разницу.
func listFiles(ctx context.Context, repo, path string, limit int) (string, error) {
	if limit <= 0 {
		limit = 200
	}
	pathArgs := []string{}
	if path != "" {
		p, err := safeArg(path)
		if err != nil {
			return "", err
		}
		pathArgs = append(pathArgs, "--", p) // "--" отсекает трактовку пути как флага
	}

	tracked, err := git(ctx, repo, append([]string{"ls-files", "--cached"}, pathArgs...)...)
	if err != nil {
		return "", err
	}
	untracked, err := git(ctx, repo, append([]string{"ls-files", "--others", "--exclude-standard"}, pathArgs...)...)
	if err != nil {
		return "", err
	}

	tr, un := splitNonEmpty(tracked), splitNonEmpty(untracked)
	rows := make([]string, 0, len(tr)+len(un))
	rows = append(rows, tr...)
	rows = append(rows, un...)
	if len(rows) == 0 {
		return "файлов: 0 (пусто — проверь путь и .gitignore)", nil
	}
	sort.Strings(rows)

	// Пометку «новый» держим в ШАПКЕ, а не на каждой строке. Первый вариант дописывал
	// «[новый, не в индексе git]» к каждому пути — и на свежем, ещё не закоммиченном
	// каталоге (где новые ВСЕ файлы) блок контекста распух с ~2 КБ до 3.2 КБ чистого
	// шума, который улетал бы в каждый промпт.
	total := len(rows)
	if total > limit {
		rows = rows[:limit]
	}
	head := fmt.Sprintf("файлов: %d (под git: %d, новых — ещё не в индексе: %d)", total, len(tr), len(un))
	if total > limit {
		head += fmt.Sprintf(" — показаны первые %d", limit)
	}
	return head + "\n" + strings.Join(rows, "\n"), nil
}

func diff(ctx context.Context, repo, ref, path string, stat bool) (string, error) {
	args := []string{"diff"}
	if stat {
		args = append(args, "--stat")
	} else {
		args = append(args, "--unified=3")
	}
	if ref != "" {
		r, err := safeArg(ref)
		if err != nil {
			return "", err
		}
		args = append(args, r)
	}
	if path != "" {
		p, err := safeArg(path)
		if err != nil {
			return "", err
		}
		args = append(args, "--", p)
	}
	out, err := git(ctx, repo, args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "изменений нет (пустой diff)", nil
	}
	return clip(out), nil
}

func gitLog(ctx context.Context, repo string, limit int, path string) (string, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	args := []string{"log", "-n", strconv.Itoa(limit), "--date=short",
		"--pretty=format:%h %ad %an — %s"}
	if path != "" {
		p, err := safeArg(path)
		if err != nil {
			return "", err
		}
		args = append(args, "--", p)
	}
	out, err := git(ctx, repo, args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "история пуста", nil
	}
	return clip(out), nil
}

// ── исполнение git ───────────────────────────────────────────────────────────

// git запускает ТОЛЬКО читающие подкоманды. Аргументы передаются вектором в exec —
// никакого shell, значит никакой инъекции через `;` или `$(...)`. Подкоманда всегда
// задаётся НАМИ (белый список ниже), из JSON приходят только ref/path, и они
// проверяются в safeArg.
func git(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) == 0 || !readOnly[args[0]] {
		return "", fmt.Errorf("подкоманда %q запрещена: сервер работает только на чтение", firstOf(args))
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// --no-pager: иначе git на TTY уйдёт в less и повиснет.
	// -c core.quotepath=false: не экранировать кириллицу в путях в \NNN.
	full := append([]string{"--no-pager", "-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // не спрашивать логин/пароль

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// readOnly — белый список подкоманд. Всё остальное (add/commit/push/checkout/reset/
// clean/config/remote…) недоступно ПО ПОСТРОЕНИЮ, а не по договорённости.
var readOnly = map[string]bool{
	"rev-parse": true,
	"status":    true,
	"ls-files":  true,
	"diff":      true,
	"log":       true,
}

// safeArg — валидация строк, пришедших из JSON от модели. Главная угроза не
// «rm -rf», а то, что аргумент притворится ФЛАГОМ (напр. path="--output=/etc/x"):
// git разберёт его как опцию. Поэтому: ведущий дефис запрещён, набор символов
// ограничен, `..` в пути запрещён.
func safeArg(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("пустой аргумент")
	}
	if strings.HasPrefix(s, "-") {
		return "", fmt.Errorf("аргумент %q начинается с дефиса — запрещено (выглядит как флаг git)", s)
	}
	if strings.Contains(s, "..") && !strings.Contains(s, "...") {
		// git-диапазоны 'a..b'/'a...b' легитимны; выход из каталога '../' — нет.
		if strings.Contains(s, "../") || strings.Contains(s, "/..") {
			return "", fmt.Errorf("выход за пределы репозитория запрещён: %q", s)
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._/-~^@", r):
		default:
			return "", fmt.Errorf("недопустимый символ %q в аргументе %q", r, s)
		}
	}
	return s, nil
}

// repoRoot ищет корень репозитория от каталога сервера вверх (git сам умеет это
// через rev-parse --show-toplevel). Можно переопределить через -repo или GIT_REPO.
func repoRoot() (string, error) {
	start := sourceDir()
	if len(os.Args) > 2 && os.Args[1] == "-repo" {
		start = os.Args[2]
	} else if v := os.Getenv("GIT_REPO"); v != "" {
		start = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", start, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse в %s: %w", start, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ── мелочи ───────────────────────────────────────────────────────────────────

func clip(s string) string {
	if len(s) <= maxOutChars {
		return s
	}
	return s[:maxOutChars] + fmt.Sprintf("\n… [усечено: %d символов из %d]", maxOutChars, len(s))
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func firstOf(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func sourceDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(file)
	}
	return "."
}

func okText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errText(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "ошибка: " + err.Error()}}}
}
