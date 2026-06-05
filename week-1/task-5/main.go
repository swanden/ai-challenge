package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/joho/godotenv"
)

// OpenAI-совместимый маршрутизатор HuggingFace Inference Providers.
const endpoint = "https://router.huggingface.co/v1/chat/completions"

const maxTokens = 2048

// Модель для сравнения. name — repo id с HuggingFace; можно закрепить провайдера
// суффиксом, например "meta-llama/Llama-3.3-70B-Instruct:together".
// Цены — в долларах за 1M токенов (ПРИМЕРНЫЕ, уточняй на странице провайдера);
// 0 — стоимость не считаем.
type model struct {
	label           string
	name            string
	inputPricePerM  float64
	outputPricePerM float64
}

var models = []model{
	{"слабая (8B)", "meta-llama/Llama-3.1-8B-Instruct:novita", 0.02, 0.05},
	{"средняя (20B)", "openai/gpt-oss-20b:novita", 0.04, 0.15},
	{"сильная (72B)", "Qwen/Qwen2.5-72B-Instruct:novita", 0.38, 0.40},
}

// Модель-судья, которая сравнивает ответы остальных моделей.
// По умолчанию — самая сильная из набора.
const judgeModel = "Qwen/Qwen2.5-72B-Instruct:novita"

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type result struct {
	m       model
	text    string
	prompt  int
	output  int
	total   int
	elapsed time.Duration
	err     error
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	token := os.Getenv("HF_TOKEN")
	if token == "" {
		log.Fatalf("не задан HF_TOKEN в .env (токен с huggingface.co/settings/tokens)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: 120 * time.Second}
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Введите запрос — он будет выполнен на трёх моделях разного размера и сравнён.")
	fmt.Println("Пустая строка — пример по умолчанию. exit — выход.")

	const defaultQuery = "Объясни простыми словами, что такое рекурсия, и приведи короткий пример на Python."

	for {
		fmt.Print("\nЗапрос> ")
		line, _ := reader.ReadString('\n')
		query := strings.TrimSpace(line)

		if query == "exit" {
			fmt.Println("Выход")
			break
		}
		if query == "" {
			query = defaultQuery
			fmt.Printf("(использую пример: %s)\n", query)
		}

		runComparison(ctx, httpClient, token, query)
	}
}

// ask отправляет один запрос к указанной модели и возвращает результат с метриками.
func ask(ctx context.Context, client *http.Client, token, modelName, prompt string) result {
	res := result{}

	body, err := json.Marshal(chatRequest{
		Model:     modelName,
		Messages:  []chatMessage{{Role: "user", Content: prompt}},
		MaxTokens: maxTokens,
	})
	if err != nil {
		res.err = err
		return res
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		res.err = err
		return res
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	res.elapsed = time.Since(start)
	if err != nil {
		res.err = err
		return res
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		res.err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return res
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		res.err = err
		return res
	}
	if cr.Error != nil {
		res.err = fmt.Errorf("api error: %s", cr.Error.Message)
		return res
	}
	if len(cr.Choices) == 0 {
		res.err = fmt.Errorf("пустой ответ")
		return res
	}

	res.text = cr.Choices[0].Message.Content
	res.prompt = cr.Usage.PromptTokens
	res.output = cr.Usage.CompletionTokens
	res.total = cr.Usage.TotalTokens
	return res
}

// runComparison прогоняет один запрос по всем моделям и печатает ответы + сводку.
func runComparison(ctx context.Context, client *http.Client, token, query string) {
	fmt.Printf("\n############### ЗАПРОС ###############\n%s\n", query)

	results := make([]result, 0, len(models))

	for _, m := range models {
		fmt.Printf("\n========== %s — %s ==========\n", m.label, m.name)
		res := ask(ctx, client, token, m.name, query)
		res.m = m

		if res.err != nil {
			fmt.Printf("ошибка: %s\n", res.err.Error())
		} else {
			fmt.Println(res.text)
		}
		results = append(results, res)
	}

	printSummary(results)
	printJudge(ctx, client, token, query, results)
}

// printSummary выводит таблицу сравнения: время, токены, скорость, стоимость.
func printSummary(results []result) {
	fmt.Println("\n========== Сравнение ==========")

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "Модель\tВремя\tТокены (in/out)\tСкорость\tСтоимость")
	fmt.Fprintln(w, "------\t-----\t---------------\t--------\t---------")

	anyPriced := false
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(w, "%s\t%s\tошибка\t-\t-\n", r.m.label, fmtDur(r.elapsed))
			continue
		}

		// Скорость генерации: выходные токены в секунду.
		var tps float64
		if sec := r.elapsed.Seconds(); sec > 0 {
			tps = float64(r.output) / sec
		}

		costStr := "—"
		if r.m.inputPricePerM > 0 || r.m.outputPricePerM > 0 {
			anyPriced = true
			c := float64(r.prompt)/1e6*r.m.inputPricePerM +
				float64(r.output)/1e6*r.m.outputPricePerM
			costStr = fmt.Sprintf("$%.6f", c)
		}

		fmt.Fprintf(w, "%s\t%s\t%d / %d\t%.1f ток/с\t%s\n",
			r.m.label, fmtDur(r.elapsed), r.prompt, r.output, tps, costStr)
	}
	w.Flush()

	if anyPriced {
		fmt.Println("\n* Цены за 1M токенов по тарифам Novita (провайдер закреплён суффиксом :novita).")
	}
}

func fmtDur(d time.Duration) string {
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// printJudge просит модель-судью сравнить ответы всех моделей по качеству,
// учитывая заодно метрики скорости и стоимости, и печатает её вывод.
func printJudge(ctx context.Context, client *http.Client, token, query string, results []result) {
	// Собираем ответы и метрики успешных моделей в один промпт.
	var sb strings.Builder
	sb.WriteString("Один и тот же запрос был отправлен на несколько моделей разного размера.\n\n")
	fmt.Fprintf(&sb, "ЗАПРОС:\n%s\n\n", query)

	successful := 0
	for _, r := range results {
		if r.err != nil {
			continue
		}
		successful++

		var tps float64
		if sec := r.elapsed.Seconds(); sec > 0 {
			tps = float64(r.output) / sec
		}
		cost := float64(r.prompt)/1e6*r.m.inputPricePerM +
			float64(r.output)/1e6*r.m.outputPricePerM

		fmt.Fprintf(&sb, "=== Модель: %s ===\n", r.m.label)
		fmt.Fprintf(&sb, "[метрики: время %s, токены %d/%d, скорость %.1f ток/с, стоимость $%.6f]\n",
			fmtDur(r.elapsed), r.prompt, r.output, tps, cost)
		fmt.Fprintf(&sb, "Ответ:\n%s\n\n", r.text)
	}

	if successful < 2 {
		return // сравнивать нечего
	}

	sb.WriteString("Сравни ответы и сделай короткий вывод по трём пунктам: " +
		"1) качество ответов (точность, полнота, корректность кода); " +
		"2) скорость; 3) стоимость и ресурсоёмкость. " +
		"В конце укажи, какая модель и для каких задач предпочтительнее. Будь краток и конкретен.")

	fmt.Println("\n========== Вывод программы (модель-судья) ==========")
	res := ask(ctx, client, token, judgeModel, sb.String())
	if res.err != nil {
		fmt.Printf("не удалось получить вывод: %s\n", res.err.Error())
		return
	}
	fmt.Println(res.text)
	fmt.Println("\n* Вывод сгенерирован моделью " + judgeModel +
		"; оценка качества субъективна и может быть пристрастна к собственному стилю.")
}
