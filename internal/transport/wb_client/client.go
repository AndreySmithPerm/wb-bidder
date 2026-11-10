package wbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

type Client struct {
	httpClient *http.Client
	limiter    *rate.Limiter
	mock       bool
}

func NewClient(mock bool) *Client {
	limit := rate.Every(200 * time.Millisecond)
	wbLimiter := rate.NewLimiter(limit, 3)

	return &Client{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter: wbLimiter,
		mock:    mock,
	}
}

// UpdateAutoCPM изменяет ставку CPM для автоматической кампании (АРК) с учетом лимитов
func (c *Client) UpdateAutoCPM(ctx context.Context, token string, campaignID int64, newBid int) error {
	// МОК-РЕЖИМ: реальный запрос к WB не отправляется
	if c.mock {
		log.Printf("[MOCK] UpdateAutoCPM: кампания %d, новая ставка %d руб. — реальный запрос к WB не отправлен", campaignID, newBid)
		return nil
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter timeout: %w", err)
	}

	targetURL := "https://wildberries.ru"

	bodyData := map[string]interface{}{
		"advertId": campaignID,
		"cpm":      newBid,
	}

	jsonData, err := json.Marshal(bodyData)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create request error: %w", err)
	}

	req.Header.Set("Header-New-Key", token)
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wb api returned error status: %d, body: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
