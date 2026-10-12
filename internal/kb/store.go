package kb

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"
)

// Chunk 是向量库里的一条切块记录（不含向量），供列表、孤儿清理和迁移使用。
type Chunk struct {
	ID      string // 形如「游戏/原神.md#3」
	Source  string // 来源文件相对路径
	Content string // 正文
}

// Store 抽象底层向量库，屏蔽 Redis / Milvus 的差异。
// 相似度统一用余弦相似度（0~1，越大越相似），与 Redis 版保持一致。
type Store interface {
	// Kind 返回后端类型：redis / milvus
	Kind() string

	// EnsureIndex 幂等地准备好索引（不存在则创建），已存在时校验向量维度。
	EnsureIndex(ctx context.Context) error
	// DropIndex 删除索引及其中的全部数据（全量重建时使用）。
	DropIndex(ctx context.Context) error

	// Upsert 对文档做 embedding 后写入；已存在的 ID 会被覆盖。返回写入条数。
	Upsert(ctx context.Context, docs []*schema.Document) (int, error)
	// Retrieve 检索与 query 最相似的 topK 条切块。
	Retrieve(ctx context.Context, query string, topK int) ([]*schema.Document, error)

	// DeleteBySource 删除某个来源文件的全部切块，返回删除条数。
	DeleteBySource(ctx context.Context, source string) (int64, error)
	// DeleteByIDs 按切块 ID 删除，返回删除条数。
	DeleteByIDs(ctx context.Context, ids []string) (int64, error)

	// AllChunks 枚举全部切块（分页拉取，数据量不大）。
	AllChunks(ctx context.Context) ([]Chunk, error)

	Close() error
}

// NewStore 按 VECTOR_STORE 配置创建向量库（默认 milvus）。
func NewStore(ctx context.Context, cfg *Config, emb embedding.Embedder) (Store, error) {
	switch strings.ToLower(cfg.VectorStore) {
	case "", "milvus":
		return newMilvusStore(ctx, cfg, emb)
	case "redis":
		return newRedisStore(ctx, cfg, emb)
	default:
		return nil, fmt.Errorf("未知的 VECTOR_STORE=%s（可选 redis / milvus）", cfg.VectorStore)
	}
}

// NewRetriever 把任意 Store 包装成 eino 的 retriever.Retriever 供 Agent 调用。
func NewRetriever(_ context.Context, cfg *Config, st Store) (retriever.Retriever, error) {
	if st == nil {
		return nil, fmt.Errorf("retriever 缺少 store")
	}
	return &storeRetriever{st: st, topK: cfg.TopK}, nil
}

type storeRetriever struct {
	st   Store
	topK int
}

func (r *storeRetriever) Retrieve(ctx context.Context, query string, _ ...retriever.Option) ([]*schema.Document, error) {
	return r.st.Retrieve(ctx, query, r.topK)
}

// sourceOf 从 doc 的 MetaData 里取来源文件，缺失时回退到 ID 的「#」前缀。
func sourceOf(doc *schema.Document) string {
	if doc.MetaData != nil {
		if s, ok := doc.MetaData[SourceKey].(string); ok && s != "" {
			return s
		}
	}
	if i := strings.LastIndexByte(doc.ID, '#'); i > 0 {
		return doc.ID[:i]
	}
	return ""
}

// splitSource 从切块 ID 反解来源文件：ID 形如「游戏/xxx.md#3」。
func splitSource(id string) (source string, ok bool) {
	i := strings.LastIndexByte(id, '#')
	if i <= 0 {
		return "", false
	}
	return id[:i], true
}
