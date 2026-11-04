package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	DbURL            string
	CaptchaSiteKey   string
	CaptchaSecretKey string
}

// LoadConfig читает файл .env и возвращает структуру с настройками
func LoadConfig() (*Config, error) {
	// Загружаем переменные из файла .env в окружение системы (если он есть)
	_ = godotenv.Load()

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:123456@localhost:5432/wbbidder"
	}

	// Централизованно собираем ключи Яндекс Капчи, заданные в docker-compose.yml
	captchaSiteKey := os.Getenv("CAPTCHA_SITEKEY")
	if captchaSiteKey == "" {
		captchaSiteKey = os.Getenv("SMARTCAPTCHA_SITEKEY") // Резервное имя
	}

	captchaSecretKey := os.Getenv("CAPTCHA_SECRETKEY")

	return &Config{
		DbURL:            dbURL,
		CaptchaSiteKey:   captchaSiteKey,
		CaptchaSecretKey: captchaSecretKey,
	}, nil
}
