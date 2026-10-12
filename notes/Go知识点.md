# Go 知识点

## 基础语法

### 变量与类型
- `:=` 短声明只能在函数内使用；未使用的变量和 import 会编译报错。
- 基本类型：int/int8~int64、uint、float32/64、bool、string、byte（uint8 别名）、rune（int32 别名，表示 Unicode 码点）。
- 零值机制：未初始化的变量自动为零值（0、""、false、nil），没有未定义行为。
- 字符串不可变；遍历 `for i, r := range s` 时 r 是 rune，按字节索引。

### 数组、切片与映射
- 数组是值类型，长度固定，赋值即拷贝；实际开发几乎都用切片。
- 切片底层结构：指向数组的指针 + len + cap；append 超过容量时按约 1.25~2 倍扩容并迁移数据。
- 切片共享底层数组可能导致意外的数据修改，必要时用 `copy` 或完整切片表达式 `s[low:high:max]` 限制容量。
- map 是哈希表，零值为 nil（只读）；迭代顺序随机；并发读写需要加锁或用 sync.Map。

### 控制流
- if/for 可带初始化语句；Go 只有 for 一种循环。
- switch 默认不穿透，需要 fallthrough 显式穿透；type switch 可判断接口动态类型。
- select 用于多路 channel 复用，无 default 时阻塞，多个就绪时随机选择。

## 函数与方法

### 函数特性
- 多返回值惯用 `(result, err)` 模式；可变参数 `func f(args ...int)`。
- 函数是一等公民，可作为参数、返回值、存入结构体；闭包捕获外部变量的引用。
- defer 延迟调用，遵循 LIFO 顺序，在函数 return 之后执行；常用于解锁、关闭资源；defer 的参数在 defer 语句执行时求值。
- panic/recover：panic 中止流程逐层向上，recover 只在 defer 中有效，可将 panic 转为 error；库代码应返回 error 而非 panic。

### 方法与接口
- 方法是带接收者的函数：值接收者拷贝副本，指针接收者可修改原值且避免大结构体拷贝。
- 接口是隐式实现（鸭子类型），只要实现了接口的全部方法即自动满足，无需显式声明。
- 接口底层是 (动态类型, 动态值) 二元组；接口为 nil 不代表动态值 nil，将具体类型的 nil 指针赋给接口后 `iface != nil` 为 true，是经典陷阱。
- `error` 就是一个含 `Error() string` 方法的接口；`errors.Is/As` 用于错误链判断与解包，`fmt.Errorf("%w", err)` 包装错误。
- 空接口 `any` 可承接任意类型，配合类型断言 `v, ok := x.(T)` 使用。

## 并发编程

### goroutine 与 channel
- goroutine 是轻量级用户态线程，初始栈仅 2KB，可轻松开数十万个；`go f()` 即启动。
- channel 是类型化的 FIFO 队列：无缓冲 channel 收发双方同步（会合），有缓冲 channel 满前发送不阻塞。
- 关闭规则：由发送方 close；向已关闭 channel 发送会 panic；从已关闭 channel 接收立即返回零值，`v, ok := <-ch` 的 ok 判断是否关闭。
- 单向 channel 类型 `chan<- int`（只发）`<-chan int`（只收）用于约束函数签名。
- 不要通过共享内存来通信，而要通过通信来共享内存。

### sync 包
- `sync.Mutex`/`RWMutex` 互斥锁与读写锁，保护共享数据；锁的零值可用，不可复制。
- `sync.WaitGroup` 等待一组 goroutine 完成：Add/ Done/ Wait。
- `sync.Once` 保证只执行一次（单例、懒加载）；`sync.Map` 适合读多写少或键集合稳定的并发场景。
- `sync.Pool` 临时对象池，减少 GC 压力。

### context
- `context.Context` 用于跨 goroutine 传递取消信号、超时控制和请求级元数据。
- `context.WithCancel/WithTimeout/WithDeadline` 派生可取消的子 context，父取消时子级全部取消。
- context 应作为函数第一个参数，只传递、不存储、可 nil 安全；数据库和 HTTP 请求都应绑定 context。

## 工程实践

### 错误处理与项目结构
- 显式处理每个 error，禁止用 `_` 丢弃；错误信息用小写、不加标点，方便层层包装。
- 标准布局：cmd/（入口）、internal/（私有代码）、pkg/（可导出库）、api/、configs/。
- go mod：go.mod 声明模块与依赖，go.sum 校验哈希；`go get` 拉依赖，`go mod tidy` 清理。

### 指针、内存与 GC
- Go 的 GC 是并发三色标记清除，目标亚毫秒级停顿；可用 GOGC/GOMEMLIMIT 调节。
- 逃逸分析决定变量分配在栈还是堆：`go build -gcflags='-m'` 查看；减少逃逸可降低 GC 压力。
- 值类型赋值即拷贝，大结构体建议传指针；string 与 []byte 互转会发生拷贝（unsafe 可避免但有风险）。

### 常用工具链
- `go build/test/vet/fmt`：编译、测试（表驱动测试是惯用法）、静态检查、格式化。
- `go test -race` 开启竞态检测器，并发代码必跑；`-bench` 跑基准测试，pprof 做性能剖析。
- `pprof` + `trace` 定位 CPU、内存、goroutine 泄漏问题；`runtime.NumGoroutine` 监控协程数。

### 泛型与版本特性
- Go 1.18 引入泛型：`func Map[T, U any](s []T, f func(T) U) []U`，类型约束用接口定义（`constraints` 或自定义）。
- Go 1.13 错误包装 %w；Go 1.16 embed 嵌入静态文件；Go 1.21 内置 min/max/clear、slices/maps 包；Go 1.22 range over int。
