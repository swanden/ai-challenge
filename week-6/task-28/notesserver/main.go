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

// День 20. Второй MCP-сервер ДРУГОЙ природы — файловое хранилище ("notes").
// Не знает ничего про погоду: принимает текст и работает с файлами в каталоге notes/.
// Демонстрирует оркестрацию тулов с РАЗНЫХ серверов: данные приходят с weatherserver,
// сохранение/чтение — здесь. Транспорт stdio (отдельный сабпроцесс агента).
func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "notes-mcp", Version: "0.1.0"}, nil)

	// save_to_file — сохранить переданный текст в файл.
	s.AddTool(&mcp.Tool{
		Name:        "save_to_file",
		Description: "Сохранить текст в файл в каталоге notes/. Возвращает путь к файлу.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filename":{"type":"string","description":"Имя файла, напр. 'trip.txt'"},
				"content":{"type":"string","description":"Содержимое для сохранения"}
			},
			"required":["filename","content"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Filename string `json:"filename"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errText(err), nil
		}
		path, err := writeNote(in.Filename, in.Content)
		if err != nil {
			return errText(err), nil
		}
		return okText(fmt.Sprintf("сохранено: %s (%d байт)", path, len(in.Content))), nil
	})

	// read_file — прочитать ранее сохранённый файл.
	s.AddTool(&mcp.Tool{
		Name:        "read_file",
		Description: "Прочитать файл из каталога notes/ и вернуть его содержимое.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"filename":{"type":"string","description":"Имя файла"}},
			"required":["filename"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errText(err), nil
		}
		data, err := readNote(in.Filename)
		if err != nil {
			return errText(err), nil
		}
		return okText(data), nil
	})

	// list_files — список сохранённых файлов.
	s.AddTool(&mcp.Tool{
		Name:        "list_files",
		Description: "Список файлов в каталоге notes/.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		names, err := listNotes()
		if err != nil {
			return errText(err), nil
		}
		if len(names) == 0 {
			return okText("файлов пока нет"), nil
		}
		return okText(strings.Join(names, "\n")), nil
	})

	// delete_file — удалить файл.
	s.AddTool(&mcp.Tool{
		Name:        "delete_file",
		Description: "Удалить файл из каталога notes/.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"filename":{"type":"string","description":"Имя файла"}},
			"required":["filename"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errText(err), nil
		}
		if err := deleteNote(in.Filename); err != nil {
			return errText(err), nil
		}
		return okText("удалено: " + in.Filename), nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// ---- работа с файлами (каталог notes/ рядом с сервером) ----

func notesDir() (string, error) {
	dir := filepath.Join(sourceDir(), "notes")
	return dir, os.MkdirAll(dir, 0o755)
}

// safeName очищает имя файла от путей (защита от traversal).
func safeName(filename string) (string, error) {
	name := filepath.Base(strings.TrimSpace(filename))
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("недопустимое имя файла: %q", filename)
	}
	return name, nil
}

func writeNote(filename, content string) (string, error) {
	name, err := safeName(filename)
	if err != nil {
		return "", err
	}
	dir, err := notesDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func readNote(filename string) (string, error) {
	name, err := safeName(filename)
	if err != nil {
		return "", err
	}
	dir, err := notesDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("не прочитать %s: %w", name, err)
	}
	return string(data), nil
}

func listNotes() ([]string, error) {
	dir, err := notesDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func deleteNote(filename string) error {
	name, err := safeName(filename)
	if err != nil {
		return err
	}
	dir, err := notesDir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, name))
}

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
