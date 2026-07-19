package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// День 35. РЕАЛЬНАЯ ЗАДАЧА: ассистент разбора логов ошибок ("Log Triage").
//
// Задача, которую хочется автоматизировать: когда сервис падает, дежурный смотрит в
// лог и пытается понять, что за ошибка и как её чинить. Большая часть падений —
// это одни и те же знакомые грабли (таймаут БД, OOM, паника из-за nil, исчерпание
// пула, 429 от внешнего API, протухший TLS). Ассистент берёт лог на вход и:
//
//   1) ищет по БАЗЕ ЗНАКОМЫХ ПРОБЛЕМ (RAG по triage/kb) — узнаёт паттерн и сразу
//      выдаёт диагноз и фикс со ссылкой на конкретную статью базы;
//   2) если паттерн НЕзнакомый (поиск ниже порога) — передаёт лог модели, и она
//      разбирает его по существу как инженер, честно помечая, что это не из базы.
//
// AI участвует в двух местах осмысленно: RAG-поиск (узнавание знакомого) и
// рассуждение модели (обобщение незнакомого). Всё переиспользует конвейер недели:
// retrieveAdvanced, grounded-ответ с цитатами, Completer, порог «не знаю» из дня 24.
//
// Домен абстрактный и общечеловеческий — типовые ошибки бэкенда, знакомые любому,
// кто держал сервис в проде. Никакой привязки к конкретной инфраструктуре.

// TriageSource — откуда пришёл разбор.
type TriageSource int

const (
	SourceKB      TriageSource = iota // узнано по базе знакомых проблем (RAG)
	SourceModel                       // разобрано моделью (паттерн незнакомый)
	SourceUnknown                     // не хватило данных
)

// TriageResult — итог разбора одного лога.
type TriageResult struct {
	Signal    string // ключевые строки лога (ошибки), по которым шёл поиск
	Source    TriageSource
	Diagnosis string // что произошло
	Fix       string // как чинить
	Article   string // статья базы (если Source==SourceKB)
	Score     float64
	Reply     GroundedReply // для KB-случая — grounded-ответ с цитатами
}

// extractSignal вытаскивает из лога строки, релевантные для поиска: уровни
// ERROR/FATAL/panic и системные признаки (OOMKilled, Killed). Обычные INFO-строки
// шумят, поэтому в сигнал не идут — по нему потом строится запрос к базе.
func extractSignal(logText string) string {
	var keep []string
	for _, ln := range strings.Split(logText, "\n") {
		up := strings.ToUpper(ln)
		switch {
		case strings.Contains(up, "ERROR"),
			strings.Contains(up, "FATAL"),
			strings.Contains(up, "PANIC"),
			strings.Contains(up, "OOM"),
			strings.Contains(up, "KILLED"),
			strings.Contains(up, "X509"),
			strings.Contains(up, "TLS"),
			strings.Contains(up, "TIMEOUT"),
			strings.Contains(up, "EXITCODE"):
			keep = append(keep, strings.TrimSpace(ln))
		}
	}
	if len(keep) == 0 {
		// нет явных маркеров — берём последние строки, часто ошибка в конце
		lines := strings.Split(strings.TrimRight(logText, "\n"), "\n")
		if len(lines) > 4 {
			lines = lines[len(lines)-4:]
		}
		keep = lines
	}
	return strings.Join(keep, "\n")
}

// triageLog — конвейер разбора одного лога.
func triageLog(ctx context.Context, a *Agent, r *Retriever, logText string,
	cfg RerankConfig, knowThreshold float64) (TriageResult, error) {

	signal := extractSignal(logText)
	res := TriageResult{Signal: signal}

	// 1. Поиск по базе знакомых проблем.
	hits, _, err := retrieveAdvanced(ctx, a, r, signal, cfg)
	if err != nil {
		return res, err
	}
	best := 0.0
	if len(hits) > 0 {
		best = hits[0].Score
	}
	res.Score = best

	// 2. Узнали паттерн → grounded-ответ с цитатой на статью базы.
	if best >= knowThreshold {
		rep, err := a.groundedAnswer(ctx, cfg.Model,
			"По этому логу: что произошло и как чинить? Опирайся только на найденные статьи базы.\n\nЛОГ:\n"+signal,
			hits)
		if err != nil {
			return res, err
		}
		verifyQuotes(&rep, hits)
		res.Source = SourceKB
		res.Reply = rep
		res.Diagnosis = rep.Answer
		if len(rep.Sources) > 0 {
			res.Article = rep.Sources[0].Source
		}
		return res, nil
	}

	// 3. Паттерн незнакомый → разбор моделью, честно помеченный.
	sys := "Ты — инженер на дежурстве. Тебе дают лог ошибки, которого НЕТ в базе знакомых " +
		"проблем. Разбери его по существу: что произошло (диагноз) и как чинить (конкретные шаги). " +
		"Будь честен: если данных мало, скажи, что нужно посмотреть дополнительно. Кратко, по делу. " +
		"В конце добавь строкой: 'СТОИТ ДОБАВИТЬ В БАЗУ: да/нет' — стоит ли занести этот случай в базу."
	out, _, err := a.gen.Complete(ctx, sys, []Msg{{Role: roleUser, Text: "ЛОГ:\n" + signal}}, CompleteOpts{MaxTokens: 800})
	if err != nil {
		return res, err
	}
	res.Source = SourceModel
	res.Diagnosis = strings.TrimSpace(out)
	return res, nil
}

// printTriage печатает разбор.
func printTriage(res TriageResult, knowThreshold float64) {
	fmt.Println("СИГНАЛ (строки, по которым шёл разбор):")
	for _, ln := range strings.Split(res.Signal, "\n") {
		fmt.Printf("   | %s\n", oneLine(ln, 110))
	}
	fmt.Println()

	switch res.Source {
	case SourceKB:
		fmt.Printf("РАСПОЗНАНО ПО БАЗЕ (score=%.2f ≥ %.1f) · статья: %s\n\n", res.Score, knowThreshold, res.Article)
		fmt.Printf("РАЗБОР:\n%s\n", res.Diagnosis)
		if len(res.Reply.Sources) > 0 {
			fmt.Println("\nИСТОЧНИКИ БАЗЫ:")
			for _, s := range res.Reply.Sources {
				fmt.Printf("   • %s · %s\n", s.Source, s.Section)
			}
		}
		if len(res.Reply.Quotes) > 0 {
			ex, pa, no := matchCounts(res.Reply.Quotes)
			fmt.Printf("ЦИТАТЫ (%d: ✓%d ~%d ✗%d)\n", len(res.Reply.Quotes), ex, pa, no)
		}
	case SourceModel:
		fmt.Printf("НЕТ В БАЗЕ (лучший score=%.2f < %.1f) — разбираю моделью:\n\n", res.Score, knowThreshold)
		fmt.Printf("%s\n", res.Diagnosis)
	default:
		fmt.Println("Недостаточно данных для разбора.")
	}
}

// ── демонстрация для видео: -triage35 ────────────────────────────────────────

func runTriage35(ctx context.Context, a *Agent, r *Retriever, cfg RerankConfig,
	knowThreshold float64, scope string) error {

	ragBanner(0, "АССИСТЕНТ РАЗБОРА ЛОГОВ ОШИБОК · день 35 (реальная задача)")
	fmt.Println("Задача: когда сервис падает, быстро понять по логу — что за ошибка и как чинить.")
	fmt.Println("КАК УЧАСТВУЕТ AI: (1) RAG-поиск по базе знакомых проблем — узнаёт паттерн;")
	fmt.Println("                  (2) если паттерн незнаком — модель разбирает лог по существу.")

	ragBanner(1, "БАЗА ЗНАКОМЫХ ПРОБЛЕМ (RAG)")
	fmt.Printf("Индекс базы: %s\n", r.Info())
	fmt.Printf("Порог «нет в базе»: %.1f (%s)\n", knowThreshold, scaleName(cfg))

	logs := []struct {
		name   string
		expect string
	}{
		{"pool.log", "исчерпание пула соединений (база жива, соединения не возвращаются)"},
		{"oom.log", "OOM — процесс убит (ExitCode 137), загрузка всего в память"},
		{"tls.log", "протухший TLS-сертификат платёжного провайдера"},
		{"unknown.log", "НЕзнакомый паттерн (ошибка парсинга конфига) — разбор моделью"},
	}

	var kb, model, idk int
	for i, lg := range logs {
		ragBanner(i+2, fmt.Sprintf("ЛОГ %d/%d: %s", i+1, len(logs), lg.name))
		fmt.Printf("ОЖИДАЕМ: %s\n\n", lg.expect)
		path := scope + "/triage/logs/" + lg.name
		body := readFileOrEmpty(path)
		if body == "" {
			fmt.Printf("  (лог %s не найден)\n", path)
			continue
		}
		res, err := triageLog(ctx, a, r, body, cfg, knowThreshold)
		if err != nil {
			return fmt.Errorf("%s: %w", lg.name, err)
		}
		printTriage(res, knowThreshold)
		switch res.Source {
		case SourceKB:
			kb++
		case SourceModel:
			model++
		default:
			idk++
		}
	}

	ragBanner(len(logs)+2, "ИТОГ")
	fmt.Printf("Разобрано логов: %d\n", len(logs))
	fmt.Printf("  распознано по базе (RAG):        %d\n", kb)
	fmt.Printf("  разобрано моделью (незнакомые):  %d\n", model)
	fmt.Printf("AI участвует: поиск знакомого (RAG) + разбор незнакомого (модель) — оба видны выше.\n")
	return nil
}

// ── одиночный запуск: лог из файла или stdin ─────────────────────────────────

func runTriage(ctx context.Context, a *Agent, r *Retriever, logPath string,
	cfg RerankConfig, knowThreshold float64) error {

	var body string
	if logPath != "" {
		body = readFileOrEmpty(logPath)
		if body == "" {
			return fmt.Errorf("не прочитал лог: %s", logPath)
		}
	} else {
		b, _ := os.ReadFile("/dev/stdin")
		body = string(b)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("пустой лог (укажи -log <файл> или подай через stdin)")
	}
	res, err := triageLog(ctx, a, r, body, cfg, knowThreshold)
	if err != nil {
		return err
	}
	printTriage(res, knowThreshold)
	return nil
}

// compile-time: показать, что тип TriageResult со всеми источниками используется.
var _ = []TriageSource{SourceKB, SourceModel, SourceUnknown}
