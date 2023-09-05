package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/joho/godotenv"
)

const (
	Endpoint        = "https://practicum.yandex.ru/api/user_api/homework_statuses/"
	RetryPeriod     = 100 * time.Second
	ApprovedStatus  = "approved"
	ReviewingStatus = "reviewing"
	RejectedStatus  = "rejected"
)

var (
	PracticumToken  string
	TelegramToken   string
	TelegramChatID  string
	lastSentMessage string
	HomeworkVerdict map[string]string
	logger          *log.Logger
	bot             *tgbotapi.BotAPI
)

type Homework struct {
	Name            string `json:"homework_name"`
	Status          string `json:"status"`
	ReviewerComment string `json:"reviewer_comment"`
	LessonName      string `json:"lesson_name"`
}

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Ошибка при загрузке файла .env")
	}

	PracticumToken = os.Getenv("PRACTICUM_TOKEN")
	TelegramToken = os.Getenv("TELEGRAM_TOKEN")
	TelegramChatID = os.Getenv("TELEGRAM_CHAT_ID")

	if PracticumToken == "" || TelegramToken == "" || TelegramChatID == "" {
		log.Fatal("Отсутствуют переменные окружения")
	}

	HomeworkVerdict = map[string]string{
		ApprovedStatus:  "Работа проверена: ревьюеру всё понравилось. Ура!",
		ReviewingStatus: "Работа взята на проверку ревьюером.",
		RejectedStatus:  "Работа проверена: у ревьюера есть замечания.",
	}

	logFile, err := os.OpenFile("logs/bot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Ошибка при открытии файла журнала:", err)
	}

	logger = log.New(io.MultiWriter(os.Stdout, logFile), "BOT: ", log.Ldate|log.Ltime|log.Lshortfile)
	logger.Println("Бот запущен")

	bot, err = tgbotapi.NewBotAPI(TelegramToken)
	if err != nil {
		log.Fatal("Ошибка при создании экземпляра бота:", err)
	}
	logger.Printf("Авторизован как @%s", bot.Self.UserName)
}

func sendMessage(chatID int64, message string) error {
	msg := tgbotapi.NewMessage(chatID, message)
	_, err := bot.Send(msg)
	return err
}

func getAPIAnswer(currentTimestamp int64) (map[string]interface{}, error) {
	timestamp := currentTimestamp - 3600
	if currentTimestamp == 0 {
		timestamp = time.Now().Unix()
	}

	url := fmt.Sprintf("%s?from_date=%d", Endpoint, timestamp)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("OAuth %s", PracticumToken))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Ошибка при выполнении запроса к API: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Запрос к API завершился с кодом статуса: %d", resp.StatusCode)
		return nil, fmt.Errorf("запрос к API завершился с кодом статуса: %d", resp.StatusCode)
	}

	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		log.Printf("Ошибка при декодировании ответа от API: %v", err)
		return nil, err
	}

	log.Println("Успешно получен ответ от API")
	return data, nil
}

func checkResponse(response map[string]interface{}) ([]Homework, error) {
	homeworksJSON, ok := response["homeworks"].([]interface{})
	if !ok {
		return nil, errors.New("неверный формат ответа: поле 'homeworks' не является списком")
	}

	homeworks := make([]Homework, len(homeworksJSON))
	for i, hwJSON := range homeworksJSON {
		hwMap, ok := hwJSON.(map[string]interface{})
		if !ok {
			return nil, errors.New("неверный формат ответа: элемент 'homework' не является словарем")
		}

		hwName, ok := hwMap["homework_name"].(string)
		if !ok {
			return nil, errors.New("неверный формат ответа: поле 'homework_name' не является строкой")
		}

		hwStatus, ok := hwMap["status"].(string)
		if !ok {
			return nil, errors.New("неверный формат ответа: поле 'status' не является строкой")
		}

		hwReviewerComment, _ := hwMap["reviewer_comment"].(string)
		hwLessonName, _ := hwMap["lesson_name"].(string)

		homeworks[i] = Homework{
			Name:            hwName,
			Status:          hwStatus,
			ReviewerComment: hwReviewerComment,
			LessonName:      hwLessonName,
		}
	}

	return homeworks, nil
}

func parseStatus(homework Homework) (string, error) {
	homeworkName := homework.Name
	homeworkStatus := homework.Status
	homeworkReviewerComment := homework.ReviewerComment
	homeworkLessonName := homework.LessonName

	switch homeworkStatus {
	case ApprovedStatus, ReviewingStatus, RejectedStatus:
		verdict := HomeworkVerdict[homeworkStatus]

		message := fmt.Sprintf(`Изменился статус проверки работы "%s" для урока "%s": %s
Комментарий ревьюера: %s`, homeworkName, homeworkLessonName, verdict, homeworkReviewerComment)
		return message, nil
	default:
		errMsg := fmt.Sprintf("Неизвестный статус домашней работы: %s", homeworkStatus)
		logger.Printf("Ошибка при разборе статуса домашней работы: %s", errMsg)
		return "", errors.New(errMsg)
	}
}

func handleCommand(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		sendMessage(msg.Chat.ID, "Привет! Я бот, который отслеживает статус проверки домашних работ.")
		logger.Printf("Получена команда /start от пользователя с ID %d\n", msg.From.ID)
	case "status":
		go func() {
			currentTimestamp := time.Now().Unix()
			response, err := getAPIAnswer(currentTimestamp)
			if err != nil {
				logger.Printf("Не удалось получить ответ от API: %v", err)
				sendMessage(msg.Chat.ID, "Не удалось получить статус домашних работ.")
				return
			}

			currentTimestamp = int64(response["current_date"].(float64))
			newHomeworks, err := checkResponse(response)
			if err != nil {
				logger.Printf("Неверный ответ от API: %v", err)
				sendMessage(msg.Chat.ID, "Не удалось получить статус домашних работ.")
				return
			}

			if len(newHomeworks) > 0 {
				currentReport := newHomeworks[0]
				message, err := parseStatus(currentReport)
				if err != nil {
					sendMessage(msg.Chat.ID, "Не удалось получить статус домашних работ.")
					return
				}

				sendMessage(msg.Chat.ID, message)
				logger.Printf("Получена команда /status от пользователя с ID %d\n", msg.From.ID)
				logger.Printf("Результат запроса к API: %s\n", message)
			} else {
				sendMessage(msg.Chat.ID, "Нет новых статусов работ.")
				logger.Printf("Получена команда /status от пользователя с ID %d\n", msg.From.ID)
				logger.Println("Результат запроса к API: Нет новых статусов работ.")
			}
		}()
	}
}

func handleUpdates(updates tgbotapi.UpdatesChannel) {
	for update := range updates {
		if update.Message != nil {
			if update.Message.IsCommand() {
				handleCommand(update.Message)
			}
		}
	}
}

func fetchAPIResponse() ([]Homework, error) {
	currentTimestamp := time.Now().Unix()
	response, err := getAPIAnswer(currentTimestamp)
	if err != nil {
		logger.Printf("Не удалось получить ответ от API: %v", err)
		return nil, err
	}

	currentTimestamp = int64(response["current_date"].(float64))
	newHomeworks, err := checkResponse(response)
	if err != nil {
		logger.Printf("Неверный ответ от API: %v", err)
		return nil, err
	}

	if len(newHomeworks) > 0 {
		currentReport := newHomeworks[0]
		message, err := parseStatus(currentReport)
		if err != nil {
			return nil, err
		}

		// Проверка, было ли отправлено сообщение с таким же статусом ранее
		if message != lastSentMessage {
			// Отправить сообщение в Telegram
			chatID, err := strconv.ParseInt(TelegramChatID, 10, 64)
			if err != nil {
				logger.Printf("Ошибка при преобразовании TelegramChatID в int64: %v", err)
				return nil, err
			}

			err = sendMessage(chatID, message)
			if err != nil {
				logger.Printf("Ошибка при отправке сообщения в Telegram: %v", err)
			} else {
				logger.Printf("Сообщение отправлено в Telegram: %s", message)
				lastSentMessage = message // Обновление последнего отправленного сообщения
			}
		}
	} else {
		logger.Println("Результат запроса к API: Нет новых статусов работ.")
	}

	return newHomeworks, nil
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Произошла ошибка:", r)
		}
	}()

	logger.Println("Бот начал работу")

	// Запрос к API перед обработкой обновлений
	go func() {
		_, err := fetchAPIResponse()
		if err != nil {
			return
		}
	}()

	ticker := time.NewTicker(RetryPeriod)

	go func() {
		for range ticker.C {
			_, err := fetchAPIResponse()
			if err != nil {
				continue
			}
		}
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		logger.Fatal(err)
	}

	go handleUpdates(updates)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	logger.Println("Бот остановлен")
	ticker.Stop()
}
