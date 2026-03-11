package googlechat

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"
)

// NewWebhookHandler creates an http.HandlerFunc for Google Chat webhook events.
// projectNumber: Google Cloud project number for OIDC verification (empty = skip verify).
// onMessage: callback for MESSAGE events, invoked in a goroutine.
func NewWebhookHandler(projectNumber string, onMessage func(event *SpaceEvent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// OIDC verification
		if projectNumber != "" {
			if err := verifyOIDC(r, projectNumber); err != nil {
				slog.Warn("googlechat: OIDC verification failed", "error", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Read and parse body (limit 1MB to prevent abuse)
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}

		var event SpaceEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		// Return 200 immediately (Google retries on timeout)
		w.WriteHeader(http.StatusOK)

		// Process asynchronously
		switch event.Type {
		case "MESSAGE":
			if onMessage != nil {
				go onMessage(&event)
			}
		case "ADDED_TO_SPACE":
			slog.Info("googlechat: bot added to space",
				"space", event.Space.GetName(),
				"user", event.User.GetDisplayName())
		case "REMOVED_FROM_SPACE":
			slog.Info("googlechat: bot removed from space",
				"space", event.Space.GetName())
		default:
			slog.Debug("googlechat: unhandled event type", "type", event.Type)
		}
	}
}

// verifyOIDC validates the Google OIDC token from the Authorization header.
func verifyOIDC(r *http.Request, projectNumber string) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return fmt.Errorf("missing Authorization header")
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth {
		return fmt.Errorf("invalid Authorization format")
	}

	// idtoken.Validate caches keys internally
	_, err := idtoken.Validate(r.Context(), token, projectNumber)
	return err
}

// isBotMentioned checks if the bot was @mentioned in the message annotations.
func isBotMentioned(event *SpaceEvent) bool {
	if event.Message == nil {
		return false
	}
	for _, ann := range event.Message.Annotations {
		if ann.Type == "USER_MENTION" && ann.UserMention != nil {
			if ann.UserMention.User != nil && ann.UserMention.User.Type == "BOT" {
				return true
			}
		}
	}
	return false
}

// extractSenderInfo returns (senderID, displayName) from the event.
// senderID uses compound format: "users/123456789|DisplayName"
func extractSenderInfo(event *SpaceEvent) (string, string) {
	user := event.User
	if event.Message != nil && event.Message.Sender != nil {
		user = event.Message.Sender
	}
	if user == nil {
		return "", ""
	}

	displayName := user.DisplayName
	senderID := user.Name
	if displayName != "" {
		senderID = user.Name + "|" + displayName
	}
	return senderID, displayName
}
