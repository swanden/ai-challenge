package main

import (
	"bufio"
	"context"
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

func main() {
	reportMode := flag.Bool("report", false, "прогнать сравнение стратегий и выйти")
	stratName := flag.String("strategy", "facts", "стратегия для чата: sliding | facts | branching")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()
	model := anthropic.ModelClaudeOpus4_8
	haiku := anthropic.ModelClaudeHaiku4_5

	if *reportMode {
		if err := runReport(ctx, client); err != nil {
			log.Fatalf("report failed: %s", err.Error())
		}
		return
	}

	// переключатель стратегии для интерактивного чата
	strategy := pickStrategy(*stratName, client, haiku)
	agent := NewAgent(client, model, strategy)

	fmt.Printf("Стратегия: %s\n", strategy.Name())
	fmt.Println("Команды: exit — выход" + branchingHelp(strategy))

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
		// команды веток для Branching: /branch <name>, /switch <name>
		if br, ok := strategy.(*Branching); ok && strings.HasPrefix(line, "/") {
			handleBranchCmd(br, line)
			continue
		}

		reply, err := agent.Ask(ctx, line)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Println("\nExiting")
				return
			}
			log.Printf("error: %s", err.Error())
			continue
		}
		fmt.Println(reply.Text)
		in, out, usd := agent.Totals()
		fmt.Printf("[tokens] %s\n", reply.Tokens.String())
		fmt.Printf("[session] input: %d · output: %d · cost: $%.5f\n", in, out, usd)
	}
}

func pickStrategy(name string, client anthropic.Client, haiku anthropic.Model) Strategy {
	switch name {
	case "sliding":
		return NewSlidingWindow(windowN)
	case "branching":
		return NewBranching()
	default:
		return NewStickyFacts(client, haiku, windowN)
	}
}

func branchingHelp(s Strategy) string {
	if _, ok := s.(*Branching); ok {
		return ", /branch <имя> — форк, /switch <имя> — переключить ветку"
	}
	return ""
}

func handleBranchCmd(br *Branching, line string) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		fmt.Println("формат: /branch <имя> или /switch <имя>")
		return
	}
	switch parts[0] {
	case "/branch":
		br.Branch(parts[1])
		fmt.Printf("ветка %q создана (форк от %q). Ветки: %v\n", parts[1], br.Current(), br.Branches())
	case "/switch":
		if err := br.Switch(parts[1]); err != nil {
			fmt.Println(err.Error())
			return
		}
		fmt.Printf("активная ветка: %q\n", br.Current())
	default:
		fmt.Println("неизвестная команда")
	}
}
