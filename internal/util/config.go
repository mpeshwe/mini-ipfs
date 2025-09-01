package util

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config holds all configuration for the mini-ipfs node
type Config struct {
	Node    NodeConfig    `mapstructure:"node"`
	DHT     DHTConfig     `mapstructure:"dht"`
	Storage StorageConfig `mapstructure:"storage"`
	API     APIConfig     `mapstructure:"api"`
	Logging LoggingConfig `mapstructure:"logging"`
}

type NodeConfig struct {
	ID            string `mapstructure:"id"`
	HTTPAddr      string `mapstructure:"http_addr"`
	DHTAddr       string `mapstructure:"dht_addr"`
	DataDir       string `mapstructure:"data_dir"`
	AdvertiseHost string `mapstructure:"advertise_host"`
}

type DHTConfig struct {
	K              int      `mapstructure:"k"`
	Alpha          int      `mapstructure:"alpha"`
	BootstrapNodes []string `mapstructure:"bootstrap_nodes"`
}

type StorageConfig struct {
	ChunkSize         int64 `mapstructure:"chunk_size"`
	MaxStorageBytes   int64 `mapstructure:"max_storage_bytes"`
	ReplicationFactor int   `mapstructure:"replication_factor"`
}

type APIConfig struct {
	EnableAuth bool `mapstructure:"enable_auth"`
	RateLimit  int  `mapstructure:"rate_limit"`
}

type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

// LoadConfig reads configuration from environment variables and config files
func LoadConfig() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./configs")
	viper.AddConfigPath(".")

	// Environment variable support with MINI_IPFS prefix
	viper.SetEnvPrefix("MINI_IPFS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Set defaults
	setDefaults()

	// Try to read config file (it's optional)
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("error reading config file: %w", err)
		}
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	// Generate node ID if not provided
	if config.Node.ID == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "unknown"
		}
		config.Node.ID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	return &config, nil
}

func setDefaults() {
	// Node defaults
	viper.SetDefault("node.http_addr", ":8080")
	viper.SetDefault("node.dht_addr", ":7000")
	viper.SetDefault("node.data_dir", "./data")

	// DHT defaults
	viper.SetDefault("dht.k", 20)
	viper.SetDefault("dht.alpha", 3)
	viper.SetDefault("dht.bootstrap_nodes", []string{})

	// Storage defaults
	viper.SetDefault("storage.chunk_size", 1048576)       // 1MB
	viper.SetDefault("storage.max_storage_bytes", 10<<30) // 10GB
	viper.SetDefault("storage.replication_factor", 3)

	// API defaults
	viper.SetDefault("api.enable_auth", false)
	viper.SetDefault("api.rate_limit", 100)

	// Logging defaults
	viper.SetDefault("logging.level", "info")
	viper.SetDefault("logging.format", "json")

	viper.SetDefault("node.advertise_host", "")
}
