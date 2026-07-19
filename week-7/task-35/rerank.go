package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"ai-challenge/week-7/task-35/ragcore"

	"github.com/anthropics/anthropic-sdk-go"
)

// День 23: второй проход retrieval поверх bi-encoder-поиска дня 22.
// Конвейер: query rewrite → bi-encoder top-N → threshold → MMR → LLM-реранк → top-K.
// Детерминированные этапы (threshold, MMR) — в ragcore; rewrite и реранк — здесь,
// т.к. им нужен LLM (клиент агента).

// RerankConfig — параметры второго прохода.
type RerankConfig struct {
	Corpus    string          // "code" | "ru" — влияет на стиль query rewrite
	TopN      int             // сколько достаёт bi-encoder (вход второго прохода)
	Threshold float64         // порог отсечения по косинусу (0 = выкл)
	MMRLambda float64         // баланс релевантность/разнообразие (1 = MMR выкл)
	TopK      int             // сколько чанков уходит в LLM после реранка
	Rewrite   bool            // включить query rewrite
	Rerank    bool            // включить LLM-реранк
	Model     anthropic.Model // модель для rewrite/rerank (обычно Haiku)
}

// RetrievalTrace фиксирует размеры выдачи на каждом этапе — для сравнения «до/после».
type RetrievalTrace struct {
	Query      string
	Rewritten  string
	NBiEncoder int
	NThreshold int
	NMMR       int
	NFinal     int
	UniqSrc    int
}

// rewriteQuery переписывает запрос перед поиском. Для кода — в английские код-термины
// (лечит кросс-языковой разрыв RU-вопрос ↔ EN-код); для русского корпуса — расширяет
// синонимами на русском. При ошибке возвращает исходный запрос (fail-open).
func rewriteQuery(ctx context.Context, a *Agent, model anthropic.Model, corpus, query string) (string, error) {
	var sys string
	switch corpus {
	case "ru":
		sys = "Ты помощник семантического поиска по русскоязычному тексту лекций. " +
			"Перепиши вопрос пользователя в короткий поисковый запрос: добавь синонимы и " +
			"ключевые термины по теме (на русском). Верни ТОЛЬКО запрос, без пояснений."
	default: // "code"
		sys = "Ты помощник поиска по кодовой базе на Go. Вопрос задан на русском, а искать " +
			"нужно по английскому коду. Перепиши вопрос в короткий поисковый запрос: добавь " +
			"вероятные английские термины, имена функций/типов, ключевые слова кода. " +
			"Верни ТОЛЬКО переписанный запрос, без пояснений."
	}
	params := anthropic.MessageNewParams{
		Model: model, MaxTokens: 200,
		System:   []anthropic.TextBlockParam{{Text: sys}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(query))},
	}
	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return query, err
	}
	if out := strings.TrimSpace(collectText(msg)); out != "" {
		return out, nil
	}
	return query, nil
}

// llmRerank оценивает пары (запрос, чанк) ОДНИМ батч-запросом (экономия вызовов):
// модель возвращает JSON-массив оценок 0–10, по ним пересортировываем и берём top-K.
// Это cross-encoder-паттерн по лекции (query+doc судятся совместно), судья — LLM.
// При ошибке парсинга — fallback на порядок bi-encoder (fail-open).
func llmRerank(ctx context.Context, a *Agent, model anthropic.Model, query string, hits []ragcore.Hit, topK int) ([]ragcore.Hit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] %s · %s\n%s\n\n", i+1, h.Chunk.Source, h.Chunk.Section, oneLine(h.Chunk.Text, 500))
	}
	sys := "Оцени релевантность каждого фрагмента для запроса по шкале 0–10 " +
		"(10 = прямо отвечает на вопрос, 0 = не относится). Верни ТОЛЬКО JSON-массив " +
		"чисел длиной по числу фрагментов, в том же порядке, без пояснений и текста. " +
		"Пример формата: [7,2,9,0]"
	user := "ЗАПРОС: " + query + "\n\nФРАГМЕНТЫ:\n" + b.String()
	params := anthropic.MessageNewParams{
		Model: model, MaxTokens: 400,
		System:   []anthropic.TextBlockParam{{Text: sys}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	}
	// Батч-реранк капризен к формату ответа: пробуем до 2 раз, при неудаче —
	// откат на порядок bi-encoder (сохраняя косинусные score, не обнуляя их).
	var scores []float64
	var ok bool
	for attempt := 0; attempt < 2; attempt++ {
		msg, err := a.client.Messages.New(ctx, params)
		if err != nil {
			return topKByScore(hits, topK), err
		}
		if scores, ok = parseScores(collectText(msg), len(hits)); ok && !allZero(scores) {
			break
		}
		ok = false
	}
	if !ok {
		return topKByScore(hits, topK), nil
	}
	type sh struct {
		h ragcore.Hit
		s float64
	}
	arr := make([]sh, len(hits))
	for i, h := range hits {
		h.Score = scores[i] // перекрываем косинус оценкой реранкера (для вывода)
		arr[i] = sh{h, scores[i]}
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].s > arr[j].s })
	out := make([]ragcore.Hit, 0, topK)
	for i := 0; i < len(arr) && i < topK; i++ {
		out = append(out, arr[i].h)
	}
	return out, nil
}

// parseScores вытаскивает JSON-массив чисел из ответа модели (терпит ```-обёртки и текст).
func parseScores(s string, n int) ([]float64, bool) {
	i, j := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if i < 0 || j <= i {
		return nil, false
	}
	var raw []float64
	if err := json.Unmarshal([]byte(s[i:j+1]), &raw); err != nil {
		return nil, false
	}
	if len(raw) < n {
		// модель вернула меньше оценок, чем фрагментов — считаем ответ негодным,
		// чтобы не обнулять валидные косинусные score (иначе ложный «порог: не знаю»).
		return nil, false
	}
	return raw[:n], true
}

// allZero — все оценки нулевые (вырожденный ответ реранка → откат на косинус).
func allZero(xs []float64) bool {
	for _, x := range xs {
		if x != 0 {
			return false
		}
	}
	return true
}

// topKByScore — фолбэк-отбор по текущему score (косинус bi-encoder).
func topKByScore(hits []ragcore.Hit, k int) []ragcore.Hit {
	out := append([]ragcore.Hit(nil), hits...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && k < len(out) {
		out = out[:k]
	}
	return out
}

// retrieveAdvanced — полный конвейер второго прохода с трассировкой размеров этапов.
func retrieveAdvanced(ctx context.Context, a *Agent, r *Retriever, query string, cfg RerankConfig) ([]ragcore.Hit, RetrievalTrace, error) {
	tr := RetrievalTrace{Query: query, Rewritten: query}
	q := query
	if cfg.Rewrite {
		if rw, err := rewriteQuery(ctx, a, cfg.Model, cfg.Corpus, query); err == nil {
			q, tr.Rewritten = rw, rw
		}
	}
	hits, err := r.idx.Search(ctx, r.emb, q, cfg.TopN)
	if err != nil {
		return nil, tr, err
	}
	tr.NBiEncoder = len(hits)

	hits = ragcore.FilterThreshold(hits, cfg.Threshold)
	tr.NThreshold = len(hits)

	hits = ragcore.MMR(hits, cfg.MMRLambda, cfg.TopN)
	tr.NMMR = len(hits)

	if cfg.Rerank {
		hits, err = llmRerank(ctx, a, cfg.Model, q, hits, cfg.TopK)
		if err != nil {
			hits = topKByScore(hits, cfg.TopK)
		}
	} else {
		hits = topKByScore(hits, cfg.TopK)
	}
	tr.NFinal = len(hits)
	tr.UniqSrc = ragcore.UniqueSources(hits)
	return hits, tr, nil
}

// ragReplyAdvanced — ответ агента по улучшенному конвейеру (день 23).
func ragReplyAdvanced(ctx context.Context, a *Agent, r *Retriever, query string, p Policy, cfg RerankConfig) (Reply, []ragcore.Hit, RetrievalTrace, error) {
	hits, tr, err := retrieveAdvanced(ctx, a, r, query, cfg)
	if err != nil {
		return Reply{}, nil, tr, err
	}
	rep, err := a.AskRAG(ctx, query, p, buildContext(hits))
	if err != nil {
		return Reply{}, hits, tr, err
	}
	rep.Sources = sourcesOf(hits)
	return rep, hits, tr, nil
}
