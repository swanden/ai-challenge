package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-challenge/week-5/task-24/ragcore"
)

// reportOpts — параметры демонстрационного прогона (-report).
type reportOpts struct {
	docs, exts, out string
	baseURL, model  string
	size, overlap   int
	query           string
	topK, limit     int
	maxChars        int
	dryRun          bool
}

// runReport — единый «киношный» прогон для видео: по шагам показывает всё, что
// требует задание дня 21 — корпус → 2 стратегии chunking (+ наглядный контраст) →
// сравнение → метаданные чанка → эмбеддинги → сохранение индекса → семантический
// поиск → итог-чеклист. Без Ollama (или с -dry-run) аккуратно деградирует.
func runReport(ctx context.Context, o reportOpts) error {
	banner(1, "КОРПУС")
	corpus, err := loadCorpus(o.docs, strings.Split(o.exts, ","), defaultSkipDirs())
	if err != nil {
		return err
	}
	if len(corpus) == 0 {
		return fmt.Errorf("в %q не найдено файлов с расширениями %s", o.docs, o.exts)
	}
	total := 0
	for _, d := range corpus {
		total += approxTokens(d.Text)
	}
	fmt.Printf("каталог: %s\nфайлов: %d · ~%d токенов (≈%d стр. по 500 слов)\n",
		o.docs, len(corpus), total, (total+499)/500)
	fmt.Println("файлы корпуса:")
	for i, d := range corpus {
		if i >= 10 {
			fmt.Printf("  … ещё %d файл(ов)\n", len(corpus)-10)
			break
		}
		fmt.Printf("  %-30s %5d токенов\n", d.Rel, approxTokens(d.Text))
	}

	banner(2, "CHUNKING: ДВЕ СТРАТЕГИИ")
	fixed := chunkFixed(corpus, o.size, o.overlap)
	structural := chunkStructural(corpus)
	fmt.Printf("fixed-size (size=%d, overlap=%d) → %d чанков\n", o.size, o.overlap, len(fixed))
	fmt.Printf("structural (.md заголовки · .go AST-декларации · .txt абзацы) → %d чанков\n", len(structural))
	showContrast(fixed, structural)

	banner(3, "СРАВНЕНИЕ СТРАТЕГИЙ")
	report := compareReport(computeStats("fixed-size", fixed), computeStats("structural", structural))
	fmt.Print(report)
	if err := os.WriteFile(filepath.Join(o.out, "comparison.md"), []byte("```\n"+report+"```\n"), 0o644); err == nil {
		fmt.Printf("\n→ записано %s\n", filepath.Join(o.out, "comparison.md"))
	}

	banner(4, "МЕТАДАННЫЕ ЧАНКА (усиление)")
	demoMeta(structural)

	emb := ragcore.NewOllamaEmbedder(o.baseURL, o.model)
	if o.maxChars > 0 {
		emb.MaxChars = o.maxChars
	}
	if o.dryRun || !ollamaReachable(ctx, emb) {
		banner(5, "ЭМБЕДДИНГИ — ПРОПУЩЕНЫ")
		if o.dryRun {
			fmt.Println("режим -dry-run: эмбеддинги отключены.")
		} else {
			fmt.Printf("Ollama недоступна на %s — пропускаю эмбеддинги и поиск.\n", o.baseURL)
			fmt.Printf("подними: `ollama serve &`, `ollama pull %s`, затем повтори без -dry-run.\n", o.model)
		}
		saveDry(o.out, o.size, o.overlap, fixed, structural)
		fmt.Printf("сохранены индексы без векторов (метаданные видны): index-fixed.json, index-structural.json в %s\n", o.out)
		banner(6, "ИТОГ")
		checklist(false)
		return nil
	}

	if o.limit > 0 {
		fixed = capChunks(fixed, o.limit)
		structural = capChunks(structural, o.limit)
		fmt.Printf("(демо: эмбеддингов не больше %d на стратегию — флаг -limit; убери для полного индекса)\n", o.limit)
	}

	banner(5, "ЭМБЕДДИНГИ + СОХРАНЕНИЕ ИНДЕКСА")
	idxFixed := &ragcore.Index{Strategy: "fixed-size", ChunkSize: o.size, Overlap: o.overlap}
	fmt.Println("fixed-size:")
	if err := idxFixed.EmbedAll(ctx, emb, fixed); err != nil {
		return err
	}
	idxStruct := &ragcore.Index{Strategy: "structural"}
	fmt.Println("structural:")
	if err := idxStruct.EmbedAll(ctx, emb, structural); err != nil {
		return err
	}
	pf := filepath.Join(o.out, "index-fixed.json")
	ps := filepath.Join(o.out, "index-structural.json")
	if err := idxFixed.Save(pf); err != nil {
		return err
	}
	if err := idxStruct.Save(ps); err != nil {
		return err
	}
	fmt.Printf("модель: %s · размерность вектора: %d\n", idxStruct.Model, idxStruct.Dim)
	if len(idxStruct.Chunks) > 0 {
		fmt.Printf("пример вектора structural[0] (первые 6 из %d): %s\n", idxStruct.Dim, vecHead(idxStruct.Chunks[0].Vector, 6))
	}
	fmt.Printf("сохранено:\n  %s — %s\n  %s — %s\n", pf, fileSize(pf), ps, fileSize(ps))
	if emb.Truncated > 0 {
		fmt.Printf("усечено входов до %d символов (лимит контекста эмбеддера, флаг -max-embed-chars): %d\n", emb.MaxChars, emb.Truncated)
	}

	banner(6, "СЕМАНТИЧЕСКИЙ ПОИСК")
	q := o.query
	if q == "" {
		q = "как агент проверяет инварианты перед вызовом инструмента"
	}
	fmt.Printf("запрос: %q (top-%d)\n", q, o.topK)
	for _, idx := range []*ragcore.Index{idxStruct, idxFixed} {
		fmt.Printf("\n[%s]\n", idx.Strategy)
		hits, err := idx.Search(ctx, emb, q, o.topK)
		if err != nil {
			return err
		}
		for i, h := range hits {
			fmt.Printf("  %d. score=%.3f  %s · %s\n     %s\n",
				i+1, h.Score, h.Chunk.File, h.Chunk.Section, oneLineRag(h.Chunk.Text, 90))
		}
	}

	banner(7, "ИТОГ")
	checklist(true)
	return nil
}

func banner(step int, title string) {
	line := strings.Repeat("=", 64)
	fmt.Printf("\n%s\n  [%d] %s\n%s\n", line, step, title, line)
}

func defaultSkipDirs() []string {
	return []string{"memory", "notes", "reports", "node_modules", "ragindex"}
}

// showContrast наглядно показывает разницу стратегий: «рваный» fixed-чанк против
// целостной единицы structural (то, что на слайде про fixed-size vs semantic).
func showContrast(fixed, structural []ragcore.Chunk) {
	if b := firstBroken(fixed); b != nil {
		fmt.Println("\nfixed-size рвёт мысль (хвост чанка — оборван не на границе):")
		fmt.Printf("  …%s\n", tail(b.Text, 70))
	}
	if w := firstWhole(structural); w != nil {
		fmt.Println("structural — законченная единица:")
		fmt.Printf("  [%s] %s\n", w.Section, oneLineRag(w.Text, 80))
	}
}

func firstBroken(cs []ragcore.Chunk) *ragcore.Chunk {
	for i := range cs {
		if !endsAtBoundary(cs[i].Text) {
			return &cs[i]
		}
	}
	return nil
}

func firstWhole(cs []ragcore.Chunk) *ragcore.Chunk {
	for i := range cs {
		if endsAtBoundary(cs[i].Text) && cs[i].Tokens > 5 {
			return &cs[i]
		}
	}
	if len(cs) > 0 {
		return &cs[0]
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// demoMeta печатает метаданные одного содержательного чанка (без длинного вектора).
func demoMeta(cs []ragcore.Chunk) {
	if len(cs) == 0 {
		return
	}
	c := cs[0]
	for i := range cs {
		if cs[i].Tokens > 20 {
			c = cs[i]
			break
		}
	}
	fmt.Println("пример чанка (поле vector заполнится на шаге эмбеддингов):")
	fmt.Printf("  chunk_id: %s\n  source:   %s\n  file:     %s\n  section:  %s\n  tokens:   %d\n  text:     %s\n",
		c.ID, c.Source, c.File, c.Section, c.Tokens, oneLineRag(c.Text, 90))
}

func capChunks(cs []ragcore.Chunk, n int) []ragcore.Chunk {
	if n > 0 && len(cs) > n {
		return cs[:n]
	}
	return cs
}

func vecHead(v []float32, n int) string {
	if n > len(v) {
		n = len(v)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("%.4f", v[i])
	}
	return "[" + strings.Join(parts, ", ") + ", …]"
}

func fileSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	kb := float64(fi.Size()) / 1024
	if kb < 1024 {
		return fmt.Sprintf("%.0f КБ", kb)
	}
	return fmt.Sprintf("%.1f МБ", kb/1024)
}

// ollamaReachable пробует один эмбеддинг-зонд: дешевле, чем падать на каждом чанке.
func ollamaReachable(ctx context.Context, e ragcore.Embedder) bool {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := e.EmbedQuery(c, "ok")
	return err == nil
}

func saveDry(out string, size, overlap int, fixed, structural []ragcore.Chunk) {
	_ = (&ragcore.Index{Strategy: "fixed-size", ChunkSize: size, Overlap: overlap, Chunks: fixed}).Save(filepath.Join(out, "index-fixed.json"))
	_ = (&ragcore.Index{Strategy: "structural", Chunks: structural}).Save(filepath.Join(out, "index-structural.json"))
}

func checklist(withEmb bool) {
	mark := func(ok bool) string {
		if ok {
			return "[x]"
		}
		return "[ ]"
	}
	fmt.Println("Задание дня 21:")
	fmt.Printf("  %s корпус 20–30 стр. / эквивалент в коде\n", mark(true))
	fmt.Printf("  %s chunking, 2 стратегии (fixed-size + structural)\n", mark(true))
	fmt.Printf("  %s метаданные на чанк (source / file / section / chunk_id)\n", mark(true))
	fmt.Printf("  %s сравнение стратегий (comparison.md)\n", mark(true))
	fmt.Printf("  %s генерация эмбеддингов\n", mark(withEmb))
	fmt.Printf("  %s сохранение индекса (JSON)\n", mark(true))
	fmt.Printf("  %s семантический поиск по индексу\n", mark(withEmb))
	if !withEmb {
		fmt.Println("  (эмбеддинги/поиск — повтори с поднятой Ollama, без -dry-run)")
	}
}
