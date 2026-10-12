package kb

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/schema"
)

// redisStore 是原来的 Redis Stack（RediSearch）实现，
// 现在作为 VECTOR_STORE=redis 的可选后端保留。
type redisStore struct {
	cfg    *Config
	emb    embedding.Embedder
	client *redis.Client
}

func newRedisStore(ctx context.Context, cfg *Config, emb embedding.Embedder) (*redisStore, error) {
	client := NewRedisClient(cfg)
	s := &redisStore{cfg: cfg, emb: emb, client: client}
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("连不上 Redis(%s): %w（请先启动 Redis Stack）", cfg.RedisAddr, err)
	}
	if err := s.EnsureIndex(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return s, nil
}

func (s *redisStore) Kind() string { return "redis" }

func (s *redisStore) EnsureIndex(ctx context.Context) error {
	return EnsureIndex(ctx, s.client, s.cfg)
}

func (s *redisStore) DropIndex(ctx context.Context) error {
	return DropIndex(ctx, s.client, s.cfg.Index)
}

func (s *redisStore) Upsert(ctx context.Context, docs []*schema.Document) (int, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	indexer, err := NewIndexer(ctx, s.cfg, s.client, s.emb)
	if err != nil {
		return 0, err
	}
	const batch = 50
	written := 0
	for start := 0; start < len(docs); start += batch {
		end := min(start+batch, len(docs))
		if _, err := indexer.Store(ctx, docs[start:end]); err != nil {
			return written, fmt.Errorf("入库失败（第 %d-%d 个分块）: %w", start, end, err)
		}
		written += end - start
	}
	return written, nil
}

func (s *redisStore) Retrieve(ctx context.Context, query string, topK int) ([]*schema.Document, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = s.cfg.TopK
	}
	ret, err := redisRetriever(ctx, s.cfg, s.client, s.emb, topK)
	if err != nil {
		return nil, err
	}
	return ret.Retrieve(ctx, query)
}

func (s *redisStore) DeleteBySource(ctx context.Context, source string) (int64, error) {
	return deleteSourceKeys(ctx, s.cfg, s.client, source)
}

func (s *redisStore) DeleteByIDs(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, s.cfg.KeyPrefix+id)
	}
	deleted := int64(0)
	for start := 0; start < len(keys); start += 200 {
		end := min(start+200, len(keys))
		n, err := s.client.Del(ctx, keys[start:end]...).Result()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

func (s *redisStore) AllChunks(ctx context.Context) ([]Chunk, error) {
	var keys []string
	iter := s.client.Scan(ctx, 0, s.cfg.KeyPrefix+"*", 500).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}

	out := make([]Chunk, 0, len(keys))
	for start := 0; start < len(keys); start += 200 {
		end := min(start+200, len(keys))
		pipe := s.client.Pipeline()
		cmds := make([]*redis.StringCmd, 0, end-start)
		ids := make([]string, 0, end-start)
		for _, key := range keys[start:end] {
			cmds = append(cmds, pipe.HGet(ctx, key, fieldContent))
			ids = append(ids, strings.TrimPrefix(key, s.cfg.KeyPrefix))
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return nil, err
		}
		for i, cmd := range cmds {
			content, err := cmd.Result()
			if err != nil {
				continue
			}
			src, ok := splitSource(ids[i])
			if !ok {
				continue
			}
			out = append(out, Chunk{ID: ids[i], Source: src, Content: content})
		}
	}
	return out, nil
}

func (s *redisStore) Close() error { return s.client.Close() }
