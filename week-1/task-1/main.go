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
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		if line == "exit" {
			fmt.Println("Выход.")
			break
		}

		msg, err := client.Messages.New(
			ctx,
			anthropic.MessageNewParams{
				Model:     anthropic.ModelClaudeOpus4_8,
				MaxTokens: 1024,
				Messages: []anthropic.MessageParam{
					anthropic.NewUserMessage(
						anthropic.NewTextBlock(line),
					),
				},
			},
		)
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
