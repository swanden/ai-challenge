package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"ai-challenge/week-5/task-25/ragcore"

	"github.com/anthropics/anthropic-sdk-go"
)

// День 24: цитаты, источники и анти-галлюцинации.
// Поверх улучшенного retrieval дня 23 модель обязана вернуть СТРОГО JSON:
//   { answer, sources:[{chunk_id,source,section}], quotes:[{chunk_id,text}] }
// Цитаты — дословные фрагменты из контекста; каждую проверяем ПО ТЕКСТУ ЧАНКОВ
// (детерминированно, кодом) — это и есть анти-галлюцинация: выдуманная цитата
// помечается как непроверенная. Если лучший кандидат ниже порога релевантности —
// режим «не знаю» (ответ не генерируется, просим уточнить).

// Citation — источник: файл + раздел + id чанка (задание: source + section/chunk_id).
type Citation struct {
	ChunkID string `json:"chunk_id"`
	Source  string `json:"source"`
	Section string `json:"section"`
}

// Quote — цитата из чанка + признак верификации (проставляется кодом).
type Quote struct {
	ChunkID string `json:"chunk_id"`
	Text    string `json:"text"`
	Match   string `json:"-"` // "exact" (дословно) | "partial" (по смыслу) | "none" (нет в чанках)
}

// GroundedReply — ответ дня 24 с обязательными источниками и цитатами.
type GroundedReply struct {
	Answer    string
	Sources   []Citation
	Quotes    []Quote
	Known     bool    // false = сработал режим «не знаю»
	IDKReason string  // причина «не знаю»: "threshold" (порог) | "model" (модель не нашла ответ в контексте)
	BestScore float64 // score лучшего кандидата (rerank 0–10 либо косинус)
}

// groundedReply — полный конвейер дня 24: retrieve (день 23) → порог «не знаю» →
// JSON-ответ с источниками и цитатами → верификация цитат по чанкам.
func groundedReply(ctx context.Context, a *Agent, r *Retriever, query string, cfg RerankConfig, knowThreshold float64) (GroundedReply, []ragcore.Hit, RetrievalTrace, error) {
	hits, tr, err := retrieveAdvanced(ctx, a, r, query, cfg)
	if err != nil {
		return GroundedReply{}, nil, tr, err
	}

	// Порог «не знаю»: лучший кандидат ниже порога (или пусто) → не отвечаем.
	best := 0.0
	if len(hits) > 0 {
		best = hits[0].Score
	}
	if len(hits) == 0 || best < knowThreshold {
		return GroundedReply{
			Known:     false,
			IDKReason: "threshold",
			BestScore: best,
			Answer:    "Не знаю: в базе знаний нет достаточно релевантного контекста для ответа. Уточните или переформулируйте вопрос.",
		}, hits, tr, nil
	}

	rep, err := a.groundedAnswer(ctx, cfg.Model, query, hits)
	if err != nil {
		return GroundedReply{}, hits, tr, err
	}
	rep.BestScore = best
	// Анти-галлюцинация: сверяем каждую цитату с текстом поднятых чанков.
	verifyQuotes(&rep, hits)
	return rep, hits, tr, nil
}

// groundedAnswer просит модель ответить СТРОГО JSON по контексту (метод агента —
// нужен клиент; инвариант/память тут не применяются: это QA-над-корпусом).
func (a *Agent) groundedAnswer(ctx context.Context, model anthropic.Model, query string, hits []ragcore.Hit) (GroundedReply, error) {
	sys := "Ты отвечаешь СТРОГО по предоставленному контексту из базы знаний. " +
		"Верни ТОЛЬКО JSON без пояснений и без ```-обёрток, по схеме:\n" +
		`{"answer": "...", "sources": [{"chunk_id":"...","source":"...","section":"..."}], "quotes": [{"chunk_id":"...","text":"..."}]}` + "\n" +
		"Требования: answer — на основе ТОЛЬКО контекста; sources — чанки, на которые опираешься; " +
		"quotes — ДОСЛОВНЫЕ фрагменты из текста этих чанков (копируй буквально, не перефразируй). " +
		"Если контекст не отвечает на вопрос — верни answer:\"не знаю\", sources:[], quotes:[]."
	user := "ВОПРОС: " + query + "\n\nКОНТЕКСТ (пронумерованные чанки):\n" + buildGroundedContext(hits)

	params := anthropic.MessageNewParams{
		Model: model, MaxTokens: 1024,
		System:   []anthropic.TextBlockParam{{Text: sys}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	}
	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return GroundedReply{}, err
	}
	rep, ok := parseGrounded(collectText(msg))
	if !ok {
		return GroundedReply{}, fmt.Errorf("не удалось разобрать JSON-ответ модели")
	}
	rep.Known = strings.TrimSpace(strings.ToLower(rep.Answer)) != "не знаю" && rep.Answer != ""
	if !rep.Known {
		rep.IDKReason = "model"
	}
	return rep, nil
}

// buildGroundedContext — контекст с chunk_id/source/section, чтобы модель могла цитировать.
func buildGroundedContext(hits []ragcore.Hit) string {
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] chunk_id=%s · source=%s · section=%s\n%s\n\n",
			i+1, h.Chunk.ID, h.Chunk.Source, h.Chunk.Section, strings.TrimSpace(h.Chunk.Text))
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseGrounded вытаскивает JSON-объект ответа (терпит ```-обёртки и текст вокруг).
func parseGrounded(s string) (GroundedReply, bool) {
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return GroundedReply{}, false
	}
	var raw struct {
		Answer  string     `json:"answer"`
		Sources []Citation `json:"sources"`
		Quotes  []Quote    `json:"quotes"`
	}
	if err := json.Unmarshal([]byte(s[i:j+1]), &raw); err != nil {
		return GroundedReply{}, false
	}
	return GroundedReply{Answer: raw.Answer, Sources: raw.Sources, Quotes: raw.Quotes}, true
}

// reLatexCmd — команды LaTeX (\emph, \tilde, \mathbb и т.п.); reNonWord — всё, кроме букв/цифр.
var (
	reLatexCmd = regexp.MustCompile(`\\[a-zA-Z]+`)
	reNonWord  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// normForMatch приводит текст к «словесному скелету»: нижний регистр, без LaTeX-команд,
// без пунктуации/математики/разметки — остаются только слова через один пробел. Это
// устраняет ложные ✗, когда модель скопировала фрагмент дословно, но иначе оформила
// юникод/пробелы/символы (\emph{}, ~$X$, L_2, ⟨w,x⟩ и т.п.).
func normForMatch(s string) string {
	s = strings.ToLower(s)
	s = reLatexCmd.ReplaceAllString(s, " ")
	s = reNonWord.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// verifyQuotes размечает каждую цитату по трём уровням достоверности:
//
//	exact   — «скелет» цитаты дословно встречается в тексте чанка (подстрока);
//	partial — не дословно, но ≥85% слов цитаты присутствуют в ОДНОМ чанке
//	          (склейка/перестановка/пропуск слова — фрагмент из источника, но не буквально);
//	none    — ни того, ни другого (вероятная выдумка/пересказ своими словами).
//
// Это градация faithfulness: строгий exact не смягчается (0 ложных «дословно»),
// а честная склейка получает partial вместо клейма none.
func verifyQuotes(rep *GroundedReply, hits []ragcore.Hit) {
	skeletons := make([]string, len(hits))
	wordsets := make([]map[string]bool, len(hits))
	for i, h := range hits {
		sk := normForMatch(h.Chunk.Text)
		skeletons[i] = sk
		wordsets[i] = toWordSet(sk)
	}
	for k := range rep.Quotes {
		q := normForMatch(rep.Quotes[k].Text)
		if len([]rune(q)) < 15 {
			rep.Quotes[k].Match = "none"
			continue
		}
		rep.Quotes[k].Match = matchLevel(q, skeletons, wordsets)
	}
}

// matchLevel: exact (подстрока) → partial (≥85% слов в одном чанке) → none.
func matchLevel(q string, skeletons []string, wordsets []map[string]bool) string {
	for _, sk := range skeletons {
		if strings.Contains(sk, q) {
			return "exact"
		}
	}
	qw := strings.Fields(q)
	if len(qw) == 0 {
		return "none"
	}
	best := 0.0
	for _, ws := range wordsets {
		hit := 0
		for _, w := range qw {
			if ws[w] {
				hit++
			}
		}
		if cov := float64(hit) / float64(len(qw)); cov > best {
			best = cov
		}
	}
	if best >= 0.85 {
		return "partial"
	}
	return "none"
}

func toWordSet(skeleton string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(skeleton) {
		m[w] = true
	}
	return m
}

// matchCounts — сколько цитат каждого уровня (exact/partial/none).
func matchCounts(qs []Quote) (exact, partial, none int) {
	for _, q := range qs {
		switch q.Match {
		case "exact":
			exact++
		case "partial":
			partial++
		default:
			none++
		}
	}
	return
}
