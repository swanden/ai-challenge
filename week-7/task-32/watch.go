package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// runWatch — день 18: «агент 24/7». Ставит города на фоновый сбор (track_location),
// затем по своему тикеру периодически дёргает weather_summary и печатает агрегат.
// Это планировщик на стороне агента; MCP-сервер пассивен (как в каноне из чата).
// Прямые вызовы тулов — без траты токенов LLM.
func runWatch(ctx context.Context, m *MCPTools, watchSec int, locations []string, intervalSec int) {
	// 1) поставить города на фоновый сбор
	for _, loc := range locations {
		loc = strings.TrimSpace(loc)
		if loc == "" {
			continue
		}
		out, _ := m.Call(ctx, "track_location", jsonArgs(map[string]any{
			"location": loc, "interval_sec": intervalSec,
		}))
		fmt.Printf("[watch] %s\n", out)
	}

	// 2) 24/7-цикл: периодическая сводка
	fmt.Printf("[watch] агент работает 24/7: сводка каждые %d с (Ctrl-C — выход)\n", watchSec)
	ticker := time.NewTicker(time.Duration(watchSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n[watch] остановлено, память сохранена")
			return
		case <-ticker.C:
			out, _ := m.Call(ctx, "weather_summary", json.RawMessage(`{}`))
			fmt.Printf("\n[сводка %s]\n%s\n", time.Now().Format("15:04:05"), out)
		}
	}
}

// jsonArgs — компактная сборка JSON-аргументов для вызова MCP-тула.
func jsonArgs(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}
