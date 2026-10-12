package kb

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	openaiemb "github.com/cloudwego/eino-ext/components/embedding/openai"
	redisidx "github.com/cloudwego/eino-ext/components/indexer/redis"
	openaimodel "github.com/cloudwego/eino-ext/components/model/openai"
	redisret "github.com/cloudwego/eino-ext/components/retriever/redis"
)

const (
	fieldContent       = "content"        // 正文
	fieldSource        = "source"         // 笔记文件路径
	fieldVectorContent = "vector_content" // 正文向量
)

// SourceKey 是 Document MetaData 中保存来源文件名的 key。
const SourceKey = "source"

// ScoreKey 是 Document MetaData 中保存相似度（0~1）的 key。
const ScoreKey = "score"

func NewEmbedder(ctx context.Context, cfg *Config) (embedding.Embedder, error) {
	embCfg := &openaiemb.EmbeddingConfig{
		APIKey:  cfg.EmbeddingAPIKey,
		BaseURL: cfg.EmbeddingBaseURL,
		Model:   cfg.EmbeddingModel,
		Timeout: 30 * time.Second,
	}

	// 只有明确支持的平台才下发 dimensions 参数：
	// OpenAI text-embedding-3 系列支持，硅基流动/智谱等会返回 400，
	// 所以默认不传，需要时把 EMBEDDING_SET_DIMENSIONS 置为 true。
	if cfg.EmbeddingSetDimensions {
		dim := cfg.EmbeddingDim
		embCfg.Dimensions = &dim
	}

	return openaiemb.NewEmbedder(ctx, embCfg)
}

func NewChatModel(ctx context.Context, cfg *Config) (model.ChatModel, error) {
	return openaimodel.NewChatModel(ctx, &openaimodel.ChatModelConfig{
		APIKey:  cfg.APIKey,
		BaseURL: cfg.BaseURL,
		Model:   cfg.ChatModel,
		Timeout: 120 * time.Second,
	})
}

// NewRedisClient 创建 Redis 客户端。
// 统一使用 RESP2 协议：go-redis v9 的 FT.* 系列命令在 RESP3 下需要显式开启
// UnstableResp3，统一走 RESP2 可以避免写入端和检索端行为不一致。
func NewRedisClient(cfg *Config) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:          cfg.RedisAddr,
		Password:      cfg.RedisPassword,
		Protocol:      2,
		UnstableResp3: true,
	})
}

// EnsureIndex 幂等地创建 RediSearch 向量索引，已存在则跳过。
// 已存在时会校验向量维度，换过 embedding 模型必须带上 -reset 重建。
func EnsureIndex(ctx context.Context, client *redis.Client, cfg *Config) error {
	if _, err := client.FTInfo(ctx, cfg.Index).Result(); err == nil {
		if dim, ok := indexVectorDim(ctx, client, cfg.Index); ok && dim != cfg.EmbeddingDim {
			return fmt.Errorf(
				"索引 %s 已存在，其向量维度是 %d，但当前 EMBEDDING_DIM=%d（说明 embedding 模型换过了）。\n请用 go run ./cmd/index -dir <笔记目录> -reset 重建索引",
				cfg.Index, dim, cfg.EmbeddingDim)
		}
		return nil
	}

	schemas := []*redis.FieldSchema{
		{FieldName: fieldContent, FieldType: redis.SearchFieldTypeText, Weight: 1},
		{FieldName: fieldSource, FieldType: redis.SearchFieldTypeText, Weight: 1},
		{
			FieldName: fieldVectorContent,
			FieldType: redis.SearchFieldTypeVector,
			VectorArgs: &redis.FTVectorArgs{
				// 个人笔记量级（<100 万条）用 FLAT 索引即可，精度最高。
				FlatOptions: &redis.FTFlatOptions{
					Type:           "FLOAT32",
					Dim:            cfg.EmbeddingDim,
					DistanceMetric: "COSINE",
				},
			},
		},
	}

	options := &redis.FTCreateOptions{
		OnHash: true,
		Prefix: []any{cfg.KeyPrefix},
	}

	if _, err := client.FTCreate(ctx, cfg.Index, options, schemas...).Result(); err != nil {
		return fmt.Errorf("创建索引失败: %w", err)
	}
	return nil
}

// DropIndex 删除索引及其中的全部文档（--reset 时使用）。
func DropIndex(ctx context.Context, client *redis.Client, index string) error {
	_, err := client.FTDropIndexWithArgs(ctx, index, &redis.FTDropIndexOptions{DeleteDocs: true}).Result()
	return err
}

// indexVectorDim 从 FT.INFO 的原始返回值中读取向量字段维度。
// go-redis 的 FTInfoResult 没有保留 VECTOR 字段的 DIM，所以走原生命令。
func indexVectorDim(ctx context.Context, client *redis.Client, index string) (int, bool) {
	res, err := client.Do(ctx, "FT.INFO", index).Slice()
	if err != nil {
		return 0, false
	}

	for i := 0; i+1 < len(res); i += 2 {
		key, ok := res[i].(string)
		if !ok || key != "attributes" {
			continue
		}
		attrs, ok := res[i+1].([]any)
		if !ok {
			return 0, false
		}
		for _, a := range attrs {
			pairs, ok := a.([]any)
			if !ok {
				continue
			}
			kv := make(map[string]any, len(pairs)/2)
			for j := 0; j+1 < len(pairs); j += 2 {
				if k, ok := pairs[j].(string); ok {
					kv[strings.ToLower(k)] = pairs[j+1]
				}
			}
			if id, _ := kv["identifier"].(string); id != fieldVectorContent {
				continue
			}
			switch v := kv["dim"].(type) {
			case int64:
				return int(v), true
			case int:
				return v, true
			case float64:
				return int(v), true
			case string:
				if n, err := strconv.Atoi(v); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

func NewIndexer(ctx context.Context, cfg *Config, client *redis.Client, emb embedding.Embedder) (*redisidx.Indexer, error) {
	return redisidx.NewIndexer(ctx, &redisidx.IndexerConfig{
		Client:           client,
		KeyPrefix:        cfg.KeyPrefix,
		BatchSize:        10,
		Embedding:        emb,
		DocumentToHashes: documentToHashes,
	})
}

// documentToHashes 只写入 content / source 两个字段，
// 避免把 MetaData 里的所有 key 都铺进 Redis hash。
func documentToHashes(_ context.Context, doc *schema.Document) (*redisidx.Hashes, error) {
	if doc.ID == "" {
		return nil, fmt.Errorf("文档缺少 ID")
	}
	fields := map[string]redisidx.FieldValue{
		fieldContent: {
			Value:    doc.Content,
			EmbedKey: fieldVectorContent,
		},
	}
	if src, ok := doc.MetaData[SourceKey]; ok {
		fields[fieldSource] = redisidx.FieldValue{Value: src}
	}
	return &redisidx.Hashes{Key: doc.ID, Field2Value: fields}, nil
}

// redisRetriever 创建 Redis 后端的检索器（VECTOR_STORE=redis 时使用）。
func redisRetriever(ctx context.Context, cfg *Config, client *redis.Client, emb embedding.Embedder, topK int) (*redisret.Retriever, error) {
	if topK <= 0 {
		topK = cfg.TopK
	}
	return redisret.NewRetriever(ctx, &redisret.RetrieverConfig{
		Client:      client,
		Index:       cfg.Index,
		VectorField: fieldVectorContent,
		TopK:        topK,
		Embedding:   emb,
		// 距离字段必须显式加入 ReturnFields，否则拿不到相似度
		ReturnFields:      []string{fieldContent, fieldSource, redisret.SortByDistanceAttributeName},
		DocumentConverter: resultParser(),
	})
}

// resultParser 把 FT.SEARCH 的原始结果转成 eino Document，
// 并把 KNN 返回的余弦距离换算成 0~1 的相似度放进 MetaData[ScoreKey]。
func resultParser() func(context.Context, redis.Document) (*schema.Document, error) {
	return func(_ context.Context, doc redis.Document) (*schema.Document, error) {
		resp := &schema.Document{
			ID:       doc.ID,
			MetaData: map[string]any{},
		}
		if v, ok := doc.Fields[fieldContent]; ok {
			resp.Content = v
		}
		if v, ok := doc.Fields[fieldSource]; ok {
			resp.MetaData[SourceKey] = v
		}
		if v, ok := doc.Fields[redisret.SortByDistanceAttributeName]; ok {
			if dist, err := strconv.ParseFloat(v, 64); err == nil {
				resp.MetaData[ScoreKey] = 1 - dist
			}
		}
		return resp, nil
	}
}
