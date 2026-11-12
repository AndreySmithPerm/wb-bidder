package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	DbURL            string
	CaptchaSiteKey   string
	CaptchaSecretKey string
	SmtpHost         string
	SmtpPort         int
	SmtpUser         string
	SmtpPass         string
	SmtpFromEmail    string
	SmtpFromName     string
	WebURL           string
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

	smtpHost := os.Getenv("SMTP_HOST")
	smtpPortStr := os.Getenv("SMTP_PORT")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	smtpFromEmail := os.Getenv("SMTP_FROM_EMAIL")
	smtpFromName := os.Getenv("SMTP_FROM_NAME")
	webURL := os.Getenv("WEB_URL")

	smtpPort := 587
	if smtpPortStr != "" {
		fmt.Sscanf(smtpPortStr, "%d", &smtpPort)
	}
	if smtpFromName == "" {
		smtpFromName = "WB-Bidder"
	}
	if webURL == "" {
		webURL = "https://wb-bidder.ru"
	}

	return &Config{
		DbURL:            dbURL,
		CaptchaSiteKey:   captchaSiteKey,
		CaptchaSecretKey: captchaSecretKey,
		SmtpHost:         smtpHost,
		SmtpPort:         smtpPort,
		SmtpUser:         smtpUser,
		SmtpPass:         smtpPass,
		SmtpFromEmail:    smtpFromEmail,
		SmtpFromName:     smtpFromName,
		WebURL:           webURL,
	}, nil
}
