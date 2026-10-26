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
// и у чьих владельцев (пользователей) на текущий момент оплачена подписка
func (r *CampaignRepository) GetActiveCampaigns(ctx context.Context) ([]model.Campaign, error) {
	// Обновленный SQL-запрос с JOIN и жестким коммерческим фильтром подписки
	query := `
		SELECT c.id, c.user_id, c.wb_token, c.keyword, c.target_place, c.max_bid, c.min_bid, c.current_bid, c.is_active 
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
		// Сканируем данные из строки БД напрямую в нашу Go-структуру
		err := rows.Scan(
			&c.ID,
			&c.UserID,
			&c.WbToken,
			&c.Strategy.Keyword,
			&c.Strategy.TargetPlace,
			&c.Strategy.MaxBid,
			&c.Strategy.MinBid,
			&c.CurrentBid,
			&c.IsActive,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		// Проставляем тип стратегии по умолчанию, так как мы работаем с АРК
		c.Strategy.Type = model.StrategyTargetPlace
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
