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
	"strings"
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
}

func sendMessage(message string) error {
	client := &http.Client{}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TelegramToken)
	data := fmt.Sprintf(`{"chat_id": "%s", "text": "%s"}`, TelegramChatID, message)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Запрос в API Telegram завершился с кодом статуса: %d", resp.StatusCode)
	}
	return nil
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
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Запрос к API завершился с кодом статуса: %d", resp.StatusCode)
	}
	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		return nil, err
	}
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

func runBot() {
	currentTimestamp := time.Now().Unix()
	currentReport := Homework{}
	prevReport := Homework{}

	for {
		response, err := getAPIAnswer(currentTimestamp)
		if err != nil {
			logger.Printf("Не удалось получить ответ от API: %v", err)
			time.Sleep(RetryPeriod)
			continue
		}

		currentTimestamp = int64(response["current_date"].(float64))
		newHomeworks, err := checkResponse(response)
		if err != nil {
			logger.Printf("Неверный ответ от API: %v", err)
			time.Sleep(RetryPeriod)
			continue
		}

		if len(newHomeworks) > 0 {
			currentReport = newHomeworks[0]
		} else {
			currentReport = Homework{Status: "Нет новых статусов работ."}
		}

		if currentReport.Status != prevReport.Status {
			message, err := parseStatus(currentReport)
			if err != nil {
				logger.Printf("Ошибка при разборе статуса домашней работы: %v", err)
			} else {
				err = sendMessage(message)
				if err != nil {
					logger.Printf("Ошибка при отправке сообщения: %v", err)
				}
			}
			prevReport = currentReport
		}

		time.Sleep(RetryPeriod)
	}
}

func main() {
	go runBot()

	// Обработка сигнала для корректного завершения приложения
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)
	<-stopChan

	logger.Println("Бот остановлен")
}
