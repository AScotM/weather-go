package main

import (
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
	cacheMu       sync.RWMutex
	metrics       *Metrics
	logger        *log.Logger
}

type Metrics struct {
	mu         sync.RWMutex
	requests   map[string]int
	errors     map[string]int
	cacheHits  map[string]int
	cacheMiss  map[string]int
	startTime  time.Time
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

func (m *Metrics) GetSummary() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	summary := make(map[string]interface{})
	summary["uptime"] = time.Since(m.startTime).String()
	summary["requests"] = m.requests
	summary["errors"] = m.errors
	summary["cache_hits"] = m.cacheHits
	summary["cache_misses"] = m.cacheMiss
	
	totalRequests := 0
	totalErrors := 0
	for _, count := range m.requests {
		totalRequests += count
	}
	for _, count := range m.errors {
		totalErrors += count
	}
	summary["total_requests"] = totalRequests
	summary["total_errors"] = totalErrors
	
	return summary
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

func NewOpenMeteoProvider(api *FreeWeatherAPI, timezone string) *OpenMeteoProvider {
	codes := map[int]string{
		0: "Clear sky", 1: "Mainly clear", 2: "Partly cloudy", 3: "Overcast",
		45: "Fog", 48: "Depositing rime fog",
		51: "Light drizzle", 53: "Moderate drizzle", 55: "Dense drizzle",
		56: "Light freezing drizzle", 57: "Dense freezing drizzle",
		61: "Slight rain", 63: "Moderate rain", 65: "Heavy rain",
		66: "Light freezing rain", 67: "Heavy freezing rain",
		71: "Slight snow fall", 73: "Moderate snow fall", 75: "Heavy snow fall",
		77: "Snow grains",
		80: "Slight rain showers", 81: "Moderate rain showers", 82: "Violent rain showers",
		85: "Slight snow showers", 86: "Heavy snow showers",
		95: "Thunderstorm", 96: "Thunderstorm with slight hail", 99: "Thunderstorm with heavy hail",
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
		"latitude":  fmt.Sprintf("%f", p.api.Latitude),
		"longitude": fmt.Sprintf("%f", p.api.Longitude),
		"current":   "temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,pressure_msl,wind_speed_10m,wind_direction_10m",
		"timezone":  p.timezone,
	}

	data, err := p.api.makeRequest(ctx, apiURL, params, p.Name())
	if err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse Open-Meteo response: %w", err)
	}

	current, ok := result["current"].(map[string]interface{})
	if !ok {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("invalid response format from Open-Meteo")
	}

	temp, ok := current["temperature_2m"].(float64)
	if !ok {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("temperature not found in Open-Meteo response")
	}

	weatherCode, _ := current["weather_code"].(float64)
	description := p.codes[int(weatherCode)]
	if description == "" {
		description = "Unknown"
	}

	feelsLike, _ := current["apparent_temperature"].(float64)
	humidity, _ := current["relative_humidity_2m"].(float64)
	pressure, _ := current["pressure_msl"].(float64)
	windSpeed, _ := current["wind_speed_10m"].(float64)
	windDir, _ := current["wind_direction_10m"].(float64)

	weatherData := &WeatherData{
		Temperature:   temp,
		FeelsLike:     feelsLike,
		Humidity:      humidity,
		Pressure:      pressure,
		WindSpeed:     windSpeed,
		WindDirection: windDir,
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

	apiURL := "http://api.weatherapi.com/v1/current.json"
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

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse WeatherAPI response: %w", err)
	}

	current, ok := result["current"].(map[string]interface{})
	if !ok {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("invalid response format from WeatherAPI")
	}

	tempC, ok := current["temp_c"].(float64)
	if !ok {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("temperature not found in WeatherAPI response")
	}

	condition, _ := current["condition"].(map[string]interface{})
	description, _ := condition["text"].(string)
	if description == "" {
		description = "Unknown"
	}

	feelsLikeC, _ := current["feelslike_c"].(float64)
	humidity, _ := current["humidity"].(float64)
	pressureMb, _ := current["pressure_mb"].(float64)
	windKph, _ := current["wind_kph"].(float64)
	windDegree, _ := current["wind_degree"].(float64)

	weatherData := &WeatherData{
		Temperature:   tempC,
		FeelsLike:     feelsLikeC,
		Humidity:      humidity,
		Pressure:      pressureMb,
		WindSpeed:     windKph * 0.277778,
		WindDirection: windDegree,
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

func (p *WttrInProvider) GetWeather(ctx context.Context) (*WeatherData, error) {
	p.api.metrics.IncrementRequests(p.Name())
	
	encodedCity := url.QueryEscape(p.api.City)
	apiURL := fmt.Sprintf("https://wttr.in/%s", encodedCity)
	params := map[string]string{
		"format": "j1",
	}

	data, err := p.api.makeRequest(ctx, apiURL, params, p.Name())
	if err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse wttr.in response: %w", err)
	}

	currentCondition, ok := result["current_condition"].([]interface{})
	if !ok || len(currentCondition) == 0 {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("invalid response format from wttr.in")
	}

	current, ok := currentCondition[0].(map[string]interface{})
	if !ok {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("invalid current condition format from wttr.in")
	}

	tempCStr, ok := current["temp_C"].(string)
	if !ok {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("temperature not found in wttr.in response")
	}

	tempC, err := strconv.ParseFloat(tempCStr, 64)
	if err != nil {
		p.api.metrics.IncrementErrors(p.Name())
		return nil, fmt.Errorf("failed to parse temperature from wttr.in: %w", err)
	}

	feelsLikeCStr, _ := current["FeelsLikeC"].(string)
	feelsLikeC, _ := strconv.ParseFloat(feelsLikeCStr, 64)

	humidityStr, _ := current["humidity"].(string)
	humidity, _ := strconv.ParseFloat(humidityStr, 64)

	pressureStr, _ := current["pressure"].(string)
	pressure, _ := strconv.ParseFloat(pressureStr, 64)

	windSpeedKmphStr, _ := current["windspeedKmph"].(string)
	windSpeedKmph, _ := strconv.ParseFloat(windSpeedKmphStr, 64)

	windDirStr, _ := current["winddirDegree"].(string)
	windDir, _ := strconv.ParseFloat(windDirStr, 64)

	weatherDesc, _ := current["weatherDesc"].([]interface{})
	var description string
	if len(weatherDesc) > 0 {
		if desc, ok := weatherDesc[0].(map[string]interface{}); ok {
			description, _ = desc["value"].(string)
		}
	}
	if description == "" {
		description = "Unknown"
	}

	weatherData := &WeatherData{
		Temperature:   tempC,
		FeelsLike:     feelsLikeC,
		Humidity:      humidity,
		Pressure:      pressure,
		WindSpeed:     windSpeedKmph * 0.277778,
		WindDirection: windDir,
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
	apiKey := os.Getenv("WEATHERAPI_KEY")
	if apiKey == "" && logger != nil {
		logger.Println("WeatherAPI key not found in environment. WeatherAPI provider will be disabled.")
	}

	cacheDir := defaultCacheDir
	if enableCache {
		if err := os.MkdirAll(cacheDir, 0755); err != nil && logger != nil {
			logger.Printf("Warning: Failed to create cache directory: %v", err)
		}
	}

	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
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

	timezone := getTimezoneFromCoordinates(lat, lon)
	api.AddProvider(NewOpenMeteoProvider(api, timezone))
	if api.WeatherAPIKey != "" {
		api.AddProvider(NewWeatherAPIProvider(api))
	}
	api.AddProvider(NewWttrInProvider(api))

	return api
}

func getTimezoneFromCoordinates(lat, lon float64) string {
	utcOffset := int(lon / 15.0)
	
	if utcOffset >= -5 && utcOffset <= -3 {
		return "America/New_York"
	} else if utcOffset >= -8 && utcOffset <= -7 {
		return "America/Los_Angeles"
	} else if utcOffset >= 0 && utcOffset <= 2 {
		return "Europe/London"
	} else if utcOffset >= 1 && utcOffset <= 3 {
		return "Europe/Paris"
	} else if utcOffset >= 5 && utcOffset <= 6 {
		return "Asia/Dhaka"
	} else if utcOffset >= 8 && utcOffset <= 9 {
		return "Asia/Shanghai"
	} else if utcOffset >= 9 && utcOffset <= 10 {
		return "Asia/Tokyo"
	}
	
	if utcOffset < 0 {
		return fmt.Sprintf("Etc/GMT+%d", -utcOffset)
	}
	return fmt.Sprintf("Etc/GMT-%d", utcOffset)
}

func (w *FreeWeatherAPI) AddProvider(p WeatherProvider) {
	w.providers = append(w.providers, p)
}

func (w *FreeWeatherAPI) getCacheKey(requestURL string, params map[string]string) string {
	u, err := url.Parse(requestURL)
	if err != nil {
		u = &url.URL{Host: "unknown"}
	}

	cacheKey := fmt.Sprintf("cache_%s", u.Host)

	for k, v := range params {
		lowerKey := strings.ToLower(k)
		if lowerKey != "key" && lowerKey != "api_key" && lowerKey != "apikey" {
			cacheKey += fmt.Sprintf("_%s=%s", k, v)
		}
	}

	hash := sha256.Sum256([]byte(cacheKey))
	return fmt.Sprintf("%x.json", hash[:8])
}

func (w *FreeWeatherAPI) updateCacheSize() {
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()

	var total int64
	err := filepath.Walk(w.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	
	if err != nil {
		w.logger.Printf("Error walking cache directory: %v", err)
		return
	}
	
	w.cacheSize = total
}

func (w *FreeWeatherAPI) enforceCacheLimit() error {
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()

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

func (w *FreeWeatherAPI) cacheResponse(ctx context.Context, requestURL string, params map[string]string, data []byte, provider string) error {
	if !w.EnableCache {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	cacheKey := w.getCacheKey(requestURL, params)
	cacheFile := filepath.Join(w.CacheDir, cacheKey)

	tempFile := cacheFile + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp cache file: %w", err)
	}

	if err := os.Rename(tempFile, cacheFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to rename cache file: %w", err)
	}

	w.updateCacheSize()
	if err := w.enforceCacheLimit(); err != nil {
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
		os.Remove(cacheFile)
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

	var filesRemoved int
	err := filepath.Walk(w.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
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
				w.updateCacheSize()
			}
		}
		return nil
	})
	
	if filesRemoved > 0 {
		w.logger.Printf("Cleaned up %d expired cache files", filesRemoved)
	}
	
	return err
}

func (w *FreeWeatherAPI) makeRequest(ctx context.Context, requestURL string, params map[string]string, provider string) ([]byte, error) {
	if w.EnableCache {
		if data, err := w.loadCachedResponse(ctx, requestURL, params, provider); err == nil && data != nil {
			return data, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; WeatherApp/1.0)")
	req.Header.Set("Accept", "application/json")

	q := req.URL.Query()
	for key, value := range params {
		q.Add(key, value)
	}
	req.URL.RawQuery = q.Encode()

	var lastErr error
	for attempt := 0; attempt <= w.Config.RetryAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		resp, err := w.Client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			if attempt == w.Config.RetryAttempts {
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if attempt == w.Config.RetryAttempts {
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			if attempt == w.Config.RetryAttempts {
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}

		if w.EnableCache {
			go w.cacheResponse(context.Background(), requestURL, params, data, provider)
		}

		return data, nil
	}

	return nil, fmt.Errorf("after %d attempts: %w", w.Config.RetryAttempts, lastErr)
}

func (w *FreeWeatherAPI) validateWeatherData(data WeatherData) bool {
	if data.Temperature < -100 || data.Temperature > 100 {
		return false
	}
	if data.Humidity < 0 || data.Humidity > 100 {
		return false
	}
	if data.Pressure < 800 || data.Pressure > 1100 {
		return false
	}
	if data.WindSpeed < 0 || data.WindSpeed > 150 {
		return false
	}
	if data.WindDirection < 0 || data.WindDirection > 360 {
		return false
	}
	if data.Description == "" || data.Source == "" || data.City == "" {
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

			select {
			case <-ctx.Done():
				return
			default:
			}

			if data, err := p.GetWeather(ctx); err == nil {
				mu.Lock()
				results[p.Name()] = data
				mu.Unlock()
			} else {
				w.logger.Printf("Error from %s: %v", p.Name(), err)
			}

			timer := time.NewTimer(w.Config.RequestDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		}(provider)
	}

	wg.Wait()

	if w.EnableCache {
		go func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := w.CleanOldCache(cleanupCtx); err != nil {
				w.logger.Printf("Error cleaning cache: %v", err)
			}
		}()
	}

	return results
}

func (w *FreeWeatherAPI) GetMetrics() map[string]interface{} {
	return w.metrics.GetSummary()
}

func getCoordinatesFromCity(ctx context.Context, city string) (*Coordinates, error) {
	client := &http.Client{Timeout: defaultGeocodeTimeout}

	apiURL := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1", url.QueryEscape(city))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
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

	var results []map[string]interface{}
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("failed to parse geocoding response: %w", err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no coordinates found for city: %s", city)
	}

	firstResult := results[0]
	latStr, latOk := firstResult["lat"].(string)
	lonStr, lonOk := firstResult["lon"].(string)

	if !latOk || !lonOk {
		return nil, fmt.Errorf("invalid coordinate format in geocoding response")
	}

	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse latitude: %w", err)
	}

	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse longitude: %w", err)
	}

	displayName, _ := firstResult["display_name"].(string)
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

func getUserInput(prompt string) string {
	fmt.Print(prompt)
	var input string
	fmt.Scanln(&input)
	return strings.TrimSpace(input)
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

	for source, data := range results {
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
	
	for source, data := range results {
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
	
	fmt.Printf("Uptime: %s\n", metrics["uptime"])
	fmt.Printf("Total Requests: %d\n", metrics["total_requests"])
	fmt.Printf("Total Errors: %d\n", metrics["total_errors"])
	fmt.Println()
	
	fmt.Println("Requests by Provider:")
	requests, _ := metrics["requests"].(map[string]int)
	for provider, count := range requests {
		fmt.Printf("  %s: %d\n", provider, count)
	}
	fmt.Println()
	
	fmt.Println("Errors by Provider:")
	errors, _ := metrics["errors"].(map[string]int)
	for provider, count := range errors {
		fmt.Printf("  %s: %d\n", provider, count)
	}
	fmt.Println()
	
	fmt.Println("Cache Performance:")
	cacheHits, _ := metrics["cache_hits"].(map[string]int)
	cacheMisses, _ := metrics["cache_misses"].(map[string]int)
	
	for provider := range requests {
		hits := cacheHits[provider]
		misses := cacheMisses[provider]
		total := hits + misses
		if total > 0 {
			hitRate := float64(hits) / float64(total) * 100
			fmt.Printf("  %s: %d hits, %d misses (%.1f%% hit rate)\n", 
				provider, hits, misses, hitRate)
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
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			fmt.Printf("Warning: Could not recreate cache directory: %v\n", err)
		}
	}
	fmt.Print("Press Enter to continue...")
	fmt.Scanln()
}

func fetchWeatherInteractive() {
	printHeader()

	fmt.Println("FETCH WEATHER BY CITY")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name (e.g., London, New York, Tokyo): ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	fmt.Printf("\nLooking up coordinates for %s...\n", city)
	
	ctx, cancel := context.WithTimeout(context.Background(), defaultGeocodeTimeout)
	defer cancel()
	
	coords, err := getCoordinatesFromCity(ctx, city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
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

	fmt.Print("Press Enter to return to menu...")
	fmt.Scanln()
}

func fetchWeatherWithCoordinates() {
	printHeader()

	fmt.Println("FETCH WEATHER WITH COORDINATES")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name: ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	latStr := getUserInput("Enter latitude (e.g., 51.5074 for London): ")
	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		fmt.Println("Invalid latitude. Please enter a valid number.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	lonStr := getUserInput("Enter longitude (e.g., -0.1278 for London): ")
	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		fmt.Println("Invalid longitude. Please enter a valid number.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	enableCache := getYesNoInput("Enable caching for faster results")

	fmt.Println("\nFetching weather data...")
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	api := NewFreeWeatherAPI(city, lat, lon, enableCache, logger)
	results := api.GetAllWeatherData()

	printHeader()
	displayWeatherData(results, city)

	fmt.Print("Press Enter to return to menu...")
	fmt.Scanln()
}

func viewRawData() {
	printHeader()

	fmt.Println("VIEW RAW DATA")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name to view raw data: ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	fmt.Printf("\nLooking up coordinates for %s...\n", city)
	
	ctx, cancel := context.WithTimeout(context.Background(), defaultGeocodeTimeout)
	defer cancel()
	
	coords, err := getCoordinatesFromCity(ctx, city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
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

	fmt.Print("Press Enter to return to menu...")
	fmt.Scanln()
}

func viewMetrics() {
	printHeader()
	
	fmt.Println("VIEW METRICS")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	city := getUserInput("Enter city name to view metrics: ")
	if city == "" {
		fmt.Println("City name cannot be empty.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	fmt.Printf("\nLooking up coordinates for %s...\n", city)
	
	ctx, cancel := context.WithTimeout(context.Background(), defaultGeocodeTimeout)
	defer cancel()
	
	coords, err := getCoordinatesFromCity(ctx, city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	fmt.Printf("Found: %s (%.4f, %.4f)\n", coords.City, coords.Latitude, coords.Longitude)
	fmt.Println()

	fmt.Println("Fetching weather data to generate metrics...")
	logger := log.New(os.Stdout, "WEATHER: ", log.LstdFlags)
	api := NewFreeWeatherAPI(coords.City, coords.Latitude, coords.Longitude, true, logger)
	api.GetAllWeatherData()

	printHeader()
	displayMetrics(api)

	fmt.Print("Press Enter to return to menu...")
	fmt.Scanln()
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
