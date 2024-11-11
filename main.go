package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/fiatjaf/khatru"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/nbd-wtf/go-nostr"
)

type RelayConfig struct {
	Port             int           `envconfig:"PORT" default:"3334"`
	DBPath           string        `envconfig:"DB_PATH" default:"./khatru-sqlite.db"`
	HTTPTimeout      time.Duration `envconfig:"HTTP_TIMEOUT" default:"30s"`
	Name             string        `envconfig:"NAME" default:"Debug Khatru Relay"`
	Description      string        `envconfig:"DESCRIPTION" default:"A configurable Nostr relay for debugging and testing"`
	PubKey           string        `envconfig:"PUBKEY"`
	AllowedKinds     []int         `envconfig:"ALLOWED_KINDS"`
	WhitelistPubkeys []string      `envconfig:"WHITELIST_PUBKEYS"`
	MaxContentLength int           `envconfig:"MAX_CONTENT_LENGTH" default:"250000"`
	MaxEventTags     int           `envconfig:"MAX_EVENT_TAGS" default:"2000"`
	Debug            bool          `envconfig:"DEBUG" default:"false"`
}

type Logger struct {
	debug bool
}

func NewLogger(debug bool) *Logger {
	return &Logger{debug: debug}
}

func (l *Logger) Info(format string, v ...interface{}) {
	log.Printf("[INFO] "+format, v...)
}

func (l *Logger) Debug(format string, v ...interface{}) {
	if l.debug {
		log.Printf("[DEBUG] "+format, v...)
	}
}

func (l *Logger) Error(format string, v ...interface{}) {
	log.Printf("[ERROR] "+format, v...)
}

func main() {
	godotenv.Load()

	var cfg RelayConfig
	if err := envconfig.Process("RELAY", &cfg); err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	logger := NewLogger(cfg.Debug)
	logger.Debug("Configuration loaded: %+v", cfg)

	relay := khatru.NewRelay()
	relay.Info.Name = cfg.Name
	relay.Info.Description = cfg.Description
	relay.Info.PubKey = cfg.PubKey

	db := sqlite3.SQLite3Backend{DatabaseURL: cfg.DBPath}
	if err := db.Init(); err != nil {
		logger.Error("Failed to initialize database: %v", err)
		return
	}

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.CountEvents = append(relay.CountEvents, db.CountEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)

	relay.RejectEvent = append(relay.RejectEvent,
		func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
			if cfg.MaxContentLength > 0 && len(event.Content) > cfg.MaxContentLength {
				return true, fmt.Sprintf("blocked: content length exceeds maximum of %d", cfg.MaxContentLength)
			}

			if cfg.MaxEventTags > 0 && len(event.Tags) > cfg.MaxEventTags {
				return true, fmt.Sprintf("blocked: number of tags exceeds maximum of %d", cfg.MaxEventTags)
			}

			if len(cfg.AllowedKinds) > 0 && !contains(cfg.AllowedKinds, event.Kind) {
				return true, fmt.Sprintf("blocked: event kind %d not allowed, allowed kinds: %v", event.Kind, cfg.AllowedKinds)
			}

			if len(cfg.WhitelistPubkeys) > 0 && !contains(cfg.WhitelistPubkeys, event.PubKey) {
				return true, "blocked: pubkey not in whitelist"
			}

			return false, ""
		},
	)

	relay.OnConnect = append(relay.OnConnect, func(ctx context.Context) {
		ws := khatru.GetConnection(ctx)
		logger.Info("New connection from %s", ws.Request.RemoteAddr)
	})

	relay.OnDisconnect = append(relay.OnDisconnect, func(ctx context.Context) {
		ws := khatru.GetConnection(ctx)
		logger.Info("Disconnected from %s", ws.Request.RemoteAddr)
	})

	relay.OnEventSaved = append(relay.OnEventSaved, func(ctx context.Context, event *nostr.Event) {
		if cfg.Debug {
			logger.Debug("Event saved - Kind: %d, Pubkey: %s", event.Kind, event.PubKey)
		}
	})

	mux := http.NewServeMux()
	mux.Handle("/", handleRoot(relay, &cfg))

	addr := fmt.Sprintf(":%d", cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  cfg.HTTPTimeout,
		WriteTimeout: cfg.HTTPTimeout,
	}

	logger.Info("Starting relay on %s", addr)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("Server failed: %v", err)
	}
}

func handleRoot(relay *khatru.Relay, cfg *RelayConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
			relay.ServeHTTP(w, r)
			return
		}

		switch r.Header.Get("Accept") {
		case "application/json":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"name":        cfg.Name,
				"description": cfg.Description,
				"pubkey":      cfg.PubKey,
				"config": map[string]interface{}{
					"allowed_kinds":      cfg.AllowedKinds,
					"whitelist_enabled":  len(cfg.WhitelistPubkeys) > 0,
					"max_content_length": cfg.MaxContentLength,
					"max_event_tags":     cfg.MaxEventTags,
					"debug_enabled":      cfg.Debug,
				},
			})

		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `
				<html>
					<head>
						<title>%s</title>
						<style>
							body { font-family: Arial, sans-serif; margin: 40px; line-height: 1.6; }
							pre { background: #f4f4f4; padding: 15px; border-radius: 5px; }
							.container { max-width: 800px; margin: 0 auto; }
						</style>
					</head>
					<body>
						<div class="container">
							<h1>%s</h1>
							<p>%s</p>
							
							<h2>Relay Configuration</h2>
							<pre>
Allowed Event Kinds: %v
Whitelist Enabled: %v
Max Content Length: %d bytes
Max Event Tags: %d
Debug Enabled: %v
							</pre>

							<h2>Connection Information</h2>
							<p>Connect to this relay using: <code>ws://%s:%d/</code></p>
						</div>
					</body>
				</html>
			`, cfg.Name, cfg.Name, cfg.Description,
				cfg.AllowedKinds, len(cfg.WhitelistPubkeys) > 0,
				cfg.MaxContentLength, cfg.MaxEventTags, cfg.Debug,
				r.Host, cfg.Port)
		}
	}
}

func contains[T comparable](slice []T, item T) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
}
