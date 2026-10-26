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
	// Описываем тестовые сценарии
	tests := []struct {
		name            string
		campaign        model.Campaign
		competitorsBids []int
		mockErr         error
		expectBid       int
		expectErr       bool
	}{
		{
			name: "Успешный расчет: перебиваем конкурента на 1 рубль",
			campaign: model.Campaign{
				ID:         111,
				CurrentBid: 200,
				Strategy: model.Strategy{
					TargetPlace: 3,
					MinBid:      150,
					MaxBid:      500,
					Keyword:     "куртка",
				},
			},
			competitorsBids: []int{400, 350, 300, 250, 200}, // Макс на топ-3 местах — 400 руб
			mockErr:         nil,
			expectBid:       401, // 400 + 1 рубль
			expectErr:       false,
		},
		{
			name: "Защита бюджета: расчетная ставка выше MaxBid, занижаем до лимита",
			campaign: model.Campaign{
				ID:         222,
				CurrentBid: 200,
				Strategy: model.Strategy{
					TargetPlace: 2,
					MinBid:      150,
					MaxBid:      300, // Лимит селлера
					Keyword:     "платье",
				},
			},
			competitorsBids: []int{450, 400, 350}, // Конкуренты дорогие
			mockErr:         nil,
			expectBid:       300, // Срезано до MaxBid
			expectErr:       false,
		},
		{
			name: "Защита от паники: пустой аукцион, ставим MinBid",
			campaign: model.Campaign{
				ID:         333,
				CurrentBid: 200,
				Strategy: model.Strategy{
					TargetPlace: 3,
					MinBid:      160,
					MaxBid:      500,
				},
			},
			competitorsBids: []int{}, // Пустой аукцион
			mockErr:         nil,
			expectBid:       160, // Сработало падение до MinBid
			expectErr:       false,
		},
		{
			name: "Обработка сетевой ошибки: API WB вернуло сбой",
			campaign: model.Campaign{
				ID:         444,
				CurrentBid: 200,
				Strategy: model.Strategy{
					TargetPlace: 1,
					MinBid:      150,
					MaxBid:      500,
				},
			},
			competitorsBids: []int{200, 180},
			mockErr:         errors.New("network timeout"),
			expectBid:       200, // Ставка в модели не должна измениться при ошибке сети
			expectErr:       true,
		},
	}

	// Запускаем каждый тест изолированно
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Инициализируем мок и движок биддера
			mockClient := &WBClientMock{ReturnError: tt.mockErr}
			engine := NewBidderEngine(mockClient)

			// Вызываем отлаживаемый метод расчёта ставки
			err := engine.CalculateAndApplyARC(context.Background(), &tt.campaign, tt.competitorsBids)

			// 1. Проверяем, совпадает ли наличие ошибки с ожидаемым
			if (err != nil) != tt.expectErr {
				t.Fatalf("Ожидалась ошибка: %v, получили: %v", tt.expectErr, err)
			}

			// 2. Если ошибки сети не было, проверяем корректность логики изменения ставки
			if tt.mockErr == nil {
				if tt.campaign.CurrentBid != tt.expectBid {
					t.Errorf("Логика биддинга сломана! Ожидали ставку %d, но код выставил %d", tt.expectBid, tt.campaign.CurrentBid)
				}
				if mockClient.CalledWithBid != tt.expectBid {
					t.Errorf("В API WB отправлена неверная ставка! Ожидали %d, отправлено %d", tt.expectBid, mockClient.CalledWithBid)
				}
			}
		})
	}
}
