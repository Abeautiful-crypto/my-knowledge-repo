// server 启动本地 Web 问答服务：聊天页面 + SSE 问答 + 笔记管理接口。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"personal-kb/internal/kb"
)

//go:embed web
var webFS embed.FS

type app struct {
	cfg      *kb.Config
	store    kb.Store
	emb      embedding.Embedder
	cm       model.ChatModel
	ret      retriever.Retriever
	notesDir string
	agent    adk.Agent

	pendMu  sync.Mutex
	pending map[string]*pendingNote // 等待用户确认是否入库的联网沉淀
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址（默认仅本机访问）")
	notesDir := flag.String("notes", "./notes", "笔记根目录")
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
		log.Fatalf("初始化向量库失败: %v", err)
	}
	defer store.Close()

	// 启动时清理孤儿切块：源 .md 已被删除、向量数据还残留在向量库里的部分
	if stale, deleted, err := kb.CleanOrphans(ctx, store, *notesDir); err != nil {
		log.Printf("清理孤儿切块失败（可忽略）: %v", err)
	} else if deleted > 0 {
		log.Printf("已清理 %d 个孤儿切块（源文件已删除）: %v", deleted, stale)
	}

	ret, err := kb.NewRetriever(ctx, cfg, store)
	if err != nil {
		log.Fatalf("创建 retriever 失败: %v", err)
	}

	agent, err := kb.NewAgent(ctx, cm, ret)
	if err != nil {
		log.Fatal(err)
	}

	a := &app{
		cfg:      cfg,
		store:    store,
		emb:      emb,
		cm:       cm,
		ret:      ret,
		notesDir: *notesDir,
		agent:    agent,
	}
	log.Printf("向量库: %s", storeDesc(cfg, store))
	log.Printf("联网检索: %s（本地命中阈值 %.2f，沉淀模式 %s）",
		searchDesc(cfg), cfg.LocalScoreThreshold, cfg.WebSaveMode)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/api/ask", a.handleAsk)
	http.HandleFunc("/api/stats", a.handleStats)
	http.HandleFunc("/api/files", a.handleFiles)
	http.HandleFunc("/api/files/content", a.handleFileContent)
	http.HandleFunc("/api/files/save", a.handleFileSave)
	http.HandleFunc("/api/files/create", a.handleFileCreate)
	http.HandleFunc("/api/files/delete", a.handleDelete)
	http.HandleFunc("/api/files/reindex", a.handleReindex)
	http.HandleFunc("/api/files/upload", a.handleUpload)
	http.HandleFunc("/api/index/rebuild", a.handleRebuild)
	http.HandleFunc("/api/kb/create", a.handleCreateKB)
	http.HandleFunc("/api/kb/delete", a.handleDeleteKB)
	http.HandleFunc("/api/web/save", a.handleWebSave)

	log.Printf("个人知识库已就绪: http://%s（笔记目录 %s）", *addr, *notesDir)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

/* ================= 问答（SSE） ================= */

func (a *app) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Question == "" {
		http.Error(w, "缺少 question 参数", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式输出", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(event string, data any) {
		payload, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		flusher.Flush()
	}

	ctx := r.Context()

	// 先在本地笔记里探一次：命中就走原来的笔记问答流程，
	// 否则转去联网检索，并把答案沉淀回知识库。
	best, snippets := a.localRetrieve(ctx, req.Question)
	log.Printf("[ask] question=%q 本地最高相似度=%.4f 命中阈值=%.2f", req.Question, best, a.cfg.LocalScoreThreshold)

	// 单次相似度会有波动，所以分三档处理：
	// 高分直接认定命中；低分直接认定没有；中间地带让模型看着片段判断一次。
	hit := best >= a.cfg.LocalScoreThreshold
	if !hit && best > missScore {
		hit = a.localRelevant(ctx, req.Question, snippets)
		log.Printf("[ask] 中间地带(%.4f) 模型复核结果: hit=%v", best, hit)
	}
	if hit {
		send("mode", "local")
		a.runLocalAgent(ctx, req.Question, send)
		send("done", "")
		return
	}

	send("mode", "web")
	results, err := kb.WebSearch(ctx, a.cfg, req.Question)
	switch {
	case err != nil && errors.Is(err, kb.ErrSearchDisabled):
		send("notice", "未配置联网检索服务（.env 里填 SEARCH_PROVIDER / SEARCH_API_KEY），以下为模型自身知识，未经联网核验")
	case err != nil:
		send("notice", "联网检索失败："+err.Error()+"；已改用模型自身知识回答")
	case len(results) == 0:
		send("notice", "联网检索没有返回结果，以下为模型自身知识")
	default:
		send("websources", results)
	}

	answer := a.streamWebAnswer(ctx, send, req.Question, results)
	if strings.TrimSpace(answer) != "" {
		info, err := a.prepareSave(ctx, req.Question, answer, results)
		if err != nil {
			log.Printf("[ask] 准备沉淀到知识库失败: %v", err)
			send("notice", "答案已生成，但准备沉淀到知识库失败："+err.Error())
		} else if strings.ToLower(a.cfg.WebSaveMode) == "inbox" {
			// inbox 模式：不打扰用户，直接进「待确认」隔离库
			saved, err := a.commitSave(ctx, info.ID, kb.WebSaveInboxName)
			if err != nil {
				log.Printf("[ask] 沉淀到知识库失败: %v", err)
				send("notice", "答案已生成，但沉淀到知识库失败："+err.Error())
			} else {
				send("saved", saved)
			}
		} else {
			// 默认：先问用户是否入库，确认后再写盘
			send("confirm", info)
		}
	}
	send("done", "")
}

// runLocalAgent 用笔记检索 Agent 生成回答（本地命中时走这条路）。
func (a *app) runLocalAgent(ctx context.Context, question string, send func(string, any)) {
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           a.agent,
		EnableStreaming: true,
	})

	gotContent := false
	iter := runner.Query(ctx, question)
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			log.Printf("[ask] 执行出错 (question=%q): %v", question, event.Err)
			send("error", event.Err.Error())
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}

		mo := event.Output.MessageOutput
		if mo.Role == schema.Tool {
			// 检索工具的输出是 SearchOutput 的 JSON，
			// 把来源和相似度作为 sources 事件推给前端做溯源卡片。
			var out kb.SearchOutput
			if err := json.Unmarshal([]byte(mo.Message.Content), &out); err == nil && len(out.Results) > 0 {
				send("sources", out.Results)
			}
			continue
		}
		// 流式事件里 Message 为 nil，正文要从 MessageStream 里读，
		// 所以必须先处理流式分支，再判断 Message。
		if mo.IsStreaming {
			if mo.MessageStream == nil {
				continue
			}
			for {
				msg, err := mo.MessageStream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Printf("[ask] 流式读取出错 (question=%q): %v", question, err)
					send("error", err.Error())
					break
				}
				// 触发工具调用的中间步骤没有正文内容，跳过即可
				if msg.Content != "" {
					gotContent = true
					send("delta", msg.Content)
				}
			}
			continue
		}
		if mo.Message == nil {
			continue
		}
		if mo.Message.Role == schema.Assistant && len(mo.Message.ToolCalls) == 0 && mo.Message.Content != "" {
			gotContent = true
			send("delta", mo.Message.Content)
		}
	}
	// 流结束但没有产生任何内容：多半是模型 API 限流/网络波动导致静默失败，
	// 明确告知前端，别让用户只看到含糊的空回答。
	if !gotContent {
		log.Printf("[ask] 未产生任何回答 (question=%q)：大概率是模型 API 限流或网络波动", question)
		send("error", "模型没有返回内容：大概率是模型接口被限流或网络波动（重试一次通常即可）")
	}
}

// missScore 低于该相似度直接判为「本地没有」，不做模型复核。
const missScore = 0.40

// localRetrieve 在本地笔记里检索一次，返回最高相似度和命中的片段文本。
func (a *app) localRetrieve(ctx context.Context, question string) (float64, []string) {
	docs, err := a.ret.Retrieve(ctx, question)
	if err != nil {
		log.Printf("[ask] 本地检索失败: %v", err)
		return 0, nil
	}
	best := 0.0
	snippets := make([]string, 0, len(docs))
	for _, d := range docs {
		score := 0.0
		if d.MetaData != nil {
			if s, ok := d.MetaData[kb.ScoreKey].(float64); ok {
				score = s
			}
		}
		if score > best {
			best = score
		}
		snippets = append(snippets, strings.TrimSpace(d.Content))
	}
	return best, snippets
}

// localRelevant 让模型判断检索到的片段能否真正回答这个问题（相似度处于中间地带时用）。
func (a *app) localRelevant(ctx context.Context, question string, snippets []string) bool {
	if len(snippets) == 0 {
		return false
	}
	var sb strings.Builder
	for i, s := range snippets {
		if len([]rune(s)) > 300 {
			s = string([]rune(s)[:300])
		}
		sb.WriteString(fmt.Sprintf("%d) %s\n", i+1, s))
	}
	msgs := []*schema.Message{{Role: schema.User, Content: fmt.Sprintf(
		`你是严格的相关性判定器。判断下面的笔记片段是否包含回答用户问题所需的【具体事实】。

判定规则：
1. 只有片段里【确实写着】能支撑答案的事实，才算能回答。
2. 只是话题相近、主题沾边 → 不能回答。严禁凭你自己的知识脑补，只认片段原文。
3. 必须在 evidence 字段里原样摘抄片段中支撑你判断的那句话（不要改写、不要编造）。
   如果摘不出这样的原句，就说明不能回答，输出 no。
4. 拿不准就判为不能回答。

只输出一行 JSON，不要任何解释：{"answerable":"yes|no","evidence":"片段原文摘抄","reason":"一句话理由"}

用户问题：%s

笔记片段：
%s`, question, sb.String())}}

	resp, err := a.cm.Generate(ctx, msgs)
	if err != nil {
		log.Printf("[ask] 相关性复核调用失败，按本地未命中处理: %v", err)
		return false
	}
	log.Printf("[ask] 相关性复核: %s", strings.TrimSpace(resp.Content))

	var out struct {
		Answerable string `json:"answerable"`
		Evidence   string `json:"evidence"`
	}
	if err := json.Unmarshal(extractJSON(resp.Content), &out); err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(out.Answerable), "yes") {
		return false
	}
	// 防幻觉：模型声称能回答时，它摘抄的原句必须真的出现在片段里
	if !evidenceFound(out.Evidence, snippets) {
		log.Printf("[ask] 复核判定为 yes，但摘抄的原句不在片段中，改判为未命中（evidence=%q）", out.Evidence)
		return false
	}
	return true
}

// evidenceFound 判断模型摘抄的原句是否真的出现在任一笔记片段中（忽略空白差异）。
func evidenceFound(evidence string, snippets []string) bool {
	ev := strings.Join(strings.Fields(evidence), "")
	if len([]rune(ev)) < 6 {
		return false
	}
	for _, s := range snippets {
		if strings.Contains(strings.Join(strings.Fields(s), ""), ev) {
			return true
		}
	}
	return false
}

const webSystemPrompt = `你是一个严谨的问答助手，基于给定的联网检索结果回答用户问题。

规则：
1. 优先使用检索结果中的信息；检索结果不足时明确说明哪里不足。
2. 用中文作答，结构清晰（要点分条），不要编造检索结果里没有的细节。
3. 末尾用「参考来源：」列出你用到的网页标题和链接。`

// streamWebAnswer 基于联网结果生成回答并流式推给前端，返回完整答案。
func (a *app) streamWebAnswer(ctx context.Context, send func(string, any), question string, results []kb.WebResult) string {
	userContent := fmt.Sprintf("问题：%s\n\n联网检索结果：\n%s", question, formatWebResults(results))
	if len(results) == 0 {
		userContent = "问题：" + question + "\n\n（没有联网检索结果，请根据你的知识回答，开头明确说明这是模型自身知识、未经联网核验。）"
	}

	sr, err := a.cm.Stream(ctx, []*schema.Message{
		{Role: schema.System, Content: webSystemPrompt},
		{Role: schema.User, Content: userContent},
	})
	if err != nil {
		send("error", "生成回答失败："+err.Error())
		return ""
	}
	defer sr.Close()

	var sb strings.Builder
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			send("error", "流式读取失败："+err.Error())
			break
		}
		if msg.Content != "" {
			sb.WriteString(msg.Content)
			send("delta", msg.Content)
		}
	}
	return sb.String()
}

func formatWebResults(results []kb.WebResult) string {
	if len(results) == 0 {
		return "（无）"
	}
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("[%d] %s\n链接: %s\n摘要: %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return sb.String()
}

// confirmInfo 通过 SSE 推给前端，请求用户确认是否入库。
type confirmInfo struct {
	ID    string `json:"id"`
	KB    string `json:"kb"`
	Title string `json:"title"`
}

// savedInfo 描述一次沉淀结果，推给前端展示。
type savedInfo struct {
	KB     string `json:"kb"`
	Source string `json:"source"`
	Mode   string `json:"mode"` // direct 直接入库 / inbox 待确认
}

// pendingNote 是一条等待用户确认的沉淀草稿，确认后才真正写盘入库。
type pendingNote struct {
	KB      string
	Title   string
	Content string
	Created time.Time
}

// prepareSave 生成待沉淀的笔记草稿：让模型挑一个知识库（不贴切就新建），
// 但不写盘，等用户在前端确认后再入库。
func (a *app) prepareSave(ctx context.Context, question, answer string, results []kb.WebResult) (*confirmInfo, error) {
	kbName := a.pickKB(ctx, question)
	// 联网沉淀不进默认笔记库：拿不到贴切结论时新建一个
	if kbName == "" || kbName == kb.DefaultKBName || kbName == "全部" {
		kbName = "联网知识-" + time.Now().Format("01021504")
	}
	title := a.pickTitle(ctx, question)
	content := buildNoteContent(question, answer, results)

	id := fmt.Sprintf("p%d", time.Now().UnixNano())
	a.pendMu.Lock()
	if a.pending == nil {
		a.pending = map[string]*pendingNote{}
	}
	for k, v := range a.pending { // 顺手清掉超过 1 小时没人确认的草稿
		if time.Since(v.Created) > time.Hour {
			delete(a.pending, k)
		}
	}
	a.pending[id] = &pendingNote{KB: kbName, Title: title, Content: content, Created: time.Now()}
	a.pendMu.Unlock()

	log.Printf("[ask] 待确认沉淀 %q → 知识库「%s」（标题 %s）", question, kbName, title)
	return &confirmInfo{ID: id, KB: kbName, Title: title}, nil
}

// commitSave 用户确认后真正写盘入库：知识库目录不存在则自动创建，
// 同名文件已存在时加序号避免覆盖。
func (a *app) commitSave(ctx context.Context, id, kbName string) (*savedInfo, error) {
	a.pendMu.Lock()
	pn := a.pending[id]
	delete(a.pending, id)
	a.pendMu.Unlock()
	if pn == nil {
		return nil, fmt.Errorf("待确认的沉淀不存在或已过期，请重新提问")
	}
	// 用户改过目标库就以用户为准；联网沉淀不允许进默认笔记库
	if kbName == "" || !validKBName.MatchString(kbName) || kbName == kb.DefaultKBName || kbName == "全部" {
		kbName = pn.KB
	}
	if kbName == "" || kbName == kb.DefaultKBName || kbName == "全部" {
		kbName = "联网知识-" + time.Now().Format("01021504")
	}

	rel := filepath.ToSlash(filepath.Join(kbName, pn.Title+".md"))
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(a.notesDir, filepath.FromSlash(rel))); err != nil {
			break
		}
		rel = filepath.ToSlash(filepath.Join(kbName, fmt.Sprintf("%s-%d.md", pn.Title, time.Now().Unix()%10000+int64(i))))
	}

	if _, err := kb.WriteSource(a.notesDir, rel, pn.Content); err != nil {
		return nil, err
	}
	chunks, err := kb.ReindexSource(ctx, a.store, a.notesDir, rel)
	if err != nil {
		return nil, err
	}
	log.Printf("[ask] 已确认入库 %s → %s（%d 切块）", pn.Title, rel, chunks)
	return &savedInfo{KB: kbName, Source: rel, Mode: "direct"}, nil
}

// handleWebSave 用户在弹窗里确认/取消沉淀：确认则写盘入库，取消则丢弃草稿。
func (a *app) handleWebSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		KB     string `json:"kb"`
		Cancel bool   `json:"cancel"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	if req.Cancel {
		a.pendMu.Lock()
		delete(a.pending, req.ID)
		a.pendMu.Unlock()
		a.writeJSON(w, map[string]any{"saved": false})
		return
	}
	saved, err := a.commitSave(r.Context(), req.ID, req.KB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeJSON(w, map[string]any{"saved": true, "kb": saved.KB, "source": saved.Source})
}

// pickKB 让模型决定归入哪个知识库：优先复用现有体系，都不贴切才新建。
// 注意：联网沉淀永不落入「默认笔记库」，模型失败时回退到新建库名。
func (a *app) pickKB(ctx context.Context, question string) string {
	existing := a.listKBDirs()
	fallback := "联网知识-" + time.Now().Format("01021504")

	msgs := []*schema.Message{{Role: schema.User, Content: fmt.Sprintf(
		`你是知识库管理员。现有知识库：%s

用户刚问了：%s

请判断这条问答该归入哪个知识库：优先从上面已有的里选一个最贴切的；确实都不贴切才新建一个，新名字用 2-6 个字的中文主题词。
注意：不要选「默认笔记库」，宁可新建一个主题库。
只输出一行 JSON，不要任何解释：{"kb":"知识库名"}`, strings.Join(existing, "、"), question)}}

	resp, err := a.cm.Generate(ctx, msgs)
	if err != nil {
		log.Printf("[ask] 归库判断失败，回退默认库: %v", err)
		return fallback
	}
	var out struct {
		KB string `json:"kb"`
	}
	if err := json.Unmarshal(extractJSON(resp.Content), &out); err != nil || out.KB == "" {
		log.Printf("[ask] 归库判断返回异常(%q)，回退默认库", resp.Content)
		return fallback
	}
	name := strings.TrimSpace(out.KB)
	if !validKBName.MatchString(name) {
		return fallback
	}
	return name
}

// pickTitle 让模型给沉淀的笔记起个文件名。
func (a *app) pickTitle(ctx context.Context, question string) string {
	msgs := []*schema.Message{{Role: schema.User, Content: fmt.Sprintf(
		`给下面这个问题起一个 8-16 字的中文笔记标题，能概括主题，只输出标题本身，不要标点、不要扩展名：

%s`, question)}}

	resp, err := a.cm.Generate(ctx, msgs)
	if err != nil {
		return "联网问答-" + time.Now().Format("01021504")
	}
	title := strings.TrimSpace(strings.Trim(resp.Content, `"`+"` \n"))
	title = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return -1
		}
		return r
	}, title)
	if len([]rune(title)) < 2 || len([]rune(title)) > 40 {
		return "联网问答-" + time.Now().Format("01021504")
	}
	return title
}

// listKBDirs 列出 notes 下的知识库目录名。
func (a *app) listKBDirs() []string {
	entries, err := os.ReadDir(a.notesDir)
	if err != nil {
		return []string{kb.DefaultKBName}
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	names = append(names, kb.DefaultKBName)
	return names
}

// extractJSON 从模型输出里抠出第一个 JSON 对象（模型常带 ```json 围栏）。
func extractJSON(s string) []byte {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = strings.TrimPrefix(s[i:], "```json")
		s = strings.TrimPrefix(s, "```")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return []byte(s)
	}
	return []byte(s[start : end+1])
}

func buildNoteContent(question, answer string, results []kb.WebResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", strings.TrimSpace(question)))
	sb.WriteString(fmt.Sprintf("> 来源：联网检索沉淀 · %s\n", time.Now().Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("> 原问题：%s\n\n", strings.TrimSpace(question)))

	if len(results) > 0 {
		sb.WriteString("## 参考来源\n\n")
		for _, r := range results {
			title := r.Title
			if title == "" {
				title = r.URL
			}
			sb.WriteString(fmt.Sprintf("- [%s](%s)\n", title, r.URL))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## 内容\n\n")
	sb.WriteString(strings.TrimSpace(answer))
	sb.WriteString("\n")
	return sb.String()
}

// storeDesc 描述当前使用的向量库后端。
func storeDesc(cfg *kb.Config, st kb.Store) string {
	if st.Kind() == "milvus" {
		return fmt.Sprintf("Milvus(%s) 集合 %s", cfg.MilvusAddr, cfg.MilvusCollection)
	}
	return fmt.Sprintf("Redis(%s) 索引 %s", cfg.RedisAddr, cfg.Index)
}

func searchDesc(cfg *kb.Config) string {
	if !cfg.SearchEnabled() {
		return "未启用（本地笔记检索不到时只能用模型自身知识）"
	}
	return fmt.Sprintf("%s（TopK=%d）", cfg.SearchProvider, cfg.SearchTopK)
}

/* ================= 统计 / 文件管理 ================= */

func (a *app) writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(data)
}

func (a *app) readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "参数错误: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (a *app) handleStats(w http.ResponseWriter, r *http.Request) {
	files, err := kb.ListFiles(r.Context(), a.store, a.notesDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type kbInfo struct {
		Name   string `json:"name"`
		Files  int    `json:"files"`
		Chunks int    `json:"chunks"`
	}
	var (
		totalChunks, totalChars int
		kbOrder                 []string
		kbMap                   = map[string]*kbInfo{}
	)
	for _, f := range files {
		totalChunks += f.Chunks
		totalChars += f.Chars
		k := kbMap[f.KB]
		if k == nil {
			k = &kbInfo{Name: f.KB}
			kbMap[f.KB] = k
			kbOrder = append(kbOrder, f.KB)
		}
		k.Files++
		k.Chunks += f.Chunks
	}
	kbs := make([]kbInfo, 0, len(kbOrder))
	for _, name := range kbOrder {
		kbs = append(kbs, *kbMap[name])
	}

	// notes 下的子目录即使还没有任何入库文件，也作为知识库体系展示，
	// 否则新建的空知识库在前端看不到。
	if entries, err := os.ReadDir(a.notesDir); err == nil {
		known := make(map[string]bool, len(kbs))
		for _, k := range kbs {
			known[k.Name] = true
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || strings.HasPrefix(name, ".") || known[name] {
				continue
			}
			kbs = append(kbs, kbInfo{Name: name})
		}
	}

	a.writeJSON(w, map[string]any{
		"files":      len(files),
		"chunks":     totalChunks,
		"chars":      totalChars,
		"tokensEst":  totalChars / 2,
		"index":      a.cfg.Index,
		"chatModel":  a.cfg.ChatModel,
		"embedModel": a.cfg.EmbeddingModel,
		"dim":        a.cfg.EmbeddingDim,
		"topk":       a.cfg.TopK,
		"redisAddr":  a.cfg.RedisAddr,
		"kbs":        kbs,
	})
}

func (a *app) handleFiles(w http.ResponseWriter, r *http.Request) {
	files, err := kb.ListFiles(r.Context(), a.store, a.notesDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, files)
}

func (a *app) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	n, err := kb.DeleteSource(r.Context(), a.store, req.Source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]any{"deleted": n})
}

/* ================= 笔记编辑 ================= */

// handleFileContent 读取笔记原文，供前端编辑器使用。
func (a *app) handleFileContent(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		http.Error(w, "缺少 source 参数", http.StatusBadRequest)
		return
	}
	content, err := kb.ReadSource(a.notesDir, source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	a.writeJSON(w, map[string]any{"source": source, "content": content})
}

// handleFileSave 保存编辑后的笔记并重新切块入库。
func (a *app) handleFileSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source  string `json:"source"`
		Content string `json:"content"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	if req.Source == "" {
		http.Error(w, "缺少 source", http.StatusBadRequest)
		return
	}
	if _, err := kb.WriteSource(a.notesDir, req.Source, req.Content); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		if _, err := kb.DeleteSource(r.Context(), a.store, req.Source); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, map[string]any{"chunks": 0})
		return
	}
	chunks, err := kb.ReindexSource(r.Context(), a.store, a.notesDir, req.Source)
	if err != nil {
		http.Error(w, "保存成功但入库失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]any{"chunks": chunks})
}

// handleFileCreate 在某个知识库下新建笔记（也支持直接写入根目录）。
func (a *app) handleFileCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KB      string `json:"kb"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	name := filepath.Base(strings.TrimSpace(req.Name))
	if !strings.HasSuffix(strings.ToLower(name), ".md") {
		name += ".md"
	}
	if !validFileName.MatchString(name) {
		http.Error(w, "文件名只能包含中文/字母/数字/下划线/连字符，且不含特殊字符", http.StatusBadRequest)
		return
	}

	rel := name
	if kbName := strings.TrimSpace(req.KB); kbName != "" && kbName != "全部" && kbName != kb.DefaultKBName {
		if !validKBName.MatchString(kbName) {
			http.Error(w, "非法知识库名", http.StatusBadRequest)
			return
		}
		rel = filepath.ToSlash(filepath.Join(kbName, name))
	}
	if _, err := kb.AbsSource(a.notesDir, rel); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := kb.WriteSource(a.notesDir, rel, req.Content); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	chunks, err := kb.ReindexSource(r.Context(), a.store, a.notesDir, rel)
	if err != nil {
		http.Error(w, "写入成功但入库失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]any{"source": rel, "chunks": chunks})
}

func (a *app) handleReindex(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	n, err := kb.ReindexSource(r.Context(), a.store, a.notesDir, req.Source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]any{"chunks": n})
}

var validFileName = regexp.MustCompile(`^[\p{Han}\w\-_. ]+\.(md|markdown)$`)

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "上传内容解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 可选的目标知识库：multipart 表单里的 kb 字段。
	// 指定后文件保存到 notes/<kb>/ 下，切块自动归属该体系。
	dstDir := a.notesDir
	if vals := r.MultipartForm.Value["kb"]; len(vals) > 0 {
		kbName := strings.TrimSpace(vals[0])
		if kbName != "" && kbName != "全部" {
			if !validKBName.MatchString(kbName) {
				http.Error(w, "非法知识库名: "+kbName, http.StatusBadRequest)
				return
			}
			dstDir = filepath.Join(a.notesDir, kbName)
			if err := os.MkdirAll(dstDir, 0o755); err != nil {
				http.Error(w, "创建知识库目录失败: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	saved := 0
	for _, fh := range r.MultipartForm.File["files"] {
		name := filepath.Base(fh.Filename)
		if !validFileName.MatchString(name) {
			http.Error(w, "仅支持 .md/.markdown 文件，文件名不含特殊字符: "+name, http.StatusBadRequest)
			return
		}
		dst := filepath.Join(dstDir, name)
		if err := saveUpload(fh, dst); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// source 是相对 notes 根目录的 / 分隔路径，决定了切块归属的知识库
		rel, err := filepath.Rel(a.notesDir, dst)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		source := filepath.ToSlash(rel)
		if _, err := kb.ReindexSource(r.Context(), a.store, a.notesDir, source); err != nil {
			http.Error(w, fmt.Sprintf("%s 入库失败: %v", source, err), http.StatusInternalServerError)
			return
		}
		saved++
	}
	a.writeJSON(w, map[string]any{"imported": saved})
}

func saveUpload(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

func (a *app) handleRebuild(w http.ResponseWriter, r *http.Request) {
	n, err := kb.ReindexAll(r.Context(), a.store, a.notesDir, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]any{"chunks": n})
}

var validKBName = regexp.MustCompile(`^[\p{Han}\w\-]{1,32}$`)

// handleDeleteKB 删除整个知识库：清掉它在向量库里的全部切块，并删除磁盘目录。
// 根目录对应的「默认笔记库」不允许删目录（它就是 notes 本身），只清切块。
func (a *app) handleDeleteKB(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	if !validKBName.MatchString(req.Name) {
		http.Error(w, "非法知识库名", http.StatusBadRequest)
		return
	}

	files, err := kb.ListFiles(r.Context(), a.store, a.notesDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deleted := 0
	for _, f := range files {
		if f.KB != req.Name {
			continue
		}
		if _, err := kb.DeleteSource(r.Context(), a.store, f.Source); err == nil {
			deleted++
		}
	}

	removedDir := false
	if req.Name != kb.DefaultKBName {
		if err := os.RemoveAll(filepath.Join(a.notesDir, req.Name)); err != nil {
			http.Error(w, "删除目录失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		removedDir = true
	}
	a.writeJSON(w, map[string]any{"deletedFiles": deleted, "removedDir": removedDir})
}

func (a *app) handleCreateKB(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !a.readBody(w, r, &req) {
		return
	}
	if !validKBName.MatchString(req.Name) {
		http.Error(w, "知识库名只能包含中文/字母/数字/下划线/连字符，长度 1-32", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(a.notesDir, req.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]any{"created": req.Name})
}
