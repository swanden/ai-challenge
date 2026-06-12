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

// Параметры сжатия для интерактивного чата.
const (
	chatKeepLast = 6 // последние сообщения держим "как есть"
	chatBatch    = 6 // копим столько сверх keepLast перед сжатием
)

func main() {
	reportMode := flag.Bool("report", false, "прогнать сравнение без сжатия / со сжатием и выйти")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()

	if *reportMode {
		if err := runReport(ctx, client); err != nil {
			log.Fatalf("report failed: %s", err.Error())
		}
		return
	}

	store := NewJSONStore(historyFile)
	agent, err := NewAgent(client, store, WithCompression(chatKeepLast, chatBatch))
	if err != nil {
		log.Fatalf("failed to init agent: %s", err.Error())
	}

	if n := agent.Messages(); n > 0 || agent.Summary() != "" {
		fmt.Printf("Restored: %d recent messages", n)
		if agent.Summary() != "" {
			fmt.Print(" + summary")
		}
		fmt.Printf(" from %s\n", historyFile)
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
			default:
				log.Printf("error: %s", err.Error())
			}
			continue
		}

		fmt.Println(reply.Text)

		in, out, usd := agent.Totals()
		fmt.Printf("[tokens] %s\n", reply.Tokens.String())
		fmt.Printf("[session] input: %d · output: %d · cost: $%.5f", in, out, usd)
		if reply.Compressed {
			fmt.Print(" · история сжата")
		}
		fmt.Println()
	}
}
