package kb

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino-ext/components/document/transformer/splitter/markdown"
	"github.com/cloudwego/eino/schema"
)

// maxChunkChars 是单个分块的最大字符数，避免超长段落打爆 embedding 的输入长度限制。
const maxChunkChars = 4000

// LoadMarkdownDir 扫描目录下的所有 .md 文件，按 Markdown 标题层级切块。
// 每个分块的 ID 形如 "golang/context.md#3"，MetaData 里带来源文件名。
func LoadMarkdownDir(ctx context.Context, dir string) ([]*schema.Document, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("无法访问笔记目录 %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", dir)
	}

	var paths []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	docs := make([]*schema.Document, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", p, err)
		}
		if strings.TrimSpace(string(b)) == "" {
			continue
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel) // 统一路径分隔符，Windows 下也用 / 存储与检索
		docs = append(docs, &schema.Document{
			ID:      rel,
			Content: string(b),
			MetaData: map[string]any{
				SourceKey: rel,
			},
		})
	}

	return SplitMarkdown(ctx, docs)
}

// SplitMarkdown 按 #/##/### 三级标题切块，并限制单块长度。
func SplitMarkdown(ctx context.Context, docs []*schema.Document) ([]*schema.Document, error) {
	splitter, err := markdown.NewHeaderSplitter(ctx, &markdown.HeaderConfig{
		Headers: map[string]string{
			"#":   "h1",
			"##":  "h2",
			"###": "h3",
		},
		TrimHeaders: false,
		IDGenerator: func(_ context.Context, originalID string, splitIndex int) string {
			return fmt.Sprintf("%s#%d", originalID, splitIndex)
		},
	})
	if err != nil {
		return nil, err
	}

	chunks, err := splitter.Transform(ctx, docs)
	if err != nil {
		return nil, fmt.Errorf("切分 markdown 失败: %w", err)
	}

	result := make([]*schema.Document, 0, len(chunks))
	for _, c := range chunks {
		if strings.TrimSpace(c.Content) == "" {
			continue
		}
		if c.MetaData == nil {
			c.MetaData = map[string]any{}
		}
		if _, ok := c.MetaData[SourceKey]; !ok {
			if base, _, found := strings.Cut(c.ID, "#"); found {
				c.MetaData[SourceKey] = base
			}
		}
		result = append(result, splitOversize(c)...)
	}
	return result, nil
}

// splitOversize 把超长分块按行再切一刀，保持来源信息不变。
func splitOversize(doc *schema.Document) []*schema.Document {
	if len(doc.Content) <= maxChunkChars {
		return []*schema.Document{doc}
	}

	var (
		parts  []*schema.Document
		buf    strings.Builder
		idx    int
		append = func() {
			text := strings.TrimSpace(buf.String())
			buf.Reset()
			if text == "" {
				return
			}
			md := make(map[string]any, len(doc.MetaData))
			for k, v := range doc.MetaData {
				md[k] = v
			}
			parts = append(parts, &schema.Document{
				ID:       fmt.Sprintf("%s-%d", doc.ID, idx),
				Content:  text,
				MetaData: md,
			})
			idx++
		}
	)

	for _, line := range strings.Split(doc.Content, "\n") {
		if buf.Len()+len(line)+1 > maxChunkChars && buf.Len() > 0 {
			append()
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	append()
	return parts
}
