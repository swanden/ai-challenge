package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"ai-challenge/week-6/task-29/ragcore"
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
	add := func(section, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
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
func chunkMarkdown(d Doc) []ragcore.Chunk {
	var out []ragcore.Chunk
	section := "(intro)"
	var buf []string
	ord := 0
	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if text == "" {
			return
		}
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
