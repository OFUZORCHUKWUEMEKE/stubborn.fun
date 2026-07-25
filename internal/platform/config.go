package platform

import "os"

// Config is the process-wide configuration, loaded from environment
// variables with sane local-dev defaults. No config file / flag parsing
// yet — add it when a second deployment target needs it.
type Config struct {
	HTTPAddr string
	Mongo    MongoConfig
}

func LoadConfig() Config {
	return Config{
		HTTPAddr: getenv("STUBBORN_HTTP_ADDR", ":8080"),
		Mongo: MongoConfig{
			URI:      getenv("STUBBORN_MONGO_URI", "mongodb://localhost:27017/?replicaSet=rs0"),
			Database: getenv("STUBBORN_MONGO_DB", "stubborn"),
		},
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
