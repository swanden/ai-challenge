// Package ragcore — единая точка правды формата RAG-индекса дня 21/22.
// Здесь живут тип чанка, индекс, эмбеддер и поиск. И утилита индексации
// (ragindex, cmd), и накопительный агент импортируют этот пакет — формат
// объявлен один раз, рассинхрон между продьюсером и консьюмером невозможен.
package ragcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// Embedder — абстракция эмбеддера. Методы РАЗДЕЛЕНЫ на документ и запрос:
// инструктивные модели (nomic-embed-text, E5, BGE…) обучены с разными
// префиксами задачи для индексируемого текста и поискового запроса, и без
// этого разделения качество поиска резко падает (один обзорный документ
// начинает матчить все запросы). См. OllamaEmbedder.
type Embedder interface {
	EmbedDocument(ctx context.Context, text string) ([]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	Name() string
}

// OllamaEmbedder — локальный эмбеддер через HTTP API Ollama (без SDK, чистый net/http).
//
//	POST {baseURL}/api/embeddings  body {"model":..,"prompt":..} -> {"embedding":[..]}
//
// Для nomic-embed-text вектор 768-мерный. Данные не покидают машину (см. «Why Ollama»).
//
// ВАЖНО: nomic-embed-text обучен с префиксами задачи. Документы индексируются с
// "search_document: ", запрос — с "search_query: ". Префиксы асимметричны намеренно:
// без них пространство «схлопывается» и длинный обзорный текст матчит всё подряд.
type OllamaEmbedder struct {
	BaseURL     string
	Model       string
	MaxChars    int    // макс. длина входа в рунах: защита от лимита контекста модели (0 = без лимита)
	DocPrefix   string // префикс для индексируемых чанков (nomic: "search_document: ")
	QueryPrefix string // префикс для поискового запроса (nomic: "search_query: ")
	Truncated   int    // сколько входов было усечено (для отчёта)
	http        *http.Client
}

func NewOllamaEmbedder(baseURL, model string) *OllamaEmbedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OllamaEmbedder{
		BaseURL:     baseURL,
		Model:       model,
		MaxChars:    1800, // у nomic-embed-text контекст 2048 токенов; токенов ≤ символов, 1800 — безопасно
		DocPrefix:   "search_document: ",
		QueryPrefix: "search_query: ",
		http:        &http.Client{Timeout: 60 * time.Second},
	}
}

func (o *OllamaEmbedder) Name() string { return "ollama/" + o.Model }

// EmbedDocument эмбеддит индексируемый чанк (префикс search_document:).
func (o *OllamaEmbedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return o.embed(ctx, o.DocPrefix, text)
}

// EmbedQuery эмбеддит поисковый запрос (префикс search_query:).
func (o *OllamaEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return o.embed(ctx, o.QueryPrefix, text)
}

func (o *OllamaEmbedder) embed(ctx context.Context, prefix, text string) ([]float32, error) {
	// Усекаем СОДЕРЖИМОЕ (не префикс) под лимит контекста модели.
	if t, cut := clipRunes(text, o.MaxChars); cut {
		text = t
		o.Truncated++
	}
	body, _ := json.Marshal(map[string]string{"model": o.Model, "prompt": prefix + text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama недоступен: %w (запущен ли `ollama serve`, есть ли `ollama pull %s`?)", err, o.Model)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama вернул статус %d", res.StatusCode)
	}

	var out struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("ollama: пустой вектор (модель %q вернула 0 чисел)", o.Model)
	}
	return l2normalize(out.Embedding), nil
}

// clipRunes усекает строку до max рун; возвращает признак усечения.
func clipRunes(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

// l2normalize приводит вектор к единичной длине, поэтому косинус сводится к
// скалярному произведению (в лекции упрощённо «делить на макс»; для cosine
// корректна именно L2-норма).
func l2normalize(v []float64) []float32 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	if norm == 0 {
		for i, x := range v {
			out[i] = float32(x)
		}
		return out
	}
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}
