package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	zap "go.uber.org/zap"

	server "github.com/inference-gateway/adk/server"
	config "github.com/inference-gateway/adk/server/config"
	types "github.com/inference-gateway/adk/types"

	"simple-a2a-server/internal/broker"
)

//go:embed skills/a2a-broker-sdk
var skillsEmbed embed.FS

func main() {
	fmt.Println("🤖 Starting A2A Broker Server...")

	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Sync()

	// Public port (serves broker + proxied default agent)
	publicPort := os.Getenv("PORT")
	if publicPort == "" {
		publicPort = "6321"
	}
	// Internal port for the ADK server hosting the default OpenAI agent.
	internalPort := os.Getenv("INTERNAL_PORT")
	if internalPort == "" {
		internalPort = "18080"
	}
	publicURL := os.Getenv("PUBLIC_URL")
	if publicURL == "" {
		publicURL = "http://www.cybertron.studio/a2a"
	}
	dbPath := os.Getenv("BROKER_DB")
	if dbPath == "" {
		dbPath = "/var/lib/simpleA2a/broker.db"
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		logger.Warn("OPENAI_API_KEY is not set; the default agent at /a2a will fail to handle tasks")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	// --- 1) Start the default OpenAI-backed ADK agent on an internal port ---
	startDefaultAgent(logger, internalPort, publicURL, apiKey, baseURL, model)

	// --- 2) Open broker store + hub ---
	store, err := broker.NewStore(dbPath)
	if err != nil {
		logger.Fatal("failed to open broker store", zap.Error(err), zap.String("path", dbPath))
	}
	defer store.Close()
	hub := broker.NewInboxHub()
	br := broker.New(store, hub, logger, publicURL)

	// Wire up the embedded skill kit so /sdk/* and /download/*.zip can serve it.
	sub, err := fs.Sub(skillsEmbed, "skills/a2a-broker-sdk")
	if err != nil {
		logger.Fatal("embed sub-fs", zap.Error(err))
	}
	broker.SetSkillFS(sub)

	// --- 3) Build the public Gin engine ---
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// Proxy /a2a POST (JSON-RPC) to the internal default agent, but serve the
	// HTML landing page on GET /a2a so browsers visiting the public URL see docs.
	internalURL, _ := url.Parse("http://127.0.0.1:" + internalPort)
	proxy := httputil.NewSingleHostReverseProxy(internalURL)
	r.GET("/a2a", br.ServeLandingHTTP)
	r.POST("/a2a", gin.WrapH(proxy))
	// Sub-paths under /a2a (streaming, etc.) are rare but still proxy through.
	r.Any("/a2a/*path", gin.WrapH(proxy))
	r.GET("/.well-known/agent-card.json", gin.WrapH(proxy))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "role": "broker"})
	})

	// Mount broker routes: /registry/*, /agents/:id/*, /, /sdk/*, /download/*
	br.Mount(r)

	// --- 4) Start the public HTTP server ---
	public := &http.Server{
		Addr:         ":" + publicPort,
		Handler:      r,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		logger.Info("🌐 public broker listening", zap.String("addr", public.Addr))
		if err := public.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("public server failed", zap.Error(err))
		}
	}()

	// Wait for shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("🛑 shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = public.Shutdown(ctx)
	logger.Info("✅ goodbye!")
}

// startDefaultAgent boots the original ADK OpenAI(-compatible) server on
// 127.0.0.1:internalPort in the background. baseURL and model are env-driven
// so the broker can front any OpenAI-compatible endpoint (Azure, OpenRouter,
// local llama.cpp / vLLM / inference-gateway, etc.).
func startDefaultAgent(logger *zap.Logger, internalPort, publicURL, apiKey, baseURL, model string) {
	cfg := config.Config{
		AgentName:        "broker-default-agent",
		AgentDescription: "Default OpenAI-compatible agent on the broker",
		AgentVersion:     "0.3.0",
		Debug:            false,
		QueueConfig:      config.QueueConfig{CleanupInterval: 5 * time.Minute},
		ServerConfig:     config.ServerConfig{Port: internalPort},
	}

	agentCfg := &config.AgentConfig{
		Provider:                    "openai",
		Model:                       model,
		BaseURL:                     baseURL,
		APIKey:                      apiKey,
		Timeout:                     60 * time.Second,
		MaxRetries:                  2,
		MaxChatCompletionIterations: 10,
		MaxTokens:                   2048,
		Temperature:                 0.7,
		SystemPrompt:                "You are a helpful AI assistant.",
	}

	llmClient, err := server.NewOpenAICompatibleLLMClient(agentCfg, logger)
	if err != nil {
		logger.Fatal("failed to create default LLM client", zap.Error(err))
	}
	agent, err := server.NewAgentBuilder(logger).
		WithConfig(agentCfg).
		WithLLMClient(llmClient).
		WithSystemPrompt(agentCfg.SystemPrompt).
		WithMaxChatCompletion(10).
		Build()
	if err != nil {
		logger.Fatal("failed to build default agent", zap.Error(err))
	}

	url := publicURL
	a2aServer, err := server.NewA2AServerBuilder(cfg, logger).
		WithAgent(agent).
		WithDefaultTaskHandlers().
		WithAgentCard(types.AgentCard{
			Name:            cfg.AgentName,
			Description:     cfg.AgentDescription,
			Version:         cfg.AgentVersion,
			URL:             &url,
			ProtocolVersion: "0.3.0",
			Capabilities: types.AgentCapabilities{
				Streaming:              &[]bool{true}[0],
				PushNotifications:      &[]bool{false}[0],
				StateTransitionHistory: &[]bool{false}[0],
			},
			DefaultInputModes:  []string{"text/plain"},
			DefaultOutputModes: []string{"text/plain"},
			Skills:             []types.AgentSkill{},
		}).
		Build()
	if err != nil {
		logger.Fatal("failed to build default ADK server", zap.Error(err))
	}
	logger.Info("✅ default OpenAI agent ready", zap.String("internal_port", internalPort))

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := a2aServer.Start(ctx); err != nil {
			logger.Error("default ADK server stopped", zap.Error(err))
		}
	}()
}
