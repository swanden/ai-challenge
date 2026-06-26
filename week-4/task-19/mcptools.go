package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTools — подключение агента к MCP-серверу (день 17).
// Держит stdio-сессию и заранее сконвертированный список инструментов
// в формате Anthropic tool-use, чтобы класть его в каждый запрос модели.
type MCPTools struct {
	session *mcp.ClientSession
	tools   []anthropic.ToolUnionParam
}

// ConnectMCP поднимает MCP-сервер как сабпроцесс (stdio), делает рукопожатие,
// запрашивает tools/list и конвертирует инструменты в формат Anthropic.
// Возвращает MCPTools и функцию остановки (закрыть сессию / убить сервер).
func ConnectMCP(ctx context.Context, serverCmd []string) (*MCPTools, func(), error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "day17-agent", Version: "0.1.0"}, nil)
	cmd := exec.CommandContext(ctx, serverCmd[0], serverCmd[1:]...)

	// Лог сервера (в т.ч. фоновые сборщики дня 18) — в файл, чтобы не засорять REPL.
	var logFile *os.File
	if f, err := os.Create(filepath.Join(sourceDir(), "weatherserver", "server.log")); err == nil {
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
		return nil, func() {}, fmt.Errorf("mcp connect: %w", err)
	}
	lt, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, func() {}, fmt.Errorf("mcp tools/list: %w", err)
	}
	stop := func() {
		_ = session.Close()
		if logFile != nil {
			_ = logFile.Close()
		}
	}
	return &MCPTools{session: session, tools: toAnthropicTools(lt.Tools)}, stop, nil
}

func (m *MCPTools) Tools() []anthropic.ToolUnionParam { return m.tools }
func (m *MCPTools) Len() int                          { return len(m.tools) }

// Names — имена доступных инструментов (для стартового вывода).
func (m *MCPTools) Names() []string {
	out := make([]string, 0, len(m.tools))
	for _, t := range m.tools {
		if t.OfTool != nil {
			out = append(out, t.OfTool.Name)
		}
	}
	return out
}

// Call вызывает MCP-инструмент по имени с сырыми аргументами модели.
// Возвращает текст результата и флаг ошибки (для tool_result.is_error).
func (m *MCPTools) Call(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "не разобрал аргументы инструмента: " + err.Error(), true
		}
	}
	res, err := m.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Sprintf("ошибка вызова %s: %v", name, err), true
	}
	return mcpResultText(res), res.IsError
}

// toAnthropicTools конвертирует MCP-инструменты в формат tools Anthropic.
func toAnthropicTools(ts []*mcp.Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(ts))
	for _, t := range ts {
		props, required := splitSchema(t.InputSchema)
		tp := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: props,
				Required:   required,
			},
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out
}

// splitSchema достаёт properties и required из JSON-схемы MCP-инструмента.
// InputSchema у тула может прийти как map[string]any или как json.RawMessage.
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
		props = map[string]any{} // схема без properties (напр. {"type":"object"})
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
