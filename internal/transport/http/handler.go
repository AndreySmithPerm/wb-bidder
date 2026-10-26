package http

import (
	"context"
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

func (h *Handler) GetStatus(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"status": "running",
	})
}

// AuthDTO описывает входящие данные для регистрации и входа
type AuthDTO struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register создает нового пользователя с захэшированным паролем
// Register создает пользователя с динамическим триал-периодом и лимитами из БД
func (h *Handler) Register(c *fiber.Ctx) error {
	var dto AuthDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON"})
	}

	// 1. Извлекаем динамические настройки триала из базы данных
	var trialDays int
	var trialLimit int

	err := h.pool.QueryRow(context.Background(), "SELECT value FROM system_settings WHERE key = 'trial_days'").Scan(&trialDays)
	if err != nil {
		trialDays = 3 // Резервный дефолт
	}

	err = h.pool.QueryRow(context.Background(), "SELECT value FROM system_settings WHERE key = 'trial_campaign_limit'").Scan(&trialLimit)
	if err != nil {
		trialLimit = 1 // Резервный дефолт
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

	// Сохраняем user_id в контекст запроса, чтобы его могли прочитать следующие методы
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

// AddCampaign теперь автоматически привязывает кампанию к авторизованному user_id
func (h *Handler) AddCampaign(c *fiber.Ctx) error {
	// Достаем user_id, который туда положила AuthMiddleware
	userID := c.Locals("user_id").(int64)

	var dto CreateCampaignDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON"})
	}

	if dto.ID == 0 || dto.WbToken == "" || dto.Keyword == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "Missing fields"})
	}

	query := `
		INSERT INTO campaigns (id, user_id, wb_token, keyword, target_place, max_bid, min_bid, current_bid, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, TRUE)
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

	// Считаем агрегаты напрямую быстрыми SQL-запросами
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
	Status string `json:"status"` // "succeeded"
	Amount int    `json:"amount"`
}

// HandlePaymentWebhook принимает сигналы от платежной системы и продлевает тариф
func (h *Handler) HandlePaymentWebhook(c *fiber.Ctx) error {
	var dto PaymentWebhookDTO
	if err := c.BodyParser(&dto); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid payload"})
	}

	if dto.Status == "succeeded" {
		// Оплатил — продлеваем подписку на 30 дней и расширяем лимит до 50 кампаний
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
		price = 2990 // Резервный дефолт, если таблицы нет
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
