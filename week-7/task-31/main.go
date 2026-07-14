package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/joho/godotenv"
)

// swarmStageOf парсит значение флага -swarm-stage в TaskState.
func swarmStageOf(v string) TaskState {
	if v == "planning" {
		return StatePlanning
	}
	return StateValidation
}

func sourceDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(file)
	}
	return "."
}

func main() {
	reportMode := flag.Bool("report", false, "демонстрация (память + профили) и выход")
	memDir := flag.String("mem", filepath.Join(sourceDir(), "memory"), "каталог памяти (short-term/working)")
	profilesDir := flag.String("profiles", filepath.Join(sourceDir(), "profiles"), "каталог профилей (<id>.md)")
	active := flag.String("profile", "senior-go", "активный профиль")
	confirm := flag.Bool("confirm", false, "спрашивать подтверждение перед переходом этапа (пауза)")
	llmCheck := flag.Bool("llm-check", false, "включить семантическую проверку инвариантов через LLM")
	swarmOn := flag.Bool("swarm", false, "включить рой ревьюеров")
	swarmStage := flag.String("swarm-stage", "validation", "этап роя: planning | validation")
	maxTokens := flag.Int64("max-tokens", 4096, "лимит токенов ответа модели")
	watchSec := flag.Int("watch", 0, "день 18: режим 24/7 — печатать сводку каждые N сек (0 = обычный REPL)")
	track := flag.String("track", "Berlin,Sofia", "watch: города через запятую для фонового сбора")
	interval := flag.Int("interval", 30, "watch: период фонового сбора на сервере, сек")
	lazy := flag.Bool("lazy", true, "день 20: ленивая загрузка схем тулов (каталог серверов → load_tools). false = слать все схемы сразу")
	useRAG := flag.Bool("rag", false, "день 22: отвечать с RAG (поиск релевантных чанков в индексе базы знаний)")
	indexPath := flag.String("index", filepath.Join(sourceDir(), "ragindex", "index-structural.json"), "день 22: путь к индексу (собран в ragindex, день 21)")
	ragK := flag.Int("rag-k", 4, "день 22: сколько чанков подмешивать в контекст")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "день 22: базовый URL Ollama для эмбеддинга запроса")
	embedModel := flag.String("embed-model", "nomic-embed-text", "день 22: модель эмбеддингов (должна совпадать с моделью индекса)")
	evalMode := flag.Bool("eval", false, "день 22: прогнать 10 контрольных вопросов (с/без RAG) самодокументируемым отчётом и выйти")
	// День 23: улучшенный RAG (query rewrite + порог + MMR + LLM-реранк) и сравнение 2×2.
	eval23Mode := flag.Bool("eval23", false, "день 23: сравнение 2×2 {код,русский}×{baseline,improved} и выход")
	indexRU := flag.String("index-ru", filepath.Join(sourceDir(), "ragindex-ru", "index-structural.json"), "день 23: индекс русского корпуса (конспекты)")
	corpus := flag.String("corpus", "code", "день 23: корпус для интерактива — code | ru")
	rerankOn := flag.Bool("rerank", true, "день 23: включить второй проход (rewrite+порог+MMR+LLM-реранк) в интерактиве -rag")
	rewriteOn := flag.Bool("rewrite", true, "день 23: query rewrite перед поиском")
	topN := flag.Int("topn", 10, "день 23: сколько кандидатов достаёт bi-encoder (вход второго прохода)")
	threshold := flag.Float64("threshold", 0.0, "день 23: порог отсечения по косинусу (0 = выкл)")
	mmrLambda := flag.Float64("mmr", 0.7, "день 23: MMR λ (1 = выкл, ~0.7 диверсификация)")
	rerankModel := flag.String("rerank-model", "claude-haiku-4-5-20251001", "день 23: модель для rewrite/реранка (Haiku). Дефолт дня 23 (claude-3-5-haiku-latest) ретайрнут → 404")
	// День 24: цитаты/источники/анти-галлюцинации.
	eval24Mode := flag.Bool("eval24", false, "день 24: grounded-ответы (источники+цитаты) по русскому корпусу + режим «не знаю» и выход")
	knowThreshold := flag.Float64("know-threshold", 5.0, "день 24: порог «не знаю» по rerank-score (0–10); лучший кандидат ниже → ассистент говорит «не знаю»")
	// День 25: в живом чате порог мягче, чем в grounded-режиме дня 24 — реранк недооценивает
	// часть релевантных чанков, и строгий порог 5.0 давал ложные отказы посреди диалога.
	chatKnowThreshold := flag.Float64("chat-know-threshold", 3.0, "день 25: порог «не знаю» для чата (мягче дня 24: 3.0)")
	// День 25: мини-чат с RAG + памятью задачи.
	chat25Mode := flag.Bool("chat25", false, "день 25: авто-прогон 2 сценариев мини-чата (RAG + память задачи) и выход")
	chatMode := flag.Bool("chat", false, "день 25: интерактивный мини-чат по русскому корпусу (RAG + память задачи)")
	// День 26: локальная генерирующая LLM через Ollama (без облака).
	local26Mode := flag.Bool("local26", false, "день 26: демо локальной LLM — 3 запроса разной сложности (без облака) и выход")
	localMode := flag.Bool("local", false, "день 26: интерактивный чат с локальной LLM (без облака)")
	localModel := flag.String("local-model", "qwen2.5:7b", "день 26: генерирующая локальная модель в Ollama")
	localRAG := flag.Bool("local-rag", false, "день 26: в -local заземлять ответы на русский корпус (локальный RAG)")
	// День 27: тумблер бэкенда генерации и демо агентских флоу на локали.
	backend := flag.String("backend", "cloud", "день 27: бэкенд текстовых флоу — cloud | local")
	agent27Mode := flag.Bool("agent27", false, "день 27: демо агентских флоу (мини-чат + рой ревьюеров) на локальном бэкенде и выход")
	// День 28: локальный RAG + сравнение local↔cloud + оценка.
	compare28Mode := flag.Bool("compare28", false, "день 28: локальный RAG + сравнение local↔cloud + оценка (качество/скорость/стабильность) и выход")
	stabRuns := flag.Int("stab-runs", 3, "день 28: сколько локальных прогонов на вопрос для оценки стабильности")
	// День 29: оптимизация локальной модели под задачу (до/после).
	optimize29Mode := flag.Bool("optimize29", false, "день 29: сравнить RU-RAG ДО/ПОСЛЕ оптимизации (параметры + промпт + квант) и выход")
	tunedModel := flag.String("tuned-model", "", "день 29: модель для пресета ПОСЛЕ (пусто = как -local-model; другой тег = A/B по кванту)")
	// День 30: локальная LLM как приватный HTTP-сервис (OpenAI-совместимый).
	serve30Mode := flag.Bool("serve30", false, "день 30: поднять приватный HTTP-сервис (OpenAI /v1/chat/completions) на локальной LLM")
	serveAddr := flag.String("addr", ":8080", "день 30: адрес прослушивания (:8080 = все интерфейсы → доступ по сети)")
	serveKey := flag.String("api-key", os.Getenv("SERVICE_API_KEY"), "день 30: Bearer-ключ доступа (пусто = открытый сервис)")
	serveRPM := flag.Int("rate-rpm", 20, "день 30: лимит запросов в минуту на IP")
	serveMaxCtx := flag.Int("max-ctx", 4096, "день 30: потолок контекстного окна (num_ctx)")
	serveConc := flag.Int("max-concurrent", 2, "день 30: одновременных генераций (стабильность)")
	serve30Demo := flag.Bool("serve30-demo", false, "день 30: одна команда для видео — поднять сервис, прогнать все проверки и оставить работать")

	// День 31: ассистент разработчика (RAG по документации проекта + git-контекст через MCP).
	help31Mode := flag.Bool("help31", false, "день 31: одна команда для видео — /help по документации проекта + живой git-контекст")
	indexDocs := flag.String("index-docs", filepath.Join(sourceDir(), "ragindex-docs", "index-structural.json"), "день 31: индекс документации ПРОЕКТА (readme + docs/ + код агента)")
	helpKnow := flag.Float64("help-know-threshold", 3.0, "день 31: порог «документация не помогла» для /help (ниже — отвечаем по живому git-контексту)")
	gitScope := flag.String("git-scope", "", "день 31: каталог проекта для git_files (пусто = автоопределение week-7/task-31 внутри репозитория)")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		// День 26: .env может отсутствовать — полностью локальный прогон (-local26/-local)
		// не требует ANTHROPIC_API_KEY. Не фаталим, только предупреждаем.
		log.Printf("\u2139 .env не загружен (%s) — облачные режимы потребуют ANTHROPIC_API_KEY", err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// День 26: локальные режимы обрабатываем ПЕРВЫМИ — им не нужен ни anthropic-клиент,
	// ни ключ, ни MCP. Всё считается на локальной модели через Ollama.
	if *local26Mode || *localMode {
		llm := NewLocalLLM(*ollamaURL, *localModel)
		if *local26Mode {
			if err := runLocal26(ctx, llm, *ollamaURL, *embedModel, *indexRU); err != nil {
				log.Fatalf("local26: %s", err.Error())
			}
			return
		}
		if err := runLocalChat(ctx, llm, *ollamaURL, *embedModel, *indexRU, *localRAG); err != nil {
			log.Fatalf("local: %s", err.Error())
		}
		return
	}

	client := anthropic.NewClient()
	model := anthropic.ModelClaudeOpus4_8

	if *reportMode {
		if err := runReport(ctx, client, *profilesDir, *memDir, *swarmOn, *swarmStage, *maxTokens); err != nil {
			log.Fatalf("report failed: %s", err.Error())
		}
		return
	}

	mem, _ := LoadMemory(*memDir)
	store := NewProfileStore(*profilesDir)
	agent := NewAgent(client, model, mem)
	if p, err := store.Load(*active); err == nil {
		agent.SetProfile(p)
	} else {
		log.Printf("профиль %q не найден, работаю без профиля", *active)
	}
	inv := NewInvariantSet(*memDir)
	agent.SetInvariants(inv)
	agent.SetLLMCheck(*llmCheck)
	agent.SetMaxTokens(*maxTokens)

	// День 22: RAG-ретривер по индексу дня 21 (нужен для -rag и -eval).
	var retriever *Retriever
	if *useRAG || *evalMode {
		r, err := NewRetriever(*indexPath, *ollamaURL, *embedModel, *ragK)
		if err != nil {
			log.Fatalf("RAG: %s", err.Error())
		}
		retriever = r
		fmt.Printf("RAG: индекс подключён — %s\n", retriever.Info())
	}

	// День 23: конфиг второго прохода retrieval из флагов.
	rcfg := RerankConfig{
		Corpus: *corpus, TopN: *topN, Threshold: *threshold, MMRLambda: *mmrLambda,
		TopK: *ragK, Rewrite: *rewriteOn, Rerank: *rerankOn,
		Model: anthropic.Model(*rerankModel),
	}

	// День 27: тумблер облако↔локаль. По умолчанию текстовые флоу идут в облако;
	// -backend local переключает их на локальную модель (шов Completer, completer.go).
	// На локали облачный RAG-реранк выключаем — retrieval идёт по bi-encoder+порогу,
	// и флоу становится полностью локальным (реранк на локали — задача дня 28).
	var gen Completer = newCloudCompleter(client, model)
	if *backend == "local" {
		gen = newLocalCompleter(NewLocalLLM(*ollamaURL, *localModel))
		rcfg.Rerank = false
		fmt.Printf("Бэкенд генерации: %s (локально, без облака)\n", gen.Name())
	}
	agent.SetCompleter(gen)

	// День 27, фикс: порог «не знаю» зависит от ШКАЛЫ score. С реранком score — это
	// оценка судьи 0–10 (день 25, порог 3.0). На локали реранк выключен, и hits[0].Score —
	// это КОСИНУС bi-encoder (~0..1). Порог 3.0 по косинусу недостижим → всегда «не знаю».
	// Поэтому для local переводим порог в косинусную шкалу (0.35, как в запросе 3 дня 26).
	chatKnow := *chatKnowThreshold
	if *backend == "local" && !rcfg.Rerank {
		chatKnow = 0.35
		fmt.Printf("Порог «не знаю»: %.2f (косинусная шкала — реранк на локали выключен)\n", chatKnow)
	}

	// День 23: матрица 2×2 {код,русский}×{baseline,improved} и выход (до MCP).
	if *eval23Mode {
		specs := []corpusSpec{
			{name: "code", index: *indexPath, qs: codeQuestions23()},
			{name: "ru", index: *indexRU, qs: ruQuestions23()},
		}
		if err := runEval23(ctx, agent, FullPolicy(), *ollamaURL, *embedModel, rcfg, specs); err != nil {
			log.Fatalf("eval23: %s", err.Error())
		}
		return
	}

	// День 24: grounded-ответы с источниками и цитатами + режим «не знаю» (русский корпус).
	if *eval24Mode {
		if err := runEval24(ctx, agent, *ollamaURL, *embedModel, *indexRU, rcfg, *knowThreshold); err != nil {
			log.Fatalf("eval24: %s", err.Error())
		}
		return
	}

	// День 25: мини-чат с RAG + памятью задачи. Для русского корпуса rewrite не нужен
	// (язык совпадает) — отключаем, чтобы экономить вызовы; реранк оставляем.
	chatCfg := rcfg
	chatCfg.Corpus = "ru"
	chatCfg.Rewrite = false
	if *chat25Mode {
		if err := runEval25(ctx, agent, *ollamaURL, *embedModel, *indexRU, chatCfg, chatKnow); err != nil {
			log.Fatalf("chat25: %s", err.Error())
		}
		return
	}
	if *chatMode {
		if err := runInteractiveChat(ctx, agent, *ollamaURL, *embedModel, *indexRU, chatCfg, chatKnow); err != nil {
			log.Fatalf("chat: %s", err.Error())
		}
		return
	}

	// День 30: приватный HTTP-сервис на локальной LLM (без облака). Поднимаем и блокируемся
	// до Ctrl+C. Ключ Anthropic не нужен — это и делает сервис приватным.
	if *serve30Mode || *serve30Demo {
		llm := NewLocalLLM(*ollamaURL, *localModel)
		cfg := serveConfig{
			Addr: *serveAddr, APIKey: *serveKey, RateRPM: *serveRPM,
			MaxCtx: *serveMaxCtx, MaxConcurrent: *serveConc,
		}
		run := runServe30
		if *serve30Demo {
			run = runServe30Demo
		}
		if err := run(ctx, llm, cfg); err != nil {
			log.Fatalf("serve30: %s", err.Error())
		}
		return
	}

	// День 29: оптимизация под RU-RAG (до/после). Судья качества — облако (если есть ключ);
	// генерация обоих пресетов — локальная. -tuned-model позволяет A/B по кванту.
	if *optimize29Mode {
		judge := newCloudCompleter(client, model)
		hasCloud := os.Getenv("ANTHROPIC_API_KEY") != ""
		tm := *tunedModel
		if tm == "" {
			tm = *localModel
		}
		if err := runOptimize29(ctx, judge, hasCloud, *ollamaURL, *embedModel, *indexRU, *localModel, tm, *ragK); err != nil {
			log.Fatalf("optimize29: %s", err.Error())
		}
		return
	}

	// День 28: локальный RAG + сравнение с облаком + оценка. Строим оба бэкенда явно
	// (compare гоняет и локаль, и облако); облако опционально — при отсутствии ключа
	// сравнение идёт только по локали, а RAG остаётся полностью локальным.
	if *compare28Mode {
		cloudGen := newCloudCompleter(client, model)
		localGen := newLocalCompleter(NewLocalLLM(*ollamaURL, *localModel))
		hasCloud := os.Getenv("ANTHROPIC_API_KEY") != ""
		if err := runCompare28(ctx, cloudGen, localGen, hasCloud, *ollamaURL, *embedModel, *indexRU, *ragK, *stabRuns); err != nil {
			log.Fatalf("compare28: %s", err.Error())
		}
		return
	}

	// День 27: демо агентских флоу на локальном бэкенде (мини-чат + рой ревьюеров).
	// Не требует MCP и облака (при -backend local). chatCfg уже: ru, rewrite off,
	// а rcfg.Rerank выключен выше для local — значит retrieval полностью локальный.
	if *agent27Mode {
		if *backend != "local" {
			log.Println("совет: -agent27 задуман для -backend local, иначе флоу пойдут в облако")
		}
		if err := runAgent27(ctx, agent, gen, *ollamaURL, *embedModel, *indexRU, chatCfg, chatKnow); err != nil {
			log.Fatalf("agent27: %s", err.Error())
		}
		return
	}

	// День 22: самодокументируемый отчёт-демонстрация (10 контрольных вопросов,
	// с/без RAG) — одна команда для видео. Не требует MCP, выходим до подключения.
	if *evalMode {
		if err := runEval(ctx, agent, retriever, FullPolicy()); err != nil {
			log.Fatalf("eval: %s", err.Error())
		}
		return
	}

	// День 17: инструменты доступны агенту всегда, вызовы проходят через инварианты.
	// День 20: оркестрация НЕСКОЛЬКИХ MCP-серверов разной природы.
	// День 31: ТРЕТИЙ сервер — git-контекст проекта. Только чтение (см. gitserver/main.go).
	servers := []ServerSpec{
		{Name: "weather", Purpose: "погода, геокодинг, фоновое слежение за городами",
			Cmd: []string{"go", "run", filepath.Join(sourceDir(), "weatherserver")}},
		{Name: "notes", Purpose: "файловое хранилище: сохранить/прочитать/список/удалить",
			Cmd: []string{"go", "run", filepath.Join(sourceDir(), "notesserver")}},
		{Name: "git", Purpose: "контекст проекта: текущая ветка, список файлов, diff, история коммитов (только чтение)",
			Cmd: []string{"go", "run", filepath.Join(sourceDir(), "gitserver")}},
	}
	mcptools, mcpStop, err := ConnectMCP(ctx, servers)
	if err != nil {
		log.Printf("MCP недоступен (%v) — работаю без инструментов", err)
	} else {
		defer mcpStop()
		agent.SetMCP(mcptools)
		agent.SetLazyTools(*lazy)
		mode := "жадный (все схемы сразу)"
		if *lazy {
			mode = "ленивый (каталог серверов → load_tools)"
		}
		// Было захардкожено «2» — с третьим сервером это стало враньём в выводе.
		fmt.Printf("MCP: %d тул(ов) с %d серверов · режим схем: %s\n  %s\n",
			mcptools.Len(), len(servers), mode, mcptools.Breakdown())
	}

	// День 31: ассистент разработчика. Требует MCP (git-контекст) — поэтому диспетч
	// стоит ПОСЛЕ ConnectMCP, в отличие от -eval/-eval24, которые выходят раньше.
	scope := *gitScope
	if scope == "" {
		scope = defaultGitScope()
	}
	if *help31Mode {
		docsRetriever, err := NewRetriever(*indexDocs, *ollamaURL, *embedModel, *ragK)
		if err != nil {
			log.Fatalf("help31: %s\n\nсобери индекс документации:\n  go run ./%s/ragindex -out %s/ragindex-docs -ext .md,.go",
				err.Error(), scope, scope)
		}
		helpCfg := rcfg
		helpCfg.Corpus = "code" // корпус смешанный (RU-доки + Go-код) → rewrite в код-термины полезен
		if err := runHelp31(ctx, agent, docsRetriever, mcptools, helpCfg, *helpKnow, scope, FullPolicy()); err != nil {
			log.Fatalf("help31: %s", err.Error())
		}
		return
	}

	// /help в REPL работает по тому же индексу — готовим ретривер лениво, при первом вызове.
	var helpRetriever *Retriever
	helpCfg := rcfg
	helpCfg.Corpus = "code"

	// День 18: режим 24/7. Агент ставит города на фоновый сбор и периодически
	// печатает агрегированную сводку. Токены не тратятся (прямые вызовы тулов).
	if *watchSec > 0 {
		if mcptools == nil {
			log.Fatal("режим -watch требует доступного MCP-сервера")
		}
		runWatch(ctx, mcptools, *watchSec, strings.Split(*track, ","), *interval)
		_ = mem.Save()
		return
	}

	fmt.Println("Накопительный агент: память (3 слоя) + персонализация.")
	fmt.Printf("Активный профиль: %q · профили: %v\n", agent.ProfileID(), store.List())
	fmt.Printf("Память: %s\n", *memDir)
	fmt.Println("Команды:")
	fmt.Println("  профиль (long-term): /profile <id> · /profiles · /whoami")
	fmt.Println("  working:  /task <имя> · /goal <..> · /plan a; b · /current <шаг> · /done <шаг> · /decide k=v")
	fmt.Println("  состояние:/state · /run (прогнать этапы) · /advance (шаг вперёд) · pause")
	fmt.Println("  инварианты:/invariants · /forbid <стоп-слово> · /invariant <правило>")
	fmt.Println("  переходы: /approve-plan · /validate-ok · /goto <state> (контроль gate)")
	fmt.Println("  прочее:   /mem · /reset-task · exit")
	fmt.Println("  RAG (день 22): /rag on · /rag off (поиск по базе знаний перед ответом)")
	if *confirm {
		fmt.Println("  режим -confirm: перед каждым переходом спрошу y/n/pause")
	}
	if mem.State().valid() && mem.TaskName() != "" {
		fmt.Printf("Возобновление: задача %q на этапе %s.\n", mem.TaskName(), mem.State())
	}

	reader := bufio.NewReader(os.Stdin)
	ragOn := *useRAG // день 22: текущий режим RAG (переключается /rag on|off)
	pipe := NewPipeline(agent, *confirm, reader)
	if *swarmOn {
		pipe.EnableSwarm(NewSwarm(gen, swarmStageOf(*swarmStage)))
	}
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "exit":
			_ = mem.Save()
			fmt.Println("Сохранил память. Exiting")
			return
		case line == "/mem":
			fmt.Print(mem.Inspect())
			continue
		case line == "/reset-task":
			mem.ResetTask()
			fmt.Println("рабочая память очищена")
			continue
		case line == "/help" || strings.HasPrefix(line, "/help "):
			// День 31. /help = документация проекта (RAG) + живой git-контекст (MCP).
			q := strings.TrimSpace(strings.TrimPrefix(line, "/help"))
			if q == "" {
				fmt.Println("использование: /help <вопрос о проекте>")
				fmt.Println("  например: /help какие MCP-серверы подключены и что умеет git-сервер?")
				continue
			}
			if helpRetriever == nil {
				r, err := NewRetriever(*indexDocs, *ollamaURL, *embedModel, *ragK)
				if err != nil {
					fmt.Printf("индекс документации не подключён: %s\n", err.Error())
					fmt.Printf("собери его: go run ./%s/ragindex -out %s/ragindex-docs -ext .md,.go\n", scope, scope)
					continue
				}
				helpRetriever = r
				fmt.Printf("документация проекта: %s\n", helpRetriever.Info())
			}
			if err := runHelp(ctx, agent, helpRetriever, mcptools, q, helpCfg, *helpKnow, scope); err != nil {
				fmt.Printf("/help: %s\n", err.Error())
			}
			continue
		case strings.HasPrefix(line, "/track "):
			if mcptools == nil {
				fmt.Println("MCP недоступен")
				continue
			}
			parts := strings.Fields(strings.TrimPrefix(line, "/track "))
			if len(parts) == 0 {
				fmt.Println("использование: /track <город> [интервал_сек]")
				continue
			}
			iv := 30
			if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil && len(parts) > 1 {
				iv = n
				parts = parts[:len(parts)-1]
			}
			out, _ := mcptools.Call(ctx, "track_location", jsonArgs(map[string]any{
				"location": strings.Join(parts, " "), "interval_sec": iv,
			}))
			fmt.Println(out)
			continue
		case line == "/summary" || strings.HasPrefix(line, "/summary "):
			if mcptools == nil {
				fmt.Println("MCP недоступен")
				continue
			}
			loc := strings.TrimSpace(strings.TrimPrefix(line, "/summary"))
			out, _ := mcptools.Call(ctx, "weather_summary", jsonArgs(map[string]any{"location": loc}))
			fmt.Println(out)
			continue
		case strings.HasPrefix(line, "/untrack "):
			if mcptools == nil {
				fmt.Println("MCP недоступен")
				continue
			}
			city := strings.TrimSpace(strings.TrimPrefix(line, "/untrack "))
			out, _ := mcptools.Call(ctx, "untrack_location", jsonArgs(map[string]any{"location": city}))
			fmt.Println(out)
			continue
		case line == "/untrack-all":
			if mcptools == nil {
				fmt.Println("MCP недоступен")
				continue
			}
			listed, _ := mcptools.Call(ctx, "list_tracked", json.RawMessage(`{}`))
			for _, row := range strings.Split(listed, "\n") {
				name := strings.TrimSpace(strings.SplitN(row, ",", 2)[0])
				if name == "" || name == "ничего не отслеживается" {
					continue
				}
				mcptools.Call(ctx, "untrack_location", jsonArgs(map[string]any{"location": name}))
			}
			fmt.Println("всё снято с отслеживания")
			continue
		case strings.HasPrefix(line, "/watch "):
			if mcptools == nil {
				fmt.Println("MCP недоступен")
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "/watch ")))
			if err != nil || n <= 0 {
				fmt.Println("использование: /watch <сек> (Ctrl-C для выхода из режима)")
				continue
			}
			runWatch(ctx, mcptools, n, nil, 30)
			continue
		case line == "/profiles":
			fmt.Printf("доступно: %v\n", store.List())
			continue
		case line == "/whoami":
			fmt.Printf("активный профиль: %q\n%s\n", agent.ProfileID(), mem.LongDoc())
			continue
		case strings.HasPrefix(line, "/profile "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/profile "))
			p, err := store.Load(id)
			if err != nil {
				fmt.Printf("профиль %q не найден; доступно: %v\n", id, store.List())
				continue
			}
			agent.SetProfile(p)
			fmt.Printf("профиль переключён на %q\n", id)
			continue
		case line == "/invariants":
			for _, iv := range inv.All() {
				fmt.Printf("  [%s/%s] %s\n", iv.Kind, iv.ID, iv.Text)
			}
			continue
		case strings.HasPrefix(line, "/forbid "):
			word := strings.TrimSpace(strings.TrimPrefix(line, "/forbid "))
			iv := inv.Add("Запрещено пользователем: "+word, []string{word})
			fmt.Printf("добавлен инвариант [%s]: запрет %q\n", iv.ID, word)
			continue
		case strings.HasPrefix(line, "/invariant "):
			rule := strings.TrimSpace(strings.TrimPrefix(line, "/invariant "))
			iv := inv.Add(rule, nil)
			fmt.Printf("добавлен инвариант [%s] (семантический, проверяется LLM): %s\n", iv.ID, rule)
			continue
		case line == "/state":
			fmt.Printf("state=%s — %s | план утверждён=%v, валидация=%v\n",
				mem.State(), expectedAction[mem.State()], mem.PlanApproved(), mem.Validated())
			continue
		case line == "/approve-plan":
			mem.ApprovePlan()
			_ = mem.Save()
			fmt.Println("план утверждён — теперь разрешён переход planning → execution")
			continue
		case line == "/validate-ok":
			mem.MarkValidated()
			_ = mem.Save()
			fmt.Println("валидация отмечена пройденной — разрешён переход validation → done")
			continue
		case strings.HasPrefix(line, "/goto "):
			target := TaskState(strings.TrimSpace(strings.TrimPrefix(line, "/goto ")))
			from := mem.State()
			if err := mem.Goto(target); err != nil {
				fmt.Printf("ПЕРЕХОД ЗАПРЕЩЁН: %s. Остаюсь на %s.\n", err.Error(), from)
			} else {
				fmt.Printf("переход %s → %s выполнен\n", from, target)
				_ = mem.Save()
			}
			continue
		case line == "/advance":
			from := mem.State()
			to, err := mem.Advance()
			if err != nil {
				fmt.Println("переход невозможен:", err.Error())
			} else {
				fmt.Printf("переход %s → %s\n", from, to)
				_ = mem.Save()
			}
			continue
		case line == "/run":
			done, err := pipe.Run(ctx)
			if err != nil {
				log.Printf("pipeline: %s", err.Error())
			} else if done {
				fmt.Println("[pipeline] задача доведена до done.")
			}
			continue
		case line == "/rag on":
			if retriever == nil {
				r, err := NewRetriever(*indexPath, *ollamaURL, *embedModel, *ragK)
				if err != nil {
					fmt.Printf("RAG недоступен: %s\n", err.Error())
					continue
				}
				retriever = r
				fmt.Printf("RAG подключён — %s\n", retriever.Info())
			}
			ragOn = true
			fmt.Println("RAG включён: ответы строятся по базе знаний с указанием источников")
			continue
		case line == "/rag off":
			ragOn = false
			fmt.Println("RAG выключен: обычные ответы модели")
			continue
		case strings.HasPrefix(line, "/"):
			handleWorkingCmd(mem, line)
			continue
		}

		var reply Reply
		var aerr error
		if ragOn && retriever != nil {
			if rcfg.Rerank || rcfg.Rewrite {
				reply, _, _, aerr = ragReplyAdvanced(ctx, agent, retriever, line, FullPolicy(), rcfg)
			} else {
				reply, _, aerr = ragReply(ctx, agent, retriever, line, FullPolicy())
			}
		} else {
			reply, aerr = agent.Ask(ctx, line, FullPolicy())
		}
		if aerr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("error: %s", aerr.Error())
			continue
		}
		fmt.Println(reply.Text)
		if reply.RAGUsed && len(reply.Sources) > 0 {
			fmt.Printf("[RAG источники: %s]\n", strings.Join(reply.Sources, "; "))
		}
		fmt.Printf("[профиль %q · слои: %s]\n", reply.ProfileID, layersString(reply.UsedLayers))
		_ = mem.Save()
	}
}

func handleWorkingCmd(mem *Memory, line string) {
	cmd, arg := splitCmd(line)
	switch cmd {
	case "/task":
		mem.SetTask(arg, mem.taskGoal())
		fmt.Println("задача задана (working)")
	case "/goal":
		mem.SetTask(mem.taskName(), arg)
		fmt.Println("цель задана (working)")
	case "/plan":
		steps := splitSemicolons(arg)
		mem.SetPlan(steps...)
		fmt.Printf("план: %v\n", steps)
	case "/current":
		mem.SetCurrent(arg)
		fmt.Println("текущий шаг задан (working)")
	case "/done":
		mem.MarkDone(arg)
		fmt.Println("шаг выполнен (working)")
	case "/decide":
		k, v := splitKV(arg)
		mem.Decide(k, v)
		fmt.Println("решение сохранено (working)")
	default:
		fmt.Println("неизвестная команда")
	}
}

func splitCmd(line string) (string, string) {
	f := strings.SplitN(line, " ", 2)
	if len(f) == 1 {
		return f[0], ""
	}
	return f[0], strings.TrimSpace(f[1])
}
func splitKV(s string) (string, string) {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
}
func splitSemicolons(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
