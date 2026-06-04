package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/joho/godotenv"
)

const (
	// Внимание: у рассуждающих моделей (Opus 4.6/4.7/4.8) параметр temperature
	// отключён — запрос с ним вернёт 400 "temperature is deprecated for this model".
	// Для задания про temperature используем модель, где параметр ещё работает.
	model     anthropic.Model = "claude-haiku-4-5-20251001"
	maxTokens                 = 1024

	// API Anthropic принимает temperature в диапазоне 0.0–1.0.
	// Значения выше будут ограничены до этого максимума.
	apiMaxTemperature = 1.0

	// Сколько раз повторять один и тот же запрос на каждой температуре —
	// чтобы наглядно увидеть разнообразие ответов.
	samplesPerTemp = 3
)

// Температуры из задания. 1.2 выходит за пределы API и будет ограничено до 1.0.
var temperatures = []float64{0.0, 0.7, 1.2}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Введите запрос — он будет выполнен с разной температурой и сравнён.")
	fmt.Println("Пустая строка — использовать пример по умолчанию. exit — выход.")

	const defaultQuery = "Придумай оригинальное название и слоган для кофейни."

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

		if err := runExperiment(ctx, client, query); err != nil {
			log.Printf("ошибка: %s", err.Error())
		}
	}
}

// ask — запрос к модели с заданной температурой.
func ask(ctx context.Context, client anthropic.Client, system, user string, temperature float64) (string, error) {
	params := anthropic.MessageNewParams{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: anthropic.Float(temperature),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}

	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}
	if len(msg.Content) == 0 {
		return "", fmt.Errorf("пустой ответ от модели")
	}
	return msg.Content[0].Text, nil
}

// clampTemp ограничивает температуру допустимым диапазоном API.
func clampTemp(t float64) (effective float64, clamped bool) {
	switch {
	case t > apiMaxTemperature:
		return apiMaxTemperature, true
	case t < 0:
		return 0, true
	default:
		return t, false
	}
}

// runExperiment выполняет один и тот же запрос на разных температурах
// (по несколько запусков на каждую) и сравнивает результаты.
func runExperiment(ctx context.Context, client anthropic.Client, query string) error {
	fmt.Printf("\n############### ЗАПРОС ###############\n%s\n", query)

	var report strings.Builder
	fmt.Fprintf(&report, "ЗАПРОС:\n%s\n\n", query)

	for _, reqTemp := range temperatures {
		temp, clamped := clampTemp(reqTemp)

		header := fmt.Sprintf("temperature = %.1f", reqTemp)
		if clamped {
			header += fmt.Sprintf(" (API ограничивает до %.1f)", temp)
		}

		fmt.Printf("\n========== %s ==========\n", header)
		fmt.Fprintf(&report, "=== %s ===\n", header)

		for i := 1; i <= samplesPerTemp; i++ {
			ans, err := ask(ctx, client, "", query, temp)
			if err != nil {
				return err
			}
			fmt.Printf("\n--- запуск %d ---\n%s\n", i, ans)
			fmt.Fprintf(&report, "Запуск %d: %s\n", i, ans)
		}
		report.WriteString("\n")
	}

	// Сравнение по точности / креативности / разнообразию.
	fmt.Println("\n========== Сравнение ==========")
	compareSystem := "Ты — беспристрастный эксперт по языковым моделям."
	compareUser := "Ниже один и тот же запрос, выполненный с разной температурой " +
		"(по несколько запусков на каждую температуру).\n\n" +
		report.String() +
		"\nСравни результаты по трём критериям: 1) точность и связность; " +
		"2) креативность; 3) разнообразие между запусками одной температуры. " +
		"Затем сформулируй, для каких задач лучше подходит каждая настройка температуры. " +
		"Будь краток и конкретен."

	comparison, err := ask(ctx, client, compareSystem, compareUser, 0.0)
	if err != nil {
		return err
	}
	fmt.Println(comparison)

	return nil
}
