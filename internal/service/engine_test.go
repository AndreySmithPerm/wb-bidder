package service

import (
	"context"
	"errors"
	"testing"
	"wb-bidder/internal/model"
)

// WBClientMock — это виртуальный симулятор API Wildberries для тестов
type WBClientMock struct {
	CalledWithBid int
	ReturnError   error
}

// Реализуем интерфейс WBClient для нашего мока
func (m *WBClientMock) UpdateAutoCPM(ctx context.Context, token string, campaignID int64, newBid int) error {
	m.CalledWithBid = newBid
	return m.ReturnError
}

// TestCalculateAndApplyARC — профессиональный табличный Unit-тест
func TestCalculateAndApplyARC(t *testing.T) {
	// Описываем тестовые сценарии с учетом реальной математики аукциона WB
	tests := []struct {
		name            string
		campaign        model.Campaign
		competitorsBids []int
		mockErr         error
		expectBid       int
		expectErr       bool
	}{
		{
			name: "Успешный расчет: удерживаем 3-е место, перебивая ставку 3-го конкурента",
			campaign: model.Campaign{
				ID:         111,
				CurrentBid: 200,
				// Используем DB-поля, которые синхронизируются методом SyncStrategyFromDB
				StrategyTypeDB: "target_place",
				TargetPlaceDB:  3, // Нужна 3-я позиция
				MinBidDB:       150,
				MaxBidDB:       500,
				KeywordDB:      "куртка",
			},
			competitorsBids: []int{400, 350, 300, 250, 200}, // Ставка на 3-м месте равна 300 руб.
			mockErr:         nil,
			expectBid:       301, // ИСПРАВЛЕНО: 300 + 1 рубль для удержания 3-го места (а не 401!)
			expectErr:       false,
		},
		{
			name: "Защита бюджета: расчетная ставка выше MaxBid, занижаем до лимита селлера",
			campaign: model.Campaign{
				ID:             222,
				CurrentBid:     200,
				StrategyTypeDB: "target_place",
				TargetPlaceDB:  2,
				MinBidDB:       150,
				MaxBidDB:       300, // Лимит селлера
				KeywordDB:      "платье",
			},
			competitorsBids: []int{450, 400, 350}, // Конкуренты слишком дорогие
			mockErr:         nil,
			expectBid:       300, // Срезано ровно до MaxBid
			expectErr:       false,
		},
		{
			name: "Защита от паники: пустой аукцион, выставляем безопасный MinBid",
			campaign: model.Campaign{
				ID:             333,
				CurrentBid:     200,
				StrategyTypeDB: "target_place",
				TargetPlaceDB:  3,
				MinBidDB:       160,
				MaxBidDB:       500,
			},
			competitorsBids: []int{}, // Пустая выдача
			mockErr:         nil,
			expectBid:       160, // Падение до MinBid
			expectErr:       false,
		},
		{
			name: "Оптимизация вниз: конкуренты просели, биддер снижает ставку для экономии",
			campaign: model.Campaign{
				ID:             555,
				CurrentBid:     450, // Текущая ставка неоправданно завышена
				StrategyTypeDB: "target_place",
				TargetPlaceDB:  2,
				MinBidDB:       150,
				MaxBidDB:       500,
			},
			competitorsBids: []int{300, 200, 180, 150}, // На 2-м месте ставка 200
			mockErr:         nil,
			expectBid:       201, // Роняем ставку до 201 рубля, спасая баланс
			expectErr:       false,
		},
		{
			name: "Обработка сетевой ошибки: API WB вернуло сбой",
			campaign: model.Campaign{
				ID:             444,
				CurrentBid:     200,
				StrategyTypeDB: "target_place",
				TargetPlaceDB:  1,
				MinBidDB:       150,
				MaxBidDB:       500,
			},
			competitorsBids: []int{200, 180},
			mockErr:         errors.New("network timeout"),
			expectBid:       200, // Ставка не должна измениться при ошибке сети
			expectErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Синхронизируем Strategy структуру перед тестом
			tt.campaign.SyncStrategyFromDB()

			mockClient := &WBClientMock{ReturnError: tt.mockErr}
			engine := NewBidderEngine(mockClient)

			err := engine.CalculateAndApplyARC(context.Background(), &tt.campaign, tt.competitorsBids)

			if (err != nil) != tt.expectErr {
				t.Fatalf("Ожидалась ошибка: %v, получили: %v", tt.expectErr, err)
			}

			if tt.mockErr == nil {
				if tt.campaign.CurrentBid != tt.expectBid {
					t.Errorf("[%s] Неверный расчет! Ожидали CPM %d, но код выставил %d", tt.name, tt.expectBid, tt.campaign.CurrentBid)
				}
				if mockClient.CalledWithBid != tt.expectBid {
					t.Errorf("[%s] В API WB отправлена неверная ставка! Ожидали %d, ушло %d", tt.name, tt.expectBid, mockClient.CalledWithBid)
				}
			}
		})
	}
}
