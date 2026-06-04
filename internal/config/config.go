package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port              string
	Env               string
	PostgresURL       string
	RedisURL          string
	APIKeyHashSecret  string
	DefaultAPIKey     string
	QueueName         string
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, reading from environment")
	}

	return &Config{
		Port:             getEnv("PORT", "8080"),
		Env:              getEnv("ENV", "development"),
		PostgresURL:      getEnv("POSTGRES_URL", ""),
		RedisURL:         getEnv("REDIS_URL", ""),
		APIKeyHashSecret: getEnv("API_KEY_HASH_SECRET", ""),
		DefaultAPIKey:    getEnv("API_KEY", ""),
		QueueName:        getEnv("QUEUE_NAME", "uploads:default"),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}