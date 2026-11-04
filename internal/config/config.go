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

// LoadConfig читает файл .env или системное окружение Docker
func LoadConfig() (*Config, error) {
	// Пытаемся загрузить .env, если он есть (для локальной разработки)
	_ = godotenv.Load()

	// ИСПРАВЛЕНО: Сначала ищем переменные напрямую в системе (Docker environment)
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:123456@localhost:5432/wbbidder"
	}

	captchaSiteKey := os.Getenv("CAPTCHA_SITEKEY")
	if captchaSiteKey == "" {
		captchaSiteKey = os.Getenv("SMARTCAPTCHA_SITEKEY")
	}

	captchaSecretKey := os.Getenv("CAPTCHA_SECRETKEY")

	return &Config{
		DbURL:            dbURL,
		CaptchaSiteKey:   captchaSiteKey,
		CaptchaSecretKey: captchaSecretKey,
	}, nil
}
