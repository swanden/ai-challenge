package main

import (
	"context"
	"fmt"
	"strings"

	"ai-challenge/week-6/task-27/ragcore"
)

// День 23. Q23 — контрольный вопрос с тегом типа: sem (семантический — «объясни
// логику», стихия RAG) / nav (навигационный — «где определено» — RAG слаб, нужен
// AST/grep). Разделение из обсуждения: эмбеддинги ловят смысл, а не адрес в коде.
type Q23 struct {
	Q       string
	Expect  string
	Sources []string
	Kind    string // "sem" | "nav"
}

// codeQuestions23 — те же 10 вопросов дня 22 (тот же бенч: 8/16 → сравниваем),
// теперь размеченные sem/nav.
func codeQuestions23() []Q23 {
	return []Q23{
		{"Сколько слоёв памяти у агента и какие они?", "три слоя: short-term, working, long-term", []string{"memory.go"}, "sem"},
		{"Какие этапы у конечного автомата задачи и какие переходы разрешены?", "planning→execution→validation→done; возврат validation→execution", []string{"state.go"}, "nav"},
		{"Что мешает перейти из planning в execution?", "gate: план не утверждён (PlanApproved)", []string{"memory.go", "state.go"}, "nav"},
		{"Как агент проверяет инварианты перед вызовом инструмента?", "runTool: EnforceCode + опц. EnforceLLM; отказ как tool_result is_error", []string{"agent.go", "enforce.go"}, "sem"},
		{"Чем проверка инвариантов кодом отличается от проверки через LLM?", "EnforceCode детерминирован; EnforceLLM семантичен; не взаимоисключают", []string{"enforce.go", "invariants.go"}, "sem"},
		{"Что делает ленивая загрузка схем инструментов (lazy tools)?", "каталог серверов + load_tools; схемы по требованию — экономия токенов", []string{"mcptools.go", "agent.go"}, "sem"},
		{"Как рой ревьюеров принимает решение на этапе validation?", "ревьюеры → оркестратор PASS YES/NO; не-PASS → rework, лимит кругов", []string{"swarm.go", "pipeline.go"}, "sem"},
		{"Как считается стоимость запроса к модели?", "costFor: токены×цена за 1e6", []string{"usage.go"}, "nav"},
		{"Как сервер погоды хранит наблюдения и почему ключ — координаты?", "Store в JSON; ключ-координаты против дублей 'Berlin'/'Берлин'", []string{"weatherserver/store.go"}, "sem"},
		{"Что происходит при паузе пайплайна и как идёт возобновление?", "state и gate-флаги персистятся; продолжаем с того же этапа", []string{"pipeline.go", "memory.go"}, "sem"},
	}
}

// ruQuestions23 — русский корпус (конспекты Соколова, ВШЭ). Семантические вопросы;
// источники выверены по содержимому лекций. Одноязычный кейс: RAG в своей стихии.
func ruQuestions23() []Q23 {
	return []Q23{
		{"Что такое решающее дерево и по какому критерию выбирается разбиение в узле?", "дерево; критерий информативности/impurity", []string{"lecture07-trees.txt"}, "sem"},
		{"Что такое гребневая регрессия и зачем нужна L2-регуляризация?", "ридж; штраф на норму весов против переобучения", []string{"lecture02-linregr.txt", "lecture03-linregr.txt"}, "sem"},
		{"Как устроена логистическая регрессия и что она предсказывает?", "линейная классификация; вероятности через сигмоиду", []string{"lecture04-linclass.txt", "lecture05-linclass.txt", "lecture06-linclass.txt"}, "sem"},
		{"Что такое бэггинг и случайный лес?", "бутстрэп-композиция; усреднение деревьев", []string{"lecture08-ensembles.txt"}, "sem"},
		{"Как работает градиентный бустинг?", "последовательная композиция по антиградиенту", []string{"lecture09-ensembles.txt", "lecture10-ensembles.txt"}, "sem"},
		{"Что такое метод главных компонент и матричные разложения?", "PCA; снижение размерности через разложение", []string{"lecture12-factorizations.txt"}, "sem"},
		{"Какие бывают методы кластеризации?", "unsupervised; k-means и др.", []string{"lecture11-unsupervised.txt"}, "sem"},
		{"Что такое отступ (margin) объекта в линейной классификации?", "margin = y·<w,x>; знак = верность, модуль = уверенность", []string{"lecture04-linclass.txt"}, "sem"},
	}
}

// corpusSpec — один корпус в матрице сравнения.
type corpusSpec struct {
	name  string // "code" | "ru"
	index string // путь к index-structural.json
	qs    []Q23
}

// runEval23 — сравнение 2×2: {код, русский} × {baseline day-22, improved day-23}.
// Метрика — retrieval: попадание в ожидаемые источники + размеры выдачи по этапам.
// Ответы LLM не генерируются (это ретрив-сравнение; экономим вызовы — см. чат про время).
func runEval23(ctx context.Context, a *Agent, p Policy, ollamaURL, embedModel string, cfg RerankConfig, specs []corpusSpec) error {
	ragBanner(0, "УЛУЧШЕННЫЙ RAG — СРАВНЕНИЕ 2×2 (день 23)")
	fmt.Println("МАТРИЦА: {код-корпус, русский-корпус} × {baseline день22, improved день23}.")
	fmt.Println("BASELINE: только bi-encoder (косинус) → top-K. Как в дне 22.")
	fmt.Println("IMPROVED: query rewrite → bi-encoder top-N → порог → MMR → LLM-реранк → top-K.")
	fmt.Printf("ПАРАМЕТРЫ improved: top-N=%d · порог=%.2f · MMR λ=%.2f · top-K=%d · rewrite=%v · rerank=%v · модель=%s\n",
		cfg.TopN, cfg.Threshold, cfg.MMRLambda, cfg.TopK, cfg.Rewrite, cfg.Rerank, cfg.Model)
	fmt.Println("МЕТРИКА: доля попадания в ожидаемые файлы-источники (retrieval), + размеры этапов.")

	type cell struct{ got, exp int }
	results := map[string]map[string]cell{} // corpus -> mode -> cell

	step := 0
	for _, sp := range specs {
		results[sp.name] = map[string]cell{"baseline": {}, "improved": {}}
		r, err := NewRetriever(sp.index, ollamaURL, embedModel, cfg.TopK)
		if err != nil {
			return fmt.Errorf("корпус %s: %w", sp.name, err)
		}
		step++
		ragBanner(step, fmt.Sprintf("КОРПУС «%s» — %s", sp.name, r.Info()))

		baseCfg := RerankConfig{Corpus: sp.name, TopN: cfg.TopK, Threshold: 0, MMRLambda: 1, TopK: cfg.TopK, Rewrite: false, Rerank: false, Model: cfg.Model}
		impCfg := cfg
		impCfg.Corpus = sp.name

		for i, q := range sp.qs {
			fmt.Printf("\n%s\n", ragRule(fmt.Sprintf("[%s] В%d/%d · %s", sp.name, i+1, len(sp.qs), strings.ToUpper(q.Kind))))
			fmt.Printf("ВОПРОС:            %s\n", q.Q)
			fmt.Printf("ОЖИДАЕМ ИСТОЧНИКИ: %s\n", strings.Join(q.Sources, ", "))

			baseHits, _, err := retrieveAdvanced(ctx, a, r, q.Q, baseCfg)
			if err != nil {
				return fmt.Errorf("%s В%d baseline: %w", sp.name, i+1, err)
			}
			bg, be := sourceHits23(q.Sources, baseHits)
			fmt.Printf("  BASELINE  top-%d: %s → источники %d/%d\n", len(baseHits), topSrc(baseHits), bg, be)

			impHits, tr, err := retrieveAdvanced(ctx, a, r, q.Q, impCfg)
			if err != nil {
				return fmt.Errorf("%s В%d improved: %w", sp.name, i+1, err)
			}
			ig, ie := sourceHits23(q.Sources, impHits)
			if impCfg.Rewrite && tr.Rewritten != q.Q {
				fmt.Printf("  REWRITE → %s\n", oneLine(tr.Rewritten, 100))
			}
			fmt.Printf("  этапы: bi-encoder %d → порог %d → MMR %d → финал %d (уник. источников %d)\n",
				tr.NBiEncoder, tr.NThreshold, tr.NMMR, tr.NFinal, tr.UniqSrc)
			fmt.Printf("  IMPROVED  top-%d: %s → источники %d/%d\n", len(impHits), topSrc(impHits), ig, ie)

			bc := results[sp.name]["baseline"]
			bc.got += bg
			bc.exp += be
			results[sp.name]["baseline"] = bc
			ic := results[sp.name]["improved"]
			ic.got += ig
			ic.exp += ie
			results[sp.name]["improved"] = ic
		}
	}

	step++
	ragBanner(step, "ИТОГ — МАТРИЦА RETRIEVAL (попадание в источники)")
	fmt.Printf("%-16s | %-14s | %-14s\n", "корпус", "baseline", "improved")
	fmt.Println(strings.Repeat("-", 50))
	for _, sp := range specs {
		b := results[sp.name]["baseline"]
		im := results[sp.name]["improved"]
		fmt.Printf("%-16s | %-14s | %-14s\n", sp.name, frac(b.got, b.exp), frac(im.got, im.exp))
	}
	fmt.Println()
	fmt.Println("ЧТО ВИДНО:")
	fmt.Println("  • код (RU-вопрос ↔ EN-код): baseline слаб (кросс-язык); improved поднимает за счёт rewrite+реранка.")
	fmt.Println("  • русский корпус (язык совпадает): baseline уже высок — RAG в своей стихии; improved причёсывает.")
	fmt.Println("  • разрыв код↔русский иллюстрирует: bi-encoder-RAG уместен для семантики на одном языке,")
	fmt.Println("    а на сыром коде + кросс-языке нужен второй проход (это и есть день 23).")
	fmt.Println()
	ragChecklist23()
	return nil
}

func sourceHits23(expected []string, hits []ragcore.Hit) (got, total int) {
	total = len(expected)
	if total == 0 {
		return 0, 0
	}
	have := map[string]bool{}
	for _, h := range hits {
		have[h.Chunk.Source] = true
	}
	for _, e := range expected {
		for src := range have {
			if strings.Contains(src, e) || strings.Contains(e, src) {
				got++
				break
			}
		}
	}
	return got, total
}

func topSrc(hits []ragcore.Hit) string {
	if len(hits) == 0 {
		return "(пусто)"
	}
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, fmt.Sprintf("%s(%.2f)", h.Chunk.Source, h.Score))
	}
	return strings.Join(parts, ", ")
}

func frac(a, b int) string {
	if b == 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", a, b, 100*float64(a)/float64(b))
}

func ragChecklist23() {
	fmt.Println("ЧЕКЛИСТ ЗАДАНИЯ ДНЯ 23:")
	fmt.Println("  [x] второй этап после поиска: порог + MMR + LLM-реранк")
	fmt.Println("  [x] query rewrite перед поиском")
	fmt.Println("  [x] порог отсечения нерелевантных результатов")
	fmt.Println("  [x] top-K до и после фильтрации (размеры этапов в выводе)")
	fmt.Println("  [x] сравнение качества без фильтра/rewrite и с ними (baseline vs improved)")
	fmt.Println("  [x] контраст двух корпусов: кросс-языковой код vs одноязычный русский текст")
}
