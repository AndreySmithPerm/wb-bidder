package model

import "time"

// StrategyType определяет тип поведения биддера
type StrategyType string

const (
	StrategyTargetPlace StrategyType = "target_place" // Удержание конкретного места
	StrategyMaxVisible  StrategyType = "max_visible"  // Максимальная видимость в рамках бюджета
)

// Strategy описывает правила, по которым бот управляет ставками
type Strategy struct {
	Type        StrategyType `json:"type"`
	TargetPlace int          `json:"target_place"` // Желаемое место (например, 3-е)
	MaxBid      int          `json:"max_bid"`      // Максимальный CPM (в рублях), который готов платить юзер
	MinBid      int          `json:"min_bid"`      // Минимальный CPM, ниже которого не опускаемся
	Keyword     string       `json:"keyword"`      // Ключевое слово, по которому чекаем выдачу
}

// Campaign описывает рекламную кампанию на Wildberries
type Campaign struct {
	ID         int64     `json:"id"`          // ID кампании в системе WB
	UserID     int64     `json:"user_id"`     // Привязка к нашему клиенту
	WbToken    string    `json:"-"`           // API-ключ WB (токен "Реклама")
	CurrentBid int       `json:"current_bid"` // Текущая ставка в рублях
	IsActive   bool      `json:"is_active"`   // Включен ли биддинг для этой кампании у нас
	Strategy   Strategy  `json:"strategy"`    // Активная стратегия для JSON API
	UpdatedAt  time.Time `json:"updated_at"`

	// ИСПРАВЛЕНО ДЛЯ PGX: Плоские поля для безопасного сканирования (Scan) из БД в репозитории
	StrategyTypeDB string `json:"-"`
	TargetPlaceDB  int    `json:"-"`
	MaxBidDB       int    `json:"-"`
	MinBidDB       int    `json:"-"`
	KeywordDB      string `json:"-"`
}

// SyncStrategyFromDB заполняет вложенную структуру Strategy на основе плоских полей из базы данных
func (c *Campaign) SyncStrategyFromDB() {
	c.Strategy = Strategy{
		Type:        StrategyType(c.StrategyTypeDB),
		TargetPlace: c.TargetPlaceDB,
		MaxBid:      c.MaxBidDB,
		MinBid:      c.MinBidDB,
		Keyword:     c.KeywordDB,
	}
}

// SyncDBFromStrategy переносит данные из структуры Strategy в плоские поля для сохранения в БД
func (c *Campaign) SyncDBFromStrategy() {
	c.StrategyTypeDB = string(c.Strategy.Type)
	c.TargetPlaceDB = c.Strategy.TargetPlace
	c.MaxBidDB = c.Strategy.MaxBid
	c.MinBidDB = c.Strategy.MinBid
	c.KeywordDB = c.Strategy.Keyword
}
