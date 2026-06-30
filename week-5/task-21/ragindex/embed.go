package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// Embedder — абстракция эмбеддера: на вход текст, на выход вектор.
// Реализован Ollama; заменить на OpenAI/Voyage/Jina = другая реализация Embed
// (профиль «минимум зависимостей»: эмбеддер прячется за интерфейсом, остальной
// пайплайн от провайдера не зависит).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Name() string
}

// OllamaEmbedder — локальный эмбеддер через HTTP API Ollama (без SDK, чистый net/http).
//
//	POST {baseURL}/api/embeddings  body {"model":..,"prompt":..} -> {"embedding":[..]}
//
// Для nomic-embed-text вектор 768-мерный. Данные не покидают машину (см. «Why Ollama»).
type OllamaEmbedder struct {
	BaseURL   string
	Model     string
	MaxChars  int // макс. длина входа в рунах: защита от лимита контекста модели (0 = без лимита)
	Truncated int // сколько входов было усечено (для отчёта)
	http      *http.Client
}

func NewOllamaEmbedder(baseURL, model string) *OllamaEmbedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OllamaEmbedder{
		BaseURL:  baseURL,
		Model:    model,
		MaxChars: 1800, // у nomic-embed-text контекст 2048 токенов; токенов ≤ символов, 1800 — безопасно
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

func (o *OllamaEmbedder) Name() string { return "ollama/" + o.Model }

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Защита от лимита контекста эмбеддера: усечь слишком длинный вход. Для этой
	// модели число токенов ≤ числа символов, поэтому лимит по рунам гарантирует
	// влезание в 2048-токенный батч (иначе Ollama отвечает 500). Прод-альтернатива —
	// под-резать чанк на части и усреднять векторы; здесь усекаем + считаем (см. отчёт).
	if t, cut := clipRunes(text, o.MaxChars); cut {
		text = t
		o.Truncated++
	}
	body, _ := json.Marshal(map[string]string{"model": o.Model, "prompt": text})
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
// скалярному произведению. В лекции упрощённо «делить на макс»; для cosine
// корректна именно L2-норма — её и используем.
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
