package googlechat

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// resolveServiceAccountJSON returns SA JSON bytes from inline content or file path.
func resolveServiceAccountJSON(cfg config.GoogleChatConfig) ([]byte, error) {
	if s := strings.TrimSpace(cfg.ServiceAccountJSON); s != "" {
		data := []byte(s)
		if !json.Valid(data) {
			return nil, fmt.Errorf("service_account_json is not valid JSON")
		}
		return data, nil
	}
	if cfg.ServiceAccountFile != "" {
		data, err := os.ReadFile(cfg.ServiceAccountFile)
		if err != nil {
			return nil, fmt.Errorf("read service account file: %w", err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("service_account_json or service_account_file is required")
}
