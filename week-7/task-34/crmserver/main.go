package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// День 33. ЧЕТВЁРТЫЙ MCP-сервер — CRM поддержки ("crm").
//
// По образцу notesserver (день 20) и gitserver (день 31): отдельный процесс,
// транспорт stdio, только чтение. Отдаёт данные тикетов и пользователей
// вымышленного продукта CloudNote из JSON-файлов (support/crm/). Это тот самый
// «контекст пользователя или тикета», который ассистент поддержки подмешивает к
// ответу по FAQ (аналог git-контекста в дне 31, только вместо ветки — тикет).
//
// Только чтение: сервер не умеет менять тикеты. Как и на дне 31, это гарантия
// по построению, а не по обещанию — кода записи здесь нет.

// Ticket — обращение пользователя.
type Ticket struct {
	ID       string   `json:"id"`
	UserID   string   `json:"user_id"`
	Subject  string   `json:"subject"`
	Status   string   `json:"status"`
	Priority string   `json:"priority"`
	Created  string   `json:"created_at"`
	Body     string   `json:"body"`
	Tags     []string `json:"tags"`
}

// User — клиент продукта.
type User struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Plan      string  `json:"plan"`
	Role      string  `json:"role,omitempty"`
	Devices   int     `json:"devices,omitempty"`
	TeamSize  int     `json:"team_size,omitempty"`
	StorageGB float64 `json:"storage_used_gb"`
	SSO       string  `json:"sso"`
	TwoFactor bool    `json:"two_factor,omitempty"`
	Created   string  `json:"created_at"`
	Notes     string  `json:"notes"`
}

var (
	tickets []Ticket
	users   []User
)

func main() {
	dir := crmDir()
	if err := load(dir); err != nil {
		// Не фаталим: сервер поднимается и честно отвечает ошибкой на вызов,
		// иначе агент решит, что весь MCP лёг, и потеряет остальные серверы.
		log.Printf("crm-mcp: не загрузил данные из %s: %v", dir, err)
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "crm-mcp", Version: "0.1.0"}, nil)

	// get_ticket — тикет по ID вместе с данными его автора.
	s.AddTool(&mcp.Tool{
		Name: "get_ticket",
		Description: "Получить тикет поддержки по ID (напр. T-5001) вместе с данными пользователя, " +
			"который его создал (тариф, хранилище, SSO). Это основной контекст для ответа.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"id":{"type":"string","description":"ID тикета, напр. T-5001"}},
			"required":["id"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errText(err), nil
		}
		t := findTicket(in.ID)
		if t == nil {
			return errText(fmt.Errorf("тикет %q не найден", in.ID)), nil
		}
		return okText(renderTicket(*t, findUser(t.UserID))), nil
	})

	// get_user — пользователь по ID или email.
	s.AddTool(&mcp.Tool{
		Name:        "get_user",
		Description: "Получить пользователя по ID (u-1001) или email. Тариф, устройства, хранилище, SSO, 2FA.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"id":{"type":"string","description":"ID (u-1001) или email пользователя"}},
			"required":["id"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errText(err), nil
		}
		u := findUser(in.ID)
		if u == nil {
			u = findUserByEmail(in.ID)
		}
		if u == nil {
			return errText(fmt.Errorf("пользователь %q не найден", in.ID)), nil
		}
		return okText(renderUser(*u)), nil
	})

	// list_tickets — список тикетов, опционально по статусу.
	s.AddTool(&mcp.Tool{
		Name:        "list_tickets",
		Description: "Список тикетов поддержки. Можно отфильтровать по статусу (open/pending/closed).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"status":{"type":"string","description":"Фильтр по статусу: open, pending, closed. Пусто = все"}}
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Status string `json:"status"`
		}
		if len(req.Params.Arguments) > 0 {
			_ = json.Unmarshal(req.Params.Arguments, &in)
		}
		return okText(listTickets(in.Status)), nil
	})

	// search_tickets — поиск по подстроке в теме/теле/тегах.
	s.AddTool(&mcp.Tool{
		Name:        "search_tickets",
		Description: "Поиск тикетов по подстроке в теме, тексте или тегах (напр. 'синхронизация', 'auth').",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"query":{"type":"string","description":"Поисковая строка"}},
			"required":["query"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errText(err), nil
		}
		return okText(searchTickets(in.Query)), nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// ── данные ───────────────────────────────────────────────────────────────────

func load(dir string) error {
	tb, err := os.ReadFile(filepath.Join(dir, "tickets.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(tb, &tickets); err != nil {
		return fmt.Errorf("tickets.json: %w", err)
	}
	ub, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(ub, &users); err != nil {
		return fmt.Errorf("users.json: %w", err)
	}
	return nil
}

// crmDir ищет каталог support/crm рядом с сервером; переопределяется через CRM_DIR.
func crmDir() string {
	if v := os.Getenv("CRM_DIR"); v != "" {
		return v
	}
	// crmserver/ лежит внутри task-33, данные — в task-33/support/crm.
	return filepath.Join(filepath.Dir(sourceDir()), "support", "crm")
}

func findTicket(id string) *Ticket {
	for i := range tickets {
		if strings.EqualFold(tickets[i].ID, id) {
			return &tickets[i]
		}
	}
	return nil
}

func findUser(id string) *User {
	for i := range users {
		if strings.EqualFold(users[i].ID, id) {
			return &users[i]
		}
	}
	return nil
}

func findUserByEmail(email string) *User {
	for i := range users {
		if strings.EqualFold(users[i].Email, email) {
			return &users[i]
		}
	}
	return nil
}

// ── рендер (человекочитаемо для промпта модели) ──────────────────────────────

func renderTicket(t Ticket, u *User) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ТИКЕТ %s [%s, приоритет: %s]\n", t.ID, t.Status, t.Priority)
	fmt.Fprintf(&b, "Тема: %s\n", t.Subject)
	fmt.Fprintf(&b, "Создан: %s · Теги: %s\n", t.Created, strings.Join(t.Tags, ", "))
	fmt.Fprintf(&b, "Текст обращения:\n%s\n", t.Body)
	if u != nil {
		fmt.Fprintf(&b, "\nАВТОР ТИКЕТА:\n%s", renderUser(*u))
	}
	return b.String()
}

func renderUser(u User) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <%s> [%s]\n", u.Name, u.Email, u.ID)
	fmt.Fprintf(&b, "Тариф: %s", u.Plan)
	if u.Role != "" {
		fmt.Fprintf(&b, " (%s)", u.Role)
	}
	fmt.Fprintf(&b, " · Хранилище: %.1f ГБ · SSO: %s", u.StorageGB, u.SSO)
	if u.Plan != "team" && u.Devices > 0 {
		fmt.Fprintf(&b, " · Устройств: %d", u.Devices)
	}
	if u.TeamSize > 0 {
		fmt.Fprintf(&b, " · Размер команды: %d", u.TeamSize)
	}
	if u.TwoFactor {
		b.WriteString(" · 2FA включена")
	}
	if u.Notes != "" {
		fmt.Fprintf(&b, "\nЗаметка CRM: %s", u.Notes)
	}
	return b.String()
}

func listTickets(status string) string {
	var out []Ticket
	for _, t := range tickets {
		if status == "" || strings.EqualFold(t.Status, status) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return "тикетов не найдено"
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	var b strings.Builder
	fmt.Fprintf(&b, "тикетов: %d\n", len(out))
	for _, t := range out {
		fmt.Fprintf(&b, "  %s [%s/%s] %s — %s\n", t.ID, t.Status, t.Priority, userName(t.UserID), t.Subject)
	}
	return b.String()
}

func searchTickets(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return "пустой запрос"
	}
	var out []Ticket
	for _, t := range tickets {
		hay := strings.ToLower(t.Subject + " " + t.Body + " " + strings.Join(t.Tags, " "))
		if strings.Contains(hay, q) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("по запросу %q тикетов не найдено", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "найдено тикетов: %d\n", len(out))
	for _, t := range out {
		fmt.Fprintf(&b, "  %s [%s] %s — %s\n", t.ID, t.Status, userName(t.UserID), t.Subject)
	}
	return b.String()
}

func userName(id string) string {
	if u := findUser(id); u != nil {
		return u.Name
	}
	return id
}

// ── mcp helpers (как в notesserver/gitserver) ────────────────────────────────

func sourceDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(file)
	}
	return "."
}

func okText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errText(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "ошибка: " + err.Error()}}}
}
