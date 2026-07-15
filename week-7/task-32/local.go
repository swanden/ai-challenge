package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// День 26: генерирующая локальная LLM через HTTP API Ollama.
//
// Это ВТОРОЙ локальный движок в проекте. На неделе 5 (RAG) мы уже ходили в Ollama
// за ВЕКТОРАМИ (ragcore.OllamaEmbedder, эндпоинт /api/embeddings). Здесь — за
// ГЕНЕРАЦИЕЙ ТЕКСТА (/api/chat, /api/generate). Лекция явно разводит эти два класса
// моделей: «векторизующие» (эмбеддер, вектор, НЕ отвечает на вопросы) и
// «генерирующие» (LLM, отвечает). Тот же транспорт (чистый net/http, без SDK,
// как у эмбеддера), тот же принцип «данные не покидают машину».
//
// Никакого anthropic-sdk-go здесь нет намеренно: LocalLLM — самодостаточный клиент
// на stdlib. Это и есть шов, вокруг которого на дне 27 (интеграция во все флоу
// + тумблер облако↔локаль) появится общий интерфейс Chat. Пока же тумблера нет —
// см. README, раздел «Развилка дня 27».
//
// CLI-эквивалент того же самого (галочка «доступна через CLABI или HTTP» из задания):
//
//	ollama run qwen2.5:7b "Столица Франции?"                      # CLI
//	curl localhost:11434/api/chat -d '{"model":"qwen2.5:7b", ...}' # HTTP (то, что делаем тут)

// LocalLLM — клиент генерирующей модели в Ollama.
type LocalLLM struct {
	BaseURL    string
	Model      string
	Temp       float64 // температура сэмплирования (0 = детерминированнее)
	NumPredict int     // потолок токенов ответа (аналог MaxTokens; -1 = без лимита)
	NumCtx     int     // день 29: размер контекстного окна (num_ctx); 0 = дефолт модели
	http       *http.Client
}

// NewLocalLLM собирает клиент с разумными дефолтами. Таймаут щедрый: локальная
// генерация на CPU/слабой GPU медленная (лекция: «локальные будут работать
// медленнее»), первый запрос ещё и грузит веса в память (load_duration).
func NewLocalLLM(baseURL, model string) *LocalLLM {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "qwen2.5:7b"
	}
	return &LocalLLM{
		BaseURL:    baseURL,
		Model:      model,
		Temp:       0.2,
		NumPredict: 512,
		http:       &http.Client{Timeout: 5 * time.Minute},
	}
}

func (l *LocalLLM) Name() string { return "ollama/" + l.Model }

// LocalStats — метрики одного запроса. Часть приходит от сервера (Ollama отдаёт
// счётчики токенов и наносекундные длительности), Wall меряем сами (клиентское
// wall-clock, включает сеть и load_duration первого запроса).
type LocalStats struct {
	PromptTokens int           // prompt_eval_count — токенов во входе
	EvalTokens   int           // eval_count — токенов сгенерировано
	EvalDur      time.Duration // eval_duration — время именно генерации (без загрузки весов)
	LoadDur      time.Duration // load_duration — загрузка весов в память (велика на первом запросе)
	Wall         time.Duration // измерено клиентом: полное время запроса
}

// TokPerSec — скорость генерации (токен/сек) по серверному eval_duration.
// Это честная метрика «толщины» модели: чем больше параметров, тем медленнее
// инференс (лекция). 0, если сервер не прислал счётчики.
func (s LocalStats) TokPerSec() float64 {
	if s.EvalDur <= 0 || s.EvalTokens <= 0 {
		return 0
	}
	return float64(s.EvalTokens) / s.EvalDur.Seconds()
}

func (s LocalStats) String() string {
	return fmt.Sprintf("вход %d ток · выход %d ток · %.1f ток/с · генерация %s · загрузка %s · wall %s",
		s.PromptTokens, s.EvalTokens, s.TokPerSec(),
		round2(s.EvalDur), round2(s.LoadDur), round2(s.Wall))
}

// chatMsg — сообщение в формате Ollama /api/chat (роль + текст).
type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaOptions — параметры сэмплирования (совместимо с полем "options" Ollama).
type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
	NumPredict  int     `json:"num_predict"`
	NumCtx      int     `json:"num_ctx,omitempty"` // день 29: контекстное окно (0 = не слать, дефолт модели)
}

// --- /api/chat: диалоговый вызов (system + история + user) ---

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMsg     `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  ollamaOptions `json:"options"`
}

type chatResponse struct {
	Message         chatMsg `json:"message"`
	Done            bool    `json:"done"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	EvalDuration    int64   `json:"eval_duration"` // наносекунды
	LoadDuration    int64   `json:"load_duration"` // наносекунды
	Error           string  `json:"error"`
}

// Chat — диалоговый запрос к локальной модели. system может быть пустым; history —
// предыдущие ходы (роли "user"/"assistant"); user — текущее сообщение.
// Non-stream (stream:false): ждём весь ответ и парсим один JSON — детерминированно
// и без склейки чанков (стриминг оставлен на потом, для видео он не нужен).
func (l *LocalLLM) Chat(ctx context.Context, system string, history []chatMsg, user string) (string, LocalStats, error) {
	msgs := make([]chatMsg, 0, len(history)+2)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, chatMsg{Role: "system", Content: system})
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, chatMsg{Role: "user", Content: user})

	reqBody := chatRequest{
		Model: l.Model, Messages: msgs, Stream: false,
		Options: ollamaOptions{Temperature: l.Temp, NumPredict: l.NumPredict, NumCtx: l.NumCtx},
	}
	var out chatResponse
	stats, err := l.do(ctx, "/api/chat", reqBody, &out)
	if err != nil {
		return "", stats, err
	}
	if out.Error != "" {
		return "", stats, fmt.Errorf("ollama /api/chat: %s", out.Error)
	}
	stats.PromptTokens = out.PromptEvalCount
	stats.EvalTokens = out.EvalCount
	stats.EvalDur = time.Duration(out.EvalDuration)
	stats.LoadDur = time.Duration(out.LoadDuration)
	text := strings.TrimSpace(out.Message.Content)
	if text == "" {
		return "", stats, fmt.Errorf("ollama вернул пустой ответ (модель %q)", l.Model)
	}
	return text, stats, nil
}

// --- /api/generate: одиночный prompt без ролей (для самого простого запроса) ---

type genRequest struct {
	Model   string        `json:"model"`
	Prompt  string        `json:"prompt"`
	Stream  bool          `json:"stream"`
	Options ollamaOptions `json:"options"`
}

type genResponse struct {
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	EvalDuration    int64  `json:"eval_duration"`
	LoadDuration    int64  `json:"load_duration"`
	Error           string `json:"error"`
}

// Generate — одиночный prompt через /api/generate (без диалоговых ролей).
// Держим этот путь, чтобы показать: локальную модель можно дёргать и «в лоб»,
// как из CLI (`ollama run <m> "<prompt>"`), а не только диалогом.
func (l *LocalLLM) Generate(ctx context.Context, prompt string) (string, LocalStats, error) {
	reqBody := genRequest{
		Model: l.Model, Prompt: prompt, Stream: false,
		Options: ollamaOptions{Temperature: l.Temp, NumPredict: l.NumPredict, NumCtx: l.NumCtx},
	}
	var out genResponse
	stats, err := l.do(ctx, "/api/generate", reqBody, &out)
	if err != nil {
		return "", stats, err
	}
	if out.Error != "" {
		return "", stats, fmt.Errorf("ollama /api/generate: %s", out.Error)
	}
	stats.PromptTokens = out.PromptEvalCount
	stats.EvalTokens = out.EvalCount
	stats.EvalDur = time.Duration(out.EvalDuration)
	stats.LoadDur = time.Duration(out.LoadDuration)
	text := strings.TrimSpace(out.Response)
	if text == "" {
		return "", stats, fmt.Errorf("ollama вернул пустой ответ (модель %q)", l.Model)
	}
	return text, stats, nil
}

// do — общий транспорт: POST JSON → декодировать в out, замерить wall-clock.
// Зеркалит стиль ragcore.OllamaEmbedder.embed: та же диагностика «запущен ли
// ollama serve, скачана ли модель».
func (l *LocalLLM) do(ctx context.Context, path string, reqBody any, out any) (LocalStats, error) {
	var stats LocalStats
	body, err := json.Marshal(reqBody)
	if err != nil {
		return stats, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return stats, err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	res, err := l.http.Do(req)
	if err != nil {
		return stats, fmt.Errorf("ollama недоступен: %w (запущен ли `ollama serve`, скачана ли модель `ollama pull %s`?)", err, l.Model)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("ollama вернул статус %d на %s (нет ли опечатки в имени модели %q?)", res.StatusCode, path, l.Model)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return stats, fmt.Errorf("ollama: не разобрать ответ %s: %w", path, err)
	}
	stats.Wall = time.Since(start)
	return stats, nil
}

// --- health: доступен ли сервер и скачана ли модель ---

type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// Health проверяет, что Ollama поднят (GET /api/tags) и что нужная модель уже
// скачана. Если модель не найдена — не фейлим жёстко (имена тегов различаются,
// напр. "qwen2.5:7b" vs "qwen2.5:latest"), а возвращаем понятную подсказку.
func (l *LocalLLM) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	res, err := l.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama недоступен на %s: %w (запусти `ollama serve`)", l.BaseURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama /api/tags вернул статус %d", res.StatusCode)
	}
	var tags tagsResponse
	if err := json.NewDecoder(res.Body).Decode(&tags); err != nil {
		return err
	}
	base := baseModel(l.Model)
	for _, m := range tags.Models {
		if m.Name == l.Model || baseModel(m.Name) == base {
			return nil
		}
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	return fmt.Errorf("модель %q не найдена в Ollama (есть: %s). Скачай: `ollama pull %s`",
		l.Model, strings.Join(names, ", "), l.Model)
}

// baseModel отбрасывает тег после ':' ("qwen2.5:7b" → "qwen2.5").
func baseModel(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i]
	}
	return s
}

// round2 округляет длительность до сотых секунды для читабельного вывода.
func round2(d time.Duration) time.Duration {
	if d == 0 {
		return 0
	}
	return d.Round(10 * time.Millisecond)
}
