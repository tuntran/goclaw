package channels

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// RunContext tracks an active agent run for streaming/reaction event forwarding.
type RunContext struct {
	ChannelName       string
	ChatID            string
	MessageID         string            // platform message ID (string to support Feishu "om_xxx", Telegram "12345", etc.)
	Metadata          map[string]string // outbound routing metadata (thread_id, local_key, group_id)
	Streaming         bool              // whether run uses streaming (to avoid double-delivery of block replies)
	BlockReplyEnabled bool              // whether block.reply delivery is enabled for this run (resolved at RegisterRun time)
	ToolStatusEnabled bool              // whether tool name shows in streaming preview during tool execution
	mu                sync.Mutex
	streamBuffer      string // accumulated streaming text (chunks are deltas)
	inToolPhase       bool   // true after tool.call, reset on next chunk (new LLM iteration)
}

// Manager manages all registered channels, handling their lifecycle
// and routing outbound messages to the correct channel.
type Manager struct {
	channels         map[string]Channel
	bus              *bus.MessageBus
	runs             sync.Map // runID string → *RunContext
	dispatchTask     *asyncTask
	mu               sync.RWMutex
	contactCollector *store.ContactCollector
	webhookMap       map[string]http.Handler // path → current handler (protected by mu)
	mountedPaths     map[string]struct{}     // paths already on mux (protected by mu)
}

type asyncTask struct {
	cancel context.CancelFunc
}

// NewManager creates a new channel manager.
// Channels are registered externally via RegisterChannel.
func NewManager(msgBus *bus.MessageBus) *Manager {
	return &Manager{
		channels:     make(map[string]Channel),
		bus:          msgBus,
		webhookMap:   make(map[string]http.Handler),
		mountedPaths: make(map[string]struct{}),
	}
}

// StartAll starts all registered channels and the outbound dispatch loop.
// The dispatcher is always started even when no channels exist yet,
// because channels may be loaded dynamically later via Reload().
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Always start the outbound dispatcher — channels may be added later via Reload().
	dispatchCtx, cancel := context.WithCancel(ctx)
	m.dispatchTask = &asyncTask{cancel: cancel}
	go m.dispatchOutbound(dispatchCtx)

	if len(m.channels) == 0 {
		slog.Warn("no channels enabled")
		return nil
	}

	slog.Info("starting all channels")

	for name, channel := range m.channels {
		slog.Info("starting channel", "channel", name)
		if err := channel.Start(ctx); err != nil {
			slog.Error("failed to start channel", "channel", name, "error", err)
		}
	}

	slog.Info("all channels started")
	return nil
}

// StopAll gracefully stops all channels and the outbound dispatch loop.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	slog.Info("stopping all channels")

	if m.dispatchTask != nil {
		m.dispatchTask.cancel()
		m.dispatchTask = nil
	}

	for name, channel := range m.channels {
		slog.Info("stopping channel", "channel", name)
		if err := channel.Stop(ctx); err != nil {
			slog.Error("error stopping channel", "channel", name, "error", err)
		}
	}

	slog.Info("all channels stopped")
	return nil
}

// GetChannel returns a channel by name.
func (m *Manager) GetChannel(name string) (Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	channel, ok := m.channels[name]
	return channel, ok
}

// GetStatus returns the running status of all channels.
func (m *Manager) GetStatus() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := make(map[string]any)
	for name, channel := range m.channels {
		status[name] = map[string]any{
			"enabled": true,
			"running": channel.IsRunning(),
		}
	}
	return status
}

// GetEnabledChannels returns the names of all enabled channels.
func (m *Manager) GetEnabledChannels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.channels))
	for name := range m.channels {
		names = append(names, name)
	}
	return names
}

// RegisterChannel adds a channel to the manager.
func (m *Manager) RegisterChannel(name string, channel Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Propagate contact collector to channels that embed BaseChannel.
	if m.contactCollector != nil {
		if bc, ok := channel.(interface{ SetContactCollector(*store.ContactCollector) }); ok {
			bc.SetContactCollector(m.contactCollector)
		}
	}
	m.channels[name] = channel
	m.rebuildWebhookMap()
}

// SetContactCollector sets the contact collector for all current and future channels.
func (m *Manager) SetContactCollector(cc *store.ContactCollector) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contactCollector = cc
	for _, ch := range m.channels {
		if bc, ok := ch.(interface{ SetContactCollector(*store.ContactCollector) }); ok {
			bc.SetContactCollector(cc)
		}
	}
}

// ChannelTypeForName returns the platform type for a channel instance name.
// Reads directly from the Channel.Type() method — no separate map needed.
func (m *Manager) ChannelTypeForName(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ch, ok := m.channels[name]; ok {
		return ch.Type()
	}
	return ""
}

// UnregisterChannel removes a channel from the manager.
func (m *Manager) UnregisterChannel(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.channels, name)
	m.rebuildWebhookMap()
}

// rebuildWebhookMap rebuilds path→handler map from current channels.
// Caller must hold m.mu write lock.
func (m *Manager) rebuildWebhookMap() {
	m.webhookMap = make(map[string]http.Handler)
	for name, ch := range m.channels {
		if wh, ok := ch.(WebhookChannel); ok {
			if path, handler := wh.WebhookHandler(); path != "" && handler != nil {
				if _, exists := m.webhookMap[path]; exists {
					slog.Warn("webhook path collision: multiple channels claim same path", "path", path, "channel", name)
				}
				m.webhookMap[path] = handler
			}
		}
	}
}

// WebhookServeHTTP dynamically dispatches webhook requests to the current channel handler.
func (m *Manager) WebhookServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	h := m.webhookMap[r.URL.Path]
	m.mu.RUnlock()
	if h != nil {
		h.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// MountNewWebhookRoutes mounts wrapper handlers for any webhook paths not yet on the mux.
func (m *Manager) MountNewWebhookRoutes(mux *http.ServeMux) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for path := range m.webhookMap {
		if _, ok := m.mountedPaths[path]; ok {
			continue
		}
		mux.Handle(path, http.HandlerFunc(m.WebhookServeHTTP))
		m.mountedPaths[path] = struct{}{}
		slog.Info("webhook route mounted on gateway", "path", path)
	}
}
