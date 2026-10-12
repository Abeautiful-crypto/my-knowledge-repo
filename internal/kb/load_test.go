package kb

import (
	"context"
	"strings"
	"testing"
)

func TestLoadMarkdownDir(t *testing.T) {
	docs, err := LoadMarkdownDir(context.Background(), "../../notes")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("没有切出任何分块")
	}

	for _, d := range docs {
		if d.ID == "" {
			t.Fatal("分块缺少 ID")
		}
		if strings.TrimSpace(d.Content) == "" {
			t.Fatalf("分块 %s 内容为空", d.ID)
		}
		if len(d.Content) > maxChunkChars {
			t.Fatalf("分块 %s 超过长度限制: %d", d.ID, len(d.Content))
		}
		src, ok := d.MetaData[SourceKey].(string)
		if !ok || src == "" {
			t.Fatalf("分块 %s 缺少来源信息", d.ID)
		}
	}
	t.Logf("共 %d 个分块，第一块 ID=%s", len(docs), docs[0].ID)
}
