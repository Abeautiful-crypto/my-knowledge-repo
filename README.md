# Personal KB（基于 eino 的个人知识库）

用 CloudWeGo eino 搭的 RAG 个人知识库：本地 Markdown 笔记 → 向量库（Milvus / Redis）→ 带检索工具的智能体问答。

**两个核心能力**

1. **编辑知识库**：Web 端直接新建 / 编辑笔记原文，保存后自动重新切块入库；也可按知识库体系上传、删除。
2. **联网兜底 + 自动沉淀**：本地笔记判定检索不到时，自动联网检索生成答案，并**写回知识库**——
   由模型挑选对应的知识库（确实没有合适的就新建一个），文件头会记录来源、原问题和参考链接。

## 1. 先启动向量库

### 方案 A：Milvus（默认，推荐）

```bash
# 首次启动（见 https://milvus.io/docs/install_standalone-docker.md）
docker run -d --name milvus_standalone -p 19530:19530 -p 9091:9091 milvusdb/milvus:v2.4.13
```

`.env` 里确认：`VECTOR_STORE=milvus`、`MILVUS_ADDR=localhost:19530`、`MILVUS_COLLECTION=kb_chunks`。
集合和索引会在首次启动时自动创建（余弦相似度 + AUTOINDEX）。

### 方案 B：Redis Stack（RediSearch）

```bash
docker run -d --name redis-stack -p 6379:6379 -p 8001:8001 redis/redis-stack:latest
```

`.env` 里改成 `VECTOR_STORE=redis` 即可，其余 Redis 配置不变。
Web 控制台：http://localhost:8001 （可查看索引和 key）

### 两个库之间迁移数据

```bash
go run ./cmd/migrate                 # redis → 当前 VECTOR_STORE（默认 milvus）
go run ./cmd/migrate -from milvus -to redis
go run ./cmd/migrate -verify         # 只比对两边切块 ID，不写入
```

## 2. 配置

```bash
cp .env.example .env   # Windows 可直接复制文件
# 编辑 .env，至少填 OPENAI_API_KEY
```

### 国内直连推荐（DeepSeek 对话 + 硅基流动向量）

国内机器通常连不上 `api.openai.com`，推荐这样配：

| 用途 | 服务商 | Base URL | 模型 | Key 领取 |
|---|---|---|---|---|
| 对话 | DeepSeek | https://api.deepseek.com/v1 | `deepseek-chat` | platform.deepseek.com/api_keys |
| 向量 | 硅基流动 | https://api.siliconflow.cn/v1 | `BAAI/bge-m3`（1024 维） | cloud.siliconflow.cn/account/ak |

`EMBEDDING_API_KEY` 必须单独填硅基流动的 Key——**DeepSeek 不提供 embedding 接口**，
留空的话代码会回落到 `OPENAI_API_KEY`，调 embedding 必定 401。

```bash
cp .env.example .env   # 模板已经按国内组合写好，只需填两个 Key
```

可选更换：

- 智谱 embedding：`EMBEDDING_BASE_URL=https://open.bigmodel.cn/api/paas/v4`、`EMBEDDING_MODEL=embedding-3`、`EMBEDDING_DIM=2048`
- OpenAI：`OPENAI_BASE_URL=https://api.openai.com/v1`、`EMBEDDING_MODEL=text-embedding-3-small`、`EMBEDDING_DIM=1536`（需能直连）

> 换 embedding 模型时 `EMBEDDING_DIM` 必须与模型维度一致。若本地已有旧索引，
> 程序会检测出维度冲突并提示，此时用 `-reset` 重建即可：
> `go run ./cmd/index -dir ./notes -reset`

## 3. 建库

```bash
go run ./cmd/index -dir ./notes        # 导入 ./notes 下的所有 .md
go run ./cmd/index -dir ./notes -reset # 清空重建
```

流程：`LoadMarkdownDir`（扫描 .md）→ `markdown.NewHeaderSplitter`（按 #/##/### 切块，超长再切）→ Embedding → 写入向量库
（Milvus 集合 `kb_chunks`，字段 `id` / `source` / `content` / `vector`）。

## 4. 问答

```bash
go run ./cmd/ask -q "eino 是什么？"
go run ./cmd/ask          # 交互式，输入 exit 退出
```

## 5. Web 服务（日常推荐）

```bash
go run ./cmd/server                 # 默认监听 :8080
go run ./cmd/server -addr :9000     # 换端口
```

浏览器打开 http://localhost:8080 ，内置聊天界面，`/api/ask` 以 SSE 流式返回：
先推 `status`（正在检索），再逐段推 `delta`（回答正文），最后 `done`。

> 服务启动时向量库连接、embedding、chat model 会一次性建好复用，
> 每次提问都会新建 Runner（轻量），组件本身是并发安全的。

流程：问题向量化 → 向量库近邻检索 Top-K → `adk.ChatModelAgent` 调用检索工具并作答（ReAct 循环由框架内部处理）。

## 目录结构

```
internal/kb/config.go        环境变量/.env 配置（含 VECTOR_STORE 选型）
internal/kb/kb.go            embedder、chat model、Redis 索引/检索底层实现
internal/kb/store.go         向量库抽象 Store（Ensure/Upsert/Retrieve/Delete/AllChunks）
internal/kb/store_milvus.go  Milvus 实现（默认）
internal/kb/store_redis.go   Redis 实现
internal/kb/load.go          Markdown 加载与切块
internal/kb/agent.go         检索工具 + ChatModelAgent 装配
cmd/index/main.go            建库 CLI
cmd/ask/main.go              问答 CLI
cmd/migrate/main.go          向量库之间迁移数据（redis ⇄ milvus）
notes/                       放你的 Markdown 笔记
```

## 关键实现说明

- **索引字段**：`content`(TEXT)、`source`(TEXT)、`vector_content`(VECTOR, FLAT, COSINE)；`EnsureIndex` 幂等，已存在则跳过。
- **Redis 客户端**：检索必须用 RESP2 协议（`Protocol: 2` + `UnstableResp3: true`），写入用默认协议，因此两处分别创建 client。
- **文档 ID**：形如 `子目录/文件.md#块序号`，作为 Redis hash key，保证可重复导入覆盖。
- **eino 依赖**：`go.mod` 里用 `replace github.com/cloudwego/eino => ../eino` 指向本地源码，改框架源码能立即生效。
