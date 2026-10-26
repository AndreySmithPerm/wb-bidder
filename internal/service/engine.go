package service

import (
	"context"
	"log"
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

	// В АРК мы ищем максимальную ставку среди конкурентов в выбранном диапазоне мест,
	// чтобы гарантированно их перебить, либо берем ставку на целевом месте.
	targetIdx := strat.TargetPlace - 1
	if targetIdx >= len(competitorsBids) {
		targetIdx = len(competitorsBids) - 1
	}

	// Смотрим ставку конкурента на целевом месте
	competitorBid := competitorsBids[targetIdx]

	// Шаг ставки в АРК обычно составляет 1–5 рублей. Сделаем шаг в 1 рубль для экономии.
	calculatedBid := competitorBid + 1

	// Ограничиваем ставку рамками бюджета селлера
	if calculatedBid > strat.MaxBid {
		log.Printf("[АРК] Кампания %d: Расчетный CPM %d выше лимита %d. Занижаем до MaxBid.", campaign.ID, calculatedBid, strat.MaxBid)
		calculatedBid = strat.MaxBid
	}
	if calculatedBid < strat.MinBid {
		calculatedBid = strat.MinBid
	}

	// Защита от лишних запросов к API WB
	if calculatedBid == campaign.CurrentBid {
		log.Printf("[АРК] Кампания %d: Текущая ставка %d руб. оптимальна.", campaign.ID, campaign.CurrentBid)
		return nil
	}

	// Вызываем новый АРК метод API
	err := e.wbClient.UpdateAutoCPM(ctx, campaign.WbToken, campaign.ID, calculatedBid)
	if err != nil {
		return err
	}

	campaign.CurrentBid = calculatedBid
	log.Printf("[АРК] Кампания %d: Успешно обновлен бустинг, новый CPM: %d руб.", campaign.ID, calculatedBid)
	return nil
}
