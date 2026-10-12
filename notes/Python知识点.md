# Python 知识点

## 基础语法

### 动态类型与引用语义
- Python 变量是对象的引用，赋值不会拷贝数据，`a = b` 后两者指向同一对象。
- `is` 比较对象身份（内存地址），`==` 比较值相等；小整数池 [-5, 256] 与短字符串会驻留。
- 可变对象（list/dict/set）作为函数默认参数是经典陷阱，应使用 `def f(x=None)` 再在函数体内初始化。

### 数据结构
- list：动态数组，尾部 append/pop 均摊 O(1)，中间插入删除 O(n)。
- tuple：不可变序列，可哈希，能作为 dict 的 key。
- dict：哈希表实现，Python 3.7+ 保证插入有序；`collections.OrderedDict` 提供move_to_end 等扩展操作。
- set/frozenset：基于哈希表，成员判断 O(1)，支持交并差运算。
- `collections.deque`：双端队列，两端操作均 O(1)，适合实现队列和滑动窗口。

### 推导式与生成器表达式
- 列表推导 `[x*x for x in range(10) if x % 2 == 0]`，字典推导 `{k: v for ...}`，集合推导 `{x for ...}`。
- 生成器表达式 `(x*x for x in range(10))` 惰性求值，处理大数据时省内存。

## 函数进阶

### 闭包与装饰器
- 装饰器本质是接收函数并返回新函数的高阶函数，`@decorator` 等价于 `f = decorator(f)`。
- 带参数的装饰器需要三层嵌套；装饰类方法时注意 `functools.wraps` 保留原函数元信息。
- 常见用途：日志、重试、缓存（`functools.lru_cache`）、权限校验、计时。

### 生成器与协程
- 含 `yield` 的函数是生成器函数，调用返回生成器对象，惰性产出值。
- `yield from` 委托子生成器，自动转发 send/throw/close。
- `send(value)` 可向生成器内传值，`throw` 注入异常，这是 asyncio 协程的历史基础。
- async/await：`async def` 定义协程，`await` 挂起等待可等待对象，事件循环（asyncio.get_event_loop / asyncio.run）驱动并发。

### 函数参数
- 位置参数、关键字参数、默认参数、`*args` 收集多余位置参数、`**kwargs` 收集关键字参数。
- 仅位置参数（`/` 之前）与仅关键字参数（`*` 之后）在 Python 3.8+ 支持。

## 面向对象

### 类与魔法方法
- `__init__` 初始化、`__repr__`/`__str__` 字符串表示、`__eq__`/`__hash__` 相等性与哈希。
- `__enter__`/`__exit__` 实现上下文管理器，配合 with 语句管理资源；`contextlib.contextmanager` 可用生成器简化。
- `__getattr__` 在属性缺失时调用，`__getattribute__` 拦截所有属性访问；`__call__` 让实例可调用。

### 属性与描述符
- `@property` 把方法伪装成属性，配合 setter 实现受控读写。
- 描述符协议 `__get__`/`__set__`/`__delete__` 是 property、classmethod、staticmethod 的实现基础。
- `__slots__` 固定实例属性集合，省内存并阻止动态添加属性。

### 继承与 MRO
- Python 支持多继承，方法解析顺序由 C3 线性化算法决定，`Cls.__mro__` 可查看。
- `super()` 按 MRO 委托下一顺位的实现，协作式多继承中使用 `*args/**kwargs` 传递参数。
- ABC（`abc.ABC` + `@abstractmethod`）定义抽象基类约束子类。

## 标准库与工程实践

### 常用标准库
- `pathlib.Path`：面向对象路径操作，推荐替代 os.path。
- `dataclasses`：`@dataclass` 自动生成 `__init__`/`__repr__`/`__eq__`。
- `typing`：类型注解（List、Dict、Optional、Union、Protocol），配合 mypy 静态检查。
- `itertools`/`functools`/`collections`：函数式工具三件套（chain、group_by、partial、reduce、Counter、defaultdict）。
- `logging`：分级日志（DEBUG/INFO/WARNING/ERROR/CRITICAL），生产环境禁用 print。

### 虚拟环境与包管理
- venv：`python -m venv .venv` 创建隔离环境，pip 安装依赖，requirements.txt 固定版本。
- 现代工具：poetry、uv、pdm 提供依赖解析与锁文件；pyproject.toml 是标准项目配置。

### GIL 与并发
- GIL（全局解释器锁）保证同一时刻只有一个线程执行 Python 字节码。
- CPU 密集任务用 `multiprocessing` 多进程绕开 GIL；IO 密集任务用线程或 asyncio 即可高效并发。
- Python 3.13 起提供 free-threaded 实验构建，可在无 GIL 模式下运行。

### 异常处理
- 用 `try/except/else/finally`；捕获具体异常类型而非裸 `except:`。
- 自定义异常继承 Exception；`raise X from e` 保留异常链上下文。
- EAFP 风格（先做再捕获异常）是 Python 惯用法，与 LBYL（先判断再做）相对。
