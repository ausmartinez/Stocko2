package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/joho/godotenv"
)

// Config represents the structure of our JSON file
type Config struct {
	AppName string `json:"app_name"`
	Version string `json:"version"`
	Debug   bool   `json:"debug"`
}

func createNewJSONFile(filename string, config Config, err error) {
	// 2. Check if the error is specifically because the file doesn't exist
	if os.IsNotExist(err) {
		fmt.Printf("'%s' not found. Creating a new one with defaults...\n", filename)

		// Set up your default object values
		config = Config{
			AppName: "MyGoApp",
			Version: "1.0.0",
			Debug:   false,
		}

		// Convert the default object to pretty JSON bytes
		defaultData, marshalErr := json.MarshalIndent(config, "", "    ")
		if marshalErr != nil {
			fmt.Printf("Error creating default JSON: %v\n", marshalErr)
			return
		}

		// Write the new file to disk (0644 gives read/write to owner)
		writeErr := os.WriteFile(filename, defaultData, 0o644)
		if writeErr != nil {
			fmt.Printf("Error writing new file: %v\n", writeErr)
			return
		}

		fmt.Println("New configuration file initialized successfully.")
	} else {
		// Catch any other unexpected OS errors (e.g., permission denied)
		fmt.Printf("Unexpected error reading file: %v\n", err)
		return
	}
}

func setupLogging() *os.File {
	logFile, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatal("Failed to open log file: ", err)
	}
	log.SetOutput(logFile)
	return logFile
}

func main() {
	logFile := setupLogging()
	defer logFile.Close()

	log.Println("Starting process...")
	// Load the .env file
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file, relying on system environment variables")
	}

	// Create an Alpaca client with paper trading credentials
	client := alpaca.NewClient(alpaca.ClientOpts{
		APIKey:    os.Getenv("APCA_API_KEY_ID"),
		APISecret: os.Getenv("APCA_API_SECRET_KEY"),
		BaseURL:   "https://paper-api.alpaca.markets",
	})

	account, err := client.GetAccount()
	if err != nil {
		log.Panicln(err)
		panic(err)
	}

	if account.TradingBlocked {
		log.Panicln("Account is currently restricted from trading.")
	}
	log.Printf("%v is available as buying power.\n", account.BuyingPower)

	filename := "config.json"
	var config Config

	// 1. Try to read the file
	data, err := os.ReadFile(filename)

	if err != nil {
		createNewJSONFile(filename, config, err)
	} else {
		// 3. If the file exists and read successfully, parse it
		log.Printf("'%s' found! Loading data...\n", filename)
		parseErr := json.Unmarshal(data, &config)
		if parseErr != nil {
			log.Printf("Error parsing existing JSON: %v\n", parseErr)
			return
		}
	}

	// 4. Use the data (whether it was loaded or newly created)
	log.Printf("Application Loaded: %s (v%s) | Debug Mode: %t\n", config.AppName, config.Version, config.Debug)
}
