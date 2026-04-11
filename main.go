package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPTimeout    = 15 * time.Second
	defaultCacheTTL       = 3600 * time.Second
	defaultRequestDelay   = 500 * time.Millisecond
	defaultMaxCacheSize   = 50 * 1024 * 1024
	defaultRetryAttempts  = 2
	defaultGeocodeTimeout = 10 * time.Second
	defaultCacheDir       = ".weather_cache"
)

type WeatherData struct {
	Temperature   float64
	FeelsLike     float64
	Humidity      float64
	Pressure      float64
	WindSpeed     float64
	WindDirection float64
	Description   string
	Source        string
	City          string
}

type Coordinates struct {
	Latitude  float64
	Longitude float64
	City      string
}

type WeatherAPIConfig struct {
	Timeout       time.Duration
	RetryAttempts int
	CacheTTL      time.Duration
	RequestDelay  time.Duration
	MaxCacheSize  int64
}

type WeatherProvider interface {
	GetWeather(ctx context.Context) (*WeatherData, error)
	Name() string
}

type FreeWeatherAPI struct {
	City          string
	Latitude      float64
	Longitude     float64
	EnableCache   bool
	Config        WeatherAPIConfig
	WeatherAPIKey string
	CacheDir      string
	Client        *http.Client
	providers     []WeatherProvider
	cacheSize     int64
	cacheMu       sync.Mutex
	metrics       *Metrics
	logger        *log.Logger
}

type Metrics struct {
	mu        sync.RWMutex
	requests  map[string]int
	errors    map[string]int
	cacheHits map[string]int
	cacheMiss map[string]int
	startTime time.Time
}

type MetricsSummary struct {
	Uptime       string
	Requests     map[string]int
	Errors       map[string]int
	CacheHits    map[string]int
	CacheMisses  map[string]int
	TotalRequests int
	TotalErrors   int
}

type OpenMeteoProvider struct {
	api      *FreeWeatherAPI
	codes    map[int]string
	timezone string
}

type WeatherAPIProvider struct {
	api *FreeWeatherAPI
}

type WttrInProvider struct {
	api *FreeWeatherAPI
}

type OpenMeteoResponse struct {
	Current struct {
		Temperature2M      float64 `json:"temperature_2m"`
		RelativeHumidity2M float64 `json:"relative_humidity_2m"`
		ApparentTemperature float64 `json:"apparent_temperature"`
		WeatherCode        int     `json:"weather_code"`
		PressureMSL        float64 `json:"pressure_msl"`
		WindSpeed10M       float64 `json:"wind_speed_10m"`
		WindDirection10M   float64 `json:"wind_direction_10m"`
	} `json:"current"`
}

type WeatherAPIResponse struct {
	Current struct {
		TempC      float64 `json:"temp_c"`
		FeelsLikeC float64 `json:"feelslike_c"`
		Humidity   float64 `json:"humidity"`
		PressureMB float64 `json:"pressure_mb"`
		WindKPH    float64 `json:"wind_kph"`
		WindDegree float64 `json:"wind_degree"`
		Condition  struct {
			Text string `json:"text"`
		} `json:"condition"`
	} `json:"current"`
}

type WttrInResponse struct {
	CurrentCondition []struct {
		TempC         string `json:"temp_C"`
		FeelsLikeC    string `json:"FeelsLikeC"`
		Humidity      string `json:"humidity"`
		Pressure      string `json:"pressure"`
		WindspeedKmph string `json:"windspeedKmph"`
		WinddirDegree string `json:"winddirDegree"`
		WeatherDesc   []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
	} `json:"current_condition"`
}

type NominatimResponse []struct {
	Lat         string `json:"lat"`
	Lon         string `json:"lon"`
	DisplayName string `json:"display_name"`
}

func NewMetrics() *Metrics {
	return &Metrics{
		requests:  make(map[string]int),
		errors:    make(map[string]int),
		cacheHits: make(map[string]int),
		cacheMiss: make(map[string]int),
		startTime: time.Now(),
	}
}

func (m *Metrics) IncrementRequests(provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[provider]++
}

func (m *Metrics) IncrementErrors(provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors[provider]++
}

func (m *Metrics) IncrementCacheHits(provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheHits[provider]++
}

func (m *Metrics) IncrementCacheMiss(provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheMiss[provider]++
}

func copyIntMap(src map[string]int) map[string]int {
	dst := make(map[string]int, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (m *Metrics) GetSummary() MetricsSummary {
	m.mu.RLock()
	defer m.mu.RUnlock()

	totalRequests := 0
	totalErrors := 0

	for _, count := range m.requests {
		totalRequests += count
	}

	for _, count := range m.errors {
		totalErrors += count
	}

	return MetricsSummary{
		Uptime:        time.Since(m.startTime).String(),
		Requests:      copyIntMap(m.requests),
		Errors:        copyIntMap(m.errors),
		CacheHits:     copyIntMap(m.cacheHits),
		CacheMisses:   copyIntMap(m.cacheMiss),
		TotalRequests: totalRequests,
		TotalErrors:   totalErrors,
	}
}

func NewOpenMeteoProvider(api *FreeWeatherAPI, timezone string) *OpenMeteoProvider {
	codes := map[int]string{
		0:  "Clear sky",
		1:  "Mainly clear",
		2:  "Partly cloudy",
		3:  "Overcast",
		45: "Fog",
		48: "Depositing rime fog",
		51: "Light drizzle",
		53: "Moderate drizzle",
		55: "Dense drizzle",
		56: "Light freezing drizzle",
		57: "Dense freezing drizzle",
		61: "Slight rain",
		63: "Moderate rain",
		65: "Heavy rain",
		66: "Light freezing rain",
		67: "Heavy freezing rain",
		71: "Slight snow fall",
		73: "Moderate snow fall",
		75: "Heavy snow fall",
		77: "Snow grains",
		80: "Slight rain showers",
		81: "Moderate rain showers",
		82: "Violent rain showers",
		85: "Slight snow showers",
		86: "Heavy snow showers",
		95: "Thunderstorm",
		96: "Thunderstorm with slight hail",
		99: "Thunderstorm with heavy hail",
	}

	return &OpenMeteoProvider{
		api:      api,
		codes:    codes,
		timezone: timezone,
	}
}

func (p *OpenMeteoProvider) Name() string {
	return "Open-Meteo"
}

func (p *OpenMeteoProvider) GetWeather(ctx context.Context) (*WeatherData, error) {
	p.api.metrics.IncrementRequests(p.Name())

	apiURL := "https://api.open-meteo.com/v1/forecast"
	params := map[string]string{
		"latitude":  strconv.FormatFloat(p.api.Latitude, 'f', 6, 64),
		"longitude": strconv.FormatFloat(p.api.Longitude, 'f', 6, 64),
		"current":   "temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,pressure_msl,wind_speed_10m,wind_direction_10m",
		"timezone":  p.timezone,
	}

	data, err := p.api.makeRequest(ctx, apiURL, params, p.Name())
	if err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, err
	}

	var result OpenMeteoResponse
	if err := json.Unmarshal(data, &result); err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse Open-Meteo response: %w", err)
	}

	description := p.codes[result.Current.WeatherCode]
	if description == "" {
		description = "Unknown"
	}

	weatherData := &WeatherData{
		Temperature:   result.Current.Temperature2M,
		FeelsLike:     result.Current.ApparentTemperature,
		Humidity:      result.Current.RelativeHumidity2M,
		Pressure:      result.Current.PressureMSL,
		WindSpeed:     result.Current.WindSpeed10M,
		WindDirection: result.Current.WindDirection10M,
		Description:   description,
		Source:        p.Name(),
		City:          p.api.City,
	}

	if p.api.validateWeatherData(*weatherData) {
		return weatherData, nil
	}

	p.api.metrics.IncrementErrors(p.Name())
	return nil, fmt.Errorf("invalid weather data from Open-Meteo")
}

func NewWeatherAPIProvider(api *FreeWeatherAPI) *WeatherAPIProvider {
	return &WeatherAPIProvider{api: api}
}

func (p *WeatherAPIProvider) Name() string {
	return "WeatherAPI"
}

func (p *WeatherAPIProvider) GetWeather(ctx context.Context) (*WeatherData, error) {
	p.api.metrics.IncrementRequests(p.Name())

	if p.api.WeatherAPIKey == "" {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("WeatherAPI key not configured")
	}

	apiURL := "https://api.weatherapi.com/v1/current.json"
	params := map[string]string{
		"key": p.api.WeatherAPIKey,
		"q":   p.api.City,
		"aqi": "no",
	}

	data, err := p.api.makeRequest(ctx, apiURL, params, p.Name())
	if err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, err
	}

	var result WeatherAPIResponse
	if err := json.Unmarshal(data, &result); err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse WeatherAPI response: %w", err)
	}

	description := result.Current.Condition.Text
	if description == "" {
		description = "Unknown"
	}

	weatherData := &WeatherData{
		Temperature:   result.Current.TempC,
		FeelsLike:     result.Current.FeelsLikeC,
		Humidity:      result.Current.Humidity,
		Pressure:      result.Current.PressureMB,
		WindSpeed:     result.Current.WindKPH * 0.277778,
		WindDirection: result.Current.WindDegree,
		Description:   description,
		Source:        p.Name(),
		City:          p.api.City,
	}

	if p.api.validateWeatherData(*weatherData) {
		return weatherData, nil
	}

	p.api.metrics.IncrementErrors(p.Name())
	return nil, fmt.Errorf("invalid weather data from WeatherAPI")
}

func NewWttrInProvider(api *FreeWeatherAPI) *WttrInProvider {
	return &WttrInProvider{api: api}
}

func (p *WttrInProvider) Name() string {
	return "wttr.in"
}

func parseStringFloat(value string) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return parsed
}

func (p *WttrInProvider) GetWeather(ctx context.Context) (*WeatherData, error) {
	p.api.metrics.IncrementRequests(p.Name())

	encodedCity := url.PathEscape(p.api.City)
	apiURL := fmt.Sprintf("https://wttr.in/%s", encodedCity)
	params := map[string]string{
		"format": "j1",
	}

	data, err := p.api.makeRequest(ctx, apiURL, params, p.Name())
	if err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, err
	}

	var result WttrInResponse
	if err := json.Unmarshal(data, &result); err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse wttr.in response: %w", err)
	}

	if len(result.CurrentCondition) == 0 {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("invalid response format from wttr.in")
	}

	current := result.CurrentCondition[0]
	description := "Unknown"
	if len(current.WeatherDesc) > 0 && current.WeatherDesc[0].Value != "" {
		description = current.WeatherDesc[0].Value
	}

	weatherData := &WeatherData{
		Temperature:   parseStringFloat(current.TempC),
		FeelsLike:     parseStringFloat(current.FeelsLikeC),
		Humidity:      parseStringFloat(current.Humidity),
		Pressure:      parseStringFloat(current.Pressure),
		WindSpeed:     parseStringFloat(current.WindspeedKmph) * 0.277778,
		WindDirection: parseStringFloat(current.WinddirDegree),
		Description:   description,
		Source:        p.Name(),
		City:          p.api.City,
	}

	if p.api.validateWeatherData(*weatherData) {
		return weatherData, nil
	}

	p.api.metrics.IncrementErrors(p.Name())
	return nil, fmt.Errorf("invalid weather data from wttr.in")
}

func NewFreeWeatherAPI(city string, lat, lon float64, enableCache bool, logger *log.Logger) *FreeWeatherAPI {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	apiKey := os.Getenv("WEATHERAPI_KEY")
	if apiKey == "" {
		logger.Println("WeatherAPI key not found in environment. WeatherAPI provider will be disabled.")
	}

	cacheDir := defaultCacheDir
	if enableCache {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			logger.Printf("Warning: Failed to create cache directory: %v", err)
		}
	}

	api := &FreeWeatherAPI{
		City:          city,
		Latitude:      lat,
		Longitude:     lon,
		EnableCache:   enableCache,
		CacheDir:      cacheDir,
		WeatherAPIKey: apiKey,
		Client: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
		Config: WeatherAPIConfig{
			Timeout:       defaultHTTPTimeout,
			RetryAttempts: defaultRetryAttempts,
			CacheTTL:      defaultCacheTTL,
			RequestDelay:  defaultRequestDelay,
			MaxCacheSize:  defaultMaxCacheSize,
		},
		metrics: NewMetrics(),
		logger:  logger,
	}

	api.AddProvider(NewOpenMeteoProvider(api, "auto"))
	if api.WeatherAPIKey != "" {
		api.AddProvider(NewWeatherAPIProvider(api))
	}
	api.AddProvider(NewWttrInProvider(api))

	return api
}

func (w *FreeWeatherAPI) AddProvider(p WeatherProvider) {
	w.providers = append(w.providers, p)
}

func sortedParamKeys(params map[string]string) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (w *FreeWeatherAPI) getCacheKey(requestURL string, params map[string]string) string {
	u, err := url.Parse(requestURL)
	if err != nil {
		u = &url.URL{Host: "unknown", Path: requestURL}
	}

	var builder strings.Builder
	builder.WriteString("cache|")
	builder.WriteString(u.Scheme)
	builder.WriteString("|")
	builder.WriteString(u.Host)
	builder.WriteString("|")
	builder.WriteString(u.Path)

	for _, k := range sortedParamKeys(params) {
		lowerKey := strings.ToLower(k)
		if lowerKey == "key" || lowerKey == "api_key" || lowerKey == "apikey" {
			continue
		}
		builder.WriteString("|")
		builder.WriteString(k)
		builder.WriteString("=")
		builder.WriteString(params[k])
	}

	hash := sha256.Sum256([]byte(builder.String()))
	return fmt.Sprintf("%x.json", hash[:16])
}

func (w *FreeWeatherAPI) updateCacheSizeLocked() {
	var total int64
	_ = filepath.Walk(w.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	w.cacheSize = total
}

func (w *FreeWeatherAPI) updateCacheSize() {
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	w.updateCacheSizeLocked()
}

func (w *FreeWeatherAPI) enforceCacheLimitLocked() error {
	if w.cacheSize <= w.Config.MaxCacheSize {
		return nil
	}

	files, err := filepath.Glob(filepath.Join(w.CacheDir, "*.json"))
	if err != nil {
		return err
	}

	type fileInfo struct {
		path    string
		modTime time.Time
		size    int64
	}

	fileInfos := make([]fileInfo, 0, len(files))
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		fileInfos = append(fileInfos, fileInfo{
			path:    file,
			modTime: info.ModTime(),
			size:    info.Size(),
		})
	}

	sort.Slice(fileInfos, func(i, j int) bool {
		return fileInfos[i].modTime.Before(fileInfos[j].modTime)
	})

	for w.cacheSize > w.Config.MaxCacheSize && len(fileInfos) > 0 {
		oldest := fileInfos[0]
		if err := os.Remove(oldest.path); err == nil {
			w.cacheSize -= oldest.size
			w.logger.Printf("Removed old cache file: %s", oldest.path)
		}
		fileInfos = fileInfos[1:]
	}

	return nil
}

func (w *FreeWeatherAPI) cacheResponse(requestURL string, params map[string]string, data []byte) error {
	if !w.EnableCache {
		return nil
	}

	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()

	cacheKey := w.getCacheKey(requestURL, params)
	cacheFile := filepath.Join(w.CacheDir, cacheKey)
	tempFile := cacheFile + ".tmp"

	if err := os.WriteFile(tempFile, data, 0o644); err != nil {
		return fmt.Errorf("failed to write temp cache file: %w", err)
	}

	if err := os.Rename(tempFile, cacheFile); err != nil {
		_ = os.Remove(tempFile)
		return fmt.Errorf("failed to rename cache file: %w", err)
	}

	w.updateCacheSizeLocked()
	if err := w.enforceCacheLimitLocked(); err != nil {
		w.logger.Printf("Error enforcing cache limit: %v", err)
	}

	return nil
}

func (w *FreeWeatherAPI) loadCachedResponse(ctx context.Context, requestURL string, params map[string]string, provider string) ([]byte, error) {
	if !w.EnableCache {
		return nil, fmt.Errorf("cache disabled")
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	cacheKey := w.getCacheKey(requestURL, params)
	cacheFile := filepath.Join(w.CacheDir, cacheKey)

	info, err := os.Stat(cacheFile)
	if err != nil {
		w.metrics.IncrementCacheMiss(provider)
		return nil, err
	}

	if time.Since(info.ModTime()) > w.Config.CacheTTL {
		_ = os.Remove(cacheFile)
		w.updateCacheSize()
		w.metrics.IncrementCacheMiss(provider)
		return nil, fmt.Errorf("cache expired")
	}

	data, err := os.ReadFile(cacheFile)
	if err != nil {
		w.metrics.IncrementCacheMiss(provider)
		return nil, err
	}

	w.metrics.IncrementCacheHits(provider)
	return data, nil
}

func (w *FreeWeatherAPI) CleanOldCache(ctx context.Context) error {
	if !w.EnableCache {
		return nil
	}

	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()

	var filesRemoved int

	err := filepath.Walk(w.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if time.Since(info.ModTime()) > w.Config.CacheTTL {
			if removeErr := os.Remove(path); removeErr == nil {
				filesRemoved++
			}
		}
		return nil
	})

	w.updateCacheSizeLocked()

	if filesRemoved > 0 {
		w.logger.Printf("Cleaned up %d expired cache files", filesRemoved)
	}

	return err
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *FreeWeatherAPI) makeRequest(ctx context.Context, requestURL string, params map[string]string, provider string) ([]byte, error) {
	if w.EnableCache {
		if data, err := w.loadCachedResponse(ctx, requestURL, params, provider); err == nil && data != nil {
			return data, nil
		}
	}

	var lastErr error

	for attempt := 0; attempt <= w.Config.RetryAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("User-Agent", "WeatherApp/1.0")
		req.Header.Set("Accept", "application/json")

		q := req.URL.Query()
		for key, value := range params {
			q.Set(key, value)
		}
		req.URL.RawQuery = q.Encode()

		resp, err := w.Client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			if attempt == w.Config.RetryAttempts {
				break
			}
			if sleepErr := sleepWithContext(ctx, time.Duration(attempt+1)*time.Second); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if attempt == w.Config.RetryAttempts {
				break
			}
			if sleepErr := sleepWithContext(ctx, time.Duration(attempt+1)*time.Second); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		if readErr != nil {
			lastErr = fmt.Errorf("failed to read response: %w", readErr)
			if attempt == w.Config.RetryAttempts {
				break
			}
			if sleepErr := sleepWithContext(ctx, time.Duration(attempt+1)*time.Second); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		if w.EnableCache {
			if err := w.cacheResponse(requestURL, params, data); err != nil {
				w.logger.Printf("Error caching response for %s: %v", provider, err)
			}
		}

		return data, nil
	}

	return nil, fmt.Errorf("after %d attempts: %w", w.Config.RetryAttempts+1, lastErr)
}

func (w *FreeWeatherAPI) validateWeatherData(data WeatherData) bool {
	if data.Temperature < -100 || data.Temperature > 100 {
		return false
	}
	if data.FeelsLike < -120 || data.FeelsLike > 100 {
		return false
	}
	if data.Humidity < 0 || data.Humidity > 100 {
		return false
	}
	if data.Pressure != 0 && (data.Pressure < 800 || data.Pressure > 1100) {
		return false
	}
	if data.WindSpeed < 0 || data.WindSpeed > 150 {
		return false
	}
	if data.WindDirection < 0 || data.WindDirection > 360 {
		return false
	}
	if strings.TrimSpace(data.Description) == "" || strings.TrimSpace(data.Source) == "" || strings.TrimSpace(data.City) == "" {
		return false
	}
	return true
}

func (w *FreeWeatherAPI) GetAllWeatherData() map[string]*WeatherData {
	results := make(map[string]*WeatherData)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex

	providerSet := make(map[string]bool)
	for _, provider := range w.providers {
		if providerSet[provider.Name()] {
			continue
		}
		providerSet[provider.Name()] = true

		wg.Add(1)
		go func(p WeatherProvider) {
			defer wg.Done()

			if err := sleepWithContext(ctx, w.Config.RequestDelay); err != nil && w.Config.RequestDelay > 0 {
				return
			}

			data, err := p.GetWeather(ctx)
			if err != nil {
				w.logger.Printf("Error from %s: %v", p.Name(), err)
				return
			}

			mu.Lock()
			results[p.Name()] = data
			mu.Unlock()
		}(provider)
	}

	wg.Wait()

	if w.EnableCache {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := w.CleanOldCache(cleanupCtx); err != nil {
			w.logger.Printf("Error cleaning cache: %v", err)
		}
	}

	return results
}

func (w *FreeWeatherAPI) GetMetrics() MetricsSummary {
	return w.metrics.GetSummary()
}

func getCoordinatesFromCity(ctx context.Context, city string) (*Coordinates, error) {
	client := &http.Client{Timeout: defaultGeocodeTimeout}

	apiURL := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1", url.QueryEscape(city))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create geocoding request: %w", err)
	}

	req.Header.Set("User-Agent", "WeatherApp/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geocoding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geocoding API returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read geocoding response: %w", err)
	}

	var results NominatimResponse
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("failed to parse geocoding response: %w", err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no coordinates found for city: %s", city)
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse latitude: %w", err)
	}

	lon, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse longitude: %w", err)
	}

	displayName := strings.TrimSpace(results[0].DisplayName)
	if displayName == "" {
		displayName = city
	}

	return &Coordinates{
		Latitude:  lat,
		Longitude: lon,
		City:      displayName,
	}, nil
}

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func printHeader() {
	clearScreen()
	fmt.Println("WEATHER DASHBOARD")
	fmt.Println("Real-time weather data from multiple sources")
	fmt.Println()
}

func readLine(prompt string) string {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	return strings.TrimSpace(line)
}

func waitForEnter() {
	fmt.Print("Press Enter to continue...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func getUserInput(prompt string) string {
	return readLine(prompt)
}

func getYesNoInput(prompt string) bool {
	for {
		input := strings.ToLower(getUserInput(prompt + " (y/n): "))
		if input == "y" || input == "yes" {
			return true
		}
		if input == "n" || input == "no" {
			return false
		}
		fmt.Println("Please enter y or n.")
	}
}

func displayWeatherData(results map[string]*WeatherData, city string) {
	if len(results) == 0 {
		fmt.Println("No weather data could be retrieved")
		fmt.Println()
		fmt.Println("Possible issues:")
		fmt.Println("  • Check internet connection")
		fmt.Println("  • Verify city name")
		fmt.Println("  • APIs might be temporarily unavailable")
		fmt.Println()
		return
	}

	fmt.Printf("Weather for %s\n", city)
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	var totalTemp float64
	var validSources int

	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, source := range names {
		data := results[source]
		fmt.Printf("%s:\n", source)
		fmt.Printf("  Temperature: %.1f°C\n", data.Temperature)
		fmt.Printf("  Feels like: %.1f°C\n", data.FeelsLike)
		fmt.Printf("  Conditions: %s\n", data.Description)
		fmt.Printf("  Humidity: %.0f%%\n", data.Humidity)
		fmt.Printf("  Pressure: %.0f hPa\n", data.Pressure)
		fmt.Printf("  Wind Speed: %.1f m/s\n", data.WindSpeed)
		fmt.Printf("  Wind Direction: %.0f°\n", data.WindDirection)
		fmt.Println()
		fmt.Println(strings.Repeat("-", 40))
		fmt.Println()

		totalTemp += data.Temperature
		validSources++
	}

	if validSources > 0 {
		avgTemp := totalTemp / float64(validSources)
		fmt.Println("Summary")
		fmt.Println(strings.Repeat("-", 47))
		fmt.Printf("Average Temperature: %.1f°C\n", avgTemp)
		fmt.Printf("Sources: %d successful\n", validSources)
		fmt.Printf("Last updated: %s\n", time.Now().Format("2006-01-02 15:04:05"))
		fmt.Println()
	}
}

func displayRawData(results map[string]*WeatherData, city string) {
	fmt.Println("Raw Data View")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	fmt.Printf("%s REPORT\n", city)
	fmt.Println(strings.Repeat("=", 40))
	fmt.Printf("Generated: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	var totalTemp float64
	var validSources int

	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, source := range names {
		data := results[source]
		fmt.Printf("%s:\n", source)
		fmt.Printf("  Temperature: %.1f°C\n", data.Temperature)
		fmt.Printf("  Feels like: %.1f°C\n", data.FeelsLike)
		fmt.Printf("  Conditions: %s\n", data.Description)
		fmt.Printf("  Humidity: %.0f%%\n", data.Humidity)
		fmt.Printf("  Pressure: %.0f hPa\n", data.Pressure)
		fmt.Printf("  Wind: %.1f m/s at %.0f°\n", data.WindSpeed, data.WindDirection)
		fmt.Println()

		totalTemp += data.Temperature
		validSources++
	}

	if validSources > 0 {
		avgTemp := totalTemp / float64(validSources)
		fmt.Printf("Average Temperature: %.1f°C\n", avgTemp)
	}
	fmt.Printf("Successful sources: %d\n", validSources)
	fmt.Println()
}

func displayMetrics(api *FreeWeatherAPI) {
	metrics := api.GetMetrics()

	fmt.Println("Metrics Dashboard")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	fmt.Printf("Uptime: %s\n", metrics.Uptime)
	fmt.Printf("Total Requests: %d\n", metrics.TotalRequests)
	fmt.Printf("Total Errors: %d\n", metrics.TotalErrors)
	fmt.Println()

	fmt.Println("Requests by Provider:")
	requestNames := make([]string, 0, len(metrics.Requests))
	for provider := range metrics.Requests {
		requestNames = append(requestNames, provider)
	}
	sort.Strings(requestNames)

	for _, provider := range requestNames {
		fmt.Printf("  %s: %d\n", provider, metrics.Requests[provider])
	}
	fmt.Println()

	fmt.Println("Errors by Provider:")
	errorNames := make([]string, 0, len(metrics.Errors))
	for provider := range metrics.Errors {
		errorNames = append(errorNames, provider)
	}
	sort.Strings(errorNames)

	for _, provider := range errorNames {
		fmt.Printf("  %s: %d\n", provider, metrics.Errors[provider])
	}
	fmt.Println()

	fmt.Println("Cache Performance:")
	allProvidersMap := make(map[string]struct{})
	for provider := range metrics.Requests {
		allProvidersMap[provider] = struct{}{}
	}
	for provider := range metrics.CacheHits {
		allProvidersMap[provider] = struct{}{}
	}
	for provider := range metrics.CacheMisses {
		allProvidersMap[provider] = struct{}{}
	}

	allProviders := make([]string, 0, len(allProvidersMap))
	for provider := range allProvidersMap {
		allProviders = append(allProviders, provider)
	}
	sort.Strings(allProviders)

	for _, provider := range allProviders {
		hits := metrics.CacheHits[provider]
		misses := metrics.CacheMisses[provider]
		total := hits + misses
		if total > 0 {
			hitRate := float64(hits) / float64(total) * 100
			fmt.Printf("  %s: %d hits, %d misses (%.1f%% hit rate)\n", provider, hits, misses, hitRate)
		}
	}
	fmt.Println()
}

func clearCache() {
	printHeader()

	cacheDir := defaultCacheDir
	if err := os.RemoveAll(cacheDir); err != nil {
		fmt.Printf("Error clearing cache: %v\n", err)
	} else {
		fmt.Println("Cache cleared successfully.")
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			fmt.Printf("Warning: Could not recreate cache directory: %v\n", err)
		}
	}

	waitForEnter()
}

func fetchWeatherInteractive() {
	printHeader()

	fmt.Println("FETCH WEATHER BY CITY")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name (e.g., London, New York, Tokyo): ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		waitForEnter()
		return
	}

	fmt.Printf("\nLooking up coordinates for %s...\n", city)

	ctx, cancel := context.WithTimeout(context.Background(), defaultGeocodeTimeout)
	defer cancel()

	coords, err := getCoordinatesFromCity(ctx, city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		waitForEnter()
		return
	}

	fmt.Printf("Found: %s (%.4f, %.4f)\n", coords.City, coords.Latitude, coords.Longitude)
	fmt.Println()

	enableCache := getYesNoInput("Enable caching for faster results")

	fmt.Println("\nFetching weather data...")
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	api := NewFreeWeatherAPI(coords.City, coords.Latitude, coords.Longitude, enableCache, logger)
	results := api.GetAllWeatherData()

	printHeader()
	displayWeatherData(results, coords.City)
	waitForEnter()
}

func fetchWeatherWithCoordinates() {
	printHeader()

	fmt.Println("FETCH WEATHER WITH COORDINATES")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name: ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		waitForEnter()
		return
	}

	latStr := getUserInput("Enter latitude (e.g., 51.5074 for London): ")
	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		fmt.Println("Invalid latitude. Please enter a valid number.")
		waitForEnter()
		return
	}

	lonStr := getUserInput("Enter longitude (e.g., -0.1278 for London): ")
	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		fmt.Println("Invalid longitude. Please enter a valid number.")
		waitForEnter()
		return
	}

	enableCache := getYesNoInput("Enable caching for faster results")

	fmt.Println("\nFetching weather data...")
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	api := NewFreeWeatherAPI(city, lat, lon, enableCache, logger)
	results := api.GetAllWeatherData()

	printHeader()
	displayWeatherData(results, city)
	waitForEnter()
}

func viewRawData() {
	printHeader()

	fmt.Println("VIEW RAW DATA")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name to view raw data: ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		waitForEnter()
		return
	}

	fmt.Printf("\nLooking up coordinates for %s...\n", city)

	ctx, cancel := context.WithTimeout(context.Background(), defaultGeocodeTimeout)
	defer cancel()

	coords, err := getCoordinatesFromCity(ctx, city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		waitForEnter()
		return
	}

	fmt.Printf("Found: %s (%.4f, %.4f)\n", coords.City, coords.Latitude, coords.Longitude)
	fmt.Println()

	fmt.Println("Fetching weather data...")
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	api := NewFreeWeatherAPI(coords.City, coords.Latitude, coords.Longitude, false, logger)
	results := api.GetAllWeatherData()

	printHeader()
	displayRawData(results, coords.City)
	waitForEnter()
}

func viewMetrics() {
	printHeader()

	fmt.Println("VIEW METRICS")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name to view metrics: ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		waitForEnter()
		return
	}

	fmt.Printf("\nLooking up coordinates for %s...\n", city)

	ctx, cancel := context.WithTimeout(context.Background(), defaultGeocodeTimeout)
	defer cancel()

	coords, err := getCoordinatesFromCity(ctx, city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		waitForEnter()
		return
	}

	fmt.Printf("Found: %s (%.4f, %.4f)\n", coords.City, coords.Latitude, coords.Longitude)
	fmt.Println()

	fmt.Println("Fetching weather data to generate metrics...")
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	api := NewFreeWeatherAPI(coords.City, coords.Latitude, coords.Longitude, true, logger)
	_ = api.GetAllWeatherData()

	printHeader()
	displayMetrics(api)
	waitForEnter()
}

func mainMenu() {
	for {
		printHeader()

		fmt.Println("MAIN MENU")
		fmt.Println(strings.Repeat("=", 50))
		fmt.Println()

		fmt.Println("1. Fetch weather by city name (auto-detect coordinates)")
		fmt.Println("2. Fetch weather with custom coordinates")
		fmt.Println("3. View raw API data")
		fmt.Println("4. View metrics dashboard")
		fmt.Println("5. Clear cache")
		fmt.Println("6. Exit")
		fmt.Println()

		choice := getUserInput("Enter your choice (1-6): ")

		switch choice {
		case "1":
			fetchWeatherInteractive()
		case "2":
			fetchWeatherWithCoordinates()
		case "3":
			viewRawData()
		case "4":
			viewMetrics()
		case "5":
			clearCache()
		case "6":
			fmt.Println("\nThank you for using Weather Dashboard!")
			fmt.Println("Goodbye!")
			return
		default:
			fmt.Println("Invalid choice. Please try again.")
			time.Sleep(1 * time.Second)
		}
	}
}

func main() {
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	logger.Println("Starting Weather Dashboard application")
	mainMenu()
}
