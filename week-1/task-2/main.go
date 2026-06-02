package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("Type exit for quit")

	client := anthropic.NewClient()
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println("\nExiting")
				break
			}
			log.Fatalf("failed to read input: %s", err.Error())
		}
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}
		if strings.EqualFold(line, "exit") {
			fmt.Println("Exiting")
			break
		}

		fmt.Print("use constrains (y/n)?: ")
		answer, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			log.Fatalf("failed to read input: %s", err.Error())
		}
		useConstraints := strings.EqualFold(strings.TrimSpace(answer), "y")

		params := anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeOpus4_8,
			MaxTokens: 1024,
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(
					anthropic.NewTextBlock(line),
				),
			},
		}

		if useConstraints {
			params.MaxTokens = 100
			params.System = []anthropic.TextBlockParam{
				{Text: "Отвечай кратко, не более 2 предложений. " +
					"Формат: обычный текст без списков. " +
					"В самом конце ответа поставь маркер ###END###."},
			}
			params.StopSequences = []string{"###END###"}
		}

		msg, err := client.Messages.New(ctx, params)
		if err != nil {
			log.Fatalf("failed to create message: %s", err.Error())
		}

		if len(msg.Content) > 0 {
			fmt.Println(msg.Content[0].Text)
		} else {
			log.Fatalf("msg.Content is empty")
		}
	}
}
