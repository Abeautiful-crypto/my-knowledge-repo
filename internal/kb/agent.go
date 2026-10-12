package kb

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
)

const instruction = `你是「个人知识库助手」，只能基于笔记内容回答问题。

规则：
1. 回答问题前，必须先调用 search_knowledge_base 工具检索笔记，最多检索 3 次。
2. 如果检索结果不足以回答，就说明知识库里没有相关内容，不要编造。
3. 回答要用中文，简洁准确，并在末尾用「来源：文件名」列出引用过的笔记。
`

type SearchInput struct {
	Query string `json:"query" jsonschema:"required" jsonschema_description:"用于检索笔记的关键词或自然语言问题"`
}

type SearchResult struct {
	Source  string  `json:"source"`
	Content string  `json:"content"`
	Score   float64 `json:"score"` // 与问题的相似度 0~1，无分数时为 0
}

type SearchOutput struct {
	Results []SearchResult `json:"results"`
}

// NewSearchTool 把 Redis retriever 包装成大模型可调用的检索工具。
func NewSearchTool(ctx context.Context, r retriever.Retriever) (tool.BaseTool, error) {
	return utils.InferTool[SearchInput, SearchOutput](
		"search_knowledge_base",
		"在个人知识库中检索与问题相关的笔记片段，返回片段内容和来源文件名。",
		func(ctx context.Context, input SearchInput) (SearchOutput, error) {
			docs, err := r.Retrieve(ctx, input.Query)
			if err != nil {
				return SearchOutput{}, err
			}
			out := SearchOutput{}
			for _, d := range docs {
				src := ""
				if d.MetaData != nil {
					if s, ok := d.MetaData[SourceKey].(string); ok {
						src = s
					}
				}
				if src == "" {
					src = d.ID
				}
				score, _ := d.MetaData[ScoreKey].(float64)
				out.Results = append(out.Results, SearchResult{Source: src, Content: d.Content, Score: score})
			}
			return out, nil
		},
	)
}

// NewAgent 创建带检索工具的 ChatModelAgent（ReAct 循环由框架内部处理）。
func NewAgent(ctx context.Context, cm model.ChatModel, r retriever.Retriever) (adk.Agent, error) {
	searchTool, err := NewSearchTool(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("创建检索工具失败: %w", err)
	}

	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "kb-assistant",
		Description:   "基于个人 Markdown 笔记回答问题的助手",
		Instruction:   instruction,
		Model:         cm,
		MaxIterations: 8,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{searchTool},
			},
		},
	})
}
