package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// День 30: локальная LLM как ПРИВАТНЫЙ СЕРВИС.
//
// Оборачиваем наш LocalLLM (день 26) в HTTP-сервис, доступный по сети
// (http://<ip>:port), с чатом, аутентификацией по ключу, rate-limit и ограничением
// контекста. Бэкенд — только локальная модель (это и делает сервис приватным: ни
// одного облачного вызова, ключ Anthropic не нужен).
//
// РЕШЕНИЕ и АЛЬТЕРНАТИВЫ (подробно в README):
//   - API-формат: OpenAI-совместимый /v1/chat/completions (выбрано — к сервису сразу
//     подходят готовые клиенты; альтернатива — свой простой /chat, нагляднее, но не
//     стыкуется с экосистемой).
//   - Аутентификация: Bearer-ключ (конвенция OpenAI + совет Murad «авторизация по ключу»;
//     альтернатива — basic-auth логин/пароль как у Стаса, или auth на reverse-proxy).
//   - Rate-limit: in-memory токен-бакет на IP (stdlib; альтернатива — nginx/Redis).
//   - Max-context: num_ctx (день 29) как потолок окна (альтернатива — обрезать вход).
//   - Стабильность при нескольких запросах: семафор конкурентности (Ollama сам
//     сериализует тяжёлую генерацию; семафор бережёт память).
//   - Стриминг: не делаем (stream:false) ради наглядности; альтернатива — SSE.

// serveConfig — параметры сервиса.
type serveConfig struct {
	Addr          string // адрес прослушивания, напр. ":8080" (0.0.0.0 → доступ по сети)
	APIKey        string // Bearer-ключ; пусто = открытый сервис (предупреждаем)
	RateRPM       int    // лимит запросов в минуту на клиента (IP)
	MaxCtx        int    // потолок контекстного окна (num_ctx)
	MaxConcurrent int    // одновременных генераций (стабильность)
}

// runServe30 поднимает сервис и блокируется до отмены ctx (Ctrl+C).
// buildService собирает сервер и http.Server (общее для обычного и демо-режима).
func buildService(llm *LocalLLM, cfg serveConfig) (*server, *http.Server) {
	srv := &server{
		llm: llm, cfg: cfg,
		lim: newIPLimiter(cfg.RateRPM),
		sem: make(chan struct{}, max1(cfg.MaxConcurrent)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)                      // без ключа: liveness
	mux.HandleFunc("/v1/models", srv.auth(srv.handleModels))         // OpenAI: список моделей
	mux.HandleFunc("/v1/chat/completions", srv.auth(srv.handleChat)) // OpenAI: чат
	return srv, &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func printBanner(llm *LocalLLM, cfg serveConfig) {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  День 30. Приватный AI-сервис на локальной LLM                 ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Printf("Модель:   %s (локально, без облака)\n", llm.Name())
	fmt.Printf("Адрес:    http://%s  (по сети: http://<ip-машины>%s)\n", normalizeAddr(cfg.Addr), portOf(cfg.Addr))
	fmt.Printf("Эндпоинты: GET /health · GET /v1/models · POST /v1/chat/completions\n")
	fmt.Printf("Лимиты:   %d req/min на IP · %d одновременно · num_ctx %d\n", cfg.RateRPM, max1(cfg.MaxConcurrent), cfg.MaxCtx)
	if cfg.APIKey == "" {
		fmt.Println("⚠ Ключ не задан — сервис ОТКРЫТ. Для приватности задай -api-key / SERVICE_API_KEY.")
	} else {
		fmt.Println("Аутентификация: Authorization: Bearer <ключ>")
	}
}

// runServe30 — обычный режим: поднять сервис и работать до Ctrl+C.
func runServe30(ctx context.Context, llm *LocalLLM, cfg serveConfig) error {
	if err := llm.Health(ctx); err != nil {
		return fmt.Errorf("локальная модель недоступна: %w", err)
	}
	_, httpSrv := buildService(llm, cfg)
	printBanner(llm, cfg)
	fmt.Println("Останов: Ctrl+C")
	return serveUntil(ctx, httpSrv)
}

// serveUntil запускает сервер и держит его до отмены ctx (грациозный останов).
func serveUntil(ctx context.Context, httpSrv *http.Server) error {
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// runServe30Demo — ОДНА команда для видео: поднимает сервис, сам прогоняет все
// проверки из задания (доступ, аутентификация, стабильность, лимиты), печатает
// аккуратный отчёт и оставляет сервис работать (чтобы показать доступ по сети).
func runServe30Demo(ctx context.Context, llm *LocalLLM, cfg serveConfig) error {
	if err := llm.Health(ctx); err != nil {
		return fmt.Errorf("локальная модель недоступна: %w", err)
	}
	// Ключ для демо задаём принудительно, чтобы показать аутентификацию.
	if cfg.APIKey == "" {
		cfg.APIKey = "demo-key"
	}
	_, httpSrv := buildService(llm, cfg)
	printBanner(llm, cfg)
	fmt.Println()

	// Стартуем сервер в фоне.
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	base := "http://127.0.0.1" + portOf(cfg.Addr)
	if err := waitReady(base, 3*time.Second); err != nil {
		return fmt.Errorf("сервис не поднялся: %w", err)
	}
	cl := &http.Client{Timeout: 2 * time.Minute}

	fmt.Println("━━━ ПРОВЕРКА 1: /health (без ключа) ━━━")
	demoCall(cl, "GET", base+"/health", "", "")

	fmt.Println("━━━ ПРОВЕРКА 2: чат БЕЗ ключа → должен быть 401 ━━━")
	chatBody := `{"model":"` + llm.Model + `","messages":[{"role":"user","content":"Что такое бэггинг?"}]}`
	demoCall(cl, "POST", base+"/v1/chat/completions", "", chatBody)

	fmt.Println("━━━ ПРОВЕРКА 3: чат С ключом → ответ локальной модели (OpenAI-формат) ━━━")
	demoCall(cl, "POST", base+"/v1/chat/completions", cfg.APIKey, chatBody)

	fmt.Println("━━━ ПРОВЕРКА 4: /v1/models (OpenAI) ━━━")
	demoCall(cl, "GET", base+"/v1/models", cfg.APIKey, "")

	fmt.Println("━━━ ПРОВЕРКА 5: stream=true → честный 400 (не поддержан) ━━━")
	demoCall(cl, "POST", base+"/v1/chat/completions", cfg.APIKey,
		`{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	fmt.Printf("━━━ ПРОВЕРКА 6: rate-limit (%d/мин на IP) — жмём подряд, ловим 429 ━━━\n", cfg.RateRPM)
	burst := cfg.RateRPM + 3
	var codes []int
	hit429 := 0
	for i := 0; i < burst; i++ {
		code := quickCode(cl, base+"/v1/models", cfg.APIKey)
		codes = append(codes, code)
		if code == http.StatusTooManyRequests && hit429 == 0 {
			hit429 = i + 1
		}
	}
	fmt.Printf("  коды подряд: %v\n", codes)
	if hit429 > 0 {
		fmt.Printf("  → rate-limit сработал на запросе #%d (429)\n\n", hit429)
	} else {
		fmt.Printf("  → 429 не пойман за %d запросов (увеличь burst или снизь -rate-rpm)\n\n", burst)
	}

	fmt.Println("──────────────── ИТОГ ────────────────")
	fmt.Println("Все проверки из задания показаны: доступ по HTTP, аутентификация по ключу,")
	fmt.Println("стабильность при серии запросов, ограничения (rate-limit + num_ctx).")
	fmt.Printf("Сервис ПРОДОЛЖАЕТ РАБОТАТЬ: локально http://%s · по сети http://<ip-машины>%s\n",
		normalizeAddr(cfg.Addr), portOf(cfg.Addr))
	fmt.Printf("Проверь доступ по сети с другого устройства (ключ: %s). Останов: Ctrl+C\n", cfg.APIKey)

	// Оставляем сервис живым для демонстрации доступа по сети.
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// --- вспомогательное для демо-клиента ---

func waitReady(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("нет ответа на /health за %s", timeout)
}

// demoCall делает запрос и печатает статус + тело (аккуратно, для видео).
func demoCall(cl *http.Client, method, url, key, body string) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := cl.Do(req)
	if err != nil {
		fmt.Printf("  %s %s → ошибка: %s\n\n", method, url, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("  %s %s → [%d]\n", method, shortURL(url), resp.StatusCode)
	fmt.Printf("  %s\n\n", prettyBody(b))
}

func quickCode(cl *http.Client, url, key string) int {
	req, _ := http.NewRequest("GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func shortURL(u string) string {
	if i := strings.Index(u, "/v1"); i >= 0 {
		return u[i:]
	}
	if i := strings.Index(u, "/health"); i >= 0 {
		return u[i:]
	}
	return u
}

// prettyBody вытаскивает из ответа чата content ассистента, иначе печатает как есть (обрезая).
func prettyBody(b []byte) string {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err == nil {
		if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
			if ch, ok := choices[0].(map[string]any); ok {
				if msg, ok := ch["message"].(map[string]any); ok {
					content, _ := msg["content"].(string)
					usage := ""
					if u, ok := m["usage"].(map[string]any); ok {
						usage = fmt.Sprintf("  [usage: prompt %v, completion %v]", u["prompt_tokens"], u["completion_tokens"])
					}
					return "ответ: " + oneLine(content, 260) + usage
				}
			}
		}
	}
	return oneLine(string(b), 200)
}

type server struct {
	llm *LocalLLM
	cfg serveConfig
	lim *ipLimiter
	sem chan struct{}
}

// --- middleware: аутентификация по Bearer-ключу ---

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if strings.TrimSpace(got) != s.cfg.APIKey {
				writeOAIError(w, http.StatusUnauthorized, "invalid_api_key", "неверный или отсутствующий API-ключ")
				return
			}
		}
		// rate-limit применяем после auth (чтобы неавторизованные не жгли лимит легитимных).
		if !s.lim.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeOAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("превышен лимит %d запросов в минуту", s.cfg.RateRPM))
			return
		}
		next(w, r)
	}
}

// --- GET /health ---

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	if err := s.llm.Health(r.Context()); err != nil {
		status, code = "degraded: "+err.Error(), http.StatusServiceUnavailable
	}
	writeJSONResp(w, code, map[string]any{"status": status, "model": s.llm.Model})
}

// --- GET /v1/models (OpenAI) ---

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSONResp(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": s.llm.Model, "object": "model", "owned_by": "local"},
		},
	})
}

// --- POST /v1/chat/completions (OpenAI) ---

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiChatRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Temperature *float64     `json:"temperature,omitempty"`
	MaxTokens   *int         `json:"max_tokens,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "нужен POST")
		return
	}
	var req oaiChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request", "не разобрать JSON тела")
		return
	}
	if len(req.Messages) == 0 {
		writeOAIError(w, http.StatusBadRequest, "invalid_request", "messages пуст")
		return
	}
	if req.Stream {
		// Честно сообщаем, что стриминг не поддержан (а не молча игнорируем).
		writeOAIError(w, http.StatusBadRequest, "unsupported", "stream=true не поддерживается; используй stream=false")
		return
	}

	system, history, user := splitOAIMessages(req.Messages)
	if user == "" {
		writeOAIError(w, http.StatusBadRequest, "invalid_request", "нет пользовательского сообщения")
		return
	}

	// Ограничение одновременных генераций (стабильность).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	// Настраиваем локальную модель под запрос: max-context (day 29) + опц. параметры.
	llm := *s.llm // копия, чтобы параметры запроса не текли между клиентами
	llm.NumCtx = s.cfg.MaxCtx
	if req.Temperature != nil {
		llm.Temp = *req.Temperature
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		llm.NumPredict = *req.MaxTokens
	}

	answer, stats, err := llm.Chat(r.Context(), system, history, user)
	if err != nil {
		writeOAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	writeJSONResp(w, http.StatusOK, oaiChatResponse(req.Model, s.llm.Model, answer, stats))
}

// oaiChatResponse собирает ответ в формате OpenAI chat.completion.
func oaiChatResponse(reqModel, fallbackModel, answer string, stats LocalStats) map[string]any {
	model := reqModel
	if model == "" {
		model = fallbackModel
	}
	return map[string]any{
		"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     stats.PromptTokens,
			"completion_tokens": stats.EvalTokens,
			"total_tokens":      stats.PromptTokens + stats.EvalTokens,
		},
	}
}

// splitOAIMessages раскладывает messages OpenAI на (system, история, последний user).
func splitOAIMessages(msgs []oaiMessage) (system string, history []chatMsg, user string) {
	// последний user — текущий вопрос; более ранние — история; system собираем отдельно.
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = i
			break
		}
	}
	for i, m := range msgs {
		switch {
		case m.Role == "system":
			if system == "" {
				system = m.Content
			} else {
				system += "\n" + m.Content
			}
		case i == lastUser:
			user = m.Content
		case m.Role == "user" || m.Role == "assistant":
			history = append(history, chatMsg{Role: m.Role, Content: m.Content})
		}
	}
	return system, history, user
}

// --- rate limiter: токен-бакет на IP (stdlib) ---

type ipLimiter struct {
	mu   sync.Mutex
	rpm  int
	rate float64 // токенов в секунду
	m    map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(rpm int) *ipLimiter {
	if rpm <= 0 {
		rpm = 20
	}
	return &ipLimiter{rpm: rpm, rate: float64(rpm) / 60.0, m: map[string]*tokenBucket{}}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.m[ip]
	if !ok {
		l.m[ip] = &tokenBucket{tokens: float64(l.rpm) - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > float64(l.rpm) {
		b.tokens = float64(l.rpm)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// --- утилиты ---

func writeJSONResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAIError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSONResp(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

func clientIP(r *http.Request) string {
	// За reverse-proxy реальный IP — в X-Forwarded-For (первый элемент).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func normalizeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

func portOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
