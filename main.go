package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

const (
	defaultTTLMinutes = 10
	redisKeyPrefix    = "tg:code:"
)

// TelegramCodePayload Redis ichida saqlanadigan JSON value strukturasidir.
type TelegramCodePayload struct {
	TelegramID       int64  `json:"telegram_id"`
	TelegramUsername string `json:"telegram_username"`
	TelegramName     string `json:"telegram_name"`
}

func main() {
	// .env fayli bo'lmasa ham dasturni to'xtatmaymiz, chunki productionda env tashqaridan beriladi.
	_ = godotenv.Load()

	botToken := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if botToken == "" {
		log.Fatal("BOT_TOKEN is required")
	}

	redisDB, err := parseRedisDB(os.Getenv("REDIS_DB"))
	if err != nil {
		log.Fatalf("invalid REDIS_DB: %v", err)
	}

	ttlMinutes, err := parseTTLMinutes(os.Getenv("CODE_TTL_MINUTES"))
	if err != nil {
		log.Fatalf("invalid CODE_TTL_MINUTES: %v", err)
	}
	codeTTL := time.Duration(ttlMinutes) * time.Minute

	rdb := redis.NewClient(&redis.Options{
		Addr:     strings.TrimSpace(os.Getenv("REDIS_ADDR")),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       redisDB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping failed: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("telegram bot init failed: %v", err)
	}

	log.Printf("Bot started as @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		if update.Message.IsCommand() && update.Message.Command() == "start" {
			handleStartCommand(bot, rdb, update.Message, codeTTL)
			continue
		}

		if update.Message.Text != "" {
			handleTextMessage(bot, rdb, update.Message, codeTTL)
		}
	}
}

func handleStartCommand(bot *tgbotapi.BotAPI, rdb *redis.Client, msg *tgbotapi.Message, ttl time.Duration) {
	processCodeRequest(bot, rdb, msg, ttl)
}

func handleTextMessage(bot *tgbotapi.BotAPI, rdb *redis.Client, msg *tgbotapi.Message, ttl time.Duration) {
	processCodeRequest(bot, rdb, msg, ttl)
}

func processCodeRequest(bot *tgbotapi.BotAPI, rdb *redis.Client, msg *tgbotapi.Message, ttl time.Duration) {
	ctx := context.Background()

	payload := TelegramCodePayload{
		TelegramID:       msg.From.ID,
		TelegramUsername: msg.From.UserName,
		TelegramName:     buildTelegramName(msg.From.FirstName, msg.From.LastName),
	}

	if payload.TelegramUsername == "" {
		payload.TelegramUsername = ""
	}

	code, err := generateUniqueCode(ctx, rdb)
	if err != nil {
		log.Printf("failed to generate unique code for user_id=%d: %v", msg.From.ID, err)
		sendSoftError(bot, msg.Chat.ID)
		return
	}

	if err := saveCodeToRedis(ctx, rdb, code, payload, ttl); err != nil {
		log.Printf("failed to save code=%s user_id=%d: %v", code, msg.From.ID, err)
		sendSoftError(bot, msg.Chat.ID)
		return
	}

	text := fmt.Sprintf(
		"Saytdagi profilingizni ulash uchun ushbu kodni kiriting: %s\nKod %d daqiqa amal qiladi.",
		code,
		int(ttl.Minutes()),
	)

	if _, err := bot.Send(tgbotapi.NewMessage(msg.Chat.ID, text)); err != nil {
		log.Printf("failed to send message to chat_id=%d: %v", msg.Chat.ID, err)
	}
}

func sendSoftError(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "Kechirasiz, hozircha ichki xatolik yuz berdi. Iltimos, birozdan so'ng qayta urinib ko'ring.")
	if _, err := bot.Send(msg); err != nil {
		log.Printf("failed to send error message to chat_id=%d: %v", chatID, err)
	}
}

func parseRedisDB(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.Atoi(raw)
}

func parseTTLMinutes(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultTTLMinutes, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("must be greater than 0")
	}
	return v, nil
}

// generate6DigitCode faqat raqamlardan iborat 6 xonali kod qaytaradi.
func generate6DigitCode() (string, error) {
	max := big.NewInt(900000) // 0..899999
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()+100000), nil
}

func generateUniqueCode(ctx context.Context, rdb *redis.Client) (string, error) {
	const maxAttempts = 5

	for i := 0; i < maxAttempts; i++ {
		code, err := generate6DigitCode()
		if err != nil {
			return "", err
		}

		key := redisKeyPrefix + code
		exists, err := rdb.Exists(ctx, key).Result()
		if err != nil {
			return "", err
		}
		if exists == 0 {
			return code, nil
		}
	}

	return "", fmt.Errorf("could not generate unique code after retries")
}

// buildTelegramName first_name + last_name dan toza fullname yasaydi.
func buildTelegramName(firstName, lastName string) string {
	return strings.TrimSpace(strings.TrimSpace(firstName) + " " + strings.TrimSpace(lastName))
}

// saveCodeToRedis kodni bir martalik verify flow uchun Redis'ga TTL bilan yozadi.
func saveCodeToRedis(ctx context.Context, rdb *redis.Client, code string, payload TelegramCodePayload, ttl time.Duration) error {
	key := redisKeyPrefix + code

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return rdb.Set(ctx, key, body, ttl).Err()
}
