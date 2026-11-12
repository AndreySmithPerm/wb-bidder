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

// isNightTime проверяет, наступило ли ночное время (с 01:00 до 06:00 по МСК) для защиты от слива бюджета
func isNightTime() bool {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		loc = time.FixedZone("MSK", 3*60*60)
	}

	now := time.Now().In(loc)
	hour := now.Hour()

	return hour >= 1 && hour < 6
}

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

	// ИСПРАВЛЕНО: Приоритетное чтение строки подключения из системного окружения (.env сервера)
	cleanDbURL := os.Getenv("DATABASE_URL")
	if cleanDbURL == "" {
		// Если в системном окружении пусто (например, локальный запуск на ПК),
		// откатываемся на кастомный конфиг из пакета config
		cleanDbURL = strings.ReplaceAll(cfg.DbURL, "*", "")
	}

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

	// --- ЭТАП 1: МОК-РЕЖИМ WB API ---
	// WB_API_MOCK=true — фейковый аукцион и ставки без внешних запросов.
	// WB_API_MOCK=false (или не задана) — боевой режим.
	mockMode := os.Getenv("WB_API_MOCK") == "true"
	if mockMode {
		log.Println("=== РЕЖИМ МОКА WB API АКТИВЕН: внешние запросы отключены ===")
	}
	wbCli := wbclient.NewClient(mockMode)
	// ---------------------------------

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

				// === МЕТОД КОНТРОЛЯ ВРЕМЕНИ (ЗАЩИТА БЮДЖЕТА ПО НОЧАМ) ===
				if isNightTime() {
					log.Printf("[Воркер %d] Внимание: Активирован ночной режим защиты бюджета (МСК). Кампания %d переведена в режим ожидания.", workerID, localCamp.ID)

					saveCtx, saveCancel := context.WithTimeout(context.Background(), 4*time.Second)

					// Исправлено обращение: через .Strategy
					if localCamp.CurrentBid > localCamp.Strategy.MinBid {
						oldBid := localCamp.CurrentBid
						localCamp.CurrentBid = localCamp.Strategy.MinBid
						_ = campRepo.UpdateCampaignBid(saveCtx, localCamp.ID, localCamp.Strategy.MinBid)
						_ = logRepo.SaveLog(saveCtx, localCamp.ID, oldBid, localCamp.Strategy.MinBid, []int32{}, "Ночная экономия: ставка снижена до минимальной")
					} else {
						_ = logRepo.SaveLog(saveCtx, localCamp.ID, localCamp.CurrentBid, localCamp.CurrentBid, []int32{}, "Ночная пауза: удержание минимальной ставки")
					}

					saveCancel()
					continue
				}

				log.Printf("[Воркер %d] Кампания %d: проверка ключевой фразы '%s'", workerID, localCamp.ID, localCamp.Strategy.Keyword)

				workerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)

				// Шаг А: Парсим реальный рекламный аукцион Wildberries
				realBids, err := wbCli.GetAuctionBids(workerCtx, localCamp.Strategy.Keyword)
				if err != nil {
					log.Printf("[Воркер %d] Ошибка парсинга аукциона для %d: %v", workerID, localCamp.ID, err)
					saveCtx, saveCancel := context.WithTimeout(context.Background(), 3*time.Second)
					_ = logRepo.SaveLog(saveCtx, localCamp.ID, localCamp.CurrentBid, localCamp.CurrentBid, []int32{}, "Ошибка парсинга аукциона")
					saveCancel()
					cancel()
					continue
				}

				oldBid := localCamp.CurrentBid
				// Шаг Б: Рассчитываем и отправляем новую ставку АРК на WB
				err = engine.CalculateAndApplyARC(workerCtx, &localCamp, realBids)

				statusMsg := "Ставка оптимальна"
				saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)

				if err != nil {
					statusMsg = "Ошибка API WB: " + err.Error()
					log.Printf("[Воркер %d] Ошибка применения ставки для %d: %v", workerID, localCamp.ID, err)
				} else if oldBid != localCamp.CurrentBid {
					statusMsg = "Ставка успешно изменена"
					_ = campRepo.UpdateCampaignBid(saveCtx, localCamp.ID, localCamp.CurrentBid)
				}

				// Исправлено: Явное приведение []int к []int32 для логирования
				realBidsInt32 := make([]int32, len(realBids))
				for i, v := range realBids {
					realBidsInt32[i] = int32(v)
				}

				// Шаг В: Сохраняем результат в историю логов
				_ = logRepo.SaveLog(saveCtx, localCamp.ID, oldBid, localCamp.CurrentBid, realBidsInt32, statusMsg)
				saveCancel()
				cancel()
			}
		}(w)
	}

	// --- ИНТЕГРАЦИЯ WEB-API FIBER ---
	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
	})

	// Инициализируем mailer
	mailer := service.NewMailer(
		cfg.SmtpHost,
		cfg.SmtpPort,
		cfg.SmtpUser,
		cfg.SmtpPass,
		cfg.SmtpFromEmail,
		cfg.SmtpFromName,
		cfg.WebURL,
	)

	apiHandler := apiHttp.NewHandler(dbPool, mailer)
	app.Static("/", "./web")

	// Маршруты API (Все методы теперь строго соответствуют заглавным буквам в handler.go)
	app.Get("/api/captcha/script", apiHandler.ProxyYandexCaptcha)
	app.Get("/api/public/price", apiHandler.GetPublicPrice)
	app.Get("/api/status", apiHandler.GetStatus)
	app.Post("/api/auth/register", apiHandler.Register)
	app.Get("/api/auth/verify", apiHandler.VerifyEmail)
	app.Post("/api/auth/resend", apiHandler.ResendVerification)
	app.Post("/api/auth/login", apiHandler.Login)
	app.Post("/api/auth/forgot", apiHandler.ForgotPassword)
	app.Post("/api/auth/reset", apiHandler.ResetPassword)
	app.Post("/api/payments/webhook", apiHandler.HandlePaymentWebhook)
	app.Get("/api/admin/stats", apiHandler.AuthMiddleware, apiHandler.AdminMiddleware, apiHandler.GetAdminStats)
	app.Post("/api/campaigns", apiHandler.AuthMiddleware, apiHandler.AddCampaign)
	app.Get("/api/campaigns/:id/logs", apiHandler.AuthMiddleware, apiHandler.GetCampaignLogs)
	app.Get("/api/user/profile", apiHandler.AuthMiddleware, apiHandler.GetUserProfile)

	go func() {
		log.Println("Запуск Web-API сервера на порту :8080")
		if err := app.Listen(":8080"); err != nil {
			log.Printf("Уведомление веб-сервера (остановка): %v", err)
		}
	}()

	// Первичный запуск опроса при старте приложения
	if activeCampaigns, err := campRepo.GetActiveCampaigns(ctx); err == nil {
		for _, c := range activeCampaigns {
			select {
			case campaignChan <- c:
			default:
				log.Println("[Предупреждение] Очередь задач переполнена при старте")
			}
		}
	}

	// 5. Потоковый цикл опроса базы данных
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Println("--- Сканирование PostgreSQL на наличие активных АРК ---")
			activeCampaigns, err := campRepo.GetActiveCampaigns(ctx)
			if err != nil {
				log.Printf("Ошибка получения кампаний из БД: %v", err)
				continue
			}

			log.Printf("Найдено %d active кампаний.", len(activeCampaigns))
			for _, c := range activeCampaigns {
				select {
				case campaignChan <- c:
				default:
					log.Printf("[Внимание] Задача для кампании %d пропущена, queue переполнена!", c.ID)
				}
			}

		case <-ctx.Done():
			log.Println("Получен сигнал остановки. Инициируется Graceful Shutdown...")

			if err := app.ShutdownWithTimeout(5 * time.Second); err != nil {
				log.Printf("Ошибка при принудительной остановке Fiber: %v", err)
			}

			close(campaignChan)
			wg.Wait()

			log.Println("Сервис биддинга полностью успешно остановлен.")
			return
		}
	}
}
