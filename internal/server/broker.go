package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
)

// SSEEvent represents an event to be sent to connected SSE clients.
type SSEEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
}

type webhook struct {
	url    string
	events map[string]bool // empty means all events
	secret string
}

// Broker manages SSE client subscriptions, event fan-out, and webhook delivery.
type Broker struct {
	clients  map[chan SSEEvent]bool
	webhooks []webhook
	mu       sync.RWMutex
}

func NewBroker() *Broker {
	return &Broker{
		clients: make(map[chan SSEEvent]bool),
	}
}

// AddWebhook registers an HTTP endpoint to receive events.
func (b *Broker) AddWebhook(cfg config.WebhookConfig) {
	wh := webhook{
		url:    cfg.URL,
		secret: cfg.Secret,
	}
	if len(cfg.Events) > 0 {
		wh.events = make(map[string]bool)
		for _, e := range cfg.Events {
			wh.events[e] = true
		}
	}
	b.webhooks = append(b.webhooks, wh)
}

func (b *Broker) Subscribe() chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan SSEEvent) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *Broker) Publish(event SSEEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Fan out to SSE clients
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			// Client can't keep up, drop event
		}
	}

	// Deliver to webhooks (fire-and-forget)
	for _, wh := range b.webhooks {
		if wh.events != nil && !wh.events[event.Type] {
			continue
		}
		go deliverWebhook(wh, event)
	}
}

func deliverWebhook(wh webhook, event SSEEvent) {
	payload, _ := json.Marshal(map[string]string{
		"id":   event.ID,
		"type": event.Type,
		"data": event.Data,
	})

	req, err := http.NewRequest("POST", wh.url, bytes.NewReader(payload))
	if err != nil {
		slog.Debug("Webhook request error", "url", wh.url, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	if wh.secret != "" {
		mac := hmac.New(sha256.New, []byte(wh.secret))
		mac.Write(payload)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Pilot-Signature", sig)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("Webhook delivery failed", "url", wh.url, "error", err)
		return
	}
	resp.Body.Close()
}
