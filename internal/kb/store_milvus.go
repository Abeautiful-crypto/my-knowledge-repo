package kb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	milvusclient "github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/schema"
)

const (
	milvusFieldID      = "id"
	milvusFieldSource  = "source"
	milvusFieldContent = "content"
	milvusFieldVector  = "vector"

	milvusIDMaxLen      = 512
	milvusSourceMaxLen  = 1024
	milvusContentMaxLen = 60000 // Milvus varchar 上限 65535，留点余量
	milvusPageSize      = 1000
	milvusEmbedBatch    = 16
)

// milvusStore 把切块存进本地 Milvus（standalone / docker），度量方式为余弦相似度。
type milvusStore struct {
	cfg  *Config
	emb  embedding.Embedder
	cli  milvusclient.Client
	coll string
}

func newMilvusStore(ctx context.Context, cfg *Config, emb embedding.Embedder) (*milvusStore, error) {
	cli, err := milvusclient.NewClient(ctx, milvusclient.Config{
		Address:  cfg.MilvusAddr,
		Username: cfg.MilvusUsername,
		Password: cfg.MilvusPassword,
	})
	if err != nil {
		return nil, fmt.Errorf("连接 Milvus(%s) 失败: %w（请先启动 Milvus）", cfg.MilvusAddr, err)
	}
	s := &milvusStore{cfg: cfg, emb: emb, cli: cli, coll: cfg.MilvusCollection}
	if err := s.EnsureIndex(ctx); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return s, nil
}

func (s *milvusStore) Kind() string { return "milvus" }

func (s *milvusStore) EnsureIndex(ctx context.Context) error {
	has, err := s.cli.HasCollection(ctx, s.coll)
	if err != nil {
		return fmt.Errorf("查询 Milvus 集合失败: %w", err)
	}
	if !has {
		return s.createCollection(ctx)
	}
	desc, err := s.cli.DescribeCollection(ctx, s.coll)
	if err != nil {
		return fmt.Errorf("读取 Milvus 集合信息失败: %w", err)
	}
	if desc != nil && desc.Schema != nil {
		for _, f := range desc.Schema.Fields {
			if f == nil || f.Name != milvusFieldVector {
				continue
			}
			if raw, ok := f.TypeParams["dim"]; ok {
				if dim, err := strconv.Atoi(raw); err == nil && dim != s.cfg.EmbeddingDim {
					return fmt.Errorf(
						"Milvus 集合 %s 的向量维度是 %d，但当前 EMBEDDING_DIM=%d（embedding 模型换过了）。\n请用 go run ./cmd/index -dir <笔记目录> -reset 重建索引",
						s.coll, dim, s.cfg.EmbeddingDim)
				}
			}
		}
	}
	return s.ensureLoaded(ctx)
}

func (s *milvusStore) createCollection(ctx context.Context) error {
	sch := &entity.Schema{
		CollectionName: s.coll,
		AutoID:         false,
		Fields: []*entity.Field{
			{
				Name:       milvusFieldID,
				DataType:   entity.FieldTypeVarChar,
				PrimaryKey: true,
				TypeParams: map[string]string{"max_length": strconv.Itoa(milvusIDMaxLen)},
			},
			{
				Name:       milvusFieldSource,
				DataType:   entity.FieldTypeVarChar,
				TypeParams: map[string]string{"max_length": strconv.Itoa(milvusSourceMaxLen)},
			},
			{
				Name:       milvusFieldContent,
				DataType:   entity.FieldTypeVarChar,
				TypeParams: map[string]string{"max_length": strconv.Itoa(milvusContentMaxLen)},
			},
			{
				Name:       milvusFieldVector,
				DataType:   entity.FieldTypeFloatVector,
				TypeParams: map[string]string{"dim": strconv.Itoa(s.cfg.EmbeddingDim)},
			},
		},
	}
	if err := s.cli.CreateCollection(ctx, sch, 2); err != nil {
		return fmt.Errorf("创建 Milvus 集合 %s 失败: %w", s.coll, err)
	}
	idx, err := entity.NewIndexAUTOINDEX(entity.COSINE)
	if err != nil {
		return err
	}
	if err := s.cli.CreateIndex(ctx, s.coll, milvusFieldVector, idx, false); err != nil {
		return fmt.Errorf("创建向量索引失败: %w", err)
	}
	return s.ensureLoaded(ctx)
}

func (s *milvusStore) ensureLoaded(ctx context.Context) error {
	state, err := s.cli.GetLoadState(ctx, s.coll, nil)
	if err == nil && state == entity.LoadStateLoaded {
		return nil
	}
	// 未加载过（或查询不到状态）时直接加载，重复调用是幂等的
	if err := s.cli.LoadCollection(ctx, s.coll, false); err != nil {
		return fmt.Errorf("加载 Milvus 集合失败: %w", err)
	}
	return nil
}

func (s *milvusStore) DropIndex(ctx context.Context) error {
	_ = s.cli.ReleaseCollection(ctx, s.coll)
	return s.cli.DropCollection(ctx, s.coll)
}

func (s *milvusStore) Upsert(ctx context.Context, docs []*schema.Document) (int, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	written := 0
	for start := 0; start < len(docs); start += milvusEmbedBatch {
		end := min(start+milvusEmbedBatch, len(docs))
		part := docs[start:end]

		ids := make([]string, 0, len(part))
		srcs := make([]string, 0, len(part))
		contents := make([]string, 0, len(part))
		texts := make([]string, 0, len(part))
		for _, d := range part {
			if d.ID == "" {
				return written, fmt.Errorf("文档缺少 ID")
			}
			ids = append(ids, trimRunes(d.ID, milvusIDMaxLen))
			srcs = append(srcs, trimRunes(sourceOf(d), milvusSourceMaxLen))
			contents = append(contents, trimRunes(d.Content, milvusContentMaxLen))
			texts = append(texts, trimRunes(d.Content, milvusContentMaxLen))
		}

		vecs, err := s.emb.EmbedStrings(ctx, texts)
		if err != nil {
			return written, fmt.Errorf("embedding 失败: %w", err)
		}
		if len(vecs) != len(part) {
			return written, fmt.Errorf("embedding 返回 %d 条，与 %d 个分块不匹配", len(vecs), len(part))
		}
		f32 := make([][]float32, 0, len(vecs))
		for _, v := range vecs {
			if len(v) != s.cfg.EmbeddingDim {
				return written, fmt.Errorf("embedding 维度是 %d，但 EMBEDDING_DIM=%d", len(v), s.cfg.EmbeddingDim)
			}
			f32 = append(f32, toFloat32(v))
		}

		if _, err := s.cli.Upsert(ctx, s.coll, "",
			entity.NewColumnVarChar(milvusFieldID, ids),
			entity.NewColumnVarChar(milvusFieldSource, srcs),
			entity.NewColumnVarChar(milvusFieldContent, contents),
			entity.NewColumnFloatVector(milvusFieldVector, s.cfg.EmbeddingDim, f32),
		); err != nil {
			return written, fmt.Errorf("写入 Milvus 失败: %w", err)
		}
		written += len(part)
	}
	// 立刻落盘，保证后续的检索/删除能看到刚写入的数据
	if err := s.cli.Flush(ctx, s.coll, false); err != nil {
		return written, fmt.Errorf("flush 失败: %w", err)
	}
	return written, nil
}

func (s *milvusStore) Retrieve(ctx context.Context, query string, topK int) ([]*schema.Document, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = s.cfg.TopK
	}
	vecs, err := s.emb.EmbedStrings(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embedding 问题失败: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) != s.cfg.EmbeddingDim {
		return nil, fmt.Errorf("embedding 维度异常（期望 %d）", s.cfg.EmbeddingDim)
	}
	sp, err := entity.NewIndexAUTOINDEXSearchParam(1)
	if err != nil {
		return nil, err
	}
	res, err := s.cli.Search(ctx, s.coll, nil, "",
		[]string{milvusFieldID, milvusFieldSource, milvusFieldContent},
		[]entity.Vector{entity.FloatVector(toFloat32(vecs[0]))},
		milvusFieldVector, entity.COSINE, topK, sp,
		milvusclient.WithSearchQueryConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, fmt.Errorf("Milvus 检索失败: %w", err)
	}

	out := make([]*schema.Document, 0, topK)
	for _, r := range res {
		if r.Err != nil {
			return nil, r.Err
		}
		if r.Fields == nil {
			continue
		}
		idCol := r.Fields.GetColumn(milvusFieldID)
		srcCol := r.Fields.GetColumn(milvusFieldSource)
		contentCol := r.Fields.GetColumn(milvusFieldContent)
		n := r.ResultCount
		if idCol != nil {
			n = min(n, idCol.Len())
		}
		for i := 0; i < n; i++ {
			doc := &schema.Document{MetaData: map[string]any{}}
			if idCol != nil {
				if v, err := idCol.GetAsString(i); err == nil {
					doc.ID = v
				}
			}
			if contentCol != nil {
				if v, err := contentCol.GetAsString(i); err == nil {
					doc.Content = v
				}
			}
			if srcCol != nil {
				if v, err := srcCol.GetAsString(i); err == nil {
					doc.MetaData[SourceKey] = v
				}
			}
			if doc.MetaData[SourceKey] == nil || doc.MetaData[SourceKey] == "" {
				if src, ok := splitSource(doc.ID); ok {
					doc.MetaData[SourceKey] = src
				}
			}
			// COSINE 度量下 Milvus 返回的 score 就是余弦相似度（-1~1），
			// 与 Redis 版「1 - 余弦距离」同量纲，阈值判断无需改动。
			if i < len(r.Scores) {
				doc.MetaData[ScoreKey] = float64(r.Scores[i])
			}
			out = append(out, doc)
		}
	}
	return out, nil
}

func (s *milvusStore) DeleteBySource(ctx context.Context, source string) (int64, error) {
	ids, err := s.queryIDs(ctx, milvusFieldSource+" == "+milvusQuote(source))
	if err != nil {
		return 0, err
	}
	return s.DeleteByIDs(ctx, ids)
}

func (s *milvusStore) DeleteByIDs(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.cli.DeleteByPks(ctx, s.coll, "", entity.NewColumnVarChar(milvusFieldID, ids)); err != nil {
		return 0, fmt.Errorf("删除 Milvus 数据失败: %w", err)
	}
	if err := s.cli.Flush(ctx, s.coll, false); err != nil {
		return 0, fmt.Errorf("flush 失败: %w", err)
	}
	return int64(len(ids)), nil
}

func (s *milvusStore) AllChunks(ctx context.Context) ([]Chunk, error) {
	out := make([]Chunk, 0, 128)
	fields := []string{milvusFieldID, milvusFieldSource, milvusFieldContent}
	for offset := int64(0); ; offset += milvusPageSize {
		rs, err := s.cli.Query(ctx, s.coll, nil, "", fields,
			milvusclient.WithLimit(milvusPageSize),
			milvusclient.WithOffset(offset),
			milvusclient.WithSearchQueryConsistencyLevel(entity.ClStrong))
		if err != nil {
			// offset 超出数据范围时 Milvus 会报错，直接按已取到的数据返回
			if offset > 0 {
				return out, nil
			}
			return nil, fmt.Errorf("读取 Milvus 数据失败: %w", err)
		}
		idCol := rs.GetColumn(milvusFieldID)
		if idCol == nil {
			break
		}
		srcCol := rs.GetColumn(milvusFieldSource)
		contentCol := rs.GetColumn(milvusFieldContent)
		for i := 0; i < idCol.Len(); i++ {
			id, err := idCol.GetAsString(i)
			if err != nil {
				continue
			}
			c := Chunk{ID: id}
			if srcCol != nil {
				if v, err := srcCol.GetAsString(i); err == nil {
					c.Source = v
				}
			}
			if c.Source == "" {
				if src, ok := splitSource(id); ok {
					c.Source = src
				}
			}
			if contentCol != nil {
				if v, err := contentCol.GetAsString(i); err == nil {
					c.Content = v
				}
			}
			out = append(out, c)
		}
		if idCol.Len() < milvusPageSize {
			break
		}
	}
	return out, nil
}

func (s *milvusStore) Close() error { return s.cli.Close() }

// queryIDs 按过滤表达式取出主键，供删除使用。
func (s *milvusStore) queryIDs(ctx context.Context, expr string) ([]string, error) {
	ids := make([]string, 0, 64)
	for offset := int64(0); ; offset += milvusPageSize {
		rs, err := s.cli.Query(ctx, s.coll, nil, expr, []string{milvusFieldID},
			milvusclient.WithLimit(milvusPageSize),
			milvusclient.WithOffset(offset),
			milvusclient.WithSearchQueryConsistencyLevel(entity.ClStrong))
		if err != nil {
			if offset > 0 {
				return ids, nil
			}
			return nil, fmt.Errorf("查询 Milvus 主键失败: %w", err)
		}
		col := rs.GetColumn(milvusFieldID)
		if col == nil {
			break
		}
		for i := 0; i < col.Len(); i++ {
			v, err := col.GetAsString(i)
			if err != nil {
				continue
			}
			ids = append(ids, v)
		}
		if col.Len() < milvusPageSize {
			break
		}
	}
	return ids, nil
}

// milvusQuote 把字符串转成 Milvus 过滤表达式里的字面量。
func milvusQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func toFloat32(in []float64) []float32 {
	out := make([]float32, len(in))
	for i, v := range in {
		out[i] = float32(v)
	}
	return out
}

// trimRunes 按字符数截断，避免超出 Milvus varchar 的 max_length。
func trimRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
