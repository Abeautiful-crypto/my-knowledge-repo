//go:build smoke

// 端到端冒烟测试：需要本地 Redis Stack（localhost:6379），不需要真实 API Key。
// 运行：go test ./internal/kb -tags smoke -v
package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"
)

// hashEmbedder 用文本特征构造确定性向量，替代真实 embedding 服务。
type hashEmbedder struct{}

func (hashEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	dim := 1536
	vecs := make([][]float64, 0, len(texts))
	for _, t := range texts {
		v := make([]float64, dim)
		if strings.Contains(t, "咖啡") {
			v[0] = 1
		}
		if strings.Contains(t, "eino") {
			v[1] = 1
		}
		vecs = append(vecs, v)
	}
	return vecs, nil
}

func TestRedisRoundTrip(t *testing.T) {
	ctx := context.Background()

	cfg, err := Load()
	if err != nil {
		cfg = &Config{
			RedisAddr:    "localhost:6379",
			Index:        "kb_smoke_index",
			KeyPrefix:    "smoke_doc:",
			TopK:         5,
			EmbeddingDim: 1536,
		}
	}
	cfg.Index = "kb_smoke_index"
	cfg.KeyPrefix = "smoke_doc:"
	cfg.EmbeddingDim = 1536

	writeClient := NewRedisClient(cfg)
	defer writeClient.Close()
	if err := writeClient.Ping(ctx).Err(); err != nil {
		t.Skipf("跳过：Redis 不可用（%v）", err)
	}
	_ = DropIndex(ctx, writeClient, cfg.Index)
	if err := EnsureIndex(ctx, writeClient, cfg); err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	t.Cleanup(func() { _ = DropIndex(ctx, writeClient, cfg.Index) })

	emb := hashEmbedder{}
	idx, err := NewIndexer(ctx, cfg, writeClient, emb)
	if err != nil {
		t.Fatalf("创建 indexer 失败: %v", err)
	}

	docs := []*schema.Document{
		{
			ID:       "note-a#0",
			Content:  "咖啡手冲参数：粉水比 1:15，水温 92 度。",
			MetaData: map[string]any{SourceKey: "note-a.md"},
		},
		{
			ID:       "note-b#0",
			Content:  "eino 是 CloudWeGo 的大模型应用框架。",
			MetaData: map[string]any{SourceKey: "note-b.md"},
		},
	}
	if _, err := idx.Store(ctx, docs); err != nil {
		t.Fatalf("入库失败: %v", err)
	}

	searchClient := NewRedisClient(cfg)
	defer searchClient.Close()
	ret, err := redisRetriever(ctx, cfg, searchClient, emb, cfg.TopK)
	if err != nil {
		t.Fatalf("创建 retriever 失败: %v", err)
	}

	got, err := ret.Retrieve(ctx, "咖啡怎么冲")
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("检索结果为空")
	}
	t.Logf("检索到 %d 条，第一条: %s / %s", len(got), got[0].MetaData[SourceKey], got[0].Content)

	if s, _ := got[0].MetaData[SourceKey].(string); s != "note-a.md" {
		t.Fatalf("期望命中 note-a.md，实际 %q", s)
	}

	// 确认 hash 里写入了预期字段
	raw, err := writeClient.HGetAll(ctx, cfg.KeyPrefix+"note-a#0").Result()
	if err != nil {
		t.Fatalf("读取 hash 失败: %v", err)
	}
	if _, ok := raw["content"]; !ok {
		t.Fatalf("hash 缺少 content 字段: %v", raw)
	}
	if _, ok := raw["vector_content"]; !ok {
		t.Fatalf("hash 缺少 vector_content 字段")
	}
	if raw["source"] != "note-a.md" {
		t.Fatalf("source 字段错误: %q", raw["source"])
	}
	_ = redis.Nil
}

func TestEnsureIndexDimMismatch(t *testing.T) {
	ctx := context.Background()
	cfg := &Config{
		RedisAddr:    "localhost:6379",
		Index:        "kb_dim_index",
		KeyPrefix:    "dim_doc:",
		EmbeddingDim: 1536,
	}

	client := NewRedisClient(cfg)
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("跳过：Redis 不可用（%v）", err)
	}
	_ = DropIndex(ctx, client, cfg.Index)
	if err := EnsureIndex(ctx, client, cfg); err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	t.Cleanup(func() { _ = DropIndex(ctx, client, cfg.Index) })

	// 同维度重复调用应当幂等
	if err := EnsureIndex(ctx, client, cfg); err != nil {
		t.Fatalf("同维度重复调用不应报错: %v", err)
	}

	// 换了 embedding 模型（维度变化）必须给出明确提示
	changed := *cfg
	changed.EmbeddingDim = 1024
	err := EnsureIndex(ctx, client, &changed)
	if err == nil {
		t.Fatal("维度不一致时应当报错")
	}
	t.Logf("维度校验提示: %v", err)
}
