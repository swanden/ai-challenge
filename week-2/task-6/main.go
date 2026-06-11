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

	// main отвечает ТОЛЬКО за интерфейс (CLI): читает stdin, печатает stdout.
	// Вся доменная логика — в агенте.
	client := anthropic.NewClient()
	agent := NewAgent(client)

	fmt.Println("Type exit for quit")

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			// Ctrl+D / закрытый stdin
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" {
			fmt.Println("Exiting")
			break
		}

		reply, err := agent.Ask(ctx, line)
		if err != nil {
			// контекст отменён сигналом — выходим тихо
			if ctx.Err() != nil {
				fmt.Println("\nExiting")
				break
			}
			log.Printf("error: %s", err.Error())
			continue
		}

		fmt.Println(reply)
	}
}
