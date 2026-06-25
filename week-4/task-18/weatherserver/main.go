package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sourceDir — каталог этого файла (рядом кладём weather-store.json).
func sourceDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(file)
	}
	return "."
}

// День 17. Свой MCP-сервер вокруг реального API (open-meteo, без ключа).
// Регистрирует два инструмента, описывает их входные параметры (JSON Schema)
// и возвращает результат. Транспорт stdio — запускается как сабпроцесс клиента/агента.
//
// API open-meteo:
//   geocoding: https://geocoding-api.open-meteo.com/v1/search?name=Berlin&count=1
//   forecast:  https://api.open-meteo.com/v1/forecast?latitude=..&longitude=..&current=...

const userAgent = "mcp-weather/0.1 (ai-challenge day17)"

func main() {
	// День 18: персистентное хранилище рядом с сервером + контекст для фоновых сборщиков.
	store := loadStore(filepath.Join(sourceDir(), "weather-store.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Возобновляем периодический сбор по уже отслеживаемым городам (после рестарта).
	for _, t := range store.List() {
		startCollector(ctx, store, t)
		log.Printf("[collector] возобновлён сбор: %s каждые %d с", t.Name, t.IntervalSec)
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "weather-mcp", Version: "0.2.0"}, nil)

	// --- Инструмент 1: geocode (название -> координаты) ---
	s.AddTool(&mcp.Tool{
		Name:        "geocode",
		Description: "Найти координаты населённого пункта по названию (open-meteo geocoding).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"name":{"type":"string","description":"Название города, напр. 'Берлин' или 'Sofia'"},
				"count":{"type":"integer","description":"Сколько результатов вернуть (1-10)","default":1}
			},
			"required":["name"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errResult(fmt.Errorf("не разобрал аргументы: %w", err)), nil
		}
		if in.Count <= 0 {
			in.Count = 1
		}
		places, err := geocode(ctx, in.Name, in.Count)
		if err != nil {
			return errResult(err), nil
		}
		if len(places) == 0 {
			return textResult("ничего не найдено по запросу: " + in.Name), nil
		}
		var b strings.Builder
		for _, p := range places {
			fmt.Fprintf(&b, "%s, %s — lat=%.4f lon=%.4f\n", p.Name, p.Country, p.Lat, p.Lon)
		}
		return textResult(strings.TrimRight(b.String(), "\n")), nil
	})

	// --- Инструмент 2: weather_current (город -> текущая погода) ---
	s.AddTool(&mcp.Tool{
		Name:        "weather_current",
		Description: "Текущая погода в городе: геокодит название и берёт текущие показатели open-meteo.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"location":{"type":"string","description":"Город, напр. 'Берлин' или 'Sofia'"},
				"units":{"type":"string","enum":["celsius","fahrenheit"],"description":"Единицы температуры","default":"celsius"}
			},
			"required":["location"]
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Location string `json:"location"`
			Units    string `json:"units"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errResult(fmt.Errorf("не разобрал аргументы: %w", err)), nil
		}
		if in.Units == "" {
			in.Units = "celsius"
		}
		places, err := geocode(ctx, in.Location, 1)
		if err != nil {
			return errResult(err), nil
		}
		if len(places) == 0 {
			return textResult("город не найден: " + in.Location), nil
		}
		p := places[0]
		w, err := currentWeather(ctx, p.Lat, p.Lon, in.Units)
		if err != nil {
			return errResult(err), nil
		}
		deg := "°C"
		if in.Units == "fahrenheit" {
			deg = "°F"
		}
		out := fmt.Sprintf("%s, %s: %.1f%s, ветер %.0f км/ч, %s",
			p.Name, p.Country, w.Temp, deg, w.Wind, weatherCodeText(w.Code))
		return textResult(out), nil
	})

	// День 18: тулы планировщика/фоновых задач.
	registerScheduledTools(ctx, s, store)

	// Run обслуживает stdio до закрытия соединения клиентом.
	if err := s.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// ---- день 18: планировщик и фоновые задачи ----

// startCollector запускает фоновую горутину, которая собирает погоду по городу
// сразу и далее каждые IntervalSec секунд, складывая наблюдения в store.
// Останавливается по отмене ctx (закрытие сервера). Токены LLM не тратятся.
func startCollector(ctx context.Context, store *Store, t *Tracked) {
	go func() {
		collectOnce(ctx, store, t)
		iv := t.IntervalSec
		if iv <= 0 {
			iv = 60
		}
		ticker := time.NewTicker(time.Duration(iv) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				collectOnce(ctx, store, t)
			}
		}
	}()
}

func collectOnce(ctx context.Context, store *Store, t *Tracked) {
	w, err := currentWeather(ctx, t.Lat, t.Lon, "celsius")
	if err != nil {
		log.Printf("[collector] %s: ошибка сбора: %v", t.Name, err)
		return
	}
	store.Append(t.Location, Sample{Time: time.Now(), Temp: w.Temp, Wind: w.Wind, Code: w.Code})
	log.Printf("[collector] %s: %.1f°C записано", t.Name, w.Temp)
}

// registerScheduledTools регистрирует тулы дня 18 на сервере.
func registerScheduledTools(ctx context.Context, s *mcp.Server, store *Store) {
	// track_location — отложенно/периодически собирать погоду по городу.
	s.AddTool(&mcp.Tool{
		Name:        "track_location",
		Description: "Начать периодический фоновый сбор погоды по городу (сохраняется в JSON). Возвращает подтверждение.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"location":{"type":"string","description":"Город, напр. 'Берлин'"},
				"interval_sec":{"type":"integer","description":"Период сбора в секундах (мин. 5)","default":60}
			},
			"required":["location"]
		}`),
	}, func(ctx2 context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Location    string `json:"location"`
			IntervalSec int    `json:"interval_sec"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return errResult(err), nil
		}
		if in.IntervalSec < 5 {
			in.IntervalSec = 60
		}
		places, err := geocode(ctx2, in.Location, 1)
		if err != nil {
			return errResult(err), nil
		}
		if len(places) == 0 {
			return textResult("город не найден: " + in.Location), nil
		}
		p := places[0]
		t := &Tracked{
			Location: in.Location, Name: p.Name, Country: p.Country,
			Lat: p.Lat, Lon: p.Lon, IntervalSec: in.IntervalSec,
		}
		store.Upsert(t)
		startCollector(ctx, store, t) // ctx сервера, а не запроса — живёт после ответа
		return textResult(fmt.Sprintf("слежу за %s, %s — сбор каждые %d с (данные в weather-store.json)",
			p.Name, p.Country, in.IntervalSec)), nil
	})

	// weather_summary — агрегированный результат по накопленным наблюдениям.
	s.AddTool(&mcp.Tool{
		Name:        "weather_summary",
		Description: "Агрегированная сводка по собранным наблюдениям (кол-во, период, min/avg/max температуры). Без location — по всем городам.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"location":{"type":"string","description":"Город; пусто = по всем отслеживаемым"}
			}
		}`),
	}, func(ctx2 context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Location string `json:"location"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return textResult(store.Summary(in.Location)), nil
	})

	// list_tracked — что сейчас отслеживается.
	s.AddTool(&mcp.Tool{
		Name:        "list_tracked",
		Description: "Список городов, по которым идёт фоновый сбор, и их интервалы.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx2 context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ts := store.List()
		if len(ts) == 0 {
			return textResult("ничего не отслеживается"), nil
		}
		var b strings.Builder
		for _, t := range ts {
			fmt.Fprintf(&b, "%s, %s — каждые %d с, наблюдений: %d\n", t.Name, t.Country, t.IntervalSec, len(t.Samples))
		}
		return textResult(strings.TrimRight(b.String(), "\n")), nil
	})
}

// ---- open-meteo ----

type place struct {
	Name    string
	Country string
	Lat     float64
	Lon     float64
}

func geocode(ctx context.Context, name string, count int) ([]place, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("count", strconv.Itoa(count))
	q.Set("language", "ru")
	q.Set("format", "json")
	u := "https://geocoding-api.open-meteo.com/v1/search?" + q.Encode()

	var resp struct {
		Results []struct {
			Name      string  `json:"name"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := getJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	out := make([]place, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, place{Name: r.Name, Country: r.Country, Lat: r.Latitude, Lon: r.Longitude})
	}
	return out, nil
}

type weather struct {
	Temp float64
	Wind float64
	Code int
}

func currentWeather(ctx context.Context, lat, lon float64, units string) (weather, error) {
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(lat, 'f', 4, 64))
	q.Set("longitude", strconv.FormatFloat(lon, 'f', 4, 64))
	q.Set("current", "temperature_2m,wind_speed_10m,weather_code")
	q.Set("temperature_unit", units)
	u := "https://api.open-meteo.com/v1/forecast?" + q.Encode()

	var resp struct {
		Current struct {
			Temp float64 `json:"temperature_2m"`
			Wind float64 `json:"wind_speed_10m"`
			Code int     `json:"weather_code"`
		} `json:"current"`
	}
	if err := getJSON(ctx, u, &resp); err != nil {
		return weather{}, err
	}
	return weather{Temp: resp.Current.Temp, Wind: resp.Current.Wind, Code: resp.Current.Code}, nil
}

func getJSON(ctx context.Context, u string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("open-meteo вернул %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(res.Body).Decode(dst)
}

// weatherCodeText — человекочитаемая расшифровка WMO weather_code.
func weatherCodeText(code int) string {
	switch {
	case code == 0:
		return "ясно"
	case code <= 3:
		return "переменная облачность"
	case code == 45 || code == 48:
		return "туман"
	case code >= 51 && code <= 57:
		return "морось"
	case code >= 61 && code <= 67:
		return "дождь"
	case code >= 71 && code <= 77:
		return "снег"
	case code >= 80 && code <= 82:
		return "ливень"
	case code >= 95:
		return "гроза"
	default:
		return fmt.Sprintf("код погоды %d", code)
	}
}

// ---- хелперы результата ----

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "ошибка: " + err.Error()}},
	}
}
