package adapter

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const configFileName = "faf-sturm-relay.conf"

// LoadConfigFile reads the config file from the same directory as the
// executable. If the file does not exist, all values remain at their
// defaults and no error is logged — this is intentional so that players
// who never created the file are not confused by warnings.
//
// Supported keys (case-insensitive):
//
//	Use_official_FAF_connections_as_lobby_creator_host = true|false
func LoadConfigFile(cfg *Config, logger *slog.Logger) {
	exePath, err := os.Executable()
	if err != nil {
		return // can't determine exe path — silently skip
	}
	confPath := filepath.Join(filepath.Dir(exePath), configFileName)

	f, err := os.Open(confPath)
	if err != nil {
		// File not found is expected — don't log anything.
		return
	}
	defer f.Close()

	logger.Info("Loading config file", "path", confPath)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.ToLower(strings.TrimSpace(value))

		switch strings.ToLower(key) {
		case "use_official_faf_connections_as_lobby_creator_host":
			cfg.UseOfficialFAF = (value == "true" || value == "1" || value == "yes")
			logger.Info("Config: Use official FAF connections as host", "value", cfg.UseOfficialFAF)
		}
	}
}
