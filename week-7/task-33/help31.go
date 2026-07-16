package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ai-challenge/week-7/task-33/ragcore"
)

// День 31. АССИСТЕНТ РАЗРАБОТЧИКА: /help
//
// Задание требует, чтобы ассистент использовал ДВА разных источника:
//   1) документацию проекта (RAG)          — «как проект устроен»;
//   2) контекст проекта через MCP (git)    — «что в нём происходит прямо сейчас».
//
// Это принципиально разные вещи, и главное решение дня — как их соединить.
//
// ГИБРИД (выбранный вариант):
//   • ВЕТКА/СТРУКТУРА — детерминированный ПРЕ-ФЕТЧ. Перед каждым /help мы САМИ
//     дёргаем git_branch и git_files прямым вызовом MCP (mcptools.Call), без
//     участия модели. Ноль токенов, ноль недетерминизма: контекст проекта есть
//     в КАЖДОМ ответе, а не «если модель догадается вызвать тул». Работает и на
//     -backend local (7B не обязан уметь tool-calling).
//   • DIFF/LOG — ТУЛАМИ. Они остаются в обычном tool-use цикле агента (Agent.ask,
//     облако): на вопрос «что изменилось?» модель сама зовёт git_diff/git_log.
//     Пре-фетчить diff в каждый ответ нельзя — он бывает на десятки килобайт.
//
// Отсюда и распределение в REPL:
//   /help <вопрос>  → пре-фетч + RAG по докам + grounded-ответ с цитатами;
//   обычная строка  → tool-use цикл, где git_diff/git_files/git_log доступны модели.

// ── 1. Пре-фетч: снимок состояния проекта ────────────────────────────────────

// ProjectSnapshot — то, что мы знаем о проекте БЕЗ участия LLM.
type ProjectSnapshot struct {
	Branch string   // сырой вывод git_branch (ветка, HEAD, upstream, грязность)
	Head   string   // шапка git_files: сколько файлов, из них новых (ещё не в индексе)
	Files  []string // пути в РАБОЧЕМ ДЕРЕВЕ (отслеживаемые + новые), в пределах scope
	Scope  string   // каталог, которым ограничен снимок ("" = весь репозиторий)
	Err    string   // почему снимок не собран (MCP лежит, git не найден…)
}

// OK — снимок пригоден к использованию.
func (p ProjectSnapshot) OK() bool { return p.Err == "" && p.Branch != "" }

// fetchProjectSnapshot делает ДВА прямых вызова MCP-тулов. Токены не тратятся:
// это не диалог с моделью, это вызов функции по имени (маршрутизация — день 20).
func fetchProjectSnapshot(ctx context.Context, m *MCPTools, scope string, limit int) ProjectSnapshot {
	ps := ProjectSnapshot{Scope: scope}
	if m == nil {
		ps.Err = "MCP не подключён"
		return ps
	}

	// ВНИМАНИЕ на сигнатуру (баг, пойманный на живом прогоне): MCPTools.Call
	// возвращает (текст, **isError**), а НЕ (текст, ok). В agent.go это видно по
	// имени (`out, isErr := a.mcp.Call(...)`), а все остальные вызывающие просто
	// глотают второе значение в `_`, поэтому неверная трактовка нигде не всплывала.
	// Прочитав его как `ok`, мы разворачивали смысл наизнанку: успешный вызов
	// (isError=false) уходил в ветку ошибки. Симптом был абсурдным — «снимок не
	// собран», и тут же корректный вывод git_branch в тексте ошибки.
	branch, isErr := m.Call(ctx, "git_branch", json.RawMessage(`{}`))
	if isErr || strings.HasPrefix(branch, "ошибка:") {
		ps.Err = "git_branch: " + oneLine(branch, 120)
		return ps
	}
	ps.Branch = strings.TrimSpace(branch)

	files, isErr := m.Call(ctx, "git_files", jsonArgs(map[string]any{"path": scope, "limit": limit}))
	if isErr || strings.HasPrefix(files, "ошибка:") {
		// Ветку получили — этого уже хватает на минимум задания. Список файлов
		// не критичен, поэтому не роняем снимок целиком.
		return ps
	}
	for i, ln := range strings.Split(files, "\n") {
		ln = strings.TrimSpace(ln)
		if i == 0 && strings.HasPrefix(ln, "файлов:") {
			ps.Head = ln // шапка со счётчиками — она информативна, не выбрасываем
			continue
		}
		if ln != "" {
			ps.Files = append(ps.Files, ln)
		}
	}
	return ps
}

// renderProjectContext — блок, который уходит в промпт. Задание просит, чтобы
// /help отвечал «на вопросы о СТРУКТУРЕ проекта», поэтому дерево файлов — не
// украшение, а основной материал ответа. Держим его компактным: группируем по
// каталогу, а не вываливаем 60 путей плоским списком.
func renderProjectContext(ps ProjectSnapshot) string {
	if !ps.OK() {
		return ""
	}
	var b strings.Builder
	b.WriteString(ps.Branch)
	if len(ps.Files) == 0 {
		return b.String()
	}

	scope := ps.Scope
	if scope == "" {
		scope = "(весь репозиторий)"
	}
	fmt.Fprintf(&b, "\n\nструктура %s — %s", scope, ps.Head)

	byDir := map[string][]string{}
	for _, f := range ps.Files {
		rel := strings.TrimPrefix(f, ps.Scope+"/")
		dir := "."
		name := rel
		if i := strings.LastIndex(rel, "/"); i >= 0 {
			dir, name = rel[:i], rel[i+1:]
		}
		byDir[dir] = append(byDir[dir], name)
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		names := byDir[d]
		sort.Strings(names)
		fmt.Fprintf(&b, "\n  %s/  (%d): %s", d, len(names), strings.Join(names, ", "))
	}
	return b.String()
}

// ── 2. Ответ /help ───────────────────────────────────────────────────────────

// helpReply — конвейер /help. Это НЕ groundedReply: у него другое поведение на
// пороге «не знаю», и вот почему.
//
// groundedReply (день 24) при best < threshold сразу отвечает «не знаю» — он знает
// ровно один источник, документацию. Но /help знает два. Вопрос «какая сейчас
// ветка?» по документации не находит НИЧЕГО (и правильно: этого нет ни в одном
// README) → старый порог убил бы ответ, хотя ответ у нас на руках, в пре-фетче.
//
// Поэтому порог здесь означает не «не знаю», а «документация не помогла»:
//   - нет чанков И нет контекста проекта → честное «не знаю»;
//   - нет чанков, но контекст проекта есть → отвечаем ТОЛЬКО по нему (sources пуст);
//   - чанки есть → обычный grounded-ответ с цитатами + контекст проекта сверху.
func helpReply(ctx context.Context, a *Agent, r *Retriever, query string, cfg RerankConfig,
	knowThreshold float64) (GroundedReply, []ragcore.Hit, RetrievalTrace, error) {

	hits, tr, err := retrieveAdvanced(ctx, a, r, query, cfg)
	if err != nil {
		return GroundedReply{}, nil, tr, err
	}
	best := 0.0
	if len(hits) > 0 {
		best = hits[0].Score
	}

	docsWeak := len(hits) == 0 || best < knowThreshold
	if docsWeak {
		if a.ProjectContext() == "" {
			return GroundedReply{
				Known:     false,
				IDKReason: "threshold",
				BestScore: best,
				Answer: "Не знаю: в документации проекта нет релевантных фрагментов, " +
					"а живой контекст проекта недоступен (MCP не подключён).",
			}, hits, tr, nil
		}
		hits = nil // отвечаем по живому контексту, цитировать нечего
	}

	rep, err := a.groundedAnswer(ctx, cfg.Model, query, hits)
	if err != nil {
		return GroundedReply{}, hits, tr, err
	}
	rep.BestScore = best
	verifyQuotes(&rep, hits) // на пустых hits просто пометит цитаты как невалидные — их и не будет
	return rep, hits, tr, nil
}

// runHelp — одна команда /help: обновить снимок проекта → ответить → напечатать.
// Снимок обновляется на КАЖДЫЙ вызов: ветка могла смениться между вопросами,
// и это дёшево (два вызова git, ноль токенов).
func runHelp(ctx context.Context, a *Agent, r *Retriever, m *MCPTools, query string,
	cfg RerankConfig, knowThreshold float64, scope string) error {

	ps := fetchProjectSnapshot(ctx, m, scope, 200)
	a.SetProjectContext(renderProjectContext(ps))
	defer a.SetProjectContext("") // не протекаем в другие флоу (чат, рой, инварианты)

	if ps.OK() {
		fmt.Printf("[контекст проекта] %s\n", oneLine(ps.Branch, 120))
	} else {
		fmt.Printf("[контекст проекта] недоступен: %s (отвечаю только по документации)\n", ps.Err)
	}

	rep, hits, tr, err := helpReply(ctx, a, r, query, cfg, knowThreshold)
	if err != nil {
		return err
	}
	printHelpReply(rep, hits, tr, knowThreshold)
	return nil
}

// printHelpReply — единый формат вывода (он же идёт в видео).
func printHelpReply(rep GroundedReply, hits []ragcore.Hit, tr RetrievalTrace, knowThreshold float64) {
	if !rep.Known {
		fmt.Printf("НЕ ЗНАЮ (%s, лучший score=%.2f < %.1f)\n  %s\n",
			rep.IDKReason, rep.BestScore, knowThreshold, oneLine(rep.Answer, 200))
		return
	}

	// Метка источника — по rep.Sources, а не по len(hits): реранк может поднять
	// нерелевантный чанк выше порога, но если модель на него не сослалась — значит
	// документация не пригодилась, и ответ построен на живом контексте проекта.
	switch {
	case len(rep.Sources) == 0 && len(hits) == 0:
		fmt.Printf("ОТВЕТ (по живому контексту проекта; в документации ничего не нашлось, best=%.2f):\n  %s\n",
			rep.BestScore, rep.Answer)
	case len(rep.Sources) == 0:
		fmt.Printf("ОТВЕТ (по живому контексту проекта; чанки поднялись (best=%.2f), но модель на них не сослалась):\n  %s\n",
			rep.BestScore, rep.Answer)
	default:
		fmt.Printf("ОТВЕТ (score=%.2f):\n  %s\n", rep.BestScore, rep.Answer)
	}

	if len(rep.Sources) > 0 {
		fmt.Printf("ИСТОЧНИКИ (%d):\n", len(rep.Sources))
		for _, s := range rep.Sources {
			fmt.Printf("   • %s · %s · %s\n", s.Source, s.Section, s.ChunkID)
		}
	}
	if len(rep.Quotes) > 0 {
		ex, pa, no := matchCounts(rep.Quotes)
		fmt.Printf("ЦИТАТЫ (%d: ✓дословно %d · ~по смыслу %d · ✗нет %d):\n", len(rep.Quotes), ex, pa, no)
		for _, qt := range rep.Quotes {
			mark := "✗ нет в чанках"
			switch qt.Match {
			case "exact":
				mark = "✓ дословно"
			case "partial":
				mark = "~ по смыслу"
			}
			fmt.Printf("   [%s] %s  — %s\n", qt.ChunkID, oneLine(qt.Text, 120), mark)
		}
	}
	fmt.Printf("RETRIEVAL: bi-encoder %d → порог %d → MMR %d → финал %d (уникальных источников: %d)\n",
		tr.NBiEncoder, tr.NThreshold, tr.NMMR, tr.NFinal, tr.UniqSrc)
	if tr.Rewritten != "" && tr.Rewritten != tr.Query {
		fmt.Printf("           запрос переписан: %s\n", oneLine(tr.Rewritten, 110))
	}
}

// ── 3. Демонстрация для видео: -help31 ───────────────────────────────────────

// helpQuestions — вопросы подобраны так, чтобы каждый бил в СВОЙ источник:
// документация / схема данных / API-описание тулов / живое состояние проекта /
// заведомо чужой (проверка честного «не знаю»).
type helpQ struct {
	Q    string
	Hits string // что должно сработать — печатаем рядом, чтобы на видео было видно ожидание
}

func helpQuestions() []helpQ {
	return []helpQ{
		{"Из каких слоёв состоит агент и что такое Completer?", "docs/architecture.md"},
		{"Что лежит в чанке индекса RAG и какие есть стратегии chunking?", "docs/index-format.md — схема данных"},
		{"Какие MCP-серверы подключены и какие тулы даёт git-сервер?", "docs/mcp-tools.md — API-описание"},
		{"Какой флаг переключает генерацию на локальную модель и что он ещё меняет?", "docs/cli.md"},
		{"На какой ветке сейчас проект и сколько в нём файлов?", "живой git через MCP (в документации этого нет)"},
		{"Какой рецепт борща?", "ничего — ожидаем честное «не знаю»"},
	}
}

func runHelp31(ctx context.Context, a *Agent, r *Retriever, m *MCPTools, cfg RerankConfig,
	knowThreshold float64, scope string, p Policy) error {

	ragBanner(0, "АССИСТЕНТ РАЗРАБОТЧИКА · /help (день 31)")
	fmt.Println("ДВА ИСТОЧНИКА В ОДНОМ ОТВЕТЕ:")
	fmt.Println("  1) ДОКУМЕНТАЦИЯ ПРОЕКТА (RAG) — readme.md + docs/ + код агента. «Как устроено».")
	fmt.Println("  2) КОНТЕКСТ ПРОЕКТА (MCP/git) — ветка, HEAD, файлы. «Что происходит сейчас».")
	fmt.Println("ГИБРИД: ветка и структура берутся ПРЕ-ФЕТЧЕМ (прямой вызов тула, 0 токенов),")
	fmt.Println("        diff и log остаются ТУЛАМИ в tool-use цикле (шаг 3 ниже).")

	// ── Шаг 1: MCP ───────────────────────────────────────────────────────────
	ragBanner(1, "MCP: КОНТЕКСТ ПРОЕКТА ЧЕРЕЗ GIT-СЕРВЕР")
	if m == nil {
		return fmt.Errorf("MCP не подключён — день 31 без него не демонстрируется")
	}
	fmt.Printf("Серверы и тулы:\n%s\n", m.Breakdown())

	ps := fetchProjectSnapshot(ctx, m, scope, 200)
	if !ps.OK() {
		return fmt.Errorf("снимок проекта не собран: %s", ps.Err)
	}
	fmt.Println("\ngit_branch (прямой вызов тула, без модели):")
	for _, ln := range strings.Split(ps.Branch, "\n") {
		fmt.Printf("   %s\n", ln)
	}
	fmt.Printf("\ngit_files: %s\n", ps.Head)

	projCtx := renderProjectContext(ps)
	a.SetProjectContext(projCtx)
	defer a.SetProjectContext("")
	fmt.Printf("В промпт уйдёт блок «КОНТЕКСТ ПРОЕКТА» (%d символов).\n", len([]rune(projCtx)))

	// ── Шаг 2: RAG + /help ───────────────────────────────────────────────────
	ragBanner(2, "RAG: ДОКУМЕНТАЦИЯ ПРОЕКТА")
	fmt.Printf("Индекс: %s\n", r.Info())
	fmt.Printf("Порог «документация не помогла»: %.1f (шкала %s)\n",
		knowThreshold, scaleName(cfg))

	qs := helpQuestions()
	var answered, fromDocs, fromProject, idk int
	for i, q := range qs {
		ragBanner(i+3, fmt.Sprintf("/help  (%d/%d)", i+1, len(qs)))
		fmt.Printf("ВОПРОС:  %s\n", q.Q)
		fmt.Printf("ОЖИДАЕМ: %s\n\n", q.Hits)

		rep, hits, tr, err := helpReply(ctx, a, r, q.Q, cfg, knowThreshold)
		if err != nil {
			return fmt.Errorf("вопрос %d: %w", i+1, err)
		}
		printHelpReply(rep, hits, tr, knowThreshold)

		// День 31, баг с живого прогона: классифицировать ответ ПО ПОРОГУ нельзя.
		// Вопрос «на какой ветке проект?» получил от реранка score=4.0 (в другом
		// прогоне — вообще 10.0) на заведомо нерелевантном чанке, порог 3.0 не сработал,
		// и ответ уехал в графу «по документации» — при том что модель честно не
		// сослалась НИ НА ОДИН источник и ответила по живому git.
		//
		// Правильный признак — не наш порог, а ПОВЕДЕНИЕ МОДЕЛИ: пустой rep.Sources
		// означает «в документации ответа не было». Модель — единственный, кто реально
		// прочитал и чанки, и контекст проекта.
		switch {
		case !rep.Known:
			idk++
		case len(rep.Sources) == 0:
			answered++
			fromProject++
		default:
			answered++
			fromDocs++
		}
	}

	// ── Шаг 3: diff по требованию (вторая половина гибрида) ──────────────────
	ragBanner(len(qs)+3, "DIFF ПО ТРЕБОВАНИЮ: ТУЛЫ В TOOL-USE ЦИКЛЕ")
	fmt.Println("Пре-фетчем diff не тянем (бывает на десятки КБ). Вместо этого git_diff/git_log")
	fmt.Println("доступны модели как обычные тулы — она зовёт их сама, когда вопрос того требует.")
	toolQ := "Посмотри в git, что сейчас изменено в рабочем дереве, и покажи последние 3 коммита. " +
		"Коротко, по делу."
	fmt.Printf("\nВОПРОС АГЕНТУ (обычный tool-use, не /help):\n  %s\n\n", toolQ)

	// Политика БЕЗ короткой памяти. Вопрос самодостаточный, и тащить в него 20 реплик
	// из прошлых сессий незачем: это и шум для модели, и лишние токены. Не косметика —
	// именно накопленная история раздула диалог до 25 сообщений, из-за чего баг с пустым
	// tool_result-сообщением проявился как невнятное «messages.24.content: Field required»
	// вместо «messages.2».
	toolPolicy := Policy{Profile: p.Profile, Working: false, ShortTermN: 0}
	rep, err := a.AskOnce(ctx, toolQ, toolPolicy)
	if err != nil {
		return fmt.Errorf("tool-use: %w", err)
	}
	fmt.Printf("ОТВЕТ АГЕНТА:\n%s\n", rep.Text)

	// ── Итог ────────────────────────────────────────────────────────────────
	ragBanner(len(qs)+4, "ИТОГ (чеклист задания дня 31)")
	fmt.Printf("RAG по документации проекта (README + docs/ + код):  %s (%d чанков)\n",
		mark(r != nil), len(r.idx.Chunks))
	fmt.Printf("MCP — минимум git branch:                             %s (git_branch отработал)\n", mark(ps.OK()))
	fmt.Printf("MCP — опция: список файлов и diff:                    %s (git_files + git_diff/git_log)\n",
		mark(len(ps.Files) > 0))
	fmt.Printf("/help отвечает на вопросы о структуре проекта:        %s\n", mark(answered > 0))
	fmt.Printf("  из них по документации:   %d\n", fromDocs)
	fmt.Printf("  из них по живому git:     %d\n", fromProject)
	fmt.Printf("  честное «не знаю»:        %d (ожидалось 1 — вопрос про борщ)\n", idk)
	return nil
}

// scaleName — в какой шкале сейчас score. Тот же урок, что и на дне 27: порог и
// score обязаны быть в одной шкале, иначе «не знаю» срабатывает всегда.
func scaleName(cfg RerankConfig) string {
	if cfg.Rerank {
		return "оценка LLM-реранка 0–10"
	}
	return "косинус bi-encoder 0–1"
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// defaultGitScope — путь каталога task-31 ОТНОСИТЕЛЬНО корня репозитория
// (например "week-7/task-33"). Именно в такой форме его понимает git ls-files.
// Корень ищем, поднимаясь вверх до каталога с .git; не нашли — пустая строка,
// то есть снимок по всему репозиторию (тоже рабочий вариант, просто шумнее).
func defaultGitScope() string {
	dir := sourceDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	root := abs
	for {
		if st, err := os.Stat(filepath.Join(root, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
			break
		}
		parent := filepath.Dir(root)
		if parent == root { // дошли до "/" — репозитория нет
			return ""
		}
		root = parent
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}
