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

// swarmStageOf парсит значение флага -swarm-stage в TaskState.
func swarmStageOf(v string) TaskState {
	if v == "planning" {
		return StatePlanning
	}
	return StateValidation
}

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
	confirm := flag.Bool("confirm", false, "спрашивать подтверждение перед переходом этапа (пауза)")
	llmCheck := flag.Bool("llm-check", false, "включить семантическую проверку инвариантов через LLM")
	swarmOn := flag.Bool("swarm", false, "включить рой ревьюеров")
	swarmStage := flag.String("swarm-stage", "validation", "этап роя: planning | validation")
	maxTokens := flag.Int64("max-tokens", 4096, "лимит токенов ответа модели")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Fatalf("failed to load .env: %s", err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := anthropic.NewClient()
	model := anthropic.ModelClaudeOpus4_8

	if *reportMode {
		if err := runReport(ctx, client, *profilesDir, *memDir, *swarmOn, *swarmStage, *maxTokens); err != nil {
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
	inv := NewInvariantSet(*memDir)
	agent.SetInvariants(inv)
	agent.SetLLMCheck(*llmCheck)
	agent.SetMaxTokens(*maxTokens)

	fmt.Println("Накопительный агент: память (3 слоя) + персонализация.")
	fmt.Printf("Активный профиль: %q · профили: %v\n", agent.ProfileID(), store.List())
	fmt.Printf("Память: %s\n", *memDir)
	fmt.Println("Команды:")
	fmt.Println("  профиль (long-term): /profile <id> · /profiles · /whoami")
	fmt.Println("  working:  /task <имя> · /goal <..> · /plan a; b · /current <шаг> · /done <шаг> · /decide k=v")
	fmt.Println("  состояние:/state · /run (прогнать этапы) · /advance (шаг вперёд) · pause")
	fmt.Println("  инварианты:/invariants · /forbid <стоп-слово> · /invariant <правило>")
	fmt.Println("  переходы: /approve-plan · /validate-ok · /goto <state> (контроль gate)")
	fmt.Println("  прочее:   /mem · /reset-task · exit")
	if *confirm {
		fmt.Println("  режим -confirm: перед каждым переходом спрошу y/n/pause")
	}
	if mem.State().valid() && mem.TaskName() != "" {
		fmt.Printf("Возобновление: задача %q на этапе %s.\n", mem.TaskName(), mem.State())
	}

	reader := bufio.NewReader(os.Stdin)
	pipe := NewPipeline(agent, *confirm, reader)
	if *swarmOn {
		pipe.EnableSwarm(NewSwarm(client, model, swarmStageOf(*swarmStage)))
	}
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
		case line == "/invariants":
			for _, iv := range inv.All() {
				fmt.Printf("  [%s/%s] %s\n", iv.Kind, iv.ID, iv.Text)
			}
			continue
		case strings.HasPrefix(line, "/forbid "):
			word := strings.TrimSpace(strings.TrimPrefix(line, "/forbid "))
			iv := inv.Add("Запрещено пользователем: "+word, []string{word})
			fmt.Printf("добавлен инвариант [%s]: запрет %q\n", iv.ID, word)
			continue
		case strings.HasPrefix(line, "/invariant "):
			rule := strings.TrimSpace(strings.TrimPrefix(line, "/invariant "))
			iv := inv.Add(rule, nil)
			fmt.Printf("добавлен инвариант [%s] (семантический, проверяется LLM): %s\n", iv.ID, rule)
			continue
		case line == "/state":
			fmt.Printf("state=%s — %s | план утверждён=%v, валидация=%v\n",
				mem.State(), expectedAction[mem.State()], mem.PlanApproved(), mem.Validated())
			continue
		case line == "/approve-plan":
			mem.ApprovePlan()
			_ = mem.Save()
			fmt.Println("план утверждён — теперь разрешён переход planning → execution")
			continue
		case line == "/validate-ok":
			mem.MarkValidated()
			_ = mem.Save()
			fmt.Println("валидация отмечена пройденной — разрешён переход validation → done")
			continue
		case strings.HasPrefix(line, "/goto "):
			target := TaskState(strings.TrimSpace(strings.TrimPrefix(line, "/goto ")))
			from := mem.State()
			if err := mem.Goto(target); err != nil {
				fmt.Printf("ПЕРЕХОД ЗАПРЕЩЁН: %s. Остаюсь на %s.\n", err.Error(), from)
			} else {
				fmt.Printf("переход %s → %s выполнен\n", from, target)
				_ = mem.Save()
			}
			continue
		case line == "/advance":
			from := mem.State()
			to, err := mem.Advance()
			if err != nil {
				fmt.Println("переход невозможен:", err.Error())
			} else {
				fmt.Printf("переход %s → %s\n", from, to)
				_ = mem.Save()
			}
			continue
		case line == "/run":
			done, err := pipe.Run(ctx)
			if err != nil {
				log.Printf("pipeline: %s", err.Error())
			} else if done {
				fmt.Println("[pipeline] задача доведена до done.")
			}
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
