package kb

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config 保存知识库的全部外部配置，均来自环境变量（或项目根目录的 .env 文件）。
type Config struct {
	// 大模型（对话）
	APIKey    string
	BaseURL   string
	ChatModel string

	// 向量模型（可以和大模型用不同的服务商）
	EmbeddingAPIKey  string
	EmbeddingBaseURL string
	EmbeddingModel   string
	EmbeddingDim     int
	// EmbeddingSetDimensions 是否把 EMBEDDING_DIM 下发给 embedding 接口。
	// 只有 OpenAI text-embedding-3 系列等支持自定义维度的模型才需要开（默认关）。
	EmbeddingSetDimensions bool

	// 向量库：VECTOR_STORE=redis 走 Redis Stack；=milvus（默认）走 Milvus
	VectorStore string
	// Redis（Redis Stack / RediSearch），VECTOR_STORE=redis 时使用
	RedisAddr     string
	RedisPassword string
	Index         string
	KeyPrefix     string
	// Milvus，VECTOR_STORE=milvus 时使用
	MilvusAddr       string
	MilvusCollection string
	MilvusUsername   string
	MilvusPassword   string
	TopK             int

	// 联网检索（本地笔记检索不到时兜底）
	SearchProvider string // tavily / bocha / none
	SearchAPIKey   string
	SearchTopK     int
	// LocalScoreThreshold 是「本地笔记算命中」的最低相似度。
	// 低于该值判定为知识库里没有，转去联网检索。
	LocalScoreThreshold float64
	// WebSaveMode 沉淀模式：
	//   direct = 联网得到的答案直接存入对应知识库（没有则新建）
	//   inbox  = 先存到「待确认」体系，人工确认后再转正
	WebSaveMode string
}

// WebSaveInboxName 是 inbox 模式下存放待确认内容的体系名。
const WebSaveInboxName = "待确认"

// SearchEnabled 报告是否配置了可用的联网检索服务。
func (c *Config) SearchEnabled() bool {
	return c.SearchProvider != "" && c.SearchProvider != "none" && c.SearchAPIKey != ""
}

// Load 读取配置。APIKey 必填，其余都有可用默认值。
func Load() (*Config, error) {
	loadDotEnv(".env")

	cfg := &Config{
		APIKey:    os.Getenv("OPENAI_API_KEY"),
		BaseURL:   envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		ChatModel: envOr("CHAT_MODEL", "gpt-4o-mini"),

		EmbeddingAPIKey:  os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingBaseURL: envOr("EMBEDDING_BASE_URL", envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")),
		EmbeddingModel:   envOr("EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingDim:     envInt("EMBEDDING_DIM", 1536),

		EmbeddingSetDimensions: envBool("EMBEDDING_SET_DIMENSIONS", false),

		VectorStore: envOr("VECTOR_STORE", "milvus"),

		RedisAddr:     envOr("REDIS_ADDR", "localhost:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		Index:         envOr("REDIS_INDEX", "kb_index"),
		KeyPrefix:     envOr("REDIS_KEY_PREFIX", "eino_doc:"),

		MilvusAddr:       envOr("MILVUS_ADDR", "localhost:19530"),
		MilvusCollection: envOr("MILVUS_COLLECTION", "kb_chunks"),
		MilvusUsername:   os.Getenv("MILVUS_USERNAME"),
		MilvusPassword:   os.Getenv("MILVUS_PASSWORD"),

		TopK: envInt("TOPK", 5),

		SearchProvider:      envOr("SEARCH_PROVIDER", "none"),
		SearchAPIKey:        os.Getenv("SEARCH_API_KEY"),
		SearchTopK:          envInt("SEARCH_TOPK", 5),
		LocalScoreThreshold: envFloat("LOCAL_SCORE_THRESHOLD", 0.85),
		WebSaveMode:         envOr("WEB_SAVE_MODE", "direct"),
	}

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("缺少 OPENAI_API_KEY，请在环境变量或 .env 中配置")
	}
	if cfg.EmbeddingAPIKey == "" {
		cfg.EmbeddingAPIKey = cfg.APIKey
	}
	return cfg, nil
}

func envOr(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func envInt(key string, defaultValue int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultValue
	}
	return n
}

func envFloat(key string, defaultValue float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return defaultValue
	}
	return f
}

func envBool(key string, defaultValue bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return defaultValue
	}
}

// loadDotEnv 读取简单的 KEY=VALUE 格式文件，已存在的环境变量不会被覆盖。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(strings.Trim(v, `"'`))
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}
