package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerSpec — один MCP-сервер: логическое имя + назначение + команда запуска.
type ServerSpec struct {
	Name    string
	Purpose string // короткое назначение для каталога (ленивый режим)
	Cmd     []string
}

// LoadToolsName — имя мета-тула ленивой загрузки схем (обрабатывается агентом, не сервером).
const LoadToolsName = "load_tools"

// MCPTools — день 20: подключение к НЕСКОЛЬКИМ MCP-серверам, агрегация их тулов
// и МАРШРУТИЗАЦИЯ вызова по имени тула к серверу-владельцу.
type MCPTools struct {
	tools    []anthropic.ToolUnionParam    // объединённый список для модели (жадный режим)
	route    map[string]*mcp.ClientSession // имя тула -> сессия владельца
	origin   map[string]string             // имя тула -> имя сервера (для вывода)
	byServer map[string][]string           // сервер -> его тулы (имена)

	// для ленивого режима (день 20, усиление):
	toolsByServer map[string][]anthropic.ToolUnionParam // сервер -> его тулы (схемы)
	purpose       map[string]string                     // сервер -> назначение (для каталога)
	serverOrder   []string                              // порядок серверов (детерминированный)
}

// ConnectMCP поднимает все указанные серверы (stdio-сабпроцессы), собирает их тулы
// в общий список и строит таблицу маршрутизации. Возвращает реестр и stop-функцию.
func ConnectMCP(ctx context.Context, servers []ServerSpec) (*MCPTools, func(), error) {
	m := &MCPTools{
		route:         map[string]*mcp.ClientSession{},
		origin:        map[string]string{},
		byServer:      map[string][]string{},
		toolsByServer: map[string][]anthropic.ToolUnionParam{},
		purpose:       map[string]string{},
	}
	var stops []func()
	stopAll := func() {
		for _, s := range stops {
			s()
		}
	}

	for _, spec := range servers {
		m.serverOrder = append(m.serverOrder, spec.Name)
		m.purpose[spec.Name] = spec.Purpose
		client := mcp.NewClient(&mcp.Implementation{Name: "day20-agent", Version: "0.1.0"}, nil)
		cmd := exec.CommandContext(ctx, spec.Cmd[0], spec.Cmd[1:]...)

		// stderr каждого сервера — в свой лог-файл, чтобы не засорять REPL.
		var logFile *os.File
		if f, err := os.Create(filepath.Join(sourceDir(), spec.Name+".log")); err == nil {
			cmd.Stderr = f
			logFile = f
		} else {
			cmd.Stderr = os.Stderr
		}

		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			stopAll()
			return nil, func() {}, fmt.Errorf("mcp connect %q: %w", spec.Name, err)
		}
		sess := session
		lf := logFile
		stops = append(stops, func() {
			_ = sess.Close()
			if lf != nil {
				_ = lf.Close()
			}
		})

		lt, err := session.ListTools(ctx, nil)
		if err != nil {
			stopAll()
			return nil, func() {}, fmt.Errorf("mcp tools/list %q: %w", spec.Name, err)
		}
		for _, t := range lt.Tools {
			if _, dup := m.route[t.Name]; dup {
				// коллизия имён между серверами — оставляем первого владельца, предупреждаем.
				fmt.Printf("[mcp] предупреждение: тул %q есть на нескольких серверах; беру %q\n", t.Name, m.origin[t.Name])
				continue
			}
			tool := oneTool(t)
			m.tools = append(m.tools, tool)
			m.toolsByServer[spec.Name] = append(m.toolsByServer[spec.Name], tool)
			m.route[t.Name] = session
			m.origin[t.Name] = spec.Name
			m.byServer[spec.Name] = append(m.byServer[spec.Name], t.Name)
		}
	}
	return m, stopAll, nil
}

func (m *MCPTools) Tools() []anthropic.ToolUnionParam { return m.tools }
func (m *MCPTools) Len() int                          { return len(m.tools) }

// CatalogTools — стартовый набор для ЛЕНИВОГО режима: один мета-тул load_tools,
// в описании которого перечислены серверы и их назначение, но БЕЗ полных схем их тулов.
// Это и есть «каталог серверов», с которого начинает модель (экономия токенов).
func (m *MCPTools) CatalogTools() []anthropic.ToolUnionParam {
	var lines []string
	enum := make([]any, 0, len(m.serverOrder))
	for _, s := range m.serverOrder {
		lines = append(lines, fmt.Sprintf("%q — %s (тулов: %d)", s, m.purpose[s], len(m.toolsByServer[s])))
		enum = append(enum, s)
	}
	desc := "Загрузить инструменты одного MCP-сервера ПЕРЕД их использованием. " +
		"Сначала загрузи сервер, подходящий под задачу, затем вызывай его тулы. " +
		"Доступные серверы: " + strings.Join(lines, "; ") + "."
	tp := anthropic.ToolParam{
		Name:        LoadToolsName,
		Description: anthropic.String(desc),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"server": map[string]any{"type": "string", "enum": enum, "description": "Имя сервера из каталога"},
			},
			Required: []string{"server"},
		},
	}
	return []anthropic.ToolUnionParam{{OfTool: &tp}}
}

// LoadServer возвращает схемы тулов запрошенного сервера (для подмешивания в запрос)
// и текстовое подтверждение для модели. Вызывается, когда модель дёрнула load_tools.
func (m *MCPTools) LoadServer(input json.RawMessage) ([]anthropic.ToolUnionParam, string) {
	var in struct {
		Server string `json:"server"`
	}
	_ = json.Unmarshal(input, &in)
	tools, ok := m.toolsByServer[in.Server]
	if !ok {
		return nil, fmt.Sprintf("нет сервера %q; доступны: %s", in.Server, strings.Join(m.serverOrder, ", "))
	}
	return tools, fmt.Sprintf("загружены инструменты сервера %q: %s. Теперь можешь их вызывать.",
		in.Server, strings.Join(m.byServer[in.Server], ", "))
}

// appendNewTools добавляет тулы, которых ещё нет (дедуп по имени).
func appendNewTools(existing, added []anthropic.ToolUnionParam) []anthropic.ToolUnionParam {
	have := map[string]bool{}
	for _, t := range existing {
		if t.OfTool != nil {
			have[t.OfTool.Name] = true
		}
	}
	for _, t := range added {
		if t.OfTool != nil && !have[t.OfTool.Name] {
			existing = append(existing, t)
			have[t.OfTool.Name] = true
		}
	}
	return existing
}

// Names — все тулы, отсортированно (для стартового вывода).
func (m *MCPTools) Names() []string {
	out := make([]string, 0, len(m.route))
	for n := range m.route {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Breakdown — какие тулы с какого сервера (для стартового вывода).
func (m *MCPTools) Breakdown() string {
	servers := make([]string, 0, len(m.byServer))
	for s := range m.byServer {
		servers = append(servers, s)
	}
	sort.Strings(servers)
	var parts []string
	for _, s := range servers {
		tools := append([]string(nil), m.byServer[s]...)
		sort.Strings(tools)
		parts = append(parts, fmt.Sprintf("%s(%d): %s", s, len(tools), strings.Join(tools, ", ")))
	}
	return strings.Join(parts, " | ")
}

// Call МАРШРУТИЗИРУЕТ вызов к серверу-владельцу тула.
func (m *MCPTools) Call(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	session, ok := m.route[name]
	if !ok {
		return "неизвестный инструмент: " + name, true
	}
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "не разобрал аргументы инструмента: " + err.Error(), true
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Sprintf("ошибка вызова %s: %v", name, err), true
	}
	return mcpResultText(res), res.IsError
}

// oneTool конвертирует один MCP-инструмент в формат tools Anthropic.
func oneTool(t *mcp.Tool) anthropic.ToolUnionParam {
	props, required := splitSchema(t.InputSchema)
	tp := anthropic.ToolParam{
		Name:        t.Name,
		Description: anthropic.String(t.Description),
		InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: required},
	}
	return anthropic.ToolUnionParam{OfTool: &tp}
}

// splitSchema достаёт properties и required из JSON-схемы MCP-инструмента.
// Properties ВСЕГДА непустой объект: иначе ToolInputSchemaParam считается "нулевым"
// и из-за omitzero выпадает из запроса целиком (API → input_schema: Field required).
func splitSchema(schema any) (props any, required []string) {
	var m map[string]any
	switch v := schema.(type) {
	case map[string]any:
		m = v
	case json.RawMessage:
		_ = json.Unmarshal(v, &m)
	case []byte:
		_ = json.Unmarshal(v, &m)
	case string:
		_ = json.Unmarshal([]byte(v), &m)
	}
	if m == nil {
		return map[string]any{}, nil
	}
	if p, ok := m["properties"].(map[string]any); ok {
		props = p
	} else {
		props = map[string]any{}
	}
	if rs, ok := m["required"].([]any); ok {
		for _, r := range rs {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	return props, required
}

// mcpResultText склеивает текстовые блоки результата MCP-инструмента.
func mcpResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
