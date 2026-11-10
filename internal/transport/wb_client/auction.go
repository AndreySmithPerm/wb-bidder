package wbclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
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
	// МОК-РЕЖИМ: имитация живого аукциона без обращения к WB
	if c.mock {
		mockBids := make([]int, 10)
		base := 900
		for i := range mockBids {
			mockBids[i] = base - i*80
			if mockBids[i] < 150 {
				mockBids[i] = 150
			}
		}
		// Сдвигаем верх аукциона случайно на ±100, чтобы ставки "дышали" между итерациями
		mockBids[0] += rand.Intn(200) - 100
		if mockBids[0] < 150 {
			mockBids[0] = 150
		}
		log.Printf("[MOCK] GetAuctionBids: фраза '%s', сгенерирован аукцион %v", keyword, mockBids)
		return mockBids, nil
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter backoff timeout: %w", err)
	}

	maxPositions := 10
	bids := make([]int, maxPositions)
	for i := range bids {
		bids[i] = 150
	}

	escapedKeyword := url.QueryEscape(keyword)

	// Вариант А: основной эндпоинт аукциона
	cleanApiURL := "https://wildberries.ru" + escapedKeyword

	req, err := http.NewRequestWithContext(ctx, "GET", cleanApiURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var auctionData WbAuctionResponse
				if err := json.NewDecoder(resp.Body).Decode(&auctionData); err == nil && len(auctionData.Adverts) > 0 {

					sort.Slice(auctionData.Adverts, func(i, j int) bool {
						return auctionData.Adverts[i].Position < auctionData.Adverts[j].Position
					})

					for idx, adv := range auctionData.Adverts {
						if idx < maxPositions && adv.CPM > 0 {
							bids[idx] = adv.CPM
						}
					}
					return bids, nil
				}
			}
		}
	}

	// Вариант Б (fallback): резервный эндпоинт
	cleanFallbackURL := "https://wb.ru" + escapedKeyword + "&resultset=catalog"

	reqFB, err := http.NewRequestWithContext(ctx, "GET", cleanFallbackURL, nil)
	if err != nil {
		return bids, fmt.Errorf("failed to create fallback request: %w", err)
	}
	reqFB.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
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
