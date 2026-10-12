// dump 把当前向量库里的所有切块导出成 JSON / CSV 文件，方便直接查看数据。
//
// 用法：
//
//	go run ./cmd/dump                          # 导出当前 VECTOR_STORE 全部切块到 kb_dump.json
//	go run ./cmd/dump -out chunks.csv -csv     # 导出 CSV（Excel 可直接打开）
//	go run ./cmd/dump -store redis             # 指定后端：redis / milvus
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"personal-kb/internal/kb"
)

func main() {
	out := flag.String("out", "kb_dump.json", "输出文件路径")
	csvMode := flag.Bool("csv", false, "输出 CSV 格式（默认 JSON）")
	storeKind := flag.String("store", "", "向量库后端：redis / milvus（留空用 .env 的 VECTOR_STORE）")
	flag.Parse()

	ctx := context.Background()
	cfg, err := kb.Load()
	if err != nil {
		log.Fatal(err)
	}
	if *storeKind != "" {
		cfg.VectorStore = *storeKind
	}

	// dump 不需要真的调 embedding，传 nil 即可
	st, err := kb.NewStore(ctx, cfg, nil)
	if err != nil {
		log.Fatalf("打开向量库(%s)失败: %v", cfg.VectorStore, err)
	}
	defer st.Close()

	chunks, err := st.AllChunks(ctx)
	if err != nil {
		log.Fatalf("读取数据失败: %v", err)
	}
	fmt.Printf("向量库 %s 共 %d 个切块\n", cfg.VectorStore, len(chunks))
	if len(chunks) == 0 {
		log.Fatal("没有数据可导出")
	}

	if *csvMode {
		writeCSV(*out, chunks)
	} else {
		writeJSON(*out, chunks)
	}
	fmt.Printf("已导出 %d 条 -> %s\n", len(chunks), *out)
}

func writeJSON(path string, chunks []kb.Chunk) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(chunks); err != nil {
		log.Fatal(err)
	}
}

func writeCSV(path string, chunks []kb.Chunk) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	// UTF-8 BOM，保证 Excel 打开中文不乱码
	_, _ = f.WriteString("\uFEFF")
	w := csv.NewWriter(f)
	if err := w.Write([]string{"id", "source", "content"}); err != nil {
		log.Fatal(err)
	}
	for _, c := range chunks {
		content := strings.ReplaceAll(c.Content, "\r\n", "\n")
		if err := w.Write([]string{c.ID, c.Source, content}); err != nil {
			log.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Fatal(err)
	}
}
