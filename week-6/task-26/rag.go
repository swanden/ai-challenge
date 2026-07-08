package main

import (
	"context"
	"fmt"
	"strings"

	"ai-challenge/week-6/task-26/ragcore"
)

// День 22: RAG-клиент для накопительного агента поверх общего пакета ragcore.
// Формат индекса (Chunk/Index), эмбеддер и поиск — едины с ragindex (день 21):
// оба импортируют ragcore, дублирования структур нет. Агент по-прежнему зависит
// от файла-индекса как от артефакта (читает то, что записал ragindex), но тип
// этого артефакта объявлен в одном месте.

// Retriever — тонкая обёртка над ragcore.Index + эмбеддером запроса.
type Retriever struct {
	idx  *ragcore.Index
	emb  *ragcore.OllamaEmbedder
	topK int
}

// NewRetriever загружает индекс (ragcore.LoadIndex) и готовит эмбеддер запросов.
func NewRetriever(indexPath, ollamaURL, model string, topK int) (*Retriever, error) {
	idx, err := ragcore.LoadIndex(indexPath)
	if err != nil {
		return nil, fmt.Errorf("индекс %s: %w (собери его в ragindex — день 21)", indexPath, err)
	}
	if len(idx.Chunks) == 0 || len(idx.Chunks[0].Vector) == 0 {
		return nil, fmt.Errorf("индекс %s пуст или без векторов (собран с -dry-run?)", indexPath)
	}
	if topK <= 0 {
		topK = 4
	}
	return &Retriever{idx: idx, emb: ragcore.NewOllamaEmbedder(ollamaURL, model), topK: topK}, nil
}

// Info — краткое описание подключённого индекса.
func (r *Retriever) Info() string {
	return fmt.Sprintf("%d чанков · strategy=%s · model=%s · dim=%d · top-%d",
		len(r.idx.Chunks), r.idx.Strategy, r.idx.Model, r.idx.Dim, r.topK)
}

// Retrieve возвращает top-k чанков по косинусной близости (поиск из ragcore).
func (r *Retriever) Retrieve(ctx context.Context, query string) ([]ragcore.Hit, error) {
	return r.idx.Search(ctx, r.emb, query, r.topK)
}

// buildContext склеивает найденные чанки в блок контекста для LLM (с источниками).
func buildContext(hits []ragcore.Hit) string {
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] источник: %s · раздел: %s\n%s\n\n",
			i+1, h.Chunk.Source, h.Chunk.Section, strings.TrimSpace(h.Chunk.Text))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sourcesOf — уникальные источники найденных чанков (для отчёта/цитат в ответе).
func sourcesOf(hits []ragcore.Hit) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hits {
		key := h.Chunk.Source + " · " + h.Chunk.Section
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// ragReply — полный RAG-запрос: retrieve → контекст → agent.AskRAG (контекст уходит
// отдельным system-блоком, не в текст вопроса, чтобы не триггерить стоп-слова инвариантов).
func ragReply(ctx context.Context, a *Agent, r *Retriever, query string, p Policy) (Reply, []ragcore.Hit, error) {
	hits, err := r.Retrieve(ctx, query)
	if err != nil {
		return Reply{}, nil, err
	}
	rep, err := a.AskRAG(ctx, query, p, buildContext(hits))
	if err != nil {
		return Reply{}, hits, err
	}
	rep.Sources = sourcesOf(hits)
	return rep, hits, nil
}
