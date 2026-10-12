// ask 基于已入库的笔记回答问题：单次提问（-q）或交互式问答。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"personal-kb/internal/kb"
)

func main() {
	question := flag.String("q", "", "直接提问；留空则进入交互式问答（输入 exit 退出）")
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

	cm, err := kb.NewChatModel(ctx, cfg)
	if err != nil {
		log.Fatalf("创建 chat model 失败: %v", err)
	}

	store, err := kb.NewStore(ctx, cfg, emb)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	ret, err := kb.NewRetriever(ctx, cfg, store)
	if err != nil {
		log.Fatalf("创建 retriever 失败: %v", err)
	}

	agent, err := kb.NewAgent(ctx, cm, ret)
	if err != nil {
		log.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})

	if *question != "" {
		answer(ctx, runner, *question)
		return
	}

	fmt.Println("个人知识库问答（输入 exit 退出）")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return
		}
		answer(ctx, runner, line)
	}
}

func answer(ctx context.Context, runner *adk.Runner, question string) {
	var final string

	iter := runner.Query(ctx, question)
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			fmt.Printf("执行出错: %v\n", event.Err)
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}

		mo := event.Output.MessageOutput
		if mo.Role == schema.Tool {
			fmt.Printf("  [检索笔记] %s\n", oneLine(mo.Message.Content, 100))
			continue
		}
		if mo.IsStreaming || mo.Message == nil || mo.Message.Role != schema.Assistant {
			continue
		}
		// 带 ToolCalls 的消息只是「准备调工具」的中间步骤，不展示给用户
		if len(mo.Message.ToolCalls) > 0 {
			continue
		}
		if mo.Message.Content != "" {
			final = mo.Message.Content
		}
	}

	if final != "" {
		fmt.Println("\n" + final)
	}
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return s
}
