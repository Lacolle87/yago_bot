package main

import (
	"encoding/json"
	"errors"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const (
	Endpoint        = "https://practicum.yandex.ru/api/user_api/homework_statuses/"
	RetryPeriod     = 600 * time.Second
	ApprovedStatus  = "approved"
	ReviewingStatus = "reviewing"
	RejectedStatus  = "rejected"
)

var (
	PracticumToken  string
	TelegramToken   string
	TelegramChatID  string
	HomeworkVerdict map[string]string
	logger          *log.Logger
	bot             *tgbotapi.BotAPI
)

type Homework struct {
	Name   string `json:"homework_name"`
	Status string `json:"status"`
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

	logFile, err := os.OpenFile("bot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Ошибка при открытии файла журнала:", err)
	}

	logger = log.New(io.MultiWriter(os.Stdout, logFile), "BOT: ", log.Ldate|log.Ltime|log.Lshortfile)
	logger.Println("Бот запущен")

	bot, err = tgbotapi.NewBotAPI(TelegramToken)
	if err != nil {
		log.Fatal("Ошибка при создании экземпляра бота:", err)
	}
	//bot.Debug = true
	logger.Printf("Авторизован как @%s", bot.Self.UserName)
}

func sendMessage(chatID int64, message string) error {
	msg := tgbotapi.NewMessage(chatID, message)
	_, err := bot.Send(msg)
	return err
}

func getAPIAnswer(currentTimestamp int64) (map[string]interface{}, error) {
	client := &http.Client{}
	url := fmt.Sprintf("%s?from_date=%d", Endpoint, currentTimestamp)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("OAuth %s", PracticumToken))
	resp, err := client.Do(req)
	if err != nil {
		logger.Printf("Ошибка при выполнении запроса к API: %v", err)
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Printf("Ошибка при закрытии тела ответа: %v", err)
		}
	}(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logger.Printf("Запрос к API завершился с кодом статуса: %d", resp.StatusCode)
		return nil, fmt.Errorf("Запрос к API завершился с кодом статуса: %d", resp.StatusCode)
	}
	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		logger.Printf("Ошибка при декодировании ответа от API: %v", err)
		return nil, err
	}
	logger.Println("Успешно получен ответ от API")
	return data, nil
}

func checkResponse(response map[string]interface{}) ([]Homework, error) {
	homeworksJSON, ok := response["homeworks"].([]interface{})
	if !ok {
		return nil, errors.New("Неверный формат ответа: поле 'homeworks' не является списком")
	}

	homeworks := make([]Homework, len(homeworksJSON))
	for i, hwJSON := range homeworksJSON {
		hwMap, ok := hwJSON.(map[string]interface{})
		if !ok {
			return nil, errors.New("Неверный формат ответа: элемент 'homework' не является словарем")
		}

		hwName, ok := hwMap["homework_name"].(string)
		if !ok {
			return nil, errors.New("Неверный формат ответа: поле 'homework_name' не является строкой")
		}

		hwStatus, ok := hwMap["status"].(string)
		if !ok {
			return nil, errors.New("Неверный формат ответа: поле 'status' не является строкой")
		}

		homeworks[i] = Homework{Name: hwName, Status: hwStatus}
	}

	return homeworks, nil
}

func parseStatus(homework Homework) (string, error) {
	verdict, ok := HomeworkVerdict[homework.Status]
	if !ok {
		if homework.Status == "Нет новых статусов работ." {
			logger.Println(homework.Status)
			return homework.Status, nil
		}
		errMsg := fmt.Sprintf("Неизвестный статус домашней работы: %s", homework.Status)
		logger.Printf("Ошибка при разборе статуса домашней работы: %s", errMsg)
		return "", errors.New(errMsg)
	}
	return fmt.Sprintf(`Изменился статус проверки работы "%s": %s`, homework.Name, verdict), nil
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

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Произошла ошибка:", r)
		}
	}()

	logger.Println("Бот начал работу")

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Fatal("Ошибка при получении канала обновлений:", err)
	}

	go handleUpdates(updates)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	logger.Println("Бот остановлен")
}
