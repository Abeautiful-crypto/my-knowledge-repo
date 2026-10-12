package kb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/cloudwego/eino/schema"
)

// FileInfo 是按来源文件聚合后的知识库条目。
type FileInfo struct {
	Source  string `json:"source"`  // 相对 notesDir 的路径（/ 分隔）
	Chunks  int    `json:"chunks"`  // 切块数
	Chars   int    `json:"chars"`   // 正文字符数
	Preview string `json:"preview"` // 开头片段，用于列表预览
	KB      string `json:"kb"`      // 所属知识库（顶层子目录名；根目录文件为 default）
	ModTime int64  `json:"modTime"` // 源文件修改时间（unix 秒；索引残留时为 0）
	Orphan  bool   `json:"orphan"`  // 源文件已不存在，只剩向量库里的切块
}

// ListFiles 扫描向量库中全部切块，按来源文件聚合。
func ListFiles(ctx context.Context, st Store, notesDir string) ([]FileInfo, error) {
	chunks, err := st.AllChunks(ctx)
	if err != nil {
		return nil, err
	}

	agg := map[string]*FileInfo{}
	var sources []string
	for _, c := range chunks {
		if c.Source == "" {
			continue
		}
		fi := agg[c.Source]
		if fi == nil {
			fi = &FileInfo{Source: c.Source}
			agg[c.Source] = fi
			sources = append(sources, c.Source)
		}
		fi.Chunks++
		fi.Chars += len([]rune(c.Content))
		if fi.Preview == "" {
			fi.Preview = firstRunes(c.Content, 80)
		}
	}

	sort.Strings(sources)
	out := make([]FileInfo, 0, len(sources))
	for _, s := range sources {
		fi := agg[s]
		if i := strings.IndexByte(s, '/'); i > 0 {
			fi.KB = s[:i]
		} else {
			fi.KB = DefaultKBName
		}
		if st, err := os.Stat(filepath.Join(notesDir, filepath.FromSlash(s))); err == nil {
			fi.ModTime = st.ModTime().Unix()
		} else {
			fi.Orphan = true
		}
		out = append(out, *fi)
	}
	return out, nil
}

// DefaultKBName 是根目录文件（不在任何子目录里）归属的知识库名。
const DefaultKBName = "默认笔记库"

// CleanOrphans 清理向量库中的「孤儿切块」：源 .md 文件已被删除、
// 但切块还留在向量库里的数据。返回被清理的来源列表与删除的切块数。
func CleanOrphans(ctx context.Context, st Store, notesDir string) ([]string, int, error) {
	chunks, err := st.AllChunks(ctx)
	if err != nil {
		return nil, 0, err
	}
	if len(chunks) == 0 {
		return nil, 0, nil
	}

	// 按 source 聚合切块 ID
	bySource := map[string][]string{}
	for _, c := range chunks {
		if c.Source == "" {
			continue
		}
		bySource[c.Source] = append(bySource[c.Source], c.ID)
	}

	var stale []string
	var staleIDs []string
	for src, ids := range bySource {
		if _, err := os.Stat(filepath.Join(notesDir, filepath.FromSlash(src))); os.IsNotExist(err) {
			stale = append(stale, src)
			staleIDs = append(staleIDs, ids...)
		}
	}
	if len(staleIDs) == 0 {
		return nil, 0, nil
	}

	n, err := st.DeleteByIDs(ctx, staleIDs)
	sort.Strings(stale)
	return stale, int(n), err
}

// DeleteSource 删除某个来源文件的全部切块，返回删除的切块数。
func DeleteSource(ctx context.Context, st Store, source string) (int64, error) {
	return st.DeleteBySource(ctx, source)
}

// deleteSourceKeys 是 Redis 后端的按来源删除实现（前缀扫描 key）。
func deleteSourceKeys(ctx context.Context, cfg *Config, client *redis.Client, source string) (int64, error) {
	var keys []string
	iter := client.Scan(ctx, 0, cfg.KeyPrefix+source+"#*", 500).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if len(keys) == 0 {
		return 0, nil
	}
	return client.Del(ctx, keys...).Result()
}

// LoadMarkdownFile 读取 notesDir 下的单个 Markdown 文件并切块。
// source 必须是相对 notesDir 的 / 分隔路径，禁止目录穿越。
func LoadMarkdownFile(ctx context.Context, notesDir, source string) ([]*schema.Document, error) {
	clean, err := filepath.Abs(filepath.Join(notesDir, filepath.FromSlash(source)))
	if err != nil {
		return nil, err
	}
	base, err := filepath.Abs(notesDir)
	if err != nil {
		return nil, err
	}
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return nil, fmt.Errorf("非法路径: %s", source)
	}
	b, err := os.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", source, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, fmt.Errorf("%s 是空文件", source)
	}
	doc := &schema.Document{
		ID:       source,
		Content:  string(b),
		MetaData: map[string]any{SourceKey: source},
	}
	return SplitMarkdown(ctx, []*schema.Document{doc})
}

// ReindexSource 对单个文件重新切块入库（先删旧块）。
func ReindexSource(ctx context.Context, st Store, notesDir, source string) (int, error) {
	docs, err := LoadMarkdownFile(ctx, notesDir, source)
	if err != nil {
		return 0, err
	}
	if _, err := DeleteSource(ctx, st, source); err != nil {
		return 0, fmt.Errorf("清理旧切块失败: %w", err)
	}
	if _, err := st.Upsert(ctx, docs); err != nil {
		return 0, err
	}
	return len(docs), nil
}

// ReindexAll 全量重建：reset=true 时先删索引，再导入 notesDir 下所有 Markdown。
func ReindexAll(ctx context.Context, st Store, notesDir string, reset bool) (int, error) {
	if reset {
		if err := st.DropIndex(ctx); err != nil {
			fmt.Printf("删除旧索引（可忽略）: %v\n", err)
		}
	}
	if err := st.EnsureIndex(ctx); err != nil {
		return 0, err
	}
	docs, err := LoadMarkdownDir(ctx, notesDir)
	if err != nil {
		return 0, err
	}
	if _, err := st.Upsert(ctx, docs); err != nil {
		return 0, err
	}
	return len(docs), nil
}

func firstRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
