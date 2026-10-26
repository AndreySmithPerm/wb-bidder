package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"wb-bidder/internal/config"
	"wb-bidder/internal/model"
	"wb-bidder/internal/repository"
	"wb-bidder/internal/service"
	apiHttp "wb-bidder/internal/transport/http"
	wbclient "wb-bidder/internal/transport/wb_client"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	log.Println("Запуск коммерческого АРК-биддера Wildberries...")

	// 1. Загружаем конфигурацию (.env)
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	cleanDbURL := strings.ReplaceAll(cfg.DbURL, "*", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Инициализируем пул соединений с PostgreSQL
	dbPool, err := pgxpool.New(ctx, cleanDbURL)
	if err != nil {
		log.Fatalf("Не удалось подключиться к PostgreSQL: %v", err)
	}
	defer dbPool.Close()

	// Проверяем связь с базой данных
	if err := dbPool.Ping(ctx); err != nil {
		log.Fatalf("База данных не отвечает на пинг: %v", err)
	}
	log.Println("Успешное подключение к PostgreSQL.")

	// 3. Инициализируем репозитории и сервисы
	campRepo := repository.NewCampaignRepository(dbPool)
	logRepo := repository.NewLogRepository(dbPool)
	wbCli := wbclient.NewClient()
	engine := service.NewBidderEngine(wbCli)

	// Настройки многопоточности
	numWorkers := 5
	campaignChan := make(chan model.Campaign, 1000)

	// 4. Запуск пула воркеров для параллельной обработки
	var wg sync.WaitGroup
	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for camp := range campaignChan {
				log.Printf("[Воркер %d] Кампания %d: проверка ключевой фразы '%s'", workerID, camp.ID, camp.Strategy.Keyword)

				// Шаг А: Парсим реальный рекламный аукцион
				realBids, err := wbCli.GetAuctionBids(ctx, camp.Strategy.Keyword)
				if err != nil {
					log.Printf("[Воркер %d] Ошибка парсинга аукциона для %d: %v", workerID, camp.ID, err)
					_ = logRepo.SaveLog(ctx, camp.ID, camp.CurrentBid, camp.CurrentBid, []int{}, "Ошибка парсинга аукциона")
					continue
				}

				oldBid := camp.CurrentBid

				// Шаг Б: Рассчитываем и отправляем новую ставку АРК на WB
				err = engine.CalculateAndApplyARC(ctx, &camp, realBids)

				// Определяем статус-сообщение для истории логов
				statusMsg := "Ставка оптимальна"
				if err != nil {
					statusMsg = "Ошибка API WB: " + err.Error()
					log.Printf("[Воркер %d] Ошибка применения ставки для %d: %v", workerID, camp.ID, err)
				} else if oldBid != camp.CurrentBid {
					statusMsg = "Ставка успешно изменена"
					// Обновляем CPM в таблице campaigns
					_ = campRepo.UpdateCampaignBid(ctx, camp.ID, camp.CurrentBid)
				}

				// Шаг В: Сохраняем результат в историю логов (для графиков в личном кабинете)
				_ = logRepo.SaveLog(ctx, camp.ID, oldBid, camp.CurrentBid, realBids, statusMsg)
			}
		}(w)
	}

	// --- ИНТЕГРАЦИЯ WEB-API FIBER ---
	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
	})

	apiHandler := apiHttp.NewHandler(dbPool)

	// Раздача статичных HTML/JS файлов фронтенда из папки web
	app.Static("/", "./web")

	// Публичные роуты (доступны всем)
	app.Get("/api/public/price", apiHandler.GetPublicPrice) // роут цены
	app.Get("/api/status", apiHandler.GetStatus)
	app.Post("/api/auth/register", apiHandler.Register)
	app.Post("/api/auth/login", apiHandler.Login)
	app.Post("/api/payments/webhook", apiHandler.HandlePaymentWebhook) // Роут для приема денег

	// Защищенный роут админки (сначала проверяем токен, потом проверяем роль админа)
	app.Get("/api/admin/stats", apiHandler.AuthMiddleware, apiHandler.AdminMiddleware, apiHandler.GetAdminStats)

	// Защищенный роут для селлеров (создание кампаний под защитой JWT)
	app.Post("/api/campaigns", apiHandler.AuthMiddleware, apiHandler.AddCampaign)

	// ВСТАВЛЯЕМ СЮДА: Роут истории логов конкретной кампании
	app.Get("/api/campaigns/:id/logs", apiHandler.AuthMiddleware, apiHandler.GetCampaignLogs)

	// Защищенный роут для получения данных профиля селлера
	app.Get("/api/user/profile", apiHandler.AuthMiddleware, apiHandler.GetUserProfile)

	go func() {
		log.Println("Запуск Web-API сервера на порту :8080")
		if err := app.Listen(":8080"); err != nil {
			log.Printf("Предупреждение при остановке веб-сервера: %v", err)
		}
	}()
	// --- КОНЕЦ БЛОКА FIBER ---

	// 5. Потоковый цикл опроса базы данных
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	// Запускаем первый цикл сканирования базы сразу при старте
	activeCampaigns, err := campRepo.GetActiveCampaigns(ctx)
	if err == nil {
		for _, c := range activeCampaigns {
			campaignChan <- c
		}
	}

	for {
		select {
		case <-ticker.C:
			log.Println("--- Сканирование PostgreSQL на наличие активных АРК ---")
			activeCampaigns, err := campRepo.GetActiveCampaigns(ctx)
			if err != nil {
				log.Printf("Ошибка получения кампаний из БД: %v", err)
				continue
			}

			log.Printf("Найдено %d active кампаний для отправки в пул воркеров.", len(activeCampaigns))
			for _, c := range activeCampaigns {
				campaignChan <- c
			}
		case <-ctx.Done():
			close(campaignChan)
			wg.Wait()
			log.Println("Сервис биддинга полностью остановлен.")
			return
		}
	}
}
