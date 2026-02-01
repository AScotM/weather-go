package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
}

type WeatherProvider interface {
	GetWeather() (*WeatherData, error)
	Name() string
}

type FreeWeatherAPI struct {
	City           string
	Latitude       float64
	Longitude      float64
	EnableCache    bool
	Config         WeatherAPIConfig
	WeatherAPIKey  string
	CacheDir       string
	Client         *http.Client
	openMeteoCodes map[int]string
	providers      []WeatherProvider
}

type OpenMeteoProvider struct {
	api        *FreeWeatherAPI
	codes      map[int]string
	timezone   string
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

func (p *OpenMeteoProvider) GetWeather() (*WeatherData, error) {
	apiURL := "https://api.open-meteo.com/v1/forecast"
	params := map[string]string{
		"latitude":  fmt.Sprintf("%f", p.api.Latitude),
		"longitude": fmt.Sprintf("%f", p.api.Longitude),
		"current":   "temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,pressure_msl,wind_speed_10m,wind_direction_10m",
		"timezone":  p.timezone,
	}

	data, err := p.api.makeRequest(apiURL, params)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	current, ok := result["current"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response format")
	}

	temp, ok := current["temperature_2m"].(float64)
	if !ok {
		return nil, fmt.Errorf("temperature not found")
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

	return nil, fmt.Errorf("invalid weather data")
}

func NewWeatherAPIProvider(api *FreeWeatherAPI) *WeatherAPIProvider {
	return &WeatherAPIProvider{api: api}
}

func (p *WeatherAPIProvider) Name() string {
	return "WeatherAPI"
}

func (p *WeatherAPIProvider) GetWeather() (*WeatherData, error) {
	if p.api.WeatherAPIKey == "" {
		return nil, fmt.Errorf("WeatherAPI key not configured")
	}

	apiURL := "http://api.weatherapi.com/v1/current.json"
	params := map[string]string{
		"key": p.api.WeatherAPIKey,
		"q":   p.api.City,
		"aqi": "no",
	}

	data, err := p.api.makeRequest(apiURL, params)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	current, ok := result["current"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response format")
	}

	tempC, ok := current["temp_c"].(float64)
	if !ok {
		return nil, fmt.Errorf("temperature not found")
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

	return nil, fmt.Errorf("invalid weather data")
}

func NewWttrInProvider(api *FreeWeatherAPI) *WttrInProvider {
	return &WttrInProvider{api: api}
}

func (p *WttrInProvider) Name() string {
	return "wttr.in"
}

func (p *WttrInProvider) GetWeather() (*WeatherData, error) {
	encodedCity := url.QueryEscape(p.api.City)
	apiURL := fmt.Sprintf("https://wttr.in/%s", encodedCity)
	params := map[string]string{
		"format": "j1",
	}

	data, err := p.api.makeRequest(apiURL, params)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	currentCondition, ok := result["current_condition"].([]interface{})
	if !ok || len(currentCondition) == 0 {
		return nil, fmt.Errorf("invalid response format")
	}

	current, ok := currentCondition[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid current condition format")
	}

	tempCStr, ok := current["temp_C"].(string)
	if !ok {
		return nil, fmt.Errorf("temperature not found")
	}

	tempC, err := strconv.ParseFloat(tempCStr, 64)
	if err != nil {
		return nil, err
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

	return nil, fmt.Errorf("invalid weather data")
}

func NewFreeWeatherAPI(city string, lat, lon float64, enableCache bool) *FreeWeatherAPI {
	apiKey := os.Getenv("WEATHERAPI_KEY")
	if apiKey == "" {
		log.Println("WeatherAPI key not found in environment. WeatherAPI provider will be disabled.")
	}

	cacheDir := ".weather_cache"
	if enableCache {
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Printf("Warning: Failed to create cache directory: %v", err)
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
			Timeout: 15 * time.Second,
		},
		openMeteoCodes: make(map[int]string),
		Config: WeatherAPIConfig{
			Timeout:       15 * time.Second,
			RetryAttempts: 2,
			CacheTTL:      3600 * time.Second,
			RequestDelay:  500 * time.Millisecond,
		},
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

func (w *FreeWeatherAPI) cacheResponse(requestURL string, params map[string]string, data []byte) error {
	if !w.EnableCache {
		return nil
	}

	cacheKey := w.getCacheKey(requestURL, params)
	cacheFile := filepath.Join(w.CacheDir, cacheKey)

	tempFile := cacheFile + ".tmp"
	if err := ioutil.WriteFile(tempFile, data, 0644); err != nil {
		return err
	}

	return os.Rename(tempFile, cacheFile)
}

func (w *FreeWeatherAPI) loadCachedResponse(requestURL string, params map[string]string) ([]byte, error) {
	if !w.EnableCache {
		return nil, fmt.Errorf("cache disabled")
	}

	cacheKey := w.getCacheKey(requestURL, params)
	cacheFile := filepath.Join(w.CacheDir, cacheKey)

	info, err := os.Stat(cacheFile)
	if err != nil {
		return nil, err
	}

	if time.Since(info.ModTime()) > w.Config.CacheTTL {
		os.Remove(cacheFile)
		return nil, fmt.Errorf("cache expired")
	}

	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (w *FreeWeatherAPI) CleanOldCache() error {
	if !w.EnableCache {
		return nil
	}

	return filepath.Walk(w.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if time.Since(info.ModTime()) > w.Config.CacheTTL {
			os.Remove(path)
		}
		return nil
	})
}

func (w *FreeWeatherAPI) makeRequest(requestURL string, params map[string]string) ([]byte, error) {
	if w.EnableCache {
		if data, err := w.loadCachedResponse(requestURL, params); err == nil && data != nil {
			return data, nil
		}
	}

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
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
		resp, err := w.Client.Do(req)
		if err != nil {
			if attempt == w.Config.RetryAttempts {
				return nil, fmt.Errorf("after %d attempts: %w", w.Config.RetryAttempts, err)
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if attempt == w.Config.RetryAttempts {
				return nil, fmt.Errorf("HTTP %d after %d attempts", resp.StatusCode, w.Config.RetryAttempts)
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if attempt == w.Config.RetryAttempts {
				return nil, fmt.Errorf("failed to read response after %d attempts: %w", w.Config.RetryAttempts, err)
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if w.EnableCache {
			w.cacheResponse(requestURL, params, data)
		}

		return data, nil
	}

	return nil, lastErr
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

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, provider := range w.providers {
		wg.Add(1)
		go func(p WeatherProvider) {
			defer wg.Done()

			if data, err := p.GetWeather(); err == nil {
				mu.Lock()
				results[p.Name()] = data
				mu.Unlock()
			}

			time.Sleep(w.Config.RequestDelay)
		}(provider)
	}

	wg.Wait()

	if w.EnableCache {
		w.CleanOldCache()
	}

	return results
}

func getCoordinatesFromCity(city string) (*Coordinates, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	apiURL := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1", url.QueryEscape(city))

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "WeatherApp/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geocoding API returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var results []map[string]interface{}
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no coordinates found for city: %s", city)
	}

	firstResult := results[0]
	latStr, latOk := firstResult["lat"].(string)
	lonStr, lonOk := firstResult["lon"].(string)

	if !latOk || !lonOk {
		return nil, fmt.Errorf("invalid coordinate format in response")
	}

	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return nil, err
	}

	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		return nil, err
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
		fmt.Println("Check internet connection")
		fmt.Println("Verify city name")
		fmt.Println("APIs might be temporarily unavailable")
		fmt.Println()
		return
	}

	fmt.Printf("Weather for %s\n", city)
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	var totalTemp float64

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
	}

	avgTemp := totalTemp / float64(len(results))

	fmt.Println("Summary")
	fmt.Println(strings.Repeat("-", 47))
	fmt.Printf("Average Temperature: %.1f°C\n", avgTemp)
	fmt.Printf("Sources: %d successful\n", len(results))
	fmt.Printf("Last updated: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Println()
}

func displayRawData(results map[string]*WeatherData, city string) {
	fmt.Println("Raw Data View")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	fmt.Printf("%s REPORT\n", city)
	fmt.Println(strings.Repeat("=", 40))
	fmt.Printf("Generated: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	var totalTemp float64
	for source, data := range results {
		fmt.Printf("%s:\n", source)
		fmt.Printf("  Temperature: %.1f°C\n", data.Temperature)
		fmt.Printf("  Feels like: %.1f°C\n", data.FeelsLike)
		fmt.Printf("  Conditions: %s\n", data.Description)
		fmt.Printf("  Humidity: %.0f%%\n", data.Humidity)
		fmt.Printf("  Pressure: %.0f hPa\n", data.Pressure)
		fmt.Printf("  Wind: %.1f m/s\n", data.WindSpeed)
		fmt.Println()

		totalTemp += data.Temperature
	}

	if len(results) > 0 {
		avgTemp := totalTemp / float64(len(results))
		fmt.Printf("Average Temperature: %.1f°C\n", avgTemp)
	}
	fmt.Printf("Successful sources: %d\n", len(results))
	fmt.Println()
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
	coords, err := getCoordinatesFromCity(city)
	if err != nil {
		fmt.Printf("Error: Could not find coordinates for '%s'\n", city)
		fmt.Println("Please check the city name and try again.")
		fmt.Print("Press Enter to continue...")
		fmt.Scanln()
		return
	}

	fmt.Printf("Found: %s (%.4f, %.4f)\n", coords.City, coords.Latitude, coords.Longitude)
	fmt.Println()

	enableCache := getYesNoInput("Enable caching for faster results?")

	fmt.Println("\nFetching weather data...")
	api := NewFreeWeatherAPI(coords.City, coords.Latitude, coords.Longitude, enableCache)
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

	enableCache := getYesNoInput("Enable caching for faster results?")

	fmt.Println("\nFetching weather data...")
	api := NewFreeWeatherAPI(city, lat, lon, enableCache)
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
	coords, err := getCoordinatesFromCity(city)
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
	api := NewFreeWeatherAPI(coords.City, coords.Latitude, coords.Longitude, false)
	results := api.GetAllWeatherData()

	printHeader()
	displayRawData(results, coords.City)

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
		fmt.Println("4. Clear cache")
		fmt.Println("5. Exit")
		fmt.Println()

		choice := getUserInput("Enter your choice (1-5): ")

		switch choice {
		case "1":
			fetchWeatherInteractive()
		case "2":
			fetchWeatherWithCoordinates()
		case "3":
			viewRawData()
		case "4":
			printHeader()
			cacheDir := ".weather_cache"
			if err := os.RemoveAll(cacheDir); err != nil {
				fmt.Printf("Error clearing cache: %v\n", err)
			} else {
				fmt.Println("Cache cleared successfully.")
			}
			fmt.Print("Press Enter to continue...")
			fmt.Scanln()
		case "5":
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
	mainMenu()
}
