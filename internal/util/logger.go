package util

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger is a global logger instance
var Logger *zap.Logger

// InitLogger initializes the global logger with the specified configuration
func InitLogger(config *LoggingConfig, nodeID string) error {
	var zapConfig zap.Config

	// Configure based on format preference
	if config.Format == "console" {
		zapConfig = zap.NewDevelopmentConfig()
		zapConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		zapConfig = zap.NewProductionConfig()
	}

	// Set log level
	level, err := zapcore.ParseLevel(config.Level)
	if err != nil {
		return err
	}
	zapConfig.Level = zap.NewAtomicLevelAt(level)

	// Build logger
	logger, err := zapConfig.Build()
	if err != nil {
		return err
	}

	// Add node ID to all log entries
	Logger = logger.With(zap.String("node_id", nodeID))

	return nil
}

// GetLogger returns the global logger instance
func GetLogger() *zap.Logger {
	if Logger == nil {
		// Fallback to a basic logger if not initialized
		Logger, _ = zap.NewProduction()
	}
	return Logger
}

// LogWithContext adds common context fields to a log entry
func LogWithContext(logger *zap.Logger, httpAddr, dhtAddr string) *zap.Logger {
	return logger.With(
		zap.String("http_addr", httpAddr),
		zap.String("dht_addr", dhtAddr),
	)
}
