package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	Timeout         time.Duration
	RetryAttempts   int
	CacheTTL        time.Duration
	RequestDelay    time.Duration
	MaxCacheAgeDays int
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
}

func NewFreeWeatherAPI(city string, lat, lon float64, enableCache bool) *FreeWeatherAPI {
	apiKey := os.Getenv("WEATHERAPI_KEY")
	if apiKey == "" {
		apiKey = "demo"
		log.Println("Using demo WeatherAPI key")
	}

	cacheDir := ".weather_cache"
	if enableCache {
		os.MkdirAll(cacheDir, 0755)
	}

	openMeteoCodes := map[int]string{
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

	return &FreeWeatherAPI{
		City:          city,
		Latitude:      lat,
		Longitude:     lon,
		EnableCache:   enableCache,
		CacheDir:      cacheDir,
		WeatherAPIKey: apiKey,
		Client: &http.Client{
			Timeout: 15 * time.Second,
		},
		openMeteoCodes: openMeteoCodes,
		Config: WeatherAPIConfig{
			Timeout:         15 * time.Second,
			RetryAttempts:   2,
			CacheTTL:        3600 * time.Second,
			RequestDelay:    500 * time.Millisecond,
			MaxCacheAgeDays: 7,
		},
	}
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

	q := req.URL.Query()
	for key, value := range params {
		q.Add(key, value)
	}
	req.URL.RawQuery = q.Encode()

	var lastErr error
	for attempt := 0; attempt < w.Config.RetryAttempts; attempt++ {
		resp, err := w.Client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = err
			continue
		}

		if w.EnableCache {
			w.cacheResponse(requestURL, params, data)
		}

		return data, nil
	}

	return nil, lastErr
}

func (w *FreeWeatherAPI) cacheResponse(requestURL string, params map[string]string, data []byte) {
	cacheKey := w.getCacheKey(requestURL, params)
	cacheFile := filepath.Join(w.CacheDir, cacheKey)
	os.WriteFile(cacheFile, data, 0644)
}

func (w *FreeWeatherAPI) loadCachedResponse(requestURL string, params map[string]string) ([]byte, error) {
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

func (w *FreeWeatherAPI) getCacheKey(requestURL string, params map[string]string) string {
	escapedURL := url.QueryEscape(requestURL)
	if len(params) == 0 {
		return fmt.Sprintf("cache_%s.json", escapedURL)
	}
	paramStr := fmt.Sprintf("%v", params)
	return fmt.Sprintf("cache_%s_%x.json", escapedURL, paramStr)
}

func (w *FreeWeatherAPI) validateWeatherData(data WeatherData) bool {
	if data.Temperature == 0 || data.Description == "" || data.Source == "" || data.City == "" {
		return false
	}
	return true
}

func (w *FreeWeatherAPI) GetOpenMeteo() (*WeatherData, error) {
	apiURL := "https://api.open-meteo.com/v1/forecast"
	params := map[string]string{
		"latitude":  fmt.Sprintf("%f", w.Latitude),
		"longitude": fmt.Sprintf("%f", w.Longitude),
		"current":   "temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,pressure_msl,wind_speed_10m,wind_direction_10m",
		"timezone":  "Europe/Vilnius",
	}

	data, err := w.makeRequest(apiURL, params)
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
	description := w.openMeteoCodes[int(weatherCode)]
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
		Source:        "Open-Meteo",
		City:          w.City,
	}

	if w.validateWeatherData(*weatherData) {
		return weatherData, nil
	}

	return nil, fmt.Errorf("invalid weather data")
}

func (w *FreeWeatherAPI) GetWeatherAPI() (*WeatherData, error) {
	apiURL := "http://api.weatherapi.com/v1/current.json"
	params := map[string]string{
		"key": w.WeatherAPIKey,
		"q":   w.City,
		"aqi": "no",
	}

	data, err := w.makeRequest(apiURL, params)
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
		Source:        "WeatherAPI",
		City:          w.City,
	}

	if w.validateWeatherData(*weatherData) {
		return weatherData, nil
	}

	return nil, fmt.Errorf("invalid weather data")
}

func (w *FreeWeatherAPI) GetWttrIn() (*WeatherData, error) {
	encodedCity := url.QueryEscape(w.City)
	apiURL := fmt.Sprintf("https://wttr.in/%s", encodedCity)
	params := map[string]string{
		"format": "j1",
	}

	data, err := w.makeRequest(apiURL, params)
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
		Source:        "wttr.in",
		City:          w.City,
	}

	if w.validateWeatherData(*weatherData) {
		return weatherData, nil
	}

	return nil, fmt.Errorf("invalid weather data")
}

func (w *FreeWeatherAPI) GetAllWeatherData() map[string]*WeatherData {
	results := make(map[string]*WeatherData)
	apis := []struct {
		name string
		fn   func() (*WeatherData, error)
	}{
		{"Open-Meteo", w.GetOpenMeteo},
		{"WeatherAPI", w.GetWeatherAPI},
		{"wttr.in", w.GetWttrIn},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, api := range apis {
		wg.Add(1)
		go func(name string, fn func() (*WeatherData, error)) {
			defer wg.Done()
			if data, err := fn(); err == nil {
				mu.Lock()
				results[name] = data
				mu.Unlock()
			}
			time.Sleep(w.Config.RequestDelay)
		}(api.name, api.fn)
	}

	wg.Wait()
	return results
}

func getCoordinatesFromCity(city string) (*Coordinates, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	
	apiURL := fmt.Sprintf("https://geocode.maps.co/search?q=%s&api_key=", url.QueryEscape(city))
	
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
	
	return &Coordinates{
		Latitude:  lat,
		Longitude: lon,
		City:      city,
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

func getFloatInput(prompt string, defaultValue float64) float64 {
	for {
		input := getUserInput(prompt)
		if input == "" {
			return defaultValue
		}
		if val, err := strconv.ParseFloat(input, 64); err == nil {
			return val
		}
		fmt.Println("Invalid number. Please try again.")
	}
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

func mainMenu() {
	var city string
	var lat, lon float64
	var enableCache bool
	var api *FreeWeatherAPI

	printHeader()

	for {
		fmt.Println("Main Menu:")
		fmt.Println("1. Enter city name (auto-detect coordinates)")
		fmt.Println("2. Enter city name and coordinates manually")
		fmt.Println("3. Fetch weather data")
		fmt.Println("4. View raw data")
		fmt.Println("5. Toggle caching")
		fmt.Println("6. Exit")
		fmt.Println()

		choice := getUserInput("Enter your choice (1-6): ")

		switch choice {
		case "1":
			printHeader()
			city = getUserInput("Enter city name: ")
			if city == "" {
				city = "Vilnius"
			}
			
			fmt.Printf("Looking up coordinates for %s...\n", city)
			coords, err := getCoordinatesFromCity(city)
			if err != nil {
				fmt.Printf("Error getting coordinates: %v\n", err)
				fmt.Println("Using default coordinates for Vilnius")
				lat = 54.6872
				lon = 25.2797
			} else {
				lat = coords.Latitude
				lon = coords.Longitude
				fmt.Printf("Found coordinates: %.4f, %.4f\n", lat, lon)
			}
			
			api = NewFreeWeatherAPI(city, lat, lon, enableCache)
			fmt.Printf("Location set to: %s (%.4f, %.4f)\n", city, lat, lon)
			fmt.Println()

		case "2":
			printHeader()
			city = getUserInput("Enter city name: ")
			if city == "" {
				city = "Vilnius"
			}
			lat = getFloatInput("Enter latitude [54.6872]: ", 54.6872)
			lon = getFloatInput("Enter longitude [25.2797]: ", 25.2797)
			api = NewFreeWeatherAPI(city, lat, lon, enableCache)
			fmt.Printf("Location set to: %s (%.4f, %.4f)\n", city, lat, lon)
			fmt.Println()

		case "3":
			if api == nil {
				fmt.Println("Please set location first")
				fmt.Println()
				continue
			}
			printHeader()
			fmt.Println("Fetching weather data...")
			fmt.Println()
			results := api.GetAllWeatherData()
			displayWeatherData(results, city)
			fmt.Print("Press Enter to continue...")
			fmt.Scanln()

		case "4":
			if api == nil {
				fmt.Println("Please fetch data first")
				fmt.Println()
				continue
			}
			printHeader()
			results := api.GetAllWeatherData()
			displayRawData(results, city)
			fmt.Print("Press Enter to continue...")
			fmt.Scanln()

		case "5":
			enableCache = !enableCache
			if api != nil {
				api.EnableCache = enableCache
			}
			fmt.Printf("Caching %s\n", func() string {
				if enableCache {
					return "enabled"
				}
				return "disabled"
			}())
			fmt.Println()

		case "6":
			fmt.Println("Goodbye!")
			return

		default:
			fmt.Println("Invalid choice.")
			fmt.Println()
		}

		printHeader()
	}
}

func main() {
	mainMenu()
}
