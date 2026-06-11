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

const historyFile = "history.json"

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Собираем зависимости: клиент -> хранилище -> агент.
	client := anthropic.NewClient()
	store := NewJSONStore(historyFile)

	agent, err := NewAgent(client, store)
	if err != nil {
		log.Fatalf("failed to init agent: %s", err.Error())
	}

	if n := agent.Messages(); n > 0 {
		fmt.Printf("Restored context: %d messages from %s\n", n, historyFile)
	}
	fmt.Println("Type exit for quit")

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
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
