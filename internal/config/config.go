package config

import (
	"os"
)

type Config struct {
	Port         string
	DebugEnabled bool
	SessionID    string
	ClientCookie string
	ClientUat    string
	ProjectID    string
	UserID       string
	AgentMode    string
	Email        string
	AdminUser    string
	AdminPass    string
	AdminPath    string
}

func Load() *Config {
	return &Config{
		Port:         getEnv("PORT", "8080"),
		DebugEnabled: getEnv("DEBUG_ENABLED", "false") == "true",
		SessionID:    getEnv("SESSION_ID", ""),
		ClientCookie: getEnv("CLIENT_COOKIE", ""),
		ClientUat:    getEnv("CLIENT_UAT", ""),
		ProjectID:    getEnv("PROJECT_ID", ""),
		UserID:       getEnv("USER_ID", ""),
		AgentMode:    getEnv("AGENT_MODE", "claude-opus-4.5"),
		Email:        getEnv("EMAIL", ""),
		AdminUser:    getEnv("ADMIN_USER", "admin"),
		AdminPass:    getEnv("ADMIN_PASS", "admin"),
		AdminPath:    getEnv("ADMIN_PATH", "/admin"),
	}
}

func requireEnv(key string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	panic("required environment variable not set: " + key)
}

func (c *Config) GetCookies() string {
	return "__client=" + c.ClientCookie + "; __client_uat=" + c.ClientUat
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
