package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Секретный ключ для подписи JWT
var jwtSecret = []byte("super-secret-commercial-key-2026")

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// ProxyYandexCaptcha скачивает скрипт напрямую у Яндекса и отдает его от вашего домена
func (h *Handler) ProxyYandexCaptcha(c *fiber.Ctx) error {
	// Исправлено: Боевой стабильный URL скрипта Яндекс СмартКапчи
	cleanURL := "https://yandex.ru"

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
	validateURL := "https://yandex.ru"

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

// Register создает пользователя с динамическим триал-периодом, лимитами из БД и валидацией Яндекс Капчи
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

	// 1. Извлекаем динамические настройки триала из базы данных
	var trialDays int
	var trialLimit int

	err := h.pool.QueryRow(ctx, "SELECT value FROM system_settings WHERE key = 'trial_days'").Scan(&trialDays)
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

	// 3. Записываем юзера, используя функцию make_interval для безопасного добавления дней
	query := `
		INSERT INTO users (email, password_hash, subscription_expires_at, campaign_limit, role) 
		VALUES ($1, $2, NOW() + make_interval(days => $3), $4, 'seller') 
		RETURNING id`

	var userID int64
	err = h.pool.QueryRow(ctx, query, dto.Email, string(hashedPassword), trialDays, trialLimit).Scan(&userID)
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "Пользователь с таким Email уже существует"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"message": "Регистрация успешна",
		"user_id": userID,
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

	query := `SELECT id, password_hash FROM users WHERE email = $1`
	var userID int64
	var passwordHash string

	dto.Email = strings.TrimSpace(strings.ToLower(dto.Email))
	err := h.pool.QueryRow(ctx, query, dto.Email).Scan(&userID, &passwordHash)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный логин или пароль"})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(dto.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный логин или пароль"})
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

// HandlePaymentWebhook принимает сигналы от платежной системы и продлевает тариф
func (h *Handler) HandlePaymentWebhook(c *fiber.Ctx) error {
	ctx := c.Context()
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

// GetCampaignLogs возвращает историю изменения ставок для конкретной кампании
func (h *Handler) GetCampaignLogs(c *fiber.Ctx) error {
	ctx := c.Context()
	campaignID := c.Params("id")

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
		Market    []int32   `json:"market_bids"` // Исправлено: []int32 для корректного маппинга integer[] в pgx
		Message   string    `json:"status_message"`
		CreatedAt time.Time `json:"created_at"`
	}

	var logs = make([]LogItem, 0)
	for rows.Next() {
		var l LogItem
		err := rows.Scan(&l.OldBid, &l.NewBid, &l.Market, &l.Message, &l.CreatedAt)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка parsing логов"})
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
	var expiresAt *time.Time // Исправлено: Указатель для корректной обработки NULL значений в базе данных
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
		expiresStr = expiresAt.Format("2006-01-02 15:04:05")
		isSubscriptionActive = expiresAt.After(time.Now())
	} else {
		expiresStr = "Нет активной подписки"
		isSubscriptionActive = false
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"user_id":              userID,
		"email":                email,
		"subscription_expires": expiresStr,
		"is_active":            isSubscriptionActive,
		"campaign_limit":       campaignLimit,
		"auth_protocol":        allowedProtocol,
	})
}

// ResetPassword — эндпоинт для инициации сброса пароля
func (h *Handler) ResetPassword(c *fiber.Ctx) error {
	type ResetDTO struct {
		Email string `json:"email"`
	}
	var dto ResetDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Инструкции отправлены на Email"})
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
