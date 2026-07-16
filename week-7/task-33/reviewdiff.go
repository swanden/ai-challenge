package main

import (
	"bufio"
	"strings"
)

// День 32. Разбор unified diff.
//
// Ревьюеру diff нужен не как сплошной текст, а структурированно: какие ФАЙЛЫ
// затронуты (чтобы под каждый построить точечный RAG-запрос по докам и коду) и
// какие СТРОКИ добавлены (ревьюим то, что автор PR внёс, а не весь файл).
//
// Парсер намеренно минимальный и устойчивый: он не пытается понять семантику
// патча, только вытащить факты из строк `diff --git`, `+++`/`---` и `+`/`-`.
// Неполный/странный diff не роняет его — что распознали, то и вернём.

// FileDiff — изменения по одному файлу.
type FileDiff struct {
	Path       string   // путь нового файла (b/…); для удаления — путь старого
	OldPath    string   // путь старого файла (a/…), если отличается (переименование)
	Added      []string // добавленные строки (без ведущего '+')
	Removed    []string // удалённые строки (без ведущего '-')
	IsNew      bool     // файл добавлен целиком
	IsDeleted  bool     // файл удалён целиком
	Hunk       string   // сырой текст всех ханков файла — уходит в промпт ревьюера
	addedCount int
}

// AddedCount — сколько строк добавлено (для сортировки «самые крупные изменения вперёд»).
func (f FileDiff) AddedCount() int { return f.addedCount }

// parseDiff разбирает unified diff в срез FileDiff, по одному на файл.
func parseDiff(diff string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var hunk strings.Builder

	flush := func() {
		if cur == nil {
			return
		}
		cur.Hunk = strings.TrimRight(hunk.String(), "\n")
		cur.addedCount = len(cur.Added)
		files = append(files, *cur)
		cur = nil
		hunk.Reset()
	}

	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // крупные ханки не должны ронять сканер

	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &FileDiff{}
			// "diff --git a/path b/path" — пути берём отсюда как запасной вариант,
			// точные придут из +++/--- ниже.
			if a, b, ok := parseGitHeader(line); ok {
				cur.OldPath, cur.Path = a, b
			}
		case cur == nil:
			// шапка до первого файла (commit message и т.п.) — пропускаем
			continue
		case strings.HasPrefix(line, "new file"):
			cur.IsNew = true
		case strings.HasPrefix(line, "deleted file"):
			cur.IsDeleted = true
		case strings.HasPrefix(line, "--- "):
			if p := pathFromMarker(line, "--- "); p != "" {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p := pathFromMarker(line, "+++ "); p != "" {
				cur.Path = p
			}
		case strings.HasPrefix(line, "@@"):
			hunk.WriteString(line + "\n")
		case strings.HasPrefix(line, "+"):
			cur.Added = append(cur.Added, strings.TrimPrefix(line, "+"))
			hunk.WriteString(line + "\n")
		case strings.HasPrefix(line, "-"):
			cur.Removed = append(cur.Removed, strings.TrimPrefix(line, "-"))
			hunk.WriteString(line + "\n")
		default:
			// контекстная строка (' ') или прочее — сохраняем в ханк для читаемости
			if strings.TrimSpace(line) != "" {
				hunk.WriteString(line + "\n")
			}
		}
	}
	flush()
	return files
}

// parseGitHeader вытаскивает пути из "diff --git a/x b/y".
func parseGitHeader(line string) (a, b string, ok bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	// Простейший разбор по " b/" — путей с пробелами в нашем корпусе нет.
	i := strings.Index(rest, " b/")
	if i < 0 {
		return "", "", false
	}
	a = strings.TrimPrefix(rest[:i], "a/")
	b = strings.TrimPrefix(rest[i+1:], "b/")
	return a, b, true
}

// pathFromMarker разбирает "+++ b/path" / "--- a/path"; "/dev/null" → "".
func pathFromMarker(line, prefix string) string {
	p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if p == "/dev/null" {
		return ""
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	// у +++/--- иногда есть табличный таймстамп после \t
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	return p
}

// changedPaths — уникальные пути затронутых файлов (для RAG-запросов и заголовка ревью).
func changedPaths(files []FileDiff) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		p := f.Path
		if p == "" {
			p = f.OldPath
		}
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
