package server

import "sync"

// SSEEvent represents an event to be sent to connected SSE clients.
type SSEEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
}

// Broker manages SSE client subscriptions and event fan-out.
type Broker struct {
	clients map[chan SSEEvent]bool
	mu      sync.RWMutex
}

func NewBroker() *Broker {
	return &Broker{
		clients: make(map[chan SSEEvent]bool),
	}
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
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			// Client can't keep up, drop event
		}
	}
}
