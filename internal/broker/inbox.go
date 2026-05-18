package broker

import (
	"encoding/json"
	"sync"
)

// InboxMessage is pushed to a subscriber's channel.
type InboxMessage struct {
	TaskID    string          `json:"task_id"`
	ContextID string          `json:"context_id"`
	Payload   json.RawMessage `json:"payload"` // MessageSendParams JSON
}

// InboxHub keeps one subscriber channel per online agent for real-time delivery.
// Only one active SSE connection per agent at a time — if a second opens, the
// first one is closed.
type InboxHub struct {
	mu   sync.Mutex
	subs map[string]chan *InboxMessage
}

// NewInboxHub creates an empty hub.
func NewInboxHub() *InboxHub {
	return &InboxHub{subs: make(map[string]chan *InboxMessage)}
}

// Subscribe registers a new subscriber for agentID. If one already exists, the
// previous channel is closed. The returned channel is buffered.
func (h *InboxHub) Subscribe(agentID string) <-chan *InboxMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev, ok := h.subs[agentID]; ok {
		close(prev)
	}
	ch := make(chan *InboxMessage, 16)
	h.subs[agentID] = ch
	return ch
}

// Unsubscribe removes the given subscription if it's still the current one.
func (h *InboxHub) Unsubscribe(agentID string, ch <-chan *InboxMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, ok := h.subs[agentID]
	if !ok {
		return
	}
	// compare via channel identity
	var curRO <-chan *InboxMessage = cur
	if curRO == ch {
		close(cur)
		delete(h.subs, agentID)
	}
}

// Publish pushes a message to the agent's subscriber, if any. Returns true if
// the message was delivered, false otherwise (agent offline).
func (h *InboxHub) Publish(agentID string, msg *InboxMessage) bool {
	h.mu.Lock()
	ch, ok := h.subs[agentID]
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

// IsOnline reports whether an agent currently has an active subscriber.
func (h *InboxHub) IsOnline(agentID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.subs[agentID]
	return ok
}
