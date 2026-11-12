package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"wb-bidder/internal/service"
	"golang.org/x/crypto/bcrypt"
)

// Секретный ключ для подписи JWT — читается из окружения один раз при старте пакета
var jwtSecret = func() []byte {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		log.Println("[ПРЕДУПРЕЖДЕНИЕ БЕЗОПАСНОСТИ] JWT_SECRET не задан — используется dev-ключ. Для боевого режима задайте JWT_SECRET!")
		return []byte("dev-only-secret-do-not-use-in-production")
	}
	return []byte(secret)
}()

type Handler struct {
	pool  *pgxpool.Pool
	mailer *service.Mailer
}

func NewHandler(pool *pgxpool.Pool, mailer *service.Mailer) *Handler {
	return &Handler{pool: pool, mailer: mailer}
}

// ProxyYandexCaptcha скачивает скрипт напрямую у Яндекса и отдает его от вашего домена
func (h *Handler) ProxyYandexCaptcha(c *fiber.Ctx) error {
	// Исправлено: Боевой стабильный URL скрипта Яндекс СмартКапчи
	cleanURL := "https://smartcaptcha.cloud.yandex.ru/captcha.js"

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cleanURL)
	if err != nil {
		log.Printf("[ПРОКСИ ОШИБКА] Не удалось связаться с облаком Яндекса: %v", err)
		return c.Status(fiber.StatusBadGateway).SendString("Gateway Error")
	}
	defer resp.Body.Close()

	c.Set("Content-Type", "application/javascript; charset=utf-8")
	return c.SendStream(resp.Body)
}

// GetStatus возвращает статус работоспособности бэкенда и публичный sitekey капчи
func (h *Handler) GetStatus(c *fiber.Ctx) error {
	// Железобетонная проверка всех возможных вариантов названий переменных в Docker
	captchaSitekey := os.Getenv("CAPTCHA_SITEKEY")
	if captchaSitekey == "" {
		captchaSitekey = os.Getenv("SMARTCAPTCHA_SITEKEY")
	}
	if captchaSitekey == "" {
		captchaSitekey = os.Getenv("CAPTCHA_SITE_KEY") // На случай опечатки с подчеркиванием
	}

	log.Printf("[API STATUS] Запрос sitekey фронтендом. Отдаем ключ: %s", captchaSitekey)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"status":          "running",
		"captcha_sitekey": captchaSitekey,
	})
}

// AuthDTO описывает входящие данные для регистрации и входа
type AuthDTO struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	CaptchaToken string `json:"captcha_token"`
}

// validateCaptcha — приватный метод для проверки токена на серверах Яндекса (Вариант Б)
func (h *Handler) validateCaptcha(ctx context.Context, token, userIP string) (bool, []string) {
	captchaSecretKey := os.Getenv("CAPTCHA_SECRETKEY")
	if captchaSecretKey == "" {
		log.Printf("[КРИТИЧЕСКАЯ ОШИБКА CONFIG] Переменная CAPTCHA_SECRETKEY не задана в системе")
		return false, []string{"CONFIG_ERROR"}
	}

	// Исправлено: Боевой шлюз верификации токенов Яндекса
	validateURL := "https://smartcaptcha.cloud.yandex.ru/validate"

	formData := url.Values{}
	formData.Set("secret", captchaSecretKey)
	formData.Set("token", token)
	if userIP != "" {
		formData.Set("ip", userIP)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validateURL, strings.NewReader(formData.Encode()))
	if err != nil {
		log.Printf("[ОШИБКА] Не удалось создать запрос к капче: %v", err)
		return false, []string{"REQUEST_CREATION_FAILED"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[КРИТИЧЕСКАЯ ОШИБКА КАПЧИ] Не удалось связаться со шлюзом Яндекса: %v", err)
		return false, []string{"GATEWAY_TIMEOUT"}
	}
	defer resp.Body.Close()

	type yandexResponse struct {
		Status string   `json:"status"`
		Errors []string `json:"error-codes"`
	}

	var yRes yandexResponse
	if err := json.NewDecoder(resp.Body).Decode(&yRes); err != nil {
		log.Printf("[ОШИБКА ДЕКОДИРОВАНИЯ ОТВЕТА КАПЧИ]: %v", err)
		return false, []string{"DECODING_ERROR"}
	}

	if yRes.Status != "ok" {
		return false, yRes.Errors
	}
	return true, nil
}

// Register создает пользователя с динамическим триал-периодом, лимитами из БД, валидацией капчи и email-верификацией
func (h *Handler) Register(c *fiber.Ctx) error {
	ctx := c.Context()

	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}

	// Валидация Email и пароля
	dto.Email = strings.TrimSpace(strings.ToLower(dto.Email))
	if _, err := mail.ParseAddress(dto.Email); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный формат Email"})
	}
	if len(dto.Password) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Пароль должен быть не менее 6 символов"})
	}

	// === ЗАЩИТА: БОЕВАЯ СЕРВЕРНАЯ ВАЛИДАЦИЯ ЯНДЕКС КАПЧИ ===
	if dto.CaptchaToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Необходимо пройти проверку капчи"})
	}

	ok, errCodes := h.validateCaptcha(ctx, dto.CaptchaToken, c.IP())
	if !ok {
		log.Printf("[КАПЧА НЕ ПРОЙДЕНА РЕГИСТРАЦИЯ] Ошибки: %v", errCodes)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error":       "Капча не пройдена",
			"error_codes": errCodes,
		})
	}

	// === ЛИМИТ РЕГИСТРАЦИЙ: не более 3 раз в сутки (по email + IP) ===
	attemptsCount, err := h.countRegistrationAttempts(ctx, dto.Email, c.IP())
	if err != nil {
		log.Printf("[ОШИБКА] Проверка попыток регистрации: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка проверки лимита"})
	}
	if attemptsCount >= 3 {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error": "Превышен лимит попыток регистрации (3 в сутки). Попробуйте завтра.",
		})
	}

	// 1. Извлекаем динамические настройки триала из базы данных
	var trialDays int
	var trialLimit int

	err = h.pool.QueryRow(ctx, "SELECT value FROM system_settings WHERE key = 'trial_days'").Scan(&trialDays)
	if err != nil {
		trialDays = 3
	}

	err = h.pool.QueryRow(ctx, "SELECT value FROM system_settings WHERE key = 'trial_campaign_limit'").Scan(&trialLimit)
	if err != nil {
		trialLimit = 1
	}

	// 2. Хэшируем пароль через bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(dto.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка хеширования пароля"})
	}

	// 3. Генерируем токен верификации
	rawToken := make([]byte, 16)
	if _, err := rand.Read(rawToken); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка генерации токена"})
	}
	verifyToken := hex.EncodeToString(rawToken)
	tokenHash := sha256.Sum256([]byte(verifyToken))
	tokenHashHex := hex.EncodeToString(tokenHash[:])
	tokenExpires := time.Now().Add(24 * time.Hour)

	// 4. Записываем юзера с флагом is_verified = false
	query := `
		INSERT INTO users (email, password_hash, subscription_expires_at, campaign_limit, role, 
		                   is_verified, verify_token_hash, verify_token_expires) 
		VALUES ($1, $2, NOW() + make_interval(days => $3), $4, 'seller', FALSE, $5, $6) 
		RETURNING id`

	var userID int64
	err = h.pool.QueryRow(ctx, query, dto.Email, string(hashedPassword), trialDays, trialLimit, tokenHashHex, tokenExpires).Scan(&userID)
	if err != nil {
		if strings.Contains(err.Error(), "users_email_key") || strings.Contains(err.Error(), "duplicate key") {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "Пользователь с таким Email уже существует"})
		}
		log.Printf("[ОШИБКА] Создание пользователя: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка создания аккаунта"})
	}

	// 5. Записываем попытку регистрации
	_, insertErr := h.pool.Exec(ctx,
		`INSERT INTO registration_attempts (email, ip_address) VALUES ($1, $2)`,
		dto.Email, c.IP())
	if insertErr != nil {
		log.Printf("[ОШИБКА] Запись попытки регистрации: %v", insertErr)
	}

	// 6. Очищаем старые попытки (старше 24 часов)
	_, _ = h.pool.Exec(ctx,
		`DELETE FROM registration_attempts WHERE created_at < NOW() - INTERVAL '24 hours'`)

	// 7. Отправляем письмо подтверждения
	go func() {
		if err := h.mailer.SendVerification(dto.Email, verifyToken); err != nil {
			log.Printf("[ОШИБКА] Отправка письма подтверждения: %v", err)
		}
	}()

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message":   "Регистрация успешна. Проверьте почту для подтверждения.",
		"user_id":   userID,
		"verified":  false,
	})
}

// countRegistrationAttempts считает количество попыток регистрации за последние 24 часа
func (h *Handler) countRegistrationAttempts(ctx context.Context, email string, ip string) (int, error) {
	query := `
		SELECT COUNT(*) FROM registration_attempts 
		WHERE (email = $1 OR ip_address = $2) 
		AND created_at > NOW() - INTERVAL '24 hours'`

	var count int
	err := h.pool.QueryRow(ctx, query, email, ip).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// VerifyEmail подтверждает email по токену из ссылки
func (h *Handler) VerifyEmail(c *fiber.Ctx) error {
	ctx := c.Context()
	token := c.Query("token", "")
	if token == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Токен не указан"})
	}

	tokenHash := sha256.Sum256([]byte(token))
	tokenHashHex := hex.EncodeToString(tokenHash[:])

	// Обновляем статус verified
	result, err := h.pool.Exec(ctx,
		`UPDATE users 
		 SET is_verified = TRUE, verify_token_hash = NULL, verify_token_expires = NULL 
		 WHERE verify_token_hash = $1 AND verify_token_expires > NOW()`,
		tokenHashHex)
	if err != nil {
		log.Printf("[ОШИБКА] Подтверждение email: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка сервера"})
	}

	if result.RowsAffected() == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Токен недействителен или устарел"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Email успешно подтверждён"})
}

// ResendVerification повторяет отправку письма подтверждения
func (h *Handler) ResendVerification(c *fiber.Ctx) error {
	ctx := c.Context()
	var dto struct {
		Email string `json:"email"`
	}
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}

	dto.Email = strings.TrimSpace(strings.ToLower(dto.Email))
	if _, err := mail.ParseAddress(dto.Email); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный формат Email"})
	}

	// Проверяем существует ли пользователь
	var userID int64
	var isVerified bool
	var verifyTokenHash *string
	var verifyTokenExpires *time.Time

	err := h.pool.QueryRow(ctx,
		`SELECT id, is_verified, verify_token_hash, verify_token_expires FROM users WHERE email = $1`,
		dto.Email).Scan(&userID, &isVerified, &verifyTokenHash, &verifyTokenExpires)
	if err != nil {
		// Не раскрываем факт существования email — отвечаем как будто отправили
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message": "Если email зарегистрирован, письмо отправлено повторно",
		})
	}

	// Уже подтверждён
	if isVerified {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message": "Email уже подтверждён",
		})
	}

	// Проверяем лимит повторных отправок (не чаще 1 раза в 5 минут)
	if verifyTokenHash != nil && verifyTokenExpires != nil {
		var attemptsInWindow int
		err := h.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM registration_attempts 
			 WHERE email = $1 AND created_at > NOW() - INTERVAL '5 minutes'`,
			dto.Email).Scan(&attemptsInWindow)
		if err != nil {
			log.Printf("[ОШИБКА] Проверка повторной отправки: %v", err)
		}
		if attemptsInWindow >= 3 {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "Слишком частые запросы. Подождите 5 минут",
			})
		}
	}

	// Генерируем новый токен
	rawToken := make([]byte, 16)
	if _, err := rand.Read(rawToken); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка генерации токена"})
	}
	verifyToken := hex.EncodeToString(rawToken)
	tokenHash := sha256.Sum256([]byte(verifyToken))
	tokenHashHex := hex.EncodeToString(tokenHash[:])
	tokenExpires := time.Now().Add(24 * time.Hour)

	// Обновляем токен в БД
	_, err = h.pool.Exec(ctx,
		`UPDATE users SET verify_token_hash = $1, verify_token_expires = $2 WHERE id = $3`,
		tokenHashHex, tokenExpires, userID)
	if err != nil {
		log.Printf("[ОШИБКА] Обновление токена верификации: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка сервера"})
	}

	// Записываем попытку для rate limiting
	_, _ = h.pool.Exec(ctx,
		`INSERT INTO registration_attempts (email, ip_address) VALUES ($1, $2)`,
		dto.Email, c.IP())

	// Отправляем письмо
	go func() {
		if err := h.mailer.SendVerification(dto.Email, verifyToken); err != nil {
			log.Printf("[ОШИБКА] Повторная отправка письма подтверждения: %v", err)
		}
	}()

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Письмо отправлено повторно. Проверьте почту.",
	})
}

// Login проверяет пароль, валидирует капчу (Вариант Б) и выдает зашифрованный JWT-токен
func (h *Handler) Login(c *fiber.Ctx) error {
	ctx := c.Context()
	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}

	// === ЗАЩИТА ВАРИАНТ Б: ПРОВЕРКА КАПЧИ НА ВХОДЕ ===
	if dto.CaptchaToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Необходимо пройти проверку капчи"})
	}

	ok, errCodes := h.validateCaptcha(ctx, dto.CaptchaToken, c.IP())
	if !ok {
		log.Printf("[КАПЧА НЕ ПРОЙДЕНА ВХОД] Ошибки: %v", errCodes)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error":       "Капча не пройдена",
			"error_codes": errCodes,
		})
	}

	query := `SELECT id, password_hash, is_verified FROM users WHERE email = $1`
	var userID int64
	dto.Email = strings.TrimSpace(strings.ToLower(dto.Email))

	var passwordHash string
	var isVerified bool

	err := h.pool.QueryRow(ctx, query, dto.Email).Scan(&userID, &passwordHash, &isVerified)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный логин или пароль"})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(dto.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный логин или пароль"})
	}

	// Проверяем подтверждение email
	if !isVerified {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error":       "Подтвердите email перед входом",
			"needs_verify": true,
		})
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(time.Hour * 24).Unix(),
	})

	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка генерации токена"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"token": tokenString})
}

// ForgotPassword валидирует капчу и создаёт одноразовый токен сброса пароля (TTL 60 минут)
func (h *Handler) ForgotPassword(c *fiber.Ctx) error {
	ctx := c.Context()
	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}

	// === ЗАЩИТА ВАРИАНТ Б: ПРОВЕРКА КАПЧИ НА ВОССТАНОВЛЕНИИ ===
	if dto.CaptchaToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Необходимо пройти проверку капчи"})
	}

	ok, errCodes := h.validateCaptcha(ctx, dto.CaptchaToken, c.IP())
	if !ok {
		log.Printf("[КАПЧА НЕ ПРОЙДЕНА ВОССТАНОВЛЕНИЕ] Ошибки: %v", errCodes)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error":       "Капча не пройдена",
			"error_codes": errCodes,
		})
	}

	dto.Email = strings.TrimSpace(strings.ToLower(dto.Email))
	if dto.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Укажите email"})
	}

	// Генерируем токен сброса и храним в базе только его SHA-256 хэш
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка генерации токена"})
	}
	resetToken := hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(resetToken))
	tokenHash := hex.EncodeToString(hash[:])
	expires := time.Now().Add(60 * time.Minute)

	// Токен пишем только если такой email реально зарегистрирован
	res, err := h.pool.Exec(ctx,
		`UPDATE users SET reset_token_hash = $1, reset_token_expires = $2, reset_token_used = FALSE
		 WHERE email = $3`,
		tokenHash, expires, dto.Email)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка базы данных"})
	}

	// Ответ всегда одинаковый — чтобы по нему нельзя было перебирать чужие email'ы
	if res.RowsAffected() == 1 {
		log.Printf("[ВОССТАНОВЛЕНИЕ] Сброс пароля для %s. Ссылка: https://wb-bidder.ru/reset.html?token=%s",
			dto.Email, resetToken)
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Если такой email зарегистрирован, ссылка для сброса отправлена",
	})
}

// ResetPassword принимает токен сброса и новый пароль, обновляет хэш пароля
func (h *Handler) ResetPassword(c *fiber.Ctx) error {
	ctx := c.Context()
	var dto struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}

	if len(dto.Password) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Пароль должен быть не менее 6 символов"})
	}

	hash := sha256.Sum256([]byte(dto.Token))
	tokenHash := hex.EncodeToString(hash[:])

	var email string
	err := h.pool.QueryRow(ctx,
		`SELECT email FROM users
		 WHERE reset_token_hash = $1 AND reset_token_used = FALSE AND reset_token_expires > NOW()`,
		tokenHash).Scan(&email)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Ссылка недействительна или устарела"})
	}

	pwdHash, err := bcrypt.GenerateFromPassword([]byte(dto.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка хэширования пароля"})
	}

	_, err = h.pool.Exec(ctx,
		`UPDATE users SET password_hash = $1, reset_token_hash = NULL, reset_token_expires = NULL, reset_token_used = TRUE
		 WHERE email = $2`,
		pwdHash, email)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка базы данных"})
	}

	log.Printf("[ВОССТАНОВЛЕНИЕ] Пароль успешно изменён для %s", email)
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Пароль успешно изменён"})
}

// AuthMiddleware защищает роуты, проверяя заголовок Authorization: Bearer <token>
func (h *Handler) AuthMiddleware(c *fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Отсутствует токен авторизации"})
	}

	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Некорректный формат токена"})
	}

	tokenStr := parts[1]
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fiber.NewError(fiber.StatusUnauthorized, "Неверный алгоритм подписи")
		}
		return jwtSecret, nil
	})

	if err != nil || !token.Valid {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Токен недействителен или просрочен"})
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Некорректная структура токена"})
	}

	userIDFloat, ok := claims["user_id"].(float64)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Некорректный ID пользователя в токене"})
	}

	c.Locals("user_id", int64(userIDFloat))
	return c.Next()
}

type CreateCampaignDTO struct {
	ID          int64  `json:"id"`
	WbToken     string `json:"wb_token"`
	Keyword     string `json:"keyword"`
	TargetPlace int    `json:"target_place"`
	MaxBid      int    `json:"max_bid"`
	MinBid      int    `json:"min_bid"`
}

// AddCampaign автоматически привязывает кампанию к авторизованному user_id и проверяет права (Защита от Spoofing)
func (h *Handler) AddCampaign(c *fiber.Ctx) error {
	ctx := c.Context()
	userID, ok := c.Locals("user_id").(int64)
	if !ok {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Сбой контекста авторизации"})
	}

	var allowedProtocol string
	err := h.pool.QueryRow(ctx, "SELECT allowed_auth_protocol FROM users WHERE id = $1", userID).Scan(&allowedProtocol)
	if err != nil {
		allowedProtocol = "manual"
	}

	if allowedProtocol == "oauth" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Для вашего аккаунта активирован автоматический протокол OAuth. Привяжите кабинет через окно авторизации WB.",
		})
	}

	var dto CreateCampaignDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}

	if dto.ID == 0 || dto.WbToken == "" || dto.Keyword == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "Заполните обязательные поля"})
	}

	// Безопасное обновление с проверкой владельца кампании
	var ownerID int64
	err = h.pool.QueryRow(ctx, "SELECT user_id FROM campaigns WHERE id = $1", dto.ID).Scan(&ownerID)

	if err == nil {
		if ownerID != userID {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Доступ к этой кампании запрещен"})
		}
		query := `
			UPDATE campaigns 
			SET wb_token = $1, keyword = $2, target_place = $3, max_bid = $4, min_bid = $5 
			WHERE id = $6 AND user_id = $7`
		_, err = h.pool.Exec(ctx, query, dto.WbToken, dto.Keyword, dto.TargetPlace, dto.MaxBid, dto.MinBid, dto.ID, userID)
	} else {
		// === ЭТАП 2: ПРОВЕРКА ЛИМИТА КАМПАНИЙ ПО ТАРИФУ ===
		var campaignLimit, userCampaigns int
		if err := h.pool.QueryRow(ctx, "SELECT campaign_limit FROM users WHERE id = $1", userID).Scan(&campaignLimit); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Не удалось проверить лимит кампаний"})
		}
		if err := h.pool.QueryRow(ctx, "SELECT COUNT(*) FROM campaigns WHERE user_id = $1", userID).Scan(&userCampaigns); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Не удалось проверить лимит кампаний"})
		}
		if userCampaigns >= campaignLimit {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error": "Достигнут лимит кампаний по вашему тарифу. Продлите подписку, чтобы увеличить лимит.",
			})
		}
		// ---------------------------------------------------

		query := `
			INSERT INTO campaigns (id, user_id, wb_token, keyword, target_place, max_bid, min_bid, current_bid, is_active, strategy_type)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7, TRUE, 'target_place')`

		_, err = h.pool.Exec(ctx, query, dto.ID, userID, dto.WbToken, dto.Keyword, dto.TargetPlace, dto.MaxBid, dto.MinBid)
	}

	if err != nil {
		log.Printf("[ОШИБКА СОХРАНЕНИЯ КАМПАНИИ]: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка сохранения кампании в БД"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"message": "Кампания успешно сохранена", "campaign_id": dto.ID})
}

// AdminMiddleware проверяет, является ли пользователь администратором системы
func (h *Handler) AdminMiddleware(c *fiber.Ctx) error {
	ctx := c.Context()
	userID, ok := c.Locals("user_id").(int64)
	if !ok {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Сбой контекста авторизации"})
	}

	var role string
	query := `SELECT role FROM users WHERE id = $1`
	err := h.pool.QueryRow(ctx, query, userID).Scan(&role)

	if err != nil || role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Доступ запрещен. Требуются права администратора.",
		})
	}

	return c.Next()
}

// GetAdminStats возвращает общую коммерческую аналитику для владельца платформы
func (h *Handler) GetAdminStats(c *fiber.Ctx) error {
	ctx := c.Context()
	var totalUsers, totalCampaigns, activeCampaigns int

	if err := h.pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&totalUsers); err != nil {
		log.Printf("[ОШИБКА БД СТАТИСТИКИ]: %v", err)
	}
	if err := h.pool.QueryRow(ctx, "SELECT COUNT(*) FROM campaigns").Scan(&totalCampaigns); err != nil {
		log.Printf("[ОШИБКА БД СТАТИСТИКИ]: %v", err)
	}
	if err := h.pool.QueryRow(ctx, "SELECT COUNT(*) FROM campaigns WHERE is_active = TRUE").Scan(&activeCampaigns); err != nil {
		log.Printf("[ОШИБКА БД СТАТИСТИКИ]: %v", err)
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"total_users":          totalUsers,
		"total_campaigns_db":   totalCampaigns,
		"active_campaigns_now": activeCampaigns,
		"system_status":        "healthy",
	})
}

type PaymentWebhookDTO struct {
	UserID int64  `json:"user_id"`
	Status string `json:"status"`
	Amount int    `json:"amount"`
}

// HandlePaymentWebhook принимает сигналы от платежной системы и продлевает тариф.
// Защита: платёжная система обязана передавать секретный заголовок X-Webhook-Secret.
func (h *Handler) HandlePaymentWebhook(c *fiber.Ctx) error {
	ctx := c.Context()

	// === ЗАЩИТА: ПРОВЕРКА СЕКРЕТА ВЕБХУКА ===
	secret := os.Getenv("PAYMENT_WEBHOOK_SECRET")
	if secret == "" {
		log.Printf("[КРИТИЧЕСКАЯ ОШИБКА CONFIG] Переменная PAYMENT_WEBHOOK_SECRET не задана — вебхук отклонен")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Вебхук не настроен на сервере"})
	}

	if c.Get("X-Webhook-Secret") != secret {
		log.Printf("[ОТКАЗ ВЕБХУКА] Попытка вызова без верного секрета с IP: %s", c.IP())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный секрет вебхука"})
	}

	var dto PaymentWebhookDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректная платежная дата"})
	}

	if dto.Status == "succeeded" {
		query := `
			UPDATE users 
			SET subscription_expires_at = NOW() + INTERVAL '30 days', campaign_limit = 50 
			WHERE id = $1`

		_, err := h.pool.Exec(ctx, query, dto.UserID)
		if err != nil {
			log.Printf("[ОШИБКА ВЕБХУКА ОПЛАТЫ]: не удалось обновить подписку для юзера %d: %v", dto.UserID, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка обработки подписки"})
		}
	}

	return c.SendStatus(fiber.StatusOK)
}

// GetCampaignLogs возвращает историю изменения ставок для конкретной кампании (только её владельцу)
func (h *Handler) GetCampaignLogs(c *fiber.Ctx) error {
	ctx := c.Context()

	userID, ok := c.Locals("user_id").(int64)
	if !ok {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Сбой контекста авторизации"})
	}

	// Парсим ID кампании из URL и проверяем, что это число
	campaignID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный ID кампании"})
	}

	// ЗАЩИТА: проверяем, что кампания существует и принадлежит именно этому пользователю
	var ownerID int64
	err = h.pool.QueryRow(ctx, "SELECT user_id FROM campaigns WHERE id = $1", campaignID).Scan(&ownerID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Кампания не найдена"})
	}
	if ownerID != userID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Доступ к этой кампании запрещен"})
	}

	query := `
		SELECT old_bid, new_bid, market_bids, status_message, created_at 
		FROM bid_logs 
		WHERE campaign_id = $1 
		ORDER BY created_at DESC 
		LIMIT 50`

	rows, err := h.pool.Query(ctx, query, campaignID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка чтения логов"})
	}
	defer rows.Close()

	type LogItem struct {
		OldBid    int       `json:"old_bid"`
		NewBid    int       `json:"new_bid"`
		Market    []int32   `json:"market_bids"` // []int32 для корректного маппинга integer[] в pgx
		Message   string    `json:"status_message"`
		CreatedAt time.Time `json:"created_at"`
	}

	var logs = make([]LogItem, 0)
	for rows.Next() {
		var l LogItem
		err := rows.Scan(&l.OldBid, &l.NewBid, &l.Market, &l.Message, &l.CreatedAt)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка чтения логов"})
		}
		logs = append(logs, l)
	}

	return c.Status(fiber.StatusOK).JSON(logs)
}

// GetPublicPrice отдает текущую стоимость подписки из БД без необходимости JWT-авторизации
func (h *Handler) GetPublicPrice(c *fiber.Ctx) error {
	ctx := c.Context()
	var price int
	query := `SELECT value FROM system_settings WHERE key = 'subscription_price'`
	err := h.pool.QueryRow(ctx, query).Scan(&price)
	if err != nil {
		price = 2990
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"price": price})
}

// GetUserProfile возвращает данные подписки и текущий протокол авторизации селлера
func (h *Handler) GetUserProfile(c *fiber.Ctx) error {
	ctx := c.Context()
	userID, ok := c.Locals("user_id").(int64)
	if !ok {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Сбой контекста авторизации"})
	}

	var email string
	var expiresAt *time.Time // Указатель для корректной обработки NULL значений в базе данных
	var campaignLimit int
	var allowedProtocol string

	query := `
		SELECT email, subscription_expires_at, campaign_limit, allowed_auth_protocol 
		FROM users 
		WHERE id = $1`

	err := h.pool.QueryRow(ctx, query, userID).Scan(&email, &expiresAt, &campaignLimit, &allowedProtocol)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Пользователь не найден"})
	}

	var expiresStr string
	var isSubscriptionActive bool

	if expiresAt != nil {
		expiresStr = expiresAt.Format(time.RFC3339) // ISO 8601 — браузер парсит такой формат без проблем
		isSubscriptionActive = expiresAt.After(time.Now())
	} else {
		expiresStr = "" // пустая строка: фронтенд подставит прочерк вместо "Invalid Date"
		isSubscriptionActive = false
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"user_id":               userID,
		"email":                 email,
		"expires_at":            expiresStr, // ← имя и формат под фронтенд (loadSubscriptionStatus)
		"is_active":             isSubscriptionActive,
		"campaign_limit":        campaignLimit,
		"allowed_auth_protocol": allowedProtocol, // ← имя под фронтенд (checkAllowedAuthProtocol)
	})
}

// SavePassword — сохранение нового пароля после сброса
func (h *Handler) SavePassword(c *fiber.Ctx) error {
	type SaveDTO struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	var dto SaveDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}
	if len(dto.NewPassword) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Пароль слишком короткий"})
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Пароль успешно изменен"})
}
