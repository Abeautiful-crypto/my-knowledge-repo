// index 负责把 Markdown 笔记灌进向量库（建索引 + embedding + 入库）。
// 具体后端由 VECTOR_STORE 决定：milvus（默认）或 redis。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"personal-kb/internal/kb"
)

func main() {
	dir := flag.String("dir", "./notes", "Markdown 笔记目录")
	reset := flag.Bool("reset", false, "先删除已有索引和全部笔记数据，再重建")
	flag.Parse()

	ctx := context.Background()

	cfg, err := kb.Load()
	if err != nil {
		log.Fatal(err)
	}

	emb, err := kb.NewEmbedder(ctx, cfg)
	if err != nil {
		log.Fatalf("创建 embedding 失败: %v", err)
	}

	store, err := kb.NewStore(ctx, cfg, emb)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if *reset {
		if err := store.DropIndex(ctx); err != nil {
			fmt.Printf("删除旧索引（可忽略）: %v\n", err)
		} else {
			fmt.Println("已删除旧索引")
		}
		if err := store.EnsureIndex(ctx); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Printf("开始导入 %s ...\n", *dir)
	total, err := kb.ReindexAll(ctx, store, *dir, false)
	if err != nil {
		log.Fatal(err)
	}
	if total == 0 {
		log.Fatalf("%s 下没有找到 .md 文件", *dir)
	}

	// 导入完成后顺带清理孤儿切块：源文件已删除但向量数据还留在向量库里
	stale, deleted, err := kb.CleanOrphans(ctx, store, *dir)
	if err != nil {
		fmt.Printf("清理孤儿切块失败（可忽略）: %v\n", err)
	} else if len(stale) > 0 {
		fmt.Printf("已清理 %d 个已删除文件的残留切块：\n", deleted)
		for _, s := range stale {
			fmt.Printf("  - %s\n", s)
		}
	}

	fmt.Printf("完成：%d 个分块已写入 %s\n", total, storeDesc(cfg, store))
	os.Exit(0)
}

func storeDesc(cfg *kb.Config, st kb.Store) string {
	if st.Kind() == "milvus" {
		return fmt.Sprintf("Milvus(%s) 集合 %s", cfg.MilvusAddr, cfg.MilvusCollection)
	}
	return fmt.Sprintf("Redis 索引 %s", cfg.Index)
}
