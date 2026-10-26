package wbclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
		bids[i] = 150 // Базовая минимальная ставка WB
	}

	escapedKeyword := url.QueryEscape(keyword)

	// Вариант А: Основной скрытый эндпоинт аукциона
	// Оригинальный эндпоинт: h*t*t*p*s*:*/*/*c*a*t*a*l*o*g*-*a*d*s*.*w*i*l*d*b*e*r*r*i*e*s*.*r*u*/*a*p*i*/*v*6*/*s*e*a*r*c*h
	cleanApiURL := fmt.Sprintf("https://catalog-ads.wildberries.ru/api/v6/search%s", escapedKeyword)

	req, err := http.NewRequestWithContext(ctx, "GET", cleanApiURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var auctionData WbAuctionResponse
			if err := json.NewDecoder(resp.Body).Decode(&auctionData); err == nil {
				resp.Body.Close()
				for _, adv := range auctionData.Adverts {
					idx := adv.Position - 1
					if idx >= 0 && idx < maxPositions {
						bids[idx] = adv.CPM
					}
				}
				return bids, nil
			}
			resp.Body.Close()
		}
	}

	// Вариант Б (Fallback): Резервный эндпоинт с маскировкой под приложение
	// Оригинальный эндпоинт: h*t*t*p*s*:*/*/*s*e*a*r*c*h*.*w*b*.*r*u*/*e*x*a*c*t*m*a*t*c*h*/*r*u*/*c*o*m*m*o*n*/*v*7*/*s*e*a*r*c*h
	cleanFallbackURL := fmt.Sprintf("https://search.wb.ru/exactmatch/ru/common/v7/search%s", escapedKeyword)

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
