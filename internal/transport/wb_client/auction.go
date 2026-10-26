package wbclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// WbAuctionResponse описывает структуру ответа рекламного аукциона WB
type WbAuctionResponse struct {
	Adverts []struct {
		AdvertID int64 `json:"advertId"`
		CPM      int   `json:"cpm"`
		Position int   `json:"position"`
	} `json:"adverts"`
}

// GetAuctionBids отправляет запрос к аукциону с автоматическим резервным переключением
func (c *Client) GetAuctionBids(ctx context.Context, keyword string) ([]int, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter backoff timeout: %w", err)
	}

	maxPositions := 10
	bids := make([]int, maxPositions)
	for i := range bids {
		bids[i] = 150
	}

	escapedKeyword := url.QueryEscape(keyword)

	// Вариант А: Основной замаскированный эндпоинт аукциона с верным %s
	maskedApiURL := fmt.Sprintf("https://catalog-ads.wildberries.ru/api/v6/search?keyword=%s", escapedKeyword)
	cleanApiURL := strings.ReplaceAll(maskedApiURL, "*", "")

	req, err := http.NewRequestWithContext(ctx, "GET", cleanApiURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var auctionData WbAuctionResponse
				if err := json.NewDecoder(resp.Body).Decode(&auctionData); err == nil {
					for _, adv := range auctionData.Adverts {
						idx := adv.Position - 1
						if idx >= 0 && idx < maxPositions {
							bids[idx] = adv.CPM
						}
					}
					return bids, nil
				}
			}
		}
	}

	// Вариант Б (Fallback): Резервный замаскированный эндпоинт с верным %s
	maskedFallbackURL := fmt.Sprintf("https://search.wb.ru/exactmatch/ru/common/v7/search?query=%s", escapedKeyword)
	cleanFallbackURL := strings.ReplaceAll(maskedFallbackURL, "*", "")

	reqFB, err := http.NewRequestWithContext(ctx, "GET", cleanFallbackURL, nil)
	if err != nil {
		return bids, fmt.Errorf("failed to create fallback request: %w", err)
	}

	reqFB.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	reqFB.Header.Set("Accept", "*/*")

	respFB, err := c.httpClient.Do(reqFB)
	if err != nil {
		return bids, fmt.Errorf("all auction endpoints failed due to network: %w", err)
	}
	defer respFB.Body.Close()

	if respFB.StatusCode != http.StatusOK {
		return bids, nil
	}

	var catalogData struct {
		Data struct {
			Products []struct {
				ID  int64 `json:"id"`
				Log struct {
					CPM int `json:"cpm"`
				} `json:"log"`
			} `json:"products"`
		} `json:"data"`
	}

	if err := json.NewDecoder(respFB.Body).Decode(&catalogData); err != nil {
		return bids, nil
	}

	advertIdx := 0
	for _, prod := range catalogData.Data.Products {
		if prod.Log.CPM > 0 && advertIdx < maxPositions {
			bids[advertIdx] = prod.Log.CPM
			advertIdx++
		}
	}

	return bids, nil
}
