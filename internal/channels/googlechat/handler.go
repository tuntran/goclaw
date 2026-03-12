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
	if projectNumber == "" {
		slog.Warn("googlechat: OIDC verification disabled — webhook endpoint is unauthenticated, set project_number to enable")
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Temporary: log all headers for debugging auth issues
		slog.Info("googlechat: webhook request received",
			"method", r.Method,
			"has_auth", r.Header.Get("Authorization") != "",
			"host", r.Host,
			"x_forwarded_proto", r.Header.Get("X-Forwarded-Proto"),
			"x_forwarded_host", r.Header.Get("X-Forwarded-Host"))

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

		// Return 200 with empty JSON (Google Chat requires a JSON response body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))

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
// Google Chat sets the JWT audience to the webhook URL (not project number),
// so we reconstruct the full URL from the request to use as expected audience.
// The projectNumber is used as fallback if URL-based verification fails.
func verifyOIDC(r *http.Request, projectNumber string) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return fmt.Errorf("missing Authorization header")
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth {
		return fmt.Errorf("invalid Authorization format")
	}

	// Google Chat JWT aud = full webhook URL (e.g. https://host/googlechat/events).
	// Reconstruct from request headers (X-Forwarded-* set by reverse proxy).
	audience := buildRequestURL(r)
	if audience != "" {
		if _, err := idtoken.Validate(r.Context(), token, audience); err == nil {
			return nil
		}
	}

	// Fallback: try project number as audience (older Google Chat behavior).
	_, err := idtoken.Validate(r.Context(), token, projectNumber)
	return err
}

// buildRequestURL reconstructs the original full URL from request headers.
// Returns empty string if scheme/host cannot be determined.
func buildRequestURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host + r.URL.Path
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
