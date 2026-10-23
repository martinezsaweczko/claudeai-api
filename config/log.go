package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type LogConfig struct {
	Level    string
	FilePath string
}

func (l *LogConfig) validate() error {
	validLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}
	if !validLevels[l.Level] {
		return fmt.Errorf("invalid log level: %s", l.Level)
	}

	// If file path is provided, we could add additional validation here (e.g., check if the directory exists)

	filePath := strings.TrimSpace(l.FilePath)
	if filePath != "" {
		dir := filepath.Dir(filePath)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("log file directory does not exist: %s", dir)
		}
	}

	return nil
}

// ParseLogLevel converts a string log level to slog.Level
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo // Default to info if invalid level is provided
	}
}

// LogFilePath opens the log file for writing, or returns stdout if "stdout" is specified
func LogFilePath(filePath string) (*os.File, error) {
	if filePath == "stdout" {
		return os.Stdout, nil
	}
	return os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
}
