package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"net/smtp"
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
	cleanURL := strings.ReplaceAll("https://captcha.yandex.ru/captcha.js?render=onload", "*", "")

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

// GetStatus возвращает статус работоспособности бэкенда и публичный sitekey капчи из .env
func (h *Handler) GetStatus(c *fiber.Ctx) error {
	// Безопасно извлекаем публичный ключ клиентской капчи из переменной окружения (.env)
	// Если переменная не задана, вернется пустая строка, и фронтенд отключит капчу
	captchaSitekey := os.Getenv("CAPTCHA_SITEKEY")
	if captchaSitekey == "" {
		// Резервный вариант, если в вашей структуре конфигурации ключ называется SMARTCAPTCHA_SITEKEY
		captchaSitekey = os.Getenv("SMARTCAPTCHA_SITEKEY")
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"status":          "running",
		"captcha_sitekey": captchaSitekey, // Передаем ключ фронтенду для динамического рендера
	})
}

// AuthDTO описывает входящие данные для регистрации и входа
type AuthDTO struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	CaptchaToken string `json:"captcha_token"`
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

	captchaSecretKey := os.Getenv("CAPTCHA_SECRETKEY")
	if captchaSecretKey == "" {
		log.Printf("[КРИТИЧЕСКАЯ ОШИБКА CONFIG] Переменная CAPTCHA_SECRETKEY не задана в файле .env")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка конфигурации сервера безопасности"})
	}

	// ПРАВИЛЬНЫЙ URL для валидации Яндекс SmartCaptcha
	validateURL := "https://smartcaptcha.cloud.yandex.ru/validate"

	payload := map[string]string{
		"secret": captchaSecretKey,
		"token":  dto.CaptchaToken,
		// "ip": c.IP(), // можно добавить IP пользователя, если нужно
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ОШИБКА] Не удалось сериализовать payload для капчи: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Внутренняя ошибка сервера"})
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequestWithContext(c.Context(), http.MethodPost, validateURL, bytes.NewReader(jsonData))
	if err != nil {
		log.Printf("[ОШИБКА] Не удалось создать запрос к капче: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Внутренняя ошибка сервера"})
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[КРИТИЧЕСКАЯ ОШИБКА КАПЧИ] Не удалось связаться со шлюзом Яндекса: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Сбой проверки безопасности"})
	}
	defer resp.Body.Close()

	type yandexResponse struct {
		Status string   `json:"status"`
		Errors []string `json:"error-codes"`
	}

	var yRes yandexResponse
	if err := json.NewDecoder(resp.Body).Decode(&yRes); err != nil {
		log.Printf("[ОШИБКА ДЕКОДИРОВАНИЯ ОТВЕТА КАПЧИ]: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Не удалось обработать ответ капчи"})
	}

	if yRes.Status != "ok" {
		log.Printf("[КАПЧА НЕ ПРОЙДЕНА] Статус: %s, ошибки: %v", yRes.Status, yRes.Errors)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error":       "Капча не пройдена",
			"status":      yRes.Status,
			"error_codes": yRes.Errors,
		})
	}
	// =============================================================

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

	// 3. Записываем юзера, рассчитывая дату окончания: NOW() + количество дней из БД
	query := `
		INSERT INTO users (email, password_hash, subscription_expires_at, campaign_limit, role) 
		VALUES ($1, $2, NOW() + $3 * INTERVAL '1 day', $4, 'seller') 
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

// Login проверяет пароль и выдает зашифрованный JWT-токен доступа
func (h *Handler) Login(c *fiber.Ctx) error {
	ctx := c.Context()
	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный JSON"})
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

// AddCampaign автоматически привязывает кампанию к авторизованному user_id
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

	query := `
		INSERT INTO campaigns (id, user_id, wb_token, keyword, target_place, max_bid, min_bid, current_bid, is_active, strategy_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, TRUE, 'target_place')
		ON CONFLICT (id) DO UPDATE 
		SET wb_token = EXCLUDED.wb_token, keyword = EXCLUDED.keyword, target_place = EXCLUDED.target_place, max_bid = EXCLUDED.max_bid, min_bid = EXCLUDED.min_bid`

	_, err = h.pool.Exec(ctx, query, dto.ID, userID, dto.WbToken, dto.Keyword, dto.TargetPlace, dto.MaxBid, dto.MinBid)
	if err != nil {
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
		Market    []int     `json:"market_bids"`
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
	var expiresAt time.Time
	var campaignLimit int
	var allowedProtocol string

	query := `SELECT email, subscription_expires_at, campaign_limit, COALESCE(allowed_auth_protocol, 'manual') FROM users WHERE id = $1`
	err := h.pool.QueryRow(ctx, query, userID).Scan(&email, &expiresAt, &campaignLimit, &allowedProtocol)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Пользователь не найден"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"email":                 email,
		"expires_at":            expiresAt,
		"campaign_limit":        campaignLimit,
		"allowed_auth_protocol": allowedProtocol,
	})
}

// ResetPassword проверяет существование email в БД и генерирует БЕЗОПАСНЫЙ JWT-токен восстановления с отправкой на SMTP Яндекса
func (h *Handler) ResetPassword(c *fiber.Ctx) error {
	ctx := c.Context()
	type resetRequest struct {
		Email string `json:"email"`
	}

	var req resetRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный формат запроса"})
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Укажите корректный Email"})
	}

	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`
	err := h.pool.QueryRow(ctx, checkQuery, req.Email).Scan(&exists)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка проверки аккаунта"})
	}

	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Пользователь с таким Email не зарегистрирован"})
	}

	resetToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"reset_email": req.Email,
		"exp":         time.Now().Add(time.Minute * 15).Unix(),
	})

	tokenStr, err := resetToken.SignedString(jwtSecret)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка генерации ключа безопасности"})
	}

	smtpHost := "smtp.yandex.ru"
	smtpPort := "587"
	senderEmail := "AndreySmithPerm@yandex.ru"
	senderPassword := "xxftlsqbhwnxmegs"

	resetLink := fmt.Sprintf("http://localhost:8080/reset-confirm.html?token=%s", tokenStr)

	fromAddress := mail.Address{Name: "Служба поддержки WB-Bidder", Address: senderEmail}
	toAddress := mail.Address{Address: req.Email}
	mailSubject := "Восстановление доступа к WB-Bidder"

	headerFrom := fmt.Sprintf("From: %s\r\n", fromAddress.String())
	headerTo := fmt.Sprintf("To: %s\r\n", toAddress.String())
	headerSubject := fmt.Sprintf("Subject: %s\r\n", mime.BEncoding.Encode("UTF-8", mailSubject))
	headerMime := "MIME-version: 1.0;\r\nContent-Type: text/html; charset=\"UTF-8\";\r\n\r\n"

	body := fmt.Sprintf(`
		<html>
		<body style="font-family: sans-serif; padding: 20px; color: #1e293b; background-color: #faf5ff;">
			<div style="max-width: 600px; margin: 0 auto; background: white; padding: 30px; border-radius: 16px; border: 1px solid #e9d5ff;">
				<h2 style="color: #9333ea; margin-top: 0;">Восстановление доступа</h2>
				<p>Здравствуйте!</p>
				<p>Для создания нового безопасного пароля перейдите по кнопке ниже (ссылка действительна 15 минут):</p>
				<p style="margin: 30px 0; text-align: center;">
					<a href="%s" style="background-color: #9333ea; color: white; padding: 14px 28px; text-decoration: none; border-radius: 10px; font-weight: bold; display: inline-block;">
						Сбросить текущий пароль
					</a>
				</p>
			</div>
		</body>
		</html>
	`, resetLink)

	msg := []byte(headerFrom + headerTo + headerSubject + headerMime + body)
	auth := smtp.PlainAuth("", senderEmail, senderPassword, smtpHost)

	if err = smtp.SendMail(smtpHost+":"+smtpPort, auth, senderEmail, []string{req.Email}, msg); err != nil {
		log.Printf("[SMTP ОШИБКА] Не удалось отправить письмо: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Не удалось отправить письмо."})
	}

	return c.JSON(fiber.Map{"message": "Инструкция отправлена на ваш Email!"})
}

// SavePassword принимает JWT-токен подтверждения и новый пароль, хэширует его и сохраняет в Postgres
func (h *Handler) SavePassword(c *fiber.Ctx) error {
	ctx := c.Context()
	type saveRequest struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}

	var req saveRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный формат запроса"})
	}

	req.Password = strings.TrimSpace(req.Password)
	if len(req.Password) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Пароль должен быть не менее 6 символов"})
	}

	token, err := jwt.Parse(req.Token, func(t *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Ссылка устарела или недействительна"})
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Ошибка валидации подписи ссылки"})
	}

	emailStr, ok := claims["reset_email"].(string)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Некорректные данные в токене"})
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Не удалось обработать пароль"})
	}

	query := `UPDATE users SET password_hash = $1 WHERE email = $2`
	result, err := h.pool.Exec(ctx, query, string(hashedPassword), emailStr)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка обновления базы данных"})
	}

	if result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Пользователь не найден"})
	}

	log.Printf("[БЕЗОПАСНОСТЬ] Пароль для аккаунта %s успешно изменен через токен", emailStr)
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Пароль успешно изменен!"})
}
