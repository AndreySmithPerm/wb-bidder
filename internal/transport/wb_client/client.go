package wbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

type Client struct {
	httpClient *http.Client
	limiter    *rate.Limiter
}

func NewClient() *Client {
	limit := rate.Every(200 * time.Millisecond)
	wbLimiter := rate.NewLimiter(limit, 3)

	return &Client{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter: wbLimiter,
	}
}

// UpdateAutoCPM изменяет ставку CPM для Автоматической кампании (АРК) с учетом лимитов
func (c *Client) UpdateAutoCPM(ctx context.Context, token string, campaignID int64, newBid int) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter timeout: %w", err)
	}

	// Настоящий рабочий эндпоинт Wildberries API
	targetURL := "https://advert-api.wildberries.ru/adv/v1/auto/upcpm"

	bodyData := map[string]interface{}{
		"advertId": campaignID,
		"cpm":      newBid,
	}

	jsonData, err := json.Marshal(bodyData)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	// Запрос отправляется строго на targetURL
	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create request error: %w", err)
	}

	// Передаем токен авторизации селлера
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	// Если WB отклонил запрос (например, неверный токен или неверный ID кампании), выводим статус
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wb api returned error status: %d", resp.StatusCode)
	}

	return nil
}
