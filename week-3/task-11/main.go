package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/joho/godotenv"
)

// defaultMemDir — каталог memory РЯДОМ С КОДОМ задачи, а не относительно CWD.
// Так `go run ./week-3/task-11 -report` из корня создаёт memory внутри task-11.
func defaultMemDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Join(filepath.Dir(file), "memory")
	}
	return "./memory"
}

func main() {
	reportMode := flag.Bool("report", false, "прогнать демонстрацию слоёв памяти и выйти")
	dir := flag.String("mem", defaultMemDir(), "каталог для раздельного хранения слоёв (по умолчанию — рядом с кодом задачи)")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()
	model := anthropic.ModelClaudeOpus4_8

	if *reportMode {
		if err := runReport(ctx, client, *dir); err != nil {
			log.Fatalf("report failed: %s", err.Error())
		}
		return
	}

	mem, _ := LoadMemory(*dir)
	agent := NewAgent(client, model, mem)

	fmt.Println("Чат с памятью в 3 слоя. Хранится раздельно в", *dir)
	fmt.Println("  short-term.json · working.json (ведёт агент) · long-term.md (ведёшь ТЫ)")
	if !mem.HasLongDoc() {
		fmt.Printf("  ⓘ long-term.md не найден в %s — создай его сам (профиль/знания),\n", *dir)
		fmt.Println("    агент подхватит при старте или по команде /reload.")
	}
	fmt.Println("Команды:")
	fmt.Println("  long-term:  /know <текст>           — дописать в long-term.md")
	fmt.Println("              /reload                  — перечитать long-term.md с диска")
	fmt.Println("  working:    /task <имя> | /goal <..> | /plan a; b; c")
	fmt.Println("              /current <шаг> | /done <шаг> | /decide ключ=значение")
	fmt.Println("  прочее:     /mem | /reset-task | exit")

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "exit":
			_ = mem.Save()
			fmt.Println("Сохранил слои. Exiting")
			return
		case line == "/mem":
			fmt.Print(mem.Inspect())
			continue
		case line == "/reset-task":
			mem.ResetTask()
			fmt.Println("рабочая память (TaskContext) очищена")
			continue
		case strings.HasPrefix(line, "/"):
			handleCmd(mem, line)
			continue
		}

		reply, err := agent.Ask(ctx, line, FullPolicy())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("error: %s", err.Error())
			continue
		}
		fmt.Println(reply.Text)
		fmt.Printf("[слои в запросе: %s]\n", layersString(reply.UsedLayers))
		_ = mem.Save()
	}
}

func handleCmd(mem *Memory, line string) {
	cmd, arg := splitCmd(line)
	switch cmd {
	case "/know":
		mem.AppendKnowledge(arg)
		_ = mem.SaveLong()
		fmt.Println("дописано в long-term.md")
	case "/reload":
		if err := mem.ReloadLongDoc(); err != nil {
			fmt.Println("не удалось перечитать long-term.md:", err.Error())
			return
		}
		fmt.Println("long-term.md перечитан")
	case "/task":
		mem.SetTask(arg, mem.taskGoal())
		fmt.Println("задача задана (working)")
	case "/goal":
		mem.SetTask(mem.taskName(), arg)
		fmt.Println("цель задачи задана (working)")
	case "/plan":
		steps := splitSemicolons(arg)
		mem.SetPlan(steps...)
		fmt.Printf("план задан: %v\n", steps)
	case "/current":
		mem.SetCurrent(arg)
		fmt.Println("текущий шаг задан (working)")
	case "/done":
		mem.MarkDone(arg)
		fmt.Println("шаг отмечен выполненным (working)")
	case "/decide":
		k, v := splitKV(arg)
		mem.Decide(k, v)
		fmt.Println("решение задачи сохранено (working)")
	default:
		fmt.Println("неизвестная команда")
	}
}

func splitCmd(line string) (string, string) {
	f := strings.SplitN(line, " ", 2)
	if len(f) == 1 {
		return f[0], ""
	}
	return f[0], strings.TrimSpace(f[1])
}

func splitKV(s string) (string, string) {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
}

func splitSemicolons(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
