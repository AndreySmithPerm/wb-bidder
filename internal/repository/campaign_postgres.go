package repository

import (
	"context"
	"fmt"
	"wb-bidder/internal/model"

	"github.com/jackc/pgx/v5/pgxpool"
)

type CampaignRepository struct {
	pool *pgxpool.Pool
}

func NewCampaignRepository(pool *pgxpool.Pool) *CampaignRepository {
	return &CampaignRepository{pool: pool}
}

// GetActiveCampaigns достает из Postgres все кампании, для которых включен биддинг
// и у чьих владельцев на текущий момент оплачена подписка.
func (r *CampaignRepository) GetActiveCampaigns(ctx context.Context) ([]model.Campaign, error) {
	// Использование COALESCE защищает приложение от ошибок Scan, если в БД окажутся NULL значения.
	// Добавлена выборка реального c.strategy_type из базы для поддержки разных режимов.
	query := `
		SELECT 
			c.id, 
			c.user_id, 
			c.wb_token, 
			COALESCE(c.keyword, ''), 
			COALESCE(c.strategy_type, 'target_place'),
			COALESCE(c.target_place, 1), 
			COALESCE(c.max_bid, 1000), 
			COALESCE(c.min_bid, 150), 
			COALESCE(c.current_bid, 150), 
			c.is_active 
		FROM campaigns c
		JOIN users u ON c.user_id = u.id
		WHERE c.is_active = TRUE AND u.subscription_expires_at > NOW()`

	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("unable to query campaigns: %w", err)
	}
	defer rows.Close()

	var campaigns []model.Campaign

	for rows.Next() {
		var c model.Campaign
		var stratType string // Временная переменная для безопасного каста строки в тип StrategyType

		err := rows.Scan(
			&c.ID,
			&c.UserID,
			&c.WbToken,
			&c.Strategy.Keyword,
			&stratType,
			&c.Strategy.TargetPlace,
			&c.Strategy.MaxBid,
			&c.Strategy.MinBid,
			&c.CurrentBid,
			&c.IsActive,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		// Валидируем и присваиваем тип стратегии
		c.Strategy.Type = model.StrategyType(stratType)
		campaigns = append(campaigns, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return campaigns, nil
}

// UpdateCampaignBid обновляет текущую ставку кампании в базе после успешного ответа от WB
func (r *CampaignRepository) UpdateCampaignBid(ctx context.Context, campaignID int64, newBid int) error {
	query := `UPDATE campaigns SET current_bid = $1, updated_at = NOW() WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, newBid, campaignID)
	if err != nil {
		return fmt.Errorf("failed to update bid in db: %w", err)
	}
	return nil
}
