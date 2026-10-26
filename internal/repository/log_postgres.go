package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type LogRepository struct {
	pool *pgxpool.Pool
}

func NewLogRepository(pool *pgxpool.Pool) *LogRepository {
	return &LogRepository{pool: pool}
}

// SaveLog записывает результат проверки ставки в таблицу bid_logs
func (r *LogRepository) SaveLog(ctx context.Context, campaignID int64, oldBid, newBid int, marketBids []int, msg string) error {
	query := `
		INSERT INTO bid_logs (campaign_id, old_bid, new_bid, market_bids, status_message) 
		VALUES ($1, $2, $3, $4, $5)`

	_, err := r.pool.Exec(ctx, query, campaignID, oldBid, newBid, marketBids, msg)
	if err != nil {
		return fmt.Errorf("failed to insert bid log: %w", err)
	}
	return nil
}
