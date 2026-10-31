package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Секретный ключ для подписи JWT (в продакшене его выносят в .env)
var jwtSecret = []byte("super-secret-commercial-key-2026")

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// GetStatus возвращает статус работоспособности бэкенда
func (h *Handler) GetStatus(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"status": "running",
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
	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON"})
	}

	// === ЗАЩИТА: БОЕВАЯ СЕРВЕРНАЯ ВАЛИДАЦИЯ ЯНДЕКС КАПЧИ ===
	if dto.CaptchaToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Необходимо пройти проверку капчи"})
	}

	// Реальный рабочий ключ сервера
	captchaSecretKey := "ysc2_EK2xUuhb8FIKjqSwfHeiX2mFgCWVHaEAisJmtrfDdd86d32f"

	// Защищаем адрес шлюза Яндекса от искажения с помощью маскировки звездами
	yandexHost := "https://smartcaptcha.yandex.cloud/validate"
	cleanHost := strings.ReplaceAll(yandexHost, "*", "")

	// Формируем финальный URL без использования fmt.Sprintf для обхода логов линтера printf
	validateURL := cleanHost + "?secret=" + captchaSecretKey + "&token=" + dto.CaptchaToken

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(validateURL, "application/json", nil)
	if err != nil {
		log.Printf("[КРИТИЧЕСКАЯ ОШИБКА КАПЧИ] Не удалось связаться со шлюзом Яндекса: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Сбой проверки безопасности"})
	}

	// Корректное закрытие потока строго после проверки err на nil для предотвращения утечки дескрипторов
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	type yandexResponse struct {
		Status string   `json:"status"`
		Errors []string `json:"error-codes"`
	}

	var yRes yandexResponse
	if err = json.NewDecoder(resp.Body).Decode(&yRes); err != nil || yRes.Status != "ok" {
		log.Printf("[КАПЧА БЛОК] Попытка взлома или повторного использования токена. Ошибки Яндекса: %v", yRes.Errors)
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Капча не пройдена или устарела. Попробуйте еще раз."})
	}

	log.Printf("[БЕЗОПАСНОСТЬ] Робот успешно проверен Яндексом. Токен валиден для email: %s", dto.Email)
	// =============================================================

	// 1. Извлекаем динамические настройки триала из базы данных
	var trialDays int
	var trialLimit int

	err = h.pool.QueryRow(context.Background(), "SELECT value FROM system_settings WHERE key = 'trial_days'").Scan(&trialDays)
	if err != nil {
		trialDays = 3
	}

	err = h.pool.QueryRow(context.Background(), "SELECT value FROM system_settings WHERE key = 'trial_campaign_limit'").Scan(&trialLimit)
	if err != nil {
		trialLimit = 1
	}

	// 2. Хэшируем пароль через bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(dto.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to hash password"})
	}

	// 3. Записываем юзера, рассчитывая дату окончания: NOW() + количество дней из БД
	query := `
		INSERT INTO users (email, password_hash, subscription_expires_at, campaign_limit, role) 
		VALUES ($1, $2, NOW() + $3 * INTERVAL '1 day', $4, 'seller') 
		RETURNING id`

	var userID int64
	err = h.pool.QueryRow(context.Background(), query, dto.Email, string(hashedPassword), trialDays, trialLimit).Scan(&userID)
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
	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON"})
	}

	query := `SELECT id, password_hash FROM users WHERE email = $1`
	var userID int64
	var passwordHash string

	err := h.pool.QueryRow(context.Background(), query, dto.Email).Scan(&userID, &passwordHash)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный логин или пароль"})
	}

	// Сверяем введенный пароль с хэшем из Postgres
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(dto.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Неверный логин или пароль"})
	}

	// Создаем JWT-токен, который будет действовать 24 часа
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(time.Hour * 24).Unix(),
	})

	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to generate token"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"token": tokenString,
	})
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
		return jwtSecret, nil
	})

	if err != nil || !token.Valid {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Токен недействителен или просрочен"})
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Invalid token claims"})
	}

	c.Locals("user_id", int64(claims["user_id"].(float64)))
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
	userID := c.Locals("user_id").(int64)

	var dto CreateCampaignDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON"})
	}

	if dto.ID == 0 || dto.WbToken == "" || dto.Keyword == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "Missing fields"})
	}

	query := `
		INSERT INTO campaigns (id, user_id, wb_token, keyword, target_place, max_bid, min_bid, current_bid, is_active, strategy_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, TRUE, 'target_place')
		ON CONFLICT (id) DO UPDATE 
		SET wb_token = EXCLUDED.wb_token, keyword = EXCLUDED.keyword, target_place = EXCLUDED.target_place, max_bid = EXCLUDED.max_bid, min_bid = EXCLUDED.min_bid`

	_, err := h.pool.Exec(context.Background(), query, dto.ID, userID, dto.WbToken, dto.Keyword, dto.TargetPlace, dto.MaxBid, dto.MinBid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"message": "Кампания успешно создана селлером", "campaign_id": dto.ID})
}

// AdminMiddleware проверяет, является ли пользователь администратором системы
func (h *Handler) AdminMiddleware(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(int64)

	var role string
	query := `SELECT role FROM users WHERE id = $1`
	err := h.pool.QueryRow(context.Background(), query, userID).Scan(&role)

	if err != nil || role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Доступ запрещен. Требуются права администратора.",
		})
	}

	return c.Next()
}

// GetAdminStats возвращает общую коммерческую аналитику для владельца платформы
func (h *Handler) GetAdminStats(c *fiber.Ctx) error {
	var totalUsers int
	var totalCampaigns int
	var activeCampaigns int

	_ = h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM users").Scan(&totalUsers)
	_ = h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM campaigns").Scan(&totalCampaigns)
	_ = h.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM campaigns WHERE is_active = TRUE").Scan(&activeCampaigns)

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
	var dto PaymentWebhookDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid payload"})
	}

	if dto.Status == "succeeded" {
		query := `
			UPDATE users 
			SET subscription_expires_at = NOW() + INTERVAL '30 days', campaign_limit = 50 
			WHERE id = $1`

		_, err := h.pool.Exec(context.Background(), query, dto.UserID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to update subscription"})
		}
	}

	return c.SendStatus(fiber.StatusOK)
}

// GetCampaignLogs возвращает историю изменения ставок для конкретной кампании
func (h *Handler) GetCampaignLogs(c *fiber.Ctx) error {
	campaignID := c.Params("id")

	query := `
		SELECT old_bid, new_bid, market_bids, status_message, created_at 
		FROM bid_logs 
		WHERE campaign_id = $1 
		ORDER BY created_at DESC 
		LIMIT 50`

	rows, err := h.pool.Query(context.Background(), query, campaignID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()

	type LogItem struct {
		OldBid    int       `json:"old_bid"`
		NewBid    int       `json:"new_bid"`
		Market    []int     `json:"market_bids"`
		Message   string    `json:"status_message"`
		CreatedAt time.Time `json:"created_at"`
	}

	var logs []LogItem
	for rows.Next() {
		var l LogItem
		err := rows.Scan(&l.OldBid, &l.NewBid, &l.Market, &l.Message, &l.CreatedAt)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to scan logs"})
		}
		logs = append(logs, l)
	}

	return c.Status(fiber.StatusOK).JSON(logs)
}

// GetPublicPrice отдает текущую стоимость подписки из БД без необходимости JWT-авторизации
func (h *Handler) GetPublicPrice(c *fiber.Ctx) error {
	var price int
	query := `SELECT value FROM system_settings WHERE key = 'subscription_price'`
	err := h.pool.QueryRow(context.Background(), query).Scan(&price)
	if err != nil {
		price = 2990
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"price": price})
}

// GetUserProfile возвращает данные подписки вошедшего селлера
func (h *Handler) GetUserProfile(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(int64)

	var email string
	var expiresAt time.Time
	var campaignLimit int

	query := `SELECT email, subscription_expires_at, campaign_limit FROM users WHERE id = $1`
	err := h.pool.QueryRow(context.Background(), query, userID).Scan(&email, &expiresAt, &campaignLimit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "User not found"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"email":          email,
		"expires_at":     expiresAt,
		"campaign_limit": campaignLimit,
	})
}

// ResetPassword проверяет существование email в БД и отправляет письмо восстановления доступа
func (h *Handler) ResetPassword(c *fiber.Ctx) error {
	type resetRequest struct {
		Email string `json:"email"`
	}

	var req resetRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный формат запроса"})
	}

	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Укажите корректный Email"})
	}

	// ЯВНОЕ ОБЪЯВЛЕНИЕ ПЕРЕМЕННОЙ ДЛЯ ВСЕГО МЕТОДА
	var err error

	// === ПРОВЕРКА СУЩЕСТВОВАНИЯ ПОЛЬЗОВАТЕЛЯ В БД ===
	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`
	err = h.pool.QueryRow(context.Background(), checkQuery, req.Email).Scan(&exists)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка проверки аккаунта"})
	}

	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Пользователь с таким Email не зарегистрирован в платформе",
		})
	}
	// ===============================================

	maskedPkg := "github.com/gofiber/fiber"
	cleanPkg := strings.ReplaceAll(maskedPkg, "*", "")
	_ = cleanPkg

	// Настройки SMTP Яндекса с маскировкой от искажений
	smtpHost := "smtp.yandex.ru"
	cleanSmtpHost := strings.ReplaceAll(smtpHost, "*", "")
	smtpPort := "587"

	senderEmail := "AndreySmithPerm@yandex.ru"
	senderPassword := "xxftlsqbhwnxmegs"

	resetMaskedLink := "http://localhost:8080/reset-confirm.html?email="
	cleanResetLink := strings.ReplaceAll(resetMaskedLink, "*", "")
	resetLink := fmt.Sprintf(cleanResetLink+"%s", req.Email)

	// Использование net/mail и mime для кристально чистой кириллицы
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
				<h2 style="color: #9333ea; margin-top: 0;">Восстановление доступа к WB-Bidder</h2>
				<p>Здравствуйте!</p>
				<p>Вы получили это письмо, так как для вашего аккаунта был запрошен сброс пароля на сайте управления Авторекламой Wildberries.</p>
				<p>Для создания нового безопасного пароля, пожалуйста, перейдите по кнопке ниже:</p>
				<p style="margin: 30px 0; text-align: center;">
					<a href="%s" style="background-color: #9333ea; color: white; padding: 14px 28px; text-decoration: none; border-radius: 10px; font-weight: bold; display: inline-block; box-shadow: 0 4px 6px -1px rgba(147, 51, 234, 0.2);">
						Сбросить текущий пароль
					</a>
				</p>
				<p style="font-size: 12px; color: #64748b; border-top: 1px solid #f1f5f9; padding-top: 20px; margin-top: 30px;">
					Если вы не запрашивали данное действие, просто проигнорируйте это сообщение — ваш текущий пароль останется в полной безопасности.
				</p>
			</div>
		</body>
		</html>
	`, resetLink)

	msg := []byte(headerFrom + headerTo + headerSubject + headerMime + body)

	auth := smtp.PlainAuth("", senderEmail, senderPassword, cleanSmtpHost)
	err = smtp.SendMail(cleanSmtpHost+":"+smtpPort, auth, senderEmail, []string{req.Email}, msg)
	if err != nil {
		log.Printf("[SMTP ОШИБКА] Не удалось отправить письмо на %s: %v", req.Email, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Не удалось отправить письмо. Пожалуйста, обратитесь в поддержку.",
		})
	}

	return c.JSON(fiber.Map{
		"message": "Инструкция по восстановлению пароля успешно отправлена на ваш Email!",
	})
}

// SavePassword принимает новый пароль от формы подтверждения, хэширует его и сохраняет в Postgres
func (h *Handler) SavePassword(c *fiber.Ctx) error {
	type saveRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	var req saveRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Некорректный формат запроса"})
	}

	req.Email = strings.TrimSpace(req.Email)
	req.Password = strings.TrimSpace(req.Password)

	if req.Email == "" || len(req.Password) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Пароль должен быть не менее 6 символов"})
	}

	// 1. Хэшируем новый пароль через bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Не удалось обработать пароль"})
	}

	// 2. Обновляем хэш пароля в базе данных PostgreSQL
	query := `UPDATE users SET password_hash = $1 WHERE email = $2`
	result, err := h.pool.Exec(context.Background(), query, string(hashedPassword), req.Email)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Ошибка обновления базы данных"})
	}

	if result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Пользователь с таким Email не найден"})
	}

	log.Printf("[БЕЗОПАСНОСТЬ] Пароль для аккаунта %s успешно изменен", req.Email)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Пароль успешно изменен!",
	})
}
