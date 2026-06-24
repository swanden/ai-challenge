package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// День 16. Подключение MCP.
// Минимальный клиент на официальном Go SDK (github.com/modelcontextprotocol/go-sdk):
// устанавливает MCP-соединение по stdio с ГОТОВЫМ сервером (свой сервер — день 17)
// и получает у него список инструментов (tools/list).
//
// LLM здесь не участвует — это чистый протокол MCP, поэтому API-ключ не нужен.
//
// Сервер задаётся как команда после флагов; по умолчанию — официальный
// референс-сервер "everything" через npx. Примеры:
//
//	go run .
//	go run . -schemas
//	go run . -- npx -y @modelcontextprotocol/server-filesystem /tmp
//	go run . -- python3 my_server.py
func main() {
	schemas := flag.Bool("schemas", false, "печатать input-схему каждого инструмента")
	timeout := flag.Duration("timeout", 60*time.Second, "общий таймаут на подключение и запрос")
	flag.Usage = usage
	flag.Parse()

	// Команда сервера = всё, что после флагов. Нет аргументов → дефолтный everything.
	srv := flag.Args()
	if len(srv) == 0 {
		srv = []string{"npx", "-y", "@modelcontextprotocol/server-everything"}
	}

	if err := run(srv, *schemas, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run(srv []string, schemas bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Клиент SDK. Транспорт — CommandTransport: SDK сам поднимает MCP-сервер как
	// дочерний процесс и общается с ним по stdin/stdout (newline-delimited JSON-RPC).
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-min-client", Version: "0.1.0"}, nil)

	cmd := exec.CommandContext(ctx, srv[0], srv[1:]...)
	cmd.Stderr = os.Stderr // лог сервера — в наш stderr

	// 1) Connect выполняет рукопожатие MCP (initialize + notifications/initialized)
	//    под капотом — проверяем, что СОЕДИНЕНИЕ устанавливается.
	fmt.Printf("[mcp] запускаю сервер: %s\n", strings.Join(srv, " "))
	fmt.Printf("[mcp] → connect/initialize\n")
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect/initialize: %w", err)
	}
	defer session.Close()

	// serverInfo и согласованную версию протокола берём из результата рукопожатия.
	if init := session.InitializeResult(); init != nil && init.ServerInfo != nil {
		name := init.ServerInfo.Name
		if init.ServerInfo.Title != "" {
			name = init.ServerInfo.Title
		}
		fmt.Printf("[mcp] ← сервер: %s v%s (согласован протокол %s)\n",
			name, init.ServerInfo.Version, init.ProtocolVersion)
	}
	fmt.Println("[mcp] соединение установлено ✓")

	// 2) tools/list — проверяем, что СПИСОК ИНСТРУМЕНТОВ возвращается корректно.
	fmt.Println("[mcp] → tools/list")
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	fmt.Printf("[mcp] ← получено инструментов: %d\n\n", len(res.Tools))

	if len(res.Tools) == 0 {
		fmt.Println("Сервер не отдал ни одного инструмента.")
		return nil
	}

	fmt.Println("Доступные инструменты:")
	for i, t := range res.Tools {
		fmt.Printf("  %2d. %s — %s\n", i+1, t.Name, oneLine(t.Description, 80))
		if schemas && t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				fmt.Printf("      input: %s\n", oneLine(string(b), 160))
			}
		}
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "MCP-клиент (день 16): подключается к готовому MCP-серверу и выводит список инструментов.")
	fmt.Fprintln(os.Stderr, "\nИспользование:")
	fmt.Fprintln(os.Stderr, "  go run . [флаги] [-- команда-запуска-MCP-сервера ...]")
	fmt.Fprintln(os.Stderr, "\nФлаги:")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "\nПримеры:")
	fmt.Fprintln(os.Stderr, "  go run .")
	fmt.Fprintln(os.Stderr, "  go run . -schemas")
	fmt.Fprintln(os.Stderr, "  go run . -- npx -y @modelcontextprotocol/server-filesystem /tmp")
}

// oneLine схлопывает многострочный текст в одну строку и обрезает по длине.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
