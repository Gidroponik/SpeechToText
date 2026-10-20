package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiConfig is everything needed to reach OpenAI.
type apiConfig struct {
	Key     string
	BaseURL string // https://api.openai.com/v1
	Proxy   string // http:// or socks5:// URL; "" = environment
}

func (c apiConfig) proxyFunc() (func(*http.Request) (*url.URL, error), error) {
	if c.Proxy == "" {
		return http.ProxyFromEnvironment, nil
	}
	u, err := url.Parse(c.Proxy)
	if err != nil || u.Host == "" {
		return nil, errors.New("неверный адрес прокси (пример: socks5://user:pass@host:1080)")
	}
	return http.ProxyURL(u), nil
}

func (c apiConfig) httpClient(timeout time.Duration) (*http.Client, error) {
	proxy, err := c.proxyFunc()
	if err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = proxy
	return &http.Client{Transport: t, Timeout: timeout}, nil
}

func (c apiConfig) base() string {
	b := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if b == "" {
		b = defaultBaseURL
	}
	return b
}

// realtimeURL is the WebSocket endpoint of a transcription session.
func (c apiConfig) realtimeURL() string {
	b := c.base()
	switch {
	case strings.HasPrefix(b, "https://"):
		b = "wss://" + b[len("https://"):]
	case strings.HasPrefix(b, "http://"):
		b = "ws://" + b[len("http://"):]
	}
	return b + "/realtime?intent=transcription"
}

// apiError is a failed OpenAI call, phrased for the user.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	switch {
	case e.Code == "unsupported_country_region_territory":
		return "OpenAI недоступен из вашего региона — включите VPN или укажите прокси в разделе API"
	case e.Code == "insufficient_quota":
		return "На балансе OpenAI закончились средства"
	case e.Status == 401 || e.Code == "invalid_api_key":
		return "Неверный API-ключ OpenAI"
	case e.Status == 403:
		return "Нет доступа: " + e.Message
	case e.Status == 404 || e.Code == "model_not_found":
		return "Модель недоступна для этого ключа: " + e.Message
	case e.Status == 429:
		return "Слишком много запросов к OpenAI — подождите немного"
	case e.Status >= 500:
		return "Сервер OpenAI временно недоступен"
	case e.Message != "":
		return e.Message
	}
	return fmt.Sprintf("OpenAI: HTTP %d", e.Status)
}

// readAPIError builds an apiError from an error response body.
func readAPIError(status int, body io.Reader) *apiError {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	_ = json.Unmarshal(b, &v)
	code, _ := v.Error.Code.(string)
	if code == "" {
		code = v.Error.Type
	}
	return &apiError{Status: status, Code: code, Message: v.Error.Message}
}

// netError rewrites transport failures into something actionable.
func netError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) || errors.Is(err, context.Canceled) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("OpenAI не отвечает (таймаут) — проверьте интернет, VPN или прокси")
	}
	return fmt.Errorf("нет связи с OpenAI: %v", err)
}

// checkKey confirms the key works and can use the transcription model.
func checkKey(ctx context.Context, c apiConfig) error {
	client, err := c.httpClient(20 * time.Second)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.base()+"/models/"+transcribeModel, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := client.Do(req)
	if err != nil {
		return netError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp.StatusCode, resp.Body)
	}
	return nil
}
