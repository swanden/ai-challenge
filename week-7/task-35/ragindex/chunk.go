package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"ai-challenge/week-7/task-35/ragcore"
)

// approxTokens — грубое приближение числа токенов: слова по пробелам.
// Точный счёт требует токенизатора модели (BPE); здесь сознательно упрощено.
func approxTokens(s string) int { return len(strings.Fields(s)) }

// ── Стратегия 1: fixed-size + overlap ───────────────────────────────────────
//
// Режет КАЖДЫЙ документ окнами фиксированного размера (в токенах≈словах) с
// перекрытием overlap. Структуру документа игнорирует — рвёт мысль на стыке.
// Пример из лекции: size=500, overlap=50 → окна 1–500 / 451–950 / 901–1400.
func chunkFixed(docs []Doc, size, overlap int) []ragcore.Chunk {
	if size <= 0 {
		size = 500
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 10
	}
	step := size - overlap
	var out []ragcore.Chunk
	for _, d := range docs {
		words := strings.Fields(d.Text)
		if len(words) == 0 {
			continue
		}
		part := 0
		for start := 0; start < len(words); start += step {
			end := start + size
			if end > len(words) {
				end = len(words)
			}
			part++
			out = append(out, ragcore.Chunk{
				ID:      fmt.Sprintf("%s#fixed-%03d", d.Rel, part),
				Source:  d.Rel,
				File:    d.Name,
				Section: fmt.Sprintf("part %d (tokens %d–%d)", part, start+1, end),
				Text:    strings.Join(words[start:end], " "),
				Tokens:  end - start,
			})
			if end == len(words) {
				break
			}
		}
	}
	return out
}

// ── Стратегия 2: structural ──────────────────────────────────────────────────
//
// Режет по СТРУКТУРЕ файла: .md — по заголовкам, .go — по верхнеуровневым
// декларациям (go/parser), прочее — по абзацам. Каждый чанк = законченная единица.
func chunkStructural(docs []Doc) []ragcore.Chunk {
	var out []ragcore.Chunk
	for _, d := range docs {
		switch d.Ext {
		case ".go":
			out = append(out, chunkGo(d)...)
		case ".md", ".markdown":
			out = append(out, chunkMarkdown(d)...)
		default:
			out = append(out, chunkParagraphs(d)...)
		}
	}
	return out
}

// chunkGo разбивает .go-файл по верхнеуровневым декларациям через go/parser:
// «шапка» (package + import), затем по чанку на каждую func/type/const/var.
func chunkGo(d Doc) []ragcore.Chunk {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, d.Name, d.Text, parser.ParseComments)
	if err != nil {
		return chunkParagraphs(d) // не распарсилось — не теряем файл
	}
	src := d.Text
	var out []ragcore.Chunk
	ord := 0
	emit := func(section, text string) {
		ord++
		out = append(out, ragcore.Chunk{
			ID:      fmt.Sprintf("%s#%03d-%s", d.Rel, ord, slug(section)),
			Source:  d.Rel,
			File:    d.Name,
			Section: section,
			Text:    text,
			Tokens:  approxTokens(text),
		})
	}
	add := func(section, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		// День 31: тот же потолок, что и у markdown, и по той же причине —
		// эмбеддер молча усекает вход до -max-embed-chars. На нашем корпусе это
		// было измерено: `main.go · func main` = 25 424 символа, то есть в вектор
		// попадали первые ~7% декларации. Всего 25 Go-чанков превышали лимит,
		// а вне вектора оставалось 19.9% символов всего корпуса.
		//
		// Крупную декларацию режем по ГРАНИЦАМ СТРОК (код строкоориентирован —
		// рвать посреди строки нельзя) и в каждую часть дублируем первую строку
		// декларации: она содержит сигнатуру, по ней чанк и ищется.
		parts := packGo(text, goMaxRunes)
		if len(parts) == 1 {
			emit(section, parts[0])
			return
		}
		for i, p := range parts {
			emit(fmt.Sprintf("%s (часть %d/%d)", section, i+1, len(parts)), p)
		}
	}

	// «Шапка» файла: package + import — до первой декларации (doc-комментарий
	// первой декларации остаётся с ней).
	headEnd := len(src)
	if len(f.Decls) > 0 {
		headEnd = declStart(fset, f.Decls[0])
	}
	add("package "+f.Name.Name, src[:headEnd])

	for _, decl := range f.Decls {
		start := declStart(fset, decl)
		end := offset(fset, decl.End())
		if start < 0 || end > len(src) || start >= end {
			continue
		}
		add(declName(decl), src[start:end])
	}
	return out
}

// goMaxRunes — потолок размера Go-чанка в символах (см. комментарий к mdMaxRunes:
// причина та же — тихое усечение на эмбеддинге).
const goMaxRunes = 1500

// packGo режет текст декларации на части не длиннее max символов, по границам строк.
// Первая строка декларации (сигнатура) повторяется в каждой части как якорь: без неё
// «часть 7 из 15» — это безымянный кусок тела функции, который ничем не найдётся.
func packGo(text string, max int) []string {
	if len([]rune(text)) <= max {
		return []string{text}
	}
	lines := strings.Split(text, "\n")

	// Якорь — первая непустая строка, не являющаяся комментарием: это и есть
	// сигнатура (у декларации с doc-комментарием строки комментария идут первыми).
	anchor := ""
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		anchor = ln
		break
	}
	anchorRunes := len([]rune(anchor))

	var out []string
	var cur []string
	curRunes := 0
	flush := func(first bool) {
		if len(cur) == 0 {
			return
		}
		body := strings.Join(cur, "\n")
		if !first && anchor != "" {
			body = anchor + "\n    // …\n" + body
		}
		out = append(out, strings.TrimRight(body, "\n"))
		cur = nil
		curRunes = 0
	}
	for _, ln := range lines {
		n := len([]rune(ln)) + 1
		if len(cur) > 0 && curRunes+n+anchorRunes > max {
			flush(len(out) == 0)
		}
		cur = append(cur, ln)
		curRunes += n
	}
	flush(len(out) == 0)
	return out
}

// declStart — байтовое смещение начала декларации с учётом её doc-комментария.
func declStart(fset *token.FileSet, d ast.Decl) int {
	if dc := declDoc(d); dc != nil {
		return offset(fset, dc.Pos())
	}
	return offset(fset, d.Pos())
}

func offset(fset *token.FileSet, p token.Pos) int { return fset.Position(p).Offset }

func declDoc(d ast.Decl) *ast.CommentGroup {
	switch v := d.(type) {
	case *ast.FuncDecl:
		return v.Doc
	case *ast.GenDecl:
		return v.Doc
	}
	return nil
}

func declName(d ast.Decl) string {
	switch v := d.(type) {
	case *ast.FuncDecl:
		name := v.Name.Name
		if v.Recv != nil && len(v.Recv.List) > 0 {
			name = recvTypeName(v.Recv.List[0].Type) + "." + name
		}
		return "func " + name
	case *ast.GenDecl:
		kind := v.Tok.String() // import | const | type | var
		if len(v.Specs) > 0 {
			switch s := v.Specs[0].(type) {
			case *ast.TypeSpec:
				return "type " + s.Name.Name
			case *ast.ValueSpec:
				if len(s.Names) > 0 {
					return kind + " " + s.Names[0].Name
				}
			}
		}
		return kind
	}
	return "decl"
}

func recvTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// chunkMarkdown разбивает .md по заголовкам (#…). Заголовок остаётся в своём чанке.
// Заголовки внутри code-fence (```) игнорируются (это не разделы, а код).
// mdMaxRunes — потолок размера markdown-чанка в СИМВОЛАХ (не в словах).
//
// День 31, найденный баг. До сегодня chunkMarkdown резал только по заголовкам и
// НЕ ограничивал размер секции. При этом эмбеддер (ragcore.OllamaEmbedder) молча
// усекает вход до MaxChars (флаг -max-embed-chars, по умолчанию 1800) — счётчик
// emb.Truncated это фиксировал, но на корпусе лекций почти не срабатывал.
// На корпусе НАШИХ доков срабатывает сразу: в readme дня 30 секции «Решение и
// альтернативы» и «Реальный прогон» — 255 и 276 слов, это уже ~1800+ символов.
// Итог: хвост секции лежал в Text чанка (и попадал в ответ модели), но НЕ попадал
// в вектор → чанк искался по своей первой половине. Тихая потеря recall ровно на
// самых содержательных секциях.
//
// Лечение: пакуем секцию в под-чанки с потолком по символам, с запасом до 1800.
// Границы под-чанков — целые абзацы и целые код-блоки (см. atomizeMarkdown), так
// что мысль не рвётся и word-overlap не нужен. Вместо overlap в каждый под-чанк
// дублируется строка заголовка: эмбеддеру важно знать, ЧТО это за раздел.
const mdMaxRunes = 1500

func chunkMarkdown(d Doc) []ragcore.Chunk {
	var out []ragcore.Chunk
	section := "(intro)"
	var buf []string
	ord := 0
	flush := func() {
		lines := append([]string(nil), buf...)
		buf = buf[:0]
		for _, text := range packMarkdown(lines, mdMaxRunes) {
			ord++
			out = append(out, ragcore.Chunk{
				ID:      fmt.Sprintf("%s#%03d-%s", d.Rel, ord, slug(section)),
				Source:  d.Rel,
				File:    d.Name,
				Section: section,
				Text:    text,
				Tokens:  approxTokens(text),
			})
		}
	}

	inFence := false
	for _, ln := range strings.Split(d.Text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
		}
		if !inFence {
			if h := mdHeader(ln); h != "" {
				flush()
				section = h
			}
		}
		buf = append(buf, ln)
	}
	flush()
	return out
}

// packMarkdown режет одну секцию на под-чанки не длиннее max символов.
// lines[0] — строка заголовка секции (если секция началась с заголовка); она
// повторяется в каждом под-чанке как контекст для эмбеддера.
func packMarkdown(lines []string, max int) []string {
	if len(lines) == 0 {
		return nil
	}
	heading := ""
	body := lines
	if mdHeader(lines[0]) != "" {
		heading = strings.TrimSpace(lines[0])
		body = lines[1:]
	}

	atoms := atomizeMarkdown(body)
	if len(atoms) == 0 {
		if heading != "" {
			return []string{heading}
		}
		return nil
	}

	headRunes := len([]rune(heading))
	var out []string
	var cur []string
	curRunes := 0
	emit := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.Join(cur, "\n\n")
		if heading != "" {
			text = heading + "\n\n" + text
		}
		out = append(out, strings.TrimSpace(text))
		cur = nil
		curRunes = 0
	}
	for _, a := range atoms {
		n := len([]rune(a))
		// Атом крупнее лимита (длинный код-блок или таблица) не режем: рвать код
		// посреди строки хуже, чем один раз усечь на эмбеддинге. Он уезжает
		// отдельным чанком, и это видно в счётчике emb.Truncated.
		if len(cur) > 0 && curRunes+n+headRunes+2 > max {
			emit()
		}
		cur = append(cur, a)
		curRunes += n + 2
	}
	emit()
	return out
}

// atomizeMarkdown разбивает тело секции на неделимые единицы: абзац (строки до
// пустой строки) или ЦЕЛЫЙ ```-код-блок (пустые строки внутри него не разрывают).
func atomizeMarkdown(lines []string) []string {
	var atoms []string
	var cur []string
	inFence := false

	flushAtom := func() {
		text := strings.TrimRight(strings.Join(cur, "\n"), "\n")
		cur = cur[:0]
		if strings.TrimSpace(text) != "" {
			atoms = append(atoms, text)
		}
	}

	for _, ln := range lines {
		isFence := strings.HasPrefix(strings.TrimSpace(ln), "```")
		switch {
		case isFence && !inFence: // открытие: закрываем текущий абзац
			flushAtom()
			inFence = true
			cur = append(cur, ln)
		case isFence && inFence: // закрытие: код-блок — один атом
			cur = append(cur, ln)
			inFence = false
			flushAtom()
		case inFence:
			cur = append(cur, ln)
		case strings.TrimSpace(ln) == "": // граница абзаца
			flushAtom()
		default:
			cur = append(cur, ln)
		}
	}
	flushAtom() // хвост (в т.ч. незакрытый fence — текст не теряем)
	return atoms
}

// mdHeader возвращает текст заголовка для строк "## Заголовок", иначе "".
func mdHeader(line string) string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return ""
	}
	return strings.TrimSpace(strings.TrimLeft(t, "#"))
}

// chunkParagraphs — фолбэк для .txt и нераспарсенных файлов: по абзацам (пустая строка).
func chunkParagraphs(d Doc) []ragcore.Chunk {
	var out []ragcore.Chunk
	ord := 0
	for _, b := range strings.Split(d.Text, "\n\n") {
		text := strings.TrimSpace(b)
		if text == "" {
			continue
		}
		ord++
		out = append(out, ragcore.Chunk{
			ID:      fmt.Sprintf("%s#para-%03d", d.Rel, ord),
			Source:  d.Rel,
			File:    d.Name,
			Section: fmt.Sprintf("paragraph %d", ord),
			Text:    text,
			Tokens:  approxTokens(text),
		})
	}
	return out
}

// slug приводит произвольный заголовок к ascii-метке для chunk_id.
func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "x"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
