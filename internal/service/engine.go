package service

import (
	"context"
	"log"
	"time"
	"wb-bidder/internal/model"
)

type WBClient interface {
	UpdateAutoCPM(ctx context.Context, token string, campaignID int64, newBid int) error
}

type BidderEngine struct {
	wbClient WBClient
}

func NewBidderEngine(client WBClient) *BidderEngine {
	return &BidderEngine{wbClient: client}
}

// CalculateAndApplyARC рассчитывает ставку для АРК на основе реального аукциона
func (e *BidderEngine) CalculateAndApplyARC(ctx context.Context, campaign *model.Campaign, competitorsBids []int) error {
	strat := campaign.Strategy

	// Внедряем ночное сбережение бюджета селлера
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		// Безопасный дефолт (MSK UTC+3), если на сервере нет базы tzdata
		loc = time.FixedZone("MSK", 3*60*60)
	}

	currentHour := time.Now().In(loc).Hour()

	// Если время от 1:00 ночи до 6:00 утра — переводим кампанию на эконом-режим
	if currentHour >= 1 && currentHour < 6 {
		log.Printf("[НОЧНАЯ ЗАЩИТА] Аккаунт кампании %d: трафик минимален. Выставляем минимальную ставку MinBid (%d руб.).", campaign.ID, strat.MinBid)

		// Сразу проверяем: если текущая ставка уже равна минимальной, то слать лишний запрос в WB API не нужно
		if campaign.CurrentBid == strat.MinBid {
			log.Printf("[НОЧНАЯ ЗАЩИТА] Кампания %d: минимальная ставка %d руб. уже установлена и оптимальна.", campaign.ID, campaign.CurrentBid)
			return nil
		}

		// Моментально применяем минимальную ставку через ваш родной вспомогательный метод
		return e.applyNewBid(ctx, campaign, strat.MinBid)
	}

	// 1. ЗАЩИТА ОТ ПАНИКИ: если аукцион пуст, ставим минимальную ставку селлера
	if len(competitorsBids) == 0 {
		log.Printf("[АРК] Кампания %d: Аукцион пуст для фразы '%s'. Выставляем MinBid (%d руб.).",
			campaign.ID, strat.Keyword, strat.MinBid)
		return e.applyNewBid(ctx, campaign, strat.MinBid)
	}

	// 2. Корректируем целевое место под реальный размер аукциона
	maxAvailablePlace := strat.TargetPlace
	if maxAvailablePlace > len(competitorsBids) {
		maxAvailablePlace = len(competitorsBids)
	}

	// 3. ПРАВИЛЬНЫЙ АЛГОРИТМ АРК:
	maxCompetitorBid := 0
	for i := 0; i < maxAvailablePlace; i++ {
		if competitorsBids[i] > maxCompetitorBid {
			maxCompetitorBid = competitorsBids[i]
		}
	}

	// Шаг ставки в АРК составляет 1 рубль.
	calculatedBid := maxCompetitorBid + 1

	// 4. Ограничиваем ставку рамками бюджета селлера
	if calculatedBid > strat.MaxBid {
		log.Printf("[АРК] Кампания %d: Расчетный CPM %d выше лимита %d. Занижаем до MaxBid.",
			campaign.ID, calculatedBid, strat.MaxBid)
		calculatedBid = strat.MaxBid
	}

	if calculatedBid < strat.MinBid {
		calculatedBid = strat.MinBid
	}

	// 5. Защита от лишних запросов к API WB (пропускаем, если ставка не изменилась)
	if calculatedBid == campaign.CurrentBid {
		log.Printf("[АРК] Кампания %d: Текущая ставка %d руб. оптимальна.", campaign.ID, campaign.CurrentBid)
		return nil
	}

	// 6. Отправка и применение ставки
	return e.applyNewBid(ctx, campaign, calculatedBid)
}

// Вспомогательный метод для отправки запроса в WB и мутации структуры
func (e *BidderEngine) applyNewBid(ctx context.Context, campaign *model.Campaign, newBid int) error {
	err := e.wbClient.UpdateAutoCPM(ctx, campaign.WbToken, campaign.ID, newBid)
	if err != nil {
		return err
	}

	campaign.CurrentBid = newBid
	log.Printf("[АРК] Кампания %d: Успешно обновлен бустинг, новый CPM: %d руб.", campaign.ID, newBid)
	return nil
}
