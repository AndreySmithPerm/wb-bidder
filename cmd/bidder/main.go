package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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

	// 1. Обработка системных сигналов для Graceful Shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Загружаем конфигурацию (.env)
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	cleanDbURL := strings.ReplaceAll(cfg.DbURL, "*", "")

	// 2. Инициализируем пул соединений с PostgreSQL
	dbPool, err := pgxpool.New(ctx, cleanDbURL)
	if err != nil {
		log.Fatalf("Не удалось подключиться к PostgreSQL: %v", err)
	}
	defer dbPool.Close()

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
				localCamp := camp

				log.Printf("[Воркер %d] Кампания %d: проверка ключевой фразы '%s'", workerID, localCamp.ID, localCamp.Strategy.Keyword)

				workerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)

				// Шаг А: Парсим реальный рекламный аукцион
				realBids, err := wbCli.GetAuctionBids(workerCtx, localCamp.Strategy.Keyword)
				if err != nil {
					log.Printf("[Воркер %d] Ошибка парсинга аукциона для %d: %v", workerID, localCamp.ID, err)
					_ = logRepo.SaveLog(ctx, localCamp.ID, localCamp.CurrentBid, localCamp.CurrentBid, []int{}, "Ошибка парсинга аукциона")
					cancel()
					continue
				}

				oldBid := localCamp.CurrentBid

				// Шаг Б: Рассчитываем и отправляем новую ставку АРК на WB
				err = engine.CalculateAndApplyARC(workerCtx, &localCamp, realBids)

				statusMsg := "Ставка оптимальна"
				if err != nil {
					statusMsg = "Ошибка API WB: " + err.Error()
					log.Printf("[Воркер %d] Ошибка применения ставки для %d: %v", workerID, localCamp.ID, err)
				} else if oldBid != localCamp.CurrentBid {
					statusMsg = "Ставка успешно изменена"
					_ = campRepo.UpdateCampaignBid(ctx, localCamp.ID, localCamp.CurrentBid)
				}

				// Шаг В: Сохраняем результат в историю логов
				_ = logRepo.SaveLog(ctx, localCamp.ID, oldBid, localCamp.CurrentBid, realBids, statusMsg)
				cancel()
			}
		}(w)
	}

	// --- ИНТЕГРАЦИЯ WEB-API FIBER ---
	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
	})

	apiHandler := apiHttp.NewHandler(dbPool)
	app.Static("/", "./web")

	app.Get("/api/public/price", apiHandler.GetPublicPrice)
	app.Get("/api/status", apiHandler.GetStatus)
	app.Post("/api/auth/register", apiHandler.Register)
	app.Post("/api/auth/login", apiHandler.Login)
	app.Post("/api/auth/reset-password", apiHandler.ResetPassword)
	app.Post("/api/auth/save-password", apiHandler.SavePassword)
	app.Post("/api/payments/webhook", apiHandler.HandlePaymentWebhook)
	app.Get("/api/admin/stats", apiHandler.AuthMiddleware, apiHandler.AdminMiddleware, apiHandler.GetAdminStats)
	app.Post("/api/campaigns", apiHandler.AuthMiddleware, apiHandler.AddCampaign)
	app.Get("/api/campaigns/:id/logs", apiHandler.AuthMiddleware, apiHandler.GetCampaignLogs)
	app.Get("/api/user/profile", apiHandler.AuthMiddleware, apiHandler.GetUserProfile)

	go func() {
		log.Println("Запуск Web-API сервера на порту :8080")
		if err := app.Listen(":8080"); err != nil && err != context.Canceled {
			log.Printf("Уведомление веб-сервера: %v", err)
		}
	}()
	// --- КОНЕЦ БЛОКА FIBER ---

	// 5. Потоковый цикл опроса базы данных
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	if activeCampaigns, err := campRepo.GetActiveCampaigns(ctx); err == nil {
		for _, c := range activeCampaigns {
			select {
			case campaignChan <- c:
			default:
				log.Println("[Предупреждение] Очередь задач переполнена при старте")
			}
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

			log.Printf("Найдено %d активных кампаний.", len(activeCampaigns))
			for _, c := range activeCampaigns {
				select {
				case campaignChan <- c:
				default:
					log.Printf("[Внимание] Задача для кампании %d пропущена, очередь переполнена!", c.ID)
				}
			}

		case <-ctx.Done():
			log.Println("Получен сигнал остановки. Инициируется Graceful Shutdown...")
			if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
				log.Printf("Ошибка при принудительной остановке Fiber: %v", err)
			}
			close(campaignChan)
			wg.Wait()
			log.Println("Сервис биддинга полностью остановлен.")
			return
		}
	}
}
