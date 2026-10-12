package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AbsSource 把 source（相对 notesDir 的 / 分隔路径）转成绝对路径，并拒绝目录穿越。
func AbsSource(notesDir, source string) (string, error) {
	clean, err := filepath.Abs(filepath.Join(notesDir, filepath.FromSlash(source)))
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(notesDir)
	if err != nil {
		return "", err
	}
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("非法路径: %s", source)
	}
	return clean, nil
}

// ReadSource 读取笔记原文。
func ReadSource(notesDir, source string) (string, error) {
	p, err := AbsSource(notesDir, source)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", source, err)
	}
	return string(b), nil
}

// WriteSource 写入笔记内容（目录不存在时自动创建），返回写入字节数。
func WriteSource(notesDir, source, content string) (int, error) {
	p, err := AbsSource(notesDir, source)
	if err != nil {
		return 0, err
	}
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, fmt.Errorf("创建目录失败: %w", err)
		}
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return 0, fmt.Errorf("写入 %s 失败: %w", source, err)
	}
	return len(content), nil
}

// DeleteSourceFile 删除磁盘上的笔记文件（向量切块由 DeleteSource 单独处理）。
func DeleteSourceFile(notesDir, source string) error {
	p, err := AbsSource(notesDir, source)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败: %w", source, err)
	}
	return nil
}
