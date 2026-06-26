package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// День 19. Композиция инструментов: пайплайн из трёх тулов на ОДНОМ сервере.
//   weather_collect (поиск/получение) → make_report (обработка) → save_to_file (сохранение)
// Цепочку оркеструет LLM по запросу пользователя (без хардкода в агенте). Данные
// передаются между тулами: collect отдаёт {cities:[...]}, report принимает cities и
// отдаёт текст, save принимает content. Сервер LLM не вызывает — «report» это итог в коде.

// cityWeather — единая схема данных, которую отдаёт collect и принимает report.
// Общий тип гарантирует корректность передачи данных между тулами.
type cityWeather struct {
	Name    string  `json:"name"`
	Country string  `json:"country"`
	Temp    float64 `json:"temp"`
	Wind    float64 `json:"wind"`
	Code    int     `json:"code"`
	Desc    string  `json:"desc"`
}

func registerPipelineTools(s *mcp.Server) {
	// 1) ПОИСК/ПОЛУЧЕНИЕ: собрать текущую погоду по списку городов → {cities:[...]}.
	s.AddTool(&mcp.Tool{
		Name: "weather_collect",
		Description: "Шаг 1 пайплайна. Собирает текущую погоду по списку городов и возвращает " +
			"JSON {\"cities\":[...]}. Этот результат передаётся в make_report.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"locations":{"type":"array","items":{"type":"string"},"description":"Города, напр. [\"Берлин\",\"София\"]"},
				"units":{"type":"string","enum":["celsius","fahrenheit"],"default":"celsius"}
			},
			"required":["locations"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Locations []string `json:"locations"`
			Units     string   `json:"units"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errResult(err), nil
		}
		if in.Units == "" {
			in.Units = "celsius"
		}
		cities := make([]cityWeather, 0, len(in.Locations))
		for _, loc := range in.Locations {
			places, err := geocode(ctx, loc, 1)
			if err != nil || len(places) == 0 {
				continue // пропускаем ненайденные, не роняя весь пайплайн
			}
			p := places[0]
			w, err := currentWeather(ctx, p.Lat, p.Lon, in.Units)
			if err != nil {
				continue
			}
			cities = append(cities, cityWeather{
				Name: p.Name, Country: p.Country,
				Temp: w.Temp, Wind: w.Wind, Code: w.Code, Desc: weatherCodeText(w.Code),
			})
		}
		out, _ := json.Marshal(struct {
			Cities []cityWeather `json:"cities"`
		}{cities})
		return textResult(string(out)), nil
	})

	// 2) ОБРАБОТКА: из собранных данных сделать текстовый отчёт (итог). Без LLM.
	s.AddTool(&mcp.Tool{
		Name: "make_report",
		Description: "Шаг 2 пайплайна. Принимает cities (из weather_collect) и формирует " +
			"текстовый отчёт: рейтинг по температуре, экстремумы, среднее. Результат передаётся в save_to_file.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"cities":{"type":"array","description":"Массив из weather_collect (поле cities)",
					"items":{"type":"object","properties":{
						"name":{"type":"string"},"country":{"type":"string"},
						"temp":{"type":"number"},"wind":{"type":"number"},
						"code":{"type":"integer"},"desc":{"type":"string"}}}},
				"title":{"type":"string","description":"Заголовок отчёта (необязательно)"}
			},
			"required":["cities"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Cities []cityWeather `json:"cities"`
			Title  string        `json:"title"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errResult(err), nil
		}
		if len(in.Cities) == 0 {
			return textResult("нет данных для отчёта"), nil
		}
		return textResult(buildReport(in.Cities, in.Title)), nil
	})

	// 3) СОХРАНЕНИЕ: записать переданный текст в файл (в каталоге reports/).
	s.AddTool(&mcp.Tool{
		Name: "save_to_file",
		Description: "Шаг 3 пайплайна. Сохраняет переданный текст (например, отчёт из make_report) " +
			"в файл в каталоге reports/. Возвращает путь к файлу.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filename":{"type":"string","description":"Имя файла, напр. 'weather.txt'"},
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
			return errResult(err), nil
		}
		path, err := saveReport(in.Filename, in.Content)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(fmt.Sprintf("сохранено: %s (%d байт)", path, len(in.Content))), nil
	})
}

func buildReport(cities []cityWeather, title string) string {
	sorted := make([]cityWeather, len(cities))
	copy(sorted, cities)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Temp > sorted[j].Temp })

	var b strings.Builder
	if title == "" {
		title = "Погодный отчёт"
	}
	fmt.Fprintf(&b, "%s\n%s\n\n", title, strings.Repeat("=", len([]rune(title))))
	fmt.Fprintf(&b, "Городов: %d\n\n", len(sorted))

	sum := 0.0
	for i, c := range sorted {
		fmt.Fprintf(&b, "%d. %s, %s — %.1f°C, ветер %.0f км/ч, %s\n",
			i+1, c.Name, c.Country, c.Temp, c.Wind, c.Desc)
		sum += c.Temp
	}
	warm, cold := sorted[0], sorted[len(sorted)-1]
	fmt.Fprintf(&b, "\nТеплее всего: %s (%.1f°C) · холоднее всего: %s (%.1f°C) · средняя %.1f°C\n",
		warm.Name, warm.Temp, cold.Name, cold.Temp, sum/float64(len(sorted)))
	return b.String()
}

// saveReport безопасно пишет файл в каталог reports/ рядом с сервером.
// Имя файла очищается от путей (защита от traversal).
func saveReport(filename, content string) (string, error) {
	name := filepath.Base(strings.TrimSpace(filename))
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("недопустимое имя файла: %q", filename)
	}
	dir := filepath.Join(sourceDir(), "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
