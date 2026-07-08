package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"ai-challenge/week-6/task-27/ragcore"
)

// День 21. Пайплайн индексации документов для RAG.
//
// Корпус → две стратегии chunking (fixed-size+overlap | structural) →
// эмбеддинги (Ollama nomic-embed-text, чистый net/http) → JSON-индекс с
// метаданными на чанк (source/file/section/chunk_id) → сравнение стратегий.
//
// Индекс спроектирован под будущую обёртку MCP-тулом семантического поиска
// (Index.Search), чтобы накопительный агент смог искать по базе знаний.

// defaultDocsDir — корпус по умолчанию: родитель каталога ragindex, т.е. корень
// task-21 (исходники накопительного агента + README). Индексируем «сами себя».
func defaultDocsDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(filepath.Dir(file))
	}
	return "."
}

func main() {
	docs := flag.String("docs", defaultDocsDir(), "каталог-корпус (рекурсивно)")
	exts := flag.String("ext", ".go,.md,.txt", "расширения файлов корпуса через запятую")
	out := flag.String("out", ".", "каталог для индексов и comparison.md")
	baseURL := flag.String("ollama", "http://localhost:11434", "базовый URL Ollama")
	model := flag.String("model", "nomic-embed-text", "модель эмбеддингов Ollama")
	size := flag.Int("chunk-size", 500, "fixed-size: размер чанка в токенах (≈словах)")
	overlap := flag.Int("overlap", 50, "fixed-size: перекрытие в токенах (≈словах)")
	query := flag.String("query", "", "опц.: после индексации прогнать поиск top-k по обеим стратегиям")
	topK := flag.Int("k", 3, "сколько результатов показать для -query")
	dryRun := flag.Bool("dry-run", false, "только chunking + сравнение, без эмбеддингов (Ollama не нужна)")
	report := flag.Bool("report", false, "демонстрационный прогон для видео: показать весь чеклист задания по шагам")
	limit := flag.Int("limit", 0, "ограничить число эмбеддируемых чанков на стратегию (0 = все; для быстрого демо)")
	maxChars := flag.Int("max-embed-chars", 1800, "макс. длина текста чанка для эмбеддинга, символов (защита от лимита контекста модели)")
	flag.Parse()

	// День 21: единый демонстрационный прогон для видео — показывает всё по шагам.
	if *report {
		if err := runReport(context.Background(), reportOpts{
			docs: *docs, exts: *exts, out: *out,
			baseURL: *baseURL, model: *model,
			size: *size, overlap: *overlap,
			query: *query, topK: *topK, limit: *limit, dryRun: *dryRun,
			maxChars: *maxChars,
		}); err != nil {
			log.Fatalf("report: %v", err)
		}
		return
	}

	corpus, err := loadCorpus(*docs, strings.Split(*exts, ","), defaultSkipDirs())
	if err != nil {
		log.Fatalf("корпус: %v", err)
	}
	if len(corpus) == 0 {
		log.Fatalf("в %q не найдено файлов с расширениями %s", *docs, *exts)
	}

	totalTokens := 0
	for _, d := range corpus {
		totalTokens += approxTokens(d.Text)
	}
	fmt.Printf("Корпус: %d файлов, ~%d токенов (≈%d стр. по 500 слов) из %s\n",
		len(corpus), totalTokens, (totalTokens+499)/500, *docs)

	fixed := chunkFixed(corpus, *size, *overlap)
	structural := chunkStructural(corpus)
	fmt.Printf("Чанки: fixed-size=%d, structural=%d\n\n", len(fixed), len(structural))

	cmp := compareReport(computeStats("fixed-size", fixed), computeStats("structural", structural))
	fmt.Println(cmp)
	if err := os.WriteFile(filepath.Join(*out, "comparison.md"), []byte("```\n"+cmp+"```\n"), 0o644); err != nil {
		log.Printf("не записал comparison.md: %v", err)
	}

	if *dryRun {
		fmt.Println("[dry-run] эмбеддинги пропущены; сохраняю чанки с метаданными без векторов.")
		mustSave(&ragcore.Index{Strategy: "fixed-size", ChunkSize: *size, Overlap: *overlap, Chunks: fixed}, filepath.Join(*out, "index-fixed.json"))
		mustSave(&ragcore.Index{Strategy: "structural", Chunks: structural}, filepath.Join(*out, "index-structural.json"))
		return
	}

	ctx := context.Background()
	emb := ragcore.NewOllamaEmbedder(*baseURL, *model)
	emb.MaxChars = *maxChars

	idxFixed := &ragcore.Index{Strategy: "fixed-size", ChunkSize: *size, Overlap: *overlap}
	fmt.Println("Эмбеддинги (fixed-size):")
	if err := idxFixed.EmbedAll(ctx, emb, fixed); err != nil {
		log.Fatalf("эмбеддинги fixed-size: %v", err)
	}
	mustSave(idxFixed, filepath.Join(*out, "index-fixed.json"))

	idxStruct := &ragcore.Index{Strategy: "structural"}
	fmt.Println("Эмбеддинги (structural):")
	if err := idxStruct.EmbedAll(ctx, emb, structural); err != nil {
		log.Fatalf("эмбеддинги structural: %v", err)
	}
	mustSave(idxStruct, filepath.Join(*out, "index-structural.json"))

	fmt.Printf("\nСохранено: index-fixed.json (%d чанков, dim=%d) · index-structural.json (%d чанков, dim=%d)\n",
		len(idxFixed.Chunks), idxFixed.Dim, len(idxStruct.Chunks), idxStruct.Dim)
	if emb.Truncated > 0 {
		fmt.Printf("усечено входов до %d символов (лимит контекста эмбеддера): %d\n", emb.MaxChars, emb.Truncated)
	}

	if *query != "" {
		fmt.Printf("\nПоиск: %q (top-%d)\n", *query, *topK)
		for _, idx := range []*ragcore.Index{idxStruct, idxFixed} {
			fmt.Printf("\n[%s]\n", idx.Strategy)
			hits, err := idx.Search(ctx, emb, *query, *topK)
			if err != nil {
				log.Printf("поиск (%s): %v", idx.Strategy, err)
				continue
			}
			for i, h := range hits {
				fmt.Printf("  %d. %.3f  %s · %s\n     %s\n",
					i+1, h.Score, h.Chunk.File, h.Chunk.Section, oneLineRag(h.Chunk.Text, 100))
			}
		}
	}
}

func mustSave(idx *ragcore.Index, path string) {
	if err := idx.Save(path); err != nil {
		log.Fatalf("save %s: %v", path, err)
	}
}

func oneLineRag(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
