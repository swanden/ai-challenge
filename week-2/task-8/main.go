package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
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
	reportMode := flag.Bool("report", false, "прогнать автоматическое сравнение токенов и выйти")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()

	// Режим отчёта: программа сама всё считает и делает выводы.
	if *reportMode {
		if err := runReport(ctx, client); err != nil {
			log.Fatalf("report failed: %s", err.Error())
		}
		return
	}

	// Иначе — обычный интерактивный чат с сохранением контекста.
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
			switch {
			case ctx.Err() != nil:
				fmt.Println("\nExiting")
				return
			case errors.Is(err, errContextOverflow):
				fmt.Printf("⚠ context overflow: %s\n", err.Error())
				fmt.Println("  дальше модель не примет запрос — нужно обрезать/суммаризовать историю")
			default:
				log.Printf("error: %s", err.Error())
			}
			continue
		}

		fmt.Println(reply.Text)

		in, out, usd := agent.SessionTokens()
		fmt.Printf("[tokens] %s\n", reply.Tokens.String())
		fmt.Printf("[session] input: %d · output: %d · cost: $%.5f\n", in, out, usd)
	}
}
