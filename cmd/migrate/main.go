// migrate 把向量库里的数据从一个后端搬到另一个后端。
// 默认 redis -> milvus（把现有知识库切块搬进本地 Milvus）。
//
// 用法：
//
//	go run ./cmd/migrate                  # redis -> 当前 VECTOR_STORE（默认 milvus）
//	go run ./cmd/migrate -from redis -to milvus
//	go run ./cmd/migrate -verify          # 只比对两边切块 ID，不写入
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/schema"

	"personal-kb/internal/kb"
)

const migrateBatch = 32

func main() {
	from := flag.String("from", "redis", "源后端：redis / milvus")
	to := flag.String("to", "", "目标后端：redis / milvus（留空则用 .env 里的 VECTOR_STORE）")
	verify := flag.Bool("verify", false, "只校验两边数据是否一致，不写入")
	flag.Parse()

	ctx := context.Background()

	cfg, err := kb.Load()
	if err != nil {
		log.Fatal(err)
	}
	target := *to
	if target == "" {
		target = cfg.VectorStore
	}
	if *from == target {
		log.Fatalf("源和目标都是 %s，无需迁移", target)
	}

	emb, err := kb.NewEmbedder(ctx, cfg)
	if err != nil {
		log.Fatalf("创建 embedding 失败: %v", err)
	}

	src, err := open(ctx, cfg, emb, *from)
	if err != nil {
		log.Fatalf("打开源库(%s)失败: %v", *from, err)
	}
	defer src.Close()

	dst, err := open(ctx, cfg, emb, target)
	if err != nil {
		log.Fatalf("打开目标库(%s)失败: %v", target, err)
	}
	defer dst.Close()

	chunks, err := src.AllChunks(ctx)
	if err != nil {
		log.Fatalf("读取源库失败: %v", err)
	}
	fmt.Printf("源库 %s 共 %d 个切块\n", *from, len(chunks))
	if len(chunks) == 0 {
		log.Fatal("源库没有数据，无需迁移")
	}

	if *verify {
		verifyOnly(ctx, chunks, dst, *from, target)
		return
	}

	if err := dst.EnsureIndex(ctx); err != nil {
		log.Fatalf("准备目标库失败: %v", err)
	}

	written := 0
	for start := 0; start < len(chunks); start += migrateBatch {
		end := start + migrateBatch
		if end > len(chunks) {
			end = len(chunks)
		}
		docs := make([]*schema.Document, 0, end-start)
		for _, c := range chunks[start:end] {
			docs = append(docs, &schema.Document{
				ID:       c.ID,
				Content:  c.Content,
				MetaData: map[string]any{kb.SourceKey: c.Source},
			})
		}
		n, err := dst.Upsert(ctx, docs)
		if err != nil {
			log.Fatalf("写入目标库失败（第 %d-%d 条）: %v", start, end, err)
		}
		written += n
		fmt.Printf("  已迁移 %d/%d\n", written, len(chunks))
	}

	after, err := dst.AllChunks(ctx)
	if err != nil {
		log.Fatalf("校验目标库失败: %v", err)
	}
	fmt.Printf("迁移完成：写入 %d 个切块，目标库 %s 现有 %d 个切块\n", written, target, len(after))

	dstIDs := idSet(after)
	missing := 0
	for _, c := range chunks {
		if !dstIDs[c.ID] {
			missing++
		}
	}
	if missing > 0 {
		fmt.Printf("警告：有 %d 个切块在目标库中缺失\n", missing)
		os.Exit(1)
	}
	fmt.Println("校验通过：源库的切块在目标库中全部存在")
}

func verifyOnly(ctx context.Context, chunks []kb.Chunk, dst kb.Store, from, to string) {
	after, err := dst.AllChunks(ctx)
	if err != nil {
		log.Fatalf("读取目标库失败: %v", err)
	}
	dstIDs := idSet(after)
	missing := 0
	for _, c := range chunks {
		if !dstIDs[c.ID] {
			missing++
		}
	}
	fmt.Printf("%s=%d 条，%s=%d 条，缺失 %d 条\n", from, len(chunks), to, len(after), missing)
	if missing > 0 {
		os.Exit(1)
	}
	fmt.Println("校验通过：两边切块 ID 完全一致")
}

// open 用指定的后端类型打开向量库（临时覆盖 VECTOR_STORE）。
func open(ctx context.Context, cfg *kb.Config, emb embedding.Embedder, kind string) (kb.Store, error) {
	c := *cfg
	c.VectorStore = kind
	return kb.NewStore(ctx, &c, emb)
}

func idSet(chunks []kb.Chunk) map[string]bool {
	m := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		m[c.ID] = true
	}
	return m
}
