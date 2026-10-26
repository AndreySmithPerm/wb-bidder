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
	Strategy   Strategy  `json:"strategy"`    // Активная стратегия
	UpdatedAt  time.Time `json:"updated_at"`
}
