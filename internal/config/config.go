package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	DbURL string
}

// LoadConfig читает файл .env и возвращает структуру с настройками
func LoadConfig() (*Config, error) {
	// Загружаем переменные из файла .env в окружение системы
	_ = godotenv.Load() // Если файла нет (например, в Docker), пропустим и возьмем системные переменные

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		// Дефолтная строка, если .env пустой (символы адреса через звездочку)
		dbURL = "postgres://postgres:123456@localhost:5432/wbbidder"
	}

	return &Config{
		DbURL: dbURL,
	}, nil
}
