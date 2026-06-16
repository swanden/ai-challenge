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

func sourceDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(file)
	}
	return "."
}

func main() {
	reportMode := flag.Bool("report", false, "демонстрация (память + профили) и выход")
	memDir := flag.String("mem", filepath.Join(sourceDir(), "memory"), "каталог памяти (short-term/working)")
	profilesDir := flag.String("profiles", filepath.Join(sourceDir(), "profiles"), "каталог профилей (<id>.md)")
	active := flag.String("profile", "senior-go", "активный профиль")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()
	model := anthropic.ModelClaudeOpus4_8

	if *reportMode {
		if err := runReport(ctx, client, *profilesDir); err != nil {
			log.Fatalf("report failed: %s", err.Error())
		}
		return
	}

	mem, _ := LoadMemory(*memDir)
	store := NewProfileStore(*profilesDir)
	agent := NewAgent(client, model, mem)
	if p, err := store.Load(*active); err == nil {
		agent.SetProfile(p)
	} else {
		log.Printf("профиль %q не найден, работаю без профиля", *active)
	}

	fmt.Println("Накопительный агент: память (3 слоя) + персонализация.")
	fmt.Printf("Активный профиль: %q · профили: %v\n", agent.ProfileID(), store.List())
	fmt.Printf("Память: %s\n", *memDir)
	fmt.Println("Команды:")
	fmt.Println("  профиль (long-term): /profile <id> · /profiles · /whoami")
	fmt.Println("  working:  /task <имя> · /goal <..> · /plan a; b · /current <шаг> · /done <шаг> · /decide k=v")
	fmt.Println("  прочее:   /mem · /reset-task · exit")

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
			fmt.Println("Сохранил память. Exiting")
			return
		case line == "/mem":
			fmt.Print(mem.Inspect())
			continue
		case line == "/reset-task":
			mem.ResetTask()
			fmt.Println("рабочая память очищена")
			continue
		case line == "/profiles":
			fmt.Printf("доступно: %v\n", store.List())
			continue
		case line == "/whoami":
			fmt.Printf("активный профиль: %q\n%s\n", agent.ProfileID(), mem.LongDoc())
			continue
		case strings.HasPrefix(line, "/profile "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/profile "))
			p, err := store.Load(id)
			if err != nil {
				fmt.Printf("профиль %q не найден; доступно: %v\n", id, store.List())
				continue
			}
			agent.SetProfile(p)
			fmt.Printf("профиль переключён на %q\n", id)
			continue
		case strings.HasPrefix(line, "/"):
			handleWorkingCmd(mem, line)
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
		fmt.Printf("[профиль %q · слои: %s]\n", reply.ProfileID, layersString(reply.UsedLayers))
		_ = mem.Save()
	}
}

func handleWorkingCmd(mem *Memory, line string) {
	cmd, arg := splitCmd(line)
	switch cmd {
	case "/task":
		mem.SetTask(arg, mem.taskGoal())
		fmt.Println("задача задана (working)")
	case "/goal":
		mem.SetTask(mem.taskName(), arg)
		fmt.Println("цель задана (working)")
	case "/plan":
		steps := splitSemicolons(arg)
		mem.SetPlan(steps...)
		fmt.Printf("план: %v\n", steps)
	case "/current":
		mem.SetCurrent(arg)
		fmt.Println("текущий шаг задан (working)")
	case "/done":
		mem.MarkDone(arg)
		fmt.Println("шаг выполнен (working)")
	case "/decide":
		k, v := splitKV(arg)
		mem.Decide(k, v)
		fmt.Println("решение сохранено (working)")
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
