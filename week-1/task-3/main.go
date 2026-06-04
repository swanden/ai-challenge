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
	model     = anthropic.ModelClaudeOpus4_8
	maxTokens = 2048
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Введите задачу (логическую, алгоритмическую или аналитическую).")
	fmt.Println("Программа решит её четырьмя способами и сравнит результаты.")
	fmt.Println("Введите exit для выхода.")

	for {
		fmt.Print("\nЗадача> ")
		line, _ := reader.ReadString('\n')
		task := strings.TrimSpace(line)

		if task == "exit" {
			fmt.Println("Выход")
			break
		}
		if task == "" {
			continue
		}

		if err := solveAllWays(ctx, client, task); err != nil {
			log.Printf("ошибка: %s", err.Error())
		}
	}
}

// ask — единый помощник для запроса к модели.
// system можно оставить пустым, если системный промпт не нужен.
func ask(ctx context.Context, client anthropic.Client, system, user string) (string, error) {
	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: maxTokens,
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

// solveAllWays решает одну задачу четырьмя способами и сравнивает результаты.
func solveAllWays(ctx context.Context, client anthropic.Client, task string) error {
	fmt.Printf("\n############### ЗАДАЧА ###############\n%s\n", task)

	// --- Способ 1: прямой ответ без дополнительных инструкций ---
	fmt.Println("\n========== Способ 1: прямой ответ ==========")
	answer1, err := ask(ctx, client, "", task)
	if err != nil {
		return err
	}
	fmt.Println(answer1)

	// --- Способ 2: инструкция «решай пошагово» ---
	fmt.Println("\n========== Способ 2: «решай пошагово» ==========")
	answer2, err := ask(ctx, client, "",
		task+"\n\nРешай задачу пошагово: "+
			"прежде чем дать итоговый ответ.")
	if err != nil {
		return err
	}
	fmt.Println(answer2)

	// --- Способ 3: модель сначала составляет промпт, затем решает по нему ---
	fmt.Println("\n========== Способ 3: модель сама пишет промпт ==========")
	generatedPrompt, err := ask(ctx, client,
		"Ты — опытный инженер промптов.",
		"Составь подробный и эффективный промпт, который поможет языковой модели "+
			"максимально качественно и точно решить задачу ниже. "+
			"В ответе верни ТОЛЬКО текст промпта, без пояснений.\n\nЗадача:\n"+task)
	if err != nil {
		return err
	}
	fmt.Println("--- Сгенерированный промпт ---")
	fmt.Println(generatedPrompt)

	answer3, err := ask(ctx, client, "", generatedPrompt)
	if err != nil {
		return err
	}
	fmt.Println("\n--- Ответ по сгенерированному промпту ---")
	fmt.Println(answer3)

	// --- Способ 4: группа экспертов — отдельное решение от каждого ---
	fmt.Println("\n========== Способ 4: группа экспертов (решение от каждого) ==========")
	experts := []struct {
		name   string
		system string
	}{
		{
			"Аналитик",
			"Ты — Аналитик. Ты формализуешь условие, выделяешь данные и зависимости " +
				"и на этой основе самостоятельно решаешь задачу. Дай своё полное решение и итоговый ответ.",
		},
		{
			"Инженер",
			"Ты — Инженер. Ты решаешь задачу практично, по шагам, опираясь на конкретные " +
				"вычисления или алгоритм. Дай своё полное решение и итоговый ответ.",
		},
		{
			"Критик",
			"Ты — Критик. Ты решаешь задачу, перепроверяя каждый шаг и отлавливая возможные " +
				"ошибки и ловушки в условии. Дай своё полное решение и итоговый ответ.",
		},
	}

	var expertSolutions strings.Builder
	for _, e := range experts {
		fmt.Printf("\n--- %s ---\n", e.name)
		ans, err := ask(ctx, client, e.system, "Задача:\n"+task)
		if err != nil {
			return err
		}
		fmt.Println(ans)
		fmt.Fprintf(&expertSolutions, "%s:\n%s\n\n", e.name, ans)
	}
	answer4 := expertSolutions.String()

	// --- Сравнение всех способов ---
	fmt.Println("\n========== Сравнение способов ==========")
	compareSystem := "Ты — беспристрастный эксперт, который сравнивает решения одной задачи."
	compareUser := fmt.Sprintf(
		"Вот одна задача и четыре решения, полученные разными способами.\n\n"+
			"ЗАДАЧА:\n%s\n\n"+
			"РЕШЕНИЕ 1 (прямой ответ):\n%s\n\n"+
			"РЕШЕНИЕ 2 (пошагово):\n%s\n\n"+
			"РЕШЕНИЕ 3 (по сгенерированному промпту):\n%s\n\n"+
			"РЕШЕНИЕ 4 (решения трёх экспертов — аналитик, инженер, критик):\n%s\n\n"+
			"Сравни решения: 1) отличаются ли ответы и чем именно; "+
			"2) какой способ дал наиболее точный и обоснованный результат и почему. "+
			"Будь краток и конкретен.",
		task, answer1, answer2, answer3, answer4)

	comparison, err := ask(ctx, client, compareSystem, compareUser)
	if err != nil {
		return err
	}
	fmt.Println(comparison)

	return nil
}
