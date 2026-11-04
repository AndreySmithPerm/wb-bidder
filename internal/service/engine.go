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

	// === СОХРАНЕНО И ОПТИМИЗИРОВАНО: Ночное сбережение бюджета селлера ===
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		loc = time.FixedZone("MSK", 3*60*60)
	}

	currentHour := time.Now().In(loc).Hour()

	// Если время от 1:00 ночи до 6:00 утра — переводим кампанию на эконом-режим
	if currentHour >= 1 && currentHour < 6 {
		log.Printf("[НОЧНАЯ ЗАЩИТА] Аккаунт кампании %d: трафик минимален. Выставляем минимальную ставку MinBid (%d руб.).", campaign.ID, strat.MinBid)

		if campaign.CurrentBid == strat.MinBid {
			log.Printf("[НОЧНАЯ ЗАЩИТА] Кампания %d: минимальная ставка %d руб. уже установлена и оптимальна.", campaign.ID, campaign.CurrentBid)
			return nil
		}

		return e.applyNewBid(ctx, campaign, strat.MinBid)
	}

	// 1. ЗАЩИТА ОТ ПАНИКИ: если аукцион пуст, ставим минимальную ставку селлера
	if len(competitorsBids) == 0 {
		log.Printf("[АРК] Кампания %d: Аукцион пуст для фразы '%s'. Выставляем MinBid (%d руб.).",
			campaign.ID, strat.Keyword, strat.MinBid)
		return e.applyNewBid(ctx, campaign, strat.MinBid)
	}

	// 2. Корректируем целевое место под реальный размер аукциона
	targetPlace := strat.TargetPlace
	if targetPlace <= 0 {
		targetPlace = 1
	}
	if targetPlace > len(competitorsBids) {
		targetPlace = len(competitorsBids)
	}

	// 3. ИСПРАВЛЕННЫЙ АЛГОРИТМ АРК: Перебиваем ставку именно целевого места на 1 рубль
	// Массивы в Go начинаются с 0, поэтому 3-е место — это индекс 2
	targetCompetitorBid := competitorsBids[targetPlace-1]
	calculatedBid := targetCompetitorBid + 1

	log.Printf("[АРК] Кампания %d: Для удержания места %d (ставка конкурента %d руб.) рассчитан CPM %d руб.",
		campaign.ID, targetPlace, targetCompetitorBid, calculatedBid)

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
