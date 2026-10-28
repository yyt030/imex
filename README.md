# imex

名称取自 **Im**port + **Ex**port（ImEx）。基于 Go 语言开发的表粒度 MySQL 逻辑备份与恢复工具，功能设计参考 IBM DB2 的 EXPORT/IMPORT，同时支持 MySQL 和 OceanBase MySQL 模式。

## 特性

- **多种导出格式**：支持 DEL（定界 ASCII）、SQL 和 Parquet 格式
- **并行分片导出**：基于主键/唯一索引的多线程并行导出，大幅提升大表导出性能；分片结果流式写出，不产生临时文件，磁盘写入量等于输出体积
- **并行导入**：支持多线程并行导入数据
- **Pipeline 模式**：导入导出采用 goroutine → channel → goroutine 流水线架构，提升吞吐量
- **联合主键支持**：完整支持复合主键的并行分片导出
- **复杂 SQL 智能检测**：集成 Vitess SQL 解析器，自动检测 JOIN 查询并降级为单线程导出，单表查询自动分片并行
- **灵活的导入模式**：INSERT / INSERT_UPDATE / REPLACE / CREATE
- **列映射导入**：支持文件列到表列的自定义映射（DB2 风格），可按位置或按名称映射
- **自定义分隔符**：支持单字符、多字节、十六进制格式
- **无包裹符模式**：支持导入导出不使用字符串定界符的纯分隔符文件
- **OceanBase 兼容**：自动检测并适配 OceanBase MySQL 模式
- **云环境连接加固**：驱动层 socket 超时 + 连接池 TTL + 事务空闲提交 + 瞬断自动重试，规避 LB/NAT 静默回收
- **日志文件**：`--messages` / `-m` 把本次运行的命令行日志整体交给该文件（终端不再打印）
- **可调进度节奏**：`--progress-interval` 控制定时进度日志的间隔（默认 5 秒），`--quiet` 则完全不打印进度记录
- **流式处理**：支持 GB 级大文件，不在内存中缓存全量数据
- **批量优化**：导入使用批量 INSERT（1000 行/批次），提升写入性能
- **单行 SQL 布局**：`--format sql` 默认每行一条 INSERT，文件逐行可切分；攒批统一由导入侧按 `--batch-size` 完成，导出侧的 `--extended-insert` 只用于换取更小的文件体积
- **低开销 DEL 解析**：定界符预计算、读取路径不做无效字节↔字符串转换，单线程解析开销降低后并行导入才有效扩展
- **密码加密**：PBKDF2-HMAC-SHA256 派生密钥 + AES-256-GCM 加密密码，避免在 ps 命令中泄露明文
- **单一二进制**：纯 Go 实现，无 CGO 依赖

## 安装

### 从源码编译

```bash
git clone <repository-url>
cd imex
go build -o imex
```

### 使用 Makefile

```bash
make build          # 编译当前平台二进制 → bin/imex
make linux          # 交叉编译 Linux amd64
make build-all      # 交叉编译发布平台（Linux amd64 / Windows amd64）
make build COMPRESS=1  # 编译后自动 upx 快速压缩（-1，需安装 upx；未装则跳过）
make compress       # 单独对 bin/ 现有产物运行 upx -1
```

### 测试

```bash
make test           # 离线单元/往返测试（不需要数据库）
```

真实数据库端到端测试默认跳过，需要提供已存在库的 DSN（表会按测试名唯一创建并在结束时删除）：

```bash
docker run -d --name imex-mysql -e MYSQL_ROOT_PASSWORD=secret \
  -e MYSQL_DATABASE=imex_it -p 3306:3306 mysql:8.0
IMEX_TEST_DSN='root:secret@tcp(127.0.0.1:3306)/imex_it' make test-live
```

覆盖 DEL/SQL/Parquet 往返、insert/insert_update/replace/create 模式、列映射、并行导出与并行导入、日志文件。

### 交叉编译

工具为纯 Go 实现、无 CGO 依赖，构建时强制 `CGO_ENABLED=0`，产物为静态链接二进制，可在任意 Linux 发行版直接运行，无需目标平台 C 工具链：

```bash
# 仅 Linux
make linux
# 产物：bin/imex-linux-amd64

# 发布矩阵（Linux amd64 + Windows amd64）
make build-all
# 产物：bin/imex-linux-amd64 + bin/imex-windows-amd64.exe

# 指定单平台（发布矩阵之外的架构按需自行产出）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o imex .
```

`make build-all` 只产出 Linux amd64 与 Windows amd64，不再构建 darwin 目标，也不产出 Linux arm64。macOS、ARM 服务器等环境同样是纯 Go 可直接编译：macOS 在本机执行 `make build` 即得 `bin/imex`，ARM64 Linux 用上面的单平台命令，只是发布包不再附带这些预编译产物。

版本信息通过 ldflags 注入 `cmd` 包，`imex version` 可打印版本、Git commit、构建时间与 Go 版本；该子命令只写标准输出、不产生任何日志记录，可直接接管道与 `grep`。

### Docker

```bash
docker build -t imex:latest .
```

## 快速开始

### 导出数据

```bash
# 导出整张表到 DEL 格式
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 导出为 SQL 格式
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.sql -f sql \
  -q "SELECT * FROM users"

# 导出为 Parquet 格式
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.parquet -f parquet \
  -q "SELECT * FROM users"

# 自定义分隔符（制表符分隔）
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.tsv \
  --col-del "\t" --no-char-del \
  -q "SELECT * FROM users"
```

### 导入数据

```bash
# 从 DEL 文件追加数据
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  -t users

# 使用 insert_update 模式（插入或更新）
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  --mode insert_update \
  -t users

# 从 SQL 文件创建表并导入（create 模式也需 -t 指定新表名）
# 前置导出必须带 --create，因为 SQL 导出默认只导数据、不含 CREATE TABLE
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.sql -f sql --create -q "SELECT * FROM users"
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.sql -f sql \
  --mode create -t users
```

## 命令说明

### export - 数据导出

```bash
imex export [连接选项] [导出选项] [修饰选项] --query "<SELECT SQL>"
```

**连接选项：**

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--host` | `-h` | 数据库主机地址 | `127.0.0.1` |
| `--port` | `-P` | TCP/IP 端口 | `3306` |
| `--user` | `-u` | 数据库用户名（支持 OceanBase 格式，见下方说明） | `root` |
| `--password` | `-p` | 数据库密码 | 空 |
| `--socket` | `-S` | UNIX 域套接字路径 | 空 |
| `--database` | `-D` | 数据库名 | 空 |
| `--session-var` | | 导出前设置的会话变量（`name=value`，可重复，见下方说明） | 空 |
| `--connect-timeout` | | 建连（TCP dial）超时，对应 JDBC `connectTimeout` | `5s` |
| `--socket-read-timeout` | | 单次 socket 读超时，对应 JDBC `socketTimeout` | `300s` |
| `--socket-write-timeout` | | 单次 socket 写超时（Go-only，无 JDBC 对应） | `300s` |

`--host`（或 `--socket`）、`--user`、`--password`、`--database` 为必填项，缺失时命令直接报错退出。

由于 `-h` 被 `--host` 占用，帮助短选项为 `-?`。

**OceanBase 用户名格式支持：**

用户名支持包含特殊字符的 OceanBase 格式，包括：
- 二段式：`root@sys`
- 三段式：`root@sys#obcluster`
- 带集群 ID：`root@sys#obcluster:id`

示例：
```bash
# OceanBase 二段式用户名
imex export -h ob-host -u "root@sys" -p password -D mydb -o /backup/users.del -q "SELECT * FROM users"

# OceanBase 三段式用户名
imex export -h ob-host -u "root@sys#obcluster" -p password -D mydb -o /backup/users.del -q "SELECT * FROM users"

# OceanBase 带集群 ID 的用户名
imex export -h ob-host -u "root@sys#obcluster:id" -p password -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

**会话变量（`--session-var`）：**

在导出/导入开始前为数据库连接设置会话变量（如 `sql_mode`、`foreign_key_checks`、OceanBase 的 `query_timeout` 等），export 与 import 子命令均支持，可重复传入：

```bash
imex export ... --threads 4 \
  --session-var "sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE" \
  --session-var foreign_key_checks=0 \
  -q "SELECT * FROM users"
```

- 变量以 DSN 附加参数方式注入，驱动会在**每一条新建物理连接**上自动执行对应 `SET`，因此主连接、并行导出的每个分片连接、并行导入的每个工作线程连接都一致生效，不受连接池复用影响。
- 值按 SQL 字面量自动处理：纯数字与 `ON/OFF/TRUE/FALSE/DEFAULT/NULL` 原样传入，其余加单引号（内部单引号双写）。值中禁止反引号、注释序列（`/*`、`--`）与反斜杠，防止破坏或注入 SET 语句。
- 变量名须匹配 `[A-Za-z][A-Za-z0-9_]*`；与工具/驱动内部 DSN 参数同名（`charset`、`parseTime`、`tls`、`timeout` 等）会被拒绝。连接/文件字符集由 `--default-charset` 统一掌管，会话级字符集变量 `character_set_client` / `character_set_connection` / `character_set_results` 同样被拒绝——否则会以会话变量绕过该不变量（如 `character_set_results=gbk` 会转码二进制列造成损坏）。未知变量名会在连接阶段由服务端报错，便于尽早发现拼写错误。
- 运行日志只打印变量**名**（`names=[sql_mode foreign_key_checks]`），**不打印值**——值可能包含密码/密钥等敏感信息，与失败 SQL 默认脱敏的原则一致。

**字符集与二进制编码（`--default-charset` / `--hex-blob`）：**

工具遵循 mysqldump 的字符集惯用法，存在两条**相互独立**的编码边界：

- **边界 A（文件 ↔ 工具）**：由 `--default-charset` 决定。它设定连接字符集（`SET NAMES`），连接字符集即等于文本列在文件中的编码——服务端把结果按此字符集送上线，导出器原样落盘，因此**文件编码 == `--default-charset`**。默认 `utf8mb4`。
- **边界 B（工具 ↔ 表列自身码页）**：由服务端在读取时自动转码，工具从不干预。文本列无论表中为何码页，导出时都会转换到 `--default-charset`。

由此，用户常见的「表是 GBK、想要 UTF-8 文件」场景**无需任何额外参数**：默认连接即 `utf8mb4`，GBK 列经服务端转码后文件就是 UTF-8；反向导入用同样的默认连接即对称还原。

`--default-charset` 取值（大小写与首尾空格容错，含 DB2 风格数字码页别名）：

| 值 | 说明 |
|----|------|
| `utf8mb4`（默认） | 等同于省略该参数 |
| `utf8` / `utf-8` / `utf8mb3` | UTF-8 |
| `ascii` / `us-ascii` / `384` | ASCII |
| `latin1` / `iso-8859-1` / `819` | Latin-1（单字节） |
| `gbk` / `936` / `1386` | GBK |
| `gb18030` / `5488` / `1392` | GB18030 |

一旦 `--default-charset` 不是 `utf8mb4`，**二进制列（BLOB/BINARY/VARBINARY/BIT 等）自动以 `0x` 十六进制字面量写出**（等价 mysqldump `--hex-blob`）：十六进制为纯 ASCII，既**字符集免疫**（不被连接转码破坏），又**自描述**（读取端凭 `0x` 前缀识别）。DEL 与 SQL 两种文本格式都采用同一 `0x` 表示。可用 `--hex-blob` 强制开启、`--no-hex-blob` 强制关闭（非 utf8mb4 关闭时会打印告警，提示原始字节可能被转码损坏）。默认（utf8mb4、hex 关）下输出与旧行为逐字节一致。

导入侧会把 `0x` 还原成字节，但判定条件不同：**SQL** 格式中字符串一律带引号，因此只有**不带引号**的 `0x…` 令牌才被解码（引号内是普通文本）；**DEL** 格式多一层歧义（`--no-char-del` 时全是裸字段），因此要求**同时满足**「该字段无引号 + 目标列是二进制类型（导入前用 `information_schema` 查得）+ 令牌是合法偶数长度十六进制」。任一条件不满足就按原文处理——例如写入 varchar 列的 `"0xdead"` 仍是这 6 个字符。孤立 `0x` 解码为**空字节串**（与 NULL 区分）。

按格式的字符集限制（越界一律退出码 2）：

- `del`：允许全部字符集。DEL 的行/列/引号分隔符都是 ASCII 且位于多字节尾字节范围之外，GBK 等 MBCS 亦可安全承载。
- `sql`：仅允许**逐字节转义安全**的字符集（`utf8mb4`/`utf8`/`ascii`/`latin1`）。GBK/Gb18030 的尾字节可等于 `0x5C`(`\`)/`0x27`(`'`)，会破坏按字节的 SQL 转义，故被拒绝，请改用 `--format del`。
- `parquet`：列式二进制容器，没有"文件字符集"概念，指定非默认字符集会报错。

SQL 格式还会在文件头写出**自描述字符集**语句 `/*!40101 SET NAMES <charset> */;`（mysqldump `--set-charset` 风格）：标准 `mysql` 客户端在恢复时据此设定连接字符集；imex 自身的导入会跳过该行，改由 `--default-charset` 对齐字符集。导入端按目标列类型对 `0x` 值做十六进制还原后写入。

**导出选项：**

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--output` | `-o` | 输出文件路径 | 无（必填） |
| `--format` | `-f` | 输出格式（del/sql/parquet） | `del` |
| `--query` | `-q` | SQL SELECT 语句（支持 OceanBase / MySQL 优化器 hint），与 `--query-file` 互斥 | 无 |
| `--query-file` | | 从该文件读取 SQL SELECT 语句（自动去掉首尾空白与结尾分号），与 `-q` 互斥；两者必须恰好给一个 | 空 |
| `--messages` | `-m` | 把全部日志记录写入该文件，终端不再打印（文件即命令行日志本身） | 空 |
| `--progress-interval` | | 定时进度日志的间隔（必须 > 0 且 ≥ 10ms，否则用法错误退出码 2） | `5s` |
| `--no-overwrite` | | 拒绝覆盖已存在的输出文件（默认允许覆盖；原名 `--no-replace`，已改名且不留别名） | `false` |
| `--threads` | | 并行导出任务数（1=不并行；上限 256）。每个 worker 全程一条连接，worker 数 + 1 超过服务端 `@@max_connections` 时在数据移动前记一条 `warn` | `1` |
| `--shard-key` | | 分片键列名（空=自动检测主键/唯一键）；**只有并行导出读取并校验它**——显式给出的列在表里不存在时为用法错误、退出码 2，自动检测失败仍回落单线程；`--threads 1` 或并行请求回落到单线程时该值不起作用，运行记一条 `warn` 并在配置记录标 `shard_key_ignored=true`（不改变退出码与产物） | 空 |
| `--batch-size` | | 读→写交接每批的行数（仅单线程导出流水线；与 `--threads>1` 同时显式给出即用法错误，退出码 2） | `1000` |
| `--pipeline-buffer` | | 读→写交接队列深度（批数；0=同步交接；仅单线程导出，与 `--threads>1` 同时显式给出即用法错误，退出码 2） | `10` |
| `--default-charset` | | 连接与输出文件字符集（见上方说明；utf8mb4/utf8/latin1/ascii/gbk/gb18030） | `utf8mb4` |
| `--hex-blob` | | 二进制列强制写为 `0x` 十六进制字面量（非 utf8mb4 时自动开启）；仅 DEL/SQL 文本格式，`--format parquet` 下传入报错 | `false` |
| `--no-hex-blob` | | 强制二进制列原样透传（非 utf8mb4 下可能损坏，会打印告警）；与 `--hex-blob` 互斥；`--format parquet` 下传入报错 | `false` |

> 提示：DEL/Parquet 文件的列头取自查询结果集的列名。若 `--query` 含未起别名的表达式列（如 `SELECT 1, COUNT(*)`），导入时该列头（`1`、`COUNT(*)`）在目标表中不存在会报错，请为表达式列起别名（`SELECT 1 AS flag`）或导入时用 `--columns` 按位置指定目标列。

**DEL 格式修饰选项（仅 `--format del` 生效，与 `--format sql`/`parquet` 组合时显式报错，退出码 2）：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--char-del` | 字符串定界符 | `"` |
| `--col-del` | 列分隔符 | `,` |
| `--row-del` | 行分隔符 | `\n` |
| `--escape` | 转义符 | `"` |
| `--no-escape` | 不使用转义（写出前校验，数据含冲突定界符时报错） | `false` |
| `--timestamp-fmt` | 时间戳格式（作用于 DATETIME/TIMESTAMP 列） | `yyyy-MM-dd HH:mm:ss` |
| `--date-fmt` | 日期格式（作用于 DATE 列） | `yyyy-MM-dd` |
| `--no-char-del` | 不使用字符串定界符（裸字段不转义：数据含列/行分隔符时导出报错） | `false` |
| `--no-col-names` | 不输出列名 | `false` |

**全局选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--log-level` | 日志级别（debug/info/warn/error） | `info` |
| `--error-stack` | 失败时打印完整调用堆栈（可用 `IMEX_ERROR_STACK=1`） | `false` |
| `--echo-sql-values` | 失败 SQL 回显内联真实数据值（默认脱敏，可用 `IMEX_ECHO_SQL_VALUES=1`） | `false` |
| `--quiet` | 禁止定时进度日志（默认每 5s 一条，间隔见 `--progress-interval`），仅保留配置、结束统计与 ERROR 记录（可用 `IMEX_QUIET=1`） | `false` |
| `--no-sql-tag` | 不在工具主动执行的语句前加 `/* imex:<commit> */` 归因注释（默认加，可用 `IMEX_NO_SQL_TAG=1`） | `false` |

**SQL 格式修饰选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--statement-size` | 单条 INSERT 语句最大字节数（只有 `--extended-insert` 时是攒批上限；默认单行布局下一行无法再拆，超过它只记一次告警） | `8388608`（8MiB） |
| `--extended-insert` | 把多行合并进一条 INSERT（按 `--statement-size` 分组，等价 mysqldump 默认行为）；若 `--statement-size` 低于单行语句宽度则一条也合不成，产物退回每行一条 INSERT 并记一次告警 | `false` |
| `--create` | 输出 CREATE TABLE 语句（SQL 导出默认只导数据，需要自带建表语句时才加；与 `--format sql` 绑定，其他格式传入报用法错误） | `false` |
| `--no-data` | 不输出 INSERT 语句 | `false` |

> SQL 导出默认只写 `INSERT` 数据语句；加 `--create` 才附带 `CREATE TABLE`。无论哪种，文件描述的始终是**一张真实表**，因此 `--format sql` 只接受单表查询：JOIN、`FROM a, b`、派生表 `FROM (SELECT …) t`、`WITH` CTE 都会在解析阶段报错并提示改用 `--format del`/`--format parquet`，报错发生在输出文件创建之前。`--query`/`--query-file` 里若表名带了 schema（`db.tbl`），导出会剥离 schema、只按裸表名写 `CREATE`/`INSERT`/`LOCK TABLES`，而 `SHOW CREATE TABLE` 仍按查询点名的 schema 查找，因此不会再出现把连接的 `--database` 二次拼接导致的 `Error 1054/1146`；`--mode create` 依赖文件里的 `CREATE TABLE`，故这类导出必须带 `--create`。`--no-create` 已随默认值反转一并移除（结构本就是 opt-in，留一个反向开关只是给 `--create` 一个否决口），传入它会得到 `unknown flag: --no-create` 并退出 2；`--single-row-insert` 同样已删除——自默认布局改成每行一条 INSERT 起它就不再改变任何行为，只剩与 `--extended-insert` 互相否决这一处用途，传入它会得到 `unknown flag: --single-row-insert` 并退出 2。

### import - 数据导入

```bash
imex import [连接选项] [导入选项] [修饰选项]
```

连接选项与 export 子命令相同（`--host`/`--port`/`--user`/`--password`/`--socket`/`--database`/`--session-var`/`--default-charset` 以及 `--connect-timeout`/`--socket-read-timeout`/`--socket-write-timeout`），`--host`（或 `--socket`）、`--user`、`--password`、`--database` 为必填。导入时 `--default-charset` 须与当初导出所用的字符集一致（文件编码 == 连接字符集），否则文本列会被误解码；SQL 文件首行的 `SET NAMES` 头仅供标准 `mysql` 客户端使用，imex 导入以本参数为准。

**导入选项：**

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--input` | `-i` | 输入文件路径 | 无（必填） |
| `--format` | `-f` | 输入格式（del/sql/parquet） | `del` |
| `--mode` | `-M` | 导入模式 | `insert` |
| `--table` | `-t` | 目标表名（insert/replace/insert_update 必填；create 模式可省略，自动取 SQL 文件中 CREATE 语句的表名，指定则作为新表名重写） | 无 |
| `--columns` | `-c` | 列映射（见下方说明） | 空 |
| `--messages` | `-m` | 把全部日志记录写入该文件，终端不再打印（文件即命令行日志本身） | 空 |
| `--progress-interval` | | 定时进度日志的间隔（必须 > 0 且 ≥ 10ms，否则用法错误退出码 2） | `5s` |
| `--threads` | | 并行导入任务数（1=不并行；上限 256）。worker 数 + 1 超过服务端 `@@max_connections`、或 `--threads` 让重放缓冲总量（每 worker 一份，下限 8 MiB）超过进程预算时，在任何数据移动之前各记一条 `warn` | `1` |
| `--batch-size` | | 读→写交接每批的行数（单线程与并行导入均生效；实际还受字节上限与占位符上限约束，见下） | `1000` |
| `--pipeline-buffer` | | 读→写交接队列深度（批数；0=同步交接），**按给定值执行**。该队列由全部 worker 共享，深度与 `--threads` 是两件正交的事（深度是驻留内存，线程是并发）。深度 × 单批字节预算可能超过 1 GiB 时不改写深度，只在数据移动前记一条 `warn` 报出这个价（`requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes`/`hint`） | `10` |
| `--statement-size` | | 单条 INSERT 语句的字节上限，只能**压低**下面「导入攒批的硬上限与可选压低」里由 `max_allowed_packet` 导出的预算，不能抬高；`0`=不额外压低。默认预算还受驱动侧 64 MiB 客户端插值上限钳制——只有服务端报出**大于** 67,108,864 B（64 MiB）的包时才轮到它——实测 67,108,864 时 `batch_bytes=60397977 limiting_factor=max_allowed_packet`，67,109,888 时同一数字改记 `limiting_factor=driver_packet_cap`，测试床按 OceanBase V4.x 默认的 `max_allowed_packet=16M` 时决定者是 `max_allowed_packet`（`batch_bytes=15099494`）。单行大于该预算时仍独立成批（一行无法再拆分） | `0` |
| `--default-charset` | | 连接与输入文件字符集，须与导出时一致（见导出侧说明） | `utf8mb4` |

**`--columns` 列映射说明：**

支持两种格式：
- **位置映射**：`--columns "name,age,email"` - 文件列按位置映射到表列
- **名称映射**：`--columns "user_name=name,user_age=age"` - 显式指定文件列到表列的映射

通过检测 `=` 符号自动识别格式。

**导入模式：**

| 模式 | 说明 |
|------|------|
| `insert` | 追加数据到表 |
| `insert_update` | 插入新行或更新主键匹配的行 |
| `replace` | 删除表所有数据后**普通 INSERT**（不是 MySQL 的 `REPLACE INTO`） |
| `create` | 创建表并导入数据（仅 SQL 格式） |

> `replace` 是**全量替换**，不是按唯一键逐行覆盖：先 `SET FOREIGN_KEY_CHECKS=0`，再用一条 `DELETE FROM <表>` 清空目标表，随后数据行仍按普通的 `INSERT INTO … VALUES (…),(…)` 写入。与 MySQL 的 `REPLACE INTO` 有三处实质差别：① 文件里没有的行会被删掉（`REPLACE INTO` 只替换键冲突的行、其余保留）；② 文件内主键/唯一键重复时第二条仍然报 `1062`（`REPLACE INTO` 会静默删旧行再插入、最后一条生效）；③ 那条 `DELETE` 是自动提交的单条语句、不在导入事务里，因此**它之后的任何失败（列超长、断连等）都会留下一个空表**，需要重跑。需要"按键覆盖、不动其它行"时用 `--mode insert_update`。清空用 `DELETE` 而不是 `TRUNCATE`，是因为后者隐式提交、不能回滚，且表被外键引用时可能直接失败。

**DEL 格式修饰选项（仅 `--format del` 生效，与 `--format sql`/`parquet` 组合时显式报错，退出码 2）：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--char-del` | 字符串定界符 | `"` |
| `--col-del` | 列分隔符 | `,` |
| `--row-del` | 行分隔符 | `\n` |
| `--escape` | 转义符 | `"` |
| `--no-escape` | 不使用转义（读取时不解析转义序列） | `false` |
| `--timestamp-fmt` | 时间戳格式（导入时按目标列类型回转换） | `yyyy-MM-dd HH:mm:ss` |
| `--date-fmt` | 日期格式（导入时按目标列类型回转换） | `yyyy-MM-dd` |
| `--skip-count` | 跳过前 n 行数据（列名行单独读取，不计入） | `0` |
| `--row-count` | 最多导入 n 行 | `0`（无限制） |
| `--commit-count` | 每 n 行提交一次事务 | `1000` |
| `--no-char-del` | 不使用字符串定界符（裸字段不转义：数据含列/行分隔符时导出报错） | `false` |
| `--no-col-names` | 输入文件第一行不是列名。无表头时必须同时用 `-c/--columns` 指定目标列，否则报清晰错误；反过来，只给 `-c` 而不给 `--no-col-names` 时第一行仍按列名行处理并被丢弃（文件本身无法区分两种情况），此时若这些字段与目标表列名全不匹配，工具会发出 `the first record of the input file was treated as the column header and not imported` 的 WARN 日志（`--messages` 打开时同样进该文件），提示补上 `--no-col-names` | `false` |

**全局选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--log-level` | 日志级别（debug/info/warn/error） | `info` |
| `--error-stack` | 失败时打印完整调用堆栈（可用 `IMEX_ERROR_STACK=1`） | `false` |
| `--echo-sql-values` | 失败 SQL 回显内联真实数据值（默认脱敏，可用 `IMEX_ECHO_SQL_VALUES=1`） | `false` |
| `--quiet` | 禁止定时进度日志（默认每 5s 一条，间隔见 `--progress-interval`），仅保留配置、结束统计与 ERROR 记录（可用 `IMEX_QUIET=1`） | `false` |
| `--no-sql-tag` | 不在工具主动执行的语句前加 `/* imex:<commit> */` 归因注释（默认加，可用 `IMEX_NO_SQL_TAG=1`） | `false` |

### version - 版本信息

```bash
imex version
```

### encrypt - 密码加密

```bash
imex encrypt <password>
```

加密数据库密码，生成 `ENC:` 前缀的密文，可在 export/import 命令中安全使用。

**参数：**
- `password`：要加密的明文密码（必填）

**环境变量：**
- `IMEX_KEY`：加密密钥（必填，与每条密文的随机盐一起经 PBKDF2-HMAC-SHA256（600000 次迭代）派生为 32 字节 AES-256-GCM 密钥；应为高熵随机串，例如 `openssl rand -base64 32`）

**示例：**

```bash
export IMEX_KEY="my-secret-key"
imex encrypt "my-password"
# 输出: ENC:SOQZx8oP6Am7l4lmk8EShfO8pjKEW02FPd4U928cLPLFgcebkdXuaus8dgCIcRUSyQHE1okGmQ==
```

**注意：**
- 解密在 export/import 起始处自动完成（`common.ResolvePassword`），不提供公开的 decrypt 命令，以降低安全风险；`-p` 的值不是 `ENC:` 开头时按明文处理。
- 密文带随机盐与随机 nonce，同一明文 + 同一密钥每次结果都不同，不要拿密文做等值比较；`imex export`/`imex import` 每次进程只在起始处解密一次（实测带 `ENC:` 口令连本地真实库导出 2 行为 0.35–0.42 s，同命令写明文口令为 0.00 s）。
- `IMEX_KEY` 未设置或与实际密钥不符时，命令失败并退出 1，错误形如 `failed to resolve password: password is encrypted but IMEX_KEY environment variable is not set` / `failed to decrypt: wrong key or corrupted data`。
- `IMEX_KEY` 本身也是凭据，此机制只解决"密码在命令行可见"，不解决密钥托管。

## 高级用法

### 并行导出

```bash
# 自动检测主键并并行导出（4线程）
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "SELECT * FROM users"

# 指定分片键进行并行导出
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/orders.del \
  --threads 8 \
  --shard-key order_id \
  -q "SELECT * FROM orders"

# JOIN 查询自动降级为单线程（智能检测）
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/user_orders.del \
  --threads 4 \
  -q "SELECT u.name, o.order_id FROM users u JOIN orders o ON u.id = o.user_id"
# 注意：检测到 JOIN，自动使用单线程导出
```

> Parquet 格式不支持并行导出（分片文件无法拼接），`--threads>1` 时会自动降级为单线程。
>
> 降级的全部触发点（命中即记一条 warn，`--quiet` 不抑制）：查询含 JOIN/派生表等复杂特性、无法从语句提取表名、检测不到可用分片键、分片只能切出一段。最后一种现在只剩 `MIN()==MAX()`（以及采样失败后回落成一段）——**字符串/UUID 主键已不再降级**，见下条。复合主键/唯一键一直是支持的，但只对首列切区间，其余列保持全范围，因此每个元组仍恰好覆盖一次；键选取过程可用 `--log-level debug` 看到（`using primary key for sharding columns=…`）。
>
> 分片边界是**采样**出来的，不再只是按 `MIN`/`MAX` 等宽切：先从 `INFORMATION_SCHEMA.TABLES` 读整表的 `TABLE_ROWS` 与 `AVG_ROW_LENGTH`，再用 `WHERE 键 > ? ORDER BY 键 LIMIT 1 OFFSET 每段行数-1` 逐段定位边界（边界走绑定参数，段行数直接内联为整数），因此稀疏键、倾斜键、CHAR/VARCHAR/UUID 这类非数值键都能切出等量的段。实测一张 100 万行、`id` 跨度 1…2 亿的稀疏表：等宽切出的 4 段是 200,000 / 0 / 0 / 800,000 行（两个 worker 全程空转，最慢段占全表 80%），采样切出的 8 段是 123,749×7 + 133,757 行。等宽仍然保留，且在两种情况下直接采用：`TABLE_ROWS × AVG_ROW_LENGTH < 256 MiB` 的稠密整数键（小表不值得为采样多跑查询，实测 `bench_lay` 20 万行的边界与等宽结果**完全一致**，代价只是多一条元数据查询），以及键值域密度可证地接近均匀时（值域跨度 ≤ 行数 × 3/2 时等宽与理想切分的偏差不超过 1.5 倍，判据用 `big.Rat` 精确算，`BIGINT` 跨度 2^63 也不会误判）。采样的全部成本可测：8 段需要 7 次探针查询，同一张稀疏表上端到端导出由 0.84 s 变为 0.90 s（3 次中位数，`--threads 4`），增量即探针开销，与一次全表扫描（0.128 s）同量级，且只在真正并行时发生。任何一步失败（统计缺失、权限不足、探针报错、取到 NULL）都记一条 WARN 并**原样回落**到等宽/单段行为，绝不因为采样而让导出失败或改变产物内容。
>
> 并行导出按分片序号顺序拼接结果，**不保证**与 `--query` 的全局行序一致；需要稳定全局顺序时使用单线程导出。分片边界使用精确整数/十进制运算，`BIGINT`/`DECIMAL` 超过 2^53 也不会丢行；每个分片有各自的一致性快照，分片之间不是同一时间点。带 `--query` 的 `WHERE` 过滤时边界仍按**整表**统计来采样，可能被过滤后的实际分布误导（只会不均，不会丢行或重复），需要精确均衡时用单线程导出或把键条件写成分段查询。
>
> 分片数与线程数解耦：默认切 `min(2×threads, 64)` 段，由恰好 `threads` 个 worker 从共享游标**按序号领取**（段数多于线程数才有人一直有活干，收尾段不再拖尾；按序领取是硬要求——写出侧按段序消费，乱序领取会死锁）。因此 `--threads 4` 通常是 8 段 4 个 worker。并行导出**不产生临时文件**：分片把序列化结果写进自己的字节队列，另一个 goroutine 按分片序号依次消费并直接写最终文件，因此磁盘写入量等于输出体积（旧实现先写 `<output>.shard_N.tmp` 再拼接，每个字节落盘两次：实测 125 MB 的 DEL 导出写入 238 MB）。队列是固定 **64 MiB 进程预算按段数均分**，`--threads 4`（8 段）时每段 8 MiB 在途，`--threads 8`（16 段）时每段 4 MiB，段数上限 64 时每段 1 MiB；实测把每段前瞻从 16 MiB 压到 4 MiB 在这台 4 核沙箱上没有可测损失（141 MB 的 DEL 导出 `--threads 8` 为 0.86 s 对 0.91 s，差异在探针成本之内），但深度调到 2.5 MiB 以下会明显拖慢 SQL 格式（慢约 20%），所以这个预算是量出来的，不是猜的。段边界对产物内容的影响也有边界：DEL 与默认单行 SQL 布局下**任意边界组合都产出逐字节相同的文件**（实测 3 张表 × DEL/单行 SQL × `--threads 1/4/8` 共 18 组、外加 `--extended-insert` 的 `--threads 1` 3 组，与改动前的二进制 md5 全同），只有 `--extended-insert` 每个边界多一条 INSERT 头（实测 `bench_lay` `--threads 4` 多 192 B、`--threads 8` 多 384 B，即每边界一条 48 B 的头）。每条 `shard export completed` 记录都带 `worker`（`0` 到 `threads-1`）与 `shard`（段序号），分片查询失败的错误信息以 `(worker N, SQL: ...)` 开头，汇总行带 `workers`——并行时出了问题可以直接定位到具体 worker；并行导入同样在错误记录上带 `thread` 编号。
>
> 失败时（分片报错、磁盘写满等）不保留半截产物：已写出的输出文件会被删除并记一条 `incomplete output file removed` 警告，其余分片立即解除阻塞。单线程导出同样如此——此前它只在报错信息里承认失败，磁盘上仍留着已经写出的部分（实测 20 万行表在第 20 万零一行被拒时留下 21 MB），现在两条路径统一为**要么完整、要么没有**。未安装信号处理器，`Ctrl-C` 仍会直接终止进程并留下一个不完整的输出文件，需要自行删除。

### 流水线与内存控制

导出/导入内部是「读 goroutine → 缓冲 channel → 写 goroutine」的两段流水线（并行导入再加 N 个写 worker）。`--batch-size` 与 `--pipeline-buffer` 控制这条交接管道的**粒度**，从而**可预测地**影响峰值内存：

- 在途行数 ≈ `batch-size × (pipeline-buffer + 线程数)`，峰值内存 ≈ 在途行数 × 平均行字节。宽表/BLOB 大字段时，调小两者即可压低峰值。
- 这是**比例控制，不是字节硬上限**：Go 堆、数据库驱动的读缓冲（≈ `max_allowed_packet`）以及每条巨型 INSERT 的解析，都在本参数之外。因此**无法**用它对任意输入保证「进程 RSS ≤ 某值」。
- 需要进程级内存天花板时，用 Go 运行时软上限：`GOMEMLIMIT=6GiB imex import ...`（配合 `GOGC` 调整回收力度）。这才是真正约束 Go 进程占用的杠杆。
- 1 GiB 这个数字只是**报价线**，不是夹紧值：导入侧算出 `深度 × 单批字节预算` 若可能超过它，就记一条 `warn` 把 `requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes` 和出路一起写进日志，然后照给定深度跑。测试床按 OceanBase V4.x 默认的 `max_allowed_packet=16M`（预算 `batch_bytes=15099494`）时，实测深度 **71 静默、72 起出声**（`queued_bytes=1087163568`），深度 1024 记 `requested=1024 batch_bytes=15099494 queued_bytes=15461881856 envelope_bytes=1073741824`。200,688,900 B / 100,000 行的 DEL 文件在 `--threads 32` 下重复多轮：深度 1024 峰值 RSS **76,616–152,040 KB**、3.78–5.07 s，深度 10 为 **122,740–149,412 KB**、4.45–4.83 s——两组重叠，说明预算收窄到 15 MB 之后峰值内存由数据本身与 worker 数量决定，深度不再是主要变量；128 MB 床上的旧对照（1024 → 最高 292,356 KB、上一轮的 280,288 KB / 下调后 112,584 KB）随下调逻辑一并失效，不再引用。导出侧的交接队列（`internal/export/exporter.go:319`）历来只按批数定深、没有字节包络，按同一原则保持"给定多少就多用多少"，本轮只补文档不改代码。
- 生效范围：`--batch-size/--pipeline-buffer` 作用于**单线程导出**、**单线程与并行导入**；**并行导出**是分片内同步流式 `scan→write`，内存几乎不随数据量增长，不受这两个参数影响，因此 `export --threads>1` 时**显式**给出这两个选项中的任一个就是用法错误（退出码 2，报错文案直接说明该选项在并行路径上不改变任何东西、并给出两条出路：去掉它或改 `--threads 1`）——只判断"有没有显式传"，不判断值是否等于默认值，所以 `--threads 8 --batch-size 50000` 这种看起来被采纳、实际无效的组合再也不会静默通过；`--threads 1` 或干脆不给 `--threads` 时两者照旧生效，`import` 侧不受此限制（并行导入真的读这两个值）。直接驱动库（手工构造 `ExportConfig`）的调用方不走 CLI 校验，仍会在确实按分片并行、且改动过任一项时收到一条 `parallel export ignores pipeline options` 的 info。SQL 导入会把单条语句一次性解析，默认布局下一条语句就是一行，规模由行本身决定；只有 `--extended-insert` 导出的文件才由 `--statement-size` 约束单条语句的大小。

### 导入攒批的硬上限与可选压低

导出侧默认**每行一条 INSERT**（只有 `--extended-insert` 才按 `--statement-size` 的**字节**预算合并），而导入侧把语句拆成行后按**行数**（`--batch-size`）重新攒批——攒批因此只发生在导入侧，实测两种布局打给服务端的语句完全相同（20,000 行、`--batch-size 1000`：单行文件 20,000 条语句与合并文件 1 条语句都只产生 20 条服务端 INSERT）。按字节和按行数并不对称：宽表或含大字段的表，几百行就可能在导入侧拼出一个超过服务端 `max_allowed_packet` 的包，在事务中途报 `error 1153 (Packet for query is too large)`。因此导入攒批额外受两道协议层上限约束，与 `--batch-size` 取最小值生效：

- **字节上限**：首次攒批前读取目标库 `SHOW VARIABLES LIKE 'max_allowed_packet'`，每批预算取该值的 9/10，**不再另设下限**；读不到（权限受限或代理屏蔽 `SHOW VARIABLES`）时按 4 MiB 保守值并打印告警。字节按值的实际宽度估算——字符串/二进制按含转义后的宽度精确计，其余标量按其最宽文本形式界定。行数或字节任一触顶即提交一批；单行自身就超过预算时独立成批（一行无法再拆分，不丢弃）。此前这里还有一条 64 KiB 下限，它会把预算抬过服务端自己刚说出的值：实测把本机 `max_allowed_packet` 降到 `40000`（生效 39936）并配 `--batch-size 20480`（让字节而非行数决定批次），旧实现算出 `batch_bytes=65536`，5,000 行的文件退出 1、`Error 1153 (08S01): Got a packet bigger than 'max_allowed_packet' bytes`、`failed=2870`、**0 行落库**，而记录还把决定者写成 `limiting_factor=max_allowed_packet`——下限才是越界的那个数；去掉下限后同一条命令为 `batch_bytes=35942`、退出 0、5,000 行全部落库。
- **占位符上限**：MySQL 预处理语句的参数个数是 2 字节无符号整数，故 `行数 × 列数 ≤ 65535`。超出时自动下调有效批大小并打印一次告警（例如 2000 列的表，批大小被压到 32 行）。这条上限是协议事实，压的是行数而不是把某个用户选项换算成另一个用户选项。
- **驱动侧插值上限**：字节预算原本只看服务端 `max_allowed_packet`，但 go-sql-driver/mysql 在客户端插值时还有一道自己的闸门——语句长度超过驱动的 `MaxAllowedPacket`（默认 64 MiB，连接串里看不到）就返回 `driver.ErrSkip`，`database/sql` 随即静默退回服务端预处理语句（长数据分块下发）。这不是失败，只是更慢：实测同一份 80 MiB 数据，插值路径 1.195 s、预处理路径 2.078 s。因此默认预算取 `min(服务端预算, 64 MiB × 9/10)`；**显式给出的更大数字不改写**，只记一条 `warn` 说明这些语句会退回预处理路径。谁在决定是看得见的：测试床按 OceanBase V4.x 的默认 `max_allowed_packet=16M` 时记 `batch_bytes=15099494 limiting_factor=max_allowed_packet`（驱动上限根本轮不到），把服务端调到 67,108,864 B 仍是 `max_allowed_packet`（预算恰等于 60,397,977），再高一档到 67,109,888 B 就改记 `limiting_factor=driver_packet_cap`；此前 128 MB 的床上记的是 `batch_bytes=60397977 limiting_factor=driver_packet_cap`。

这两道上限只影响导入侧自己构造的批，不改变文件内容。想主动把批次切小（例如把一次导入的压力分给多个较小的包，或在代理/网关上有更严的单包限制），`--statement-size` 就是为此而设的第三道**可选**上限：它取「给定字节数」与上面那条 `max_allowed_packet` 预算的**较小值**，因此只会压低、永远不会抬高（抬高的话一批就可能在服务端中途触发 `error 1153`）；给的值再小也按给定值执行，因为显式的数字是有意选择，而这里已没有一条下限会反过来改写它。它与导出侧同名选项语义不同——导出侧是合并语句的预算、默认 8 MiB 且不接受 `0`，导入侧是可选上限、默认 `0` 表示不额外压低。单线程与并行导入、DEL/SQL/Parquet 三种输入格式走的是同一处预算解析，实测都生效（`--threads 4` 与 SQL、Parquet 文件同样是 1→7 条）。实测（本地 MariaDB，测试床按 **OceanBase V4.x 默认的 `max_allowed_packet=16M`** → 预算 15,099,494 B；424,882 B / 2,048 行 × 约 207 B 的 DEL 文件；`--batch-size 20480` 使行数不触顶，只看字节；以目标库 `Com_insert` 全局计数差）：不加该选项服务端收到 **1** 条 INSERT（`limiting_factor=max_allowed_packet`）；`--statement-size 65536` 为 **7** 条（`--threads 4`、SQL 输入、Parquet 输入同样各 7 条）；`--statement-size 1024` 为 **410** 条（约 5 行/批，1024 B 装不下 207 B 的 6 行；上一轮那 342 条是另一份更窄的 fixture，差异来自行宽而非床）；`--statement-size 268435456`（高于服务端预算）仍是 **1** 条并记 `batch_bytes=15099494 limiting_factor=max_allowed_packet`——数字再大也抬不高预算。四种落库行数与 `CRC32` 校验和完全一致（2048 / 4463837020299）。每次运行解析一次，记一条 `import batch byte cap resolved max_allowed_packet=… batch_bytes=… statement_size=… limiting_factor=…`。

### DEL 读取路径的开销

DEL 导入的 CPU 主要在两处：**记录分帧**（按 `--row-del` 找行边界，要处理引号内的分隔符）和**字段解析**（拆列、去引号、还原转义）。读取路径按"每个值只拷贝一次"设计：

- 定界符在建立 reader 时各转一次 `[]byte` 并预置首字节，逐位置比较是定长比较，不在每字节的循环里重复转换字符串
- 行切片直接引用读缓冲，不先物化成整行 `string`
- 没有被转义的字段从行切片一次生成值；含双写引号或反斜杠转义的字段才走拼装缓冲，`fields`/`quoted`/拼装缓冲在同一 reader 内跨行复用
- 字段值类型保持 `string`：改成 `[]byte` 不会减少分配（装箱进 `driver.Value` 时切片头比字符串头更宽），还会改变预处理语句的字符集语义

单线程导入时这条路径与数据库写入串在一起，因此它变快之后 `--threads` 才有意义：实测 50 万行 × 11 列（DEL 125 MB）在 `--threads 1` 下墙钟 6.81s→5.70s、进程 CPU 时间 8.02s→3.76s，`--threads 4` 从 5.61s 降到 3.64s，即 t1→t4 的加速比由 1.21× 提升到 1.87×；再往上加线程时瓶颈转到数据库写入侧。微基准与差分测试见 `internal/import/del_reader_bench_test.go`、`internal/import/del_reader_equiv_test.go`。

> 单条记录仍然一次性驻留内存（巨型行如此，SQL 格式的巨型语句也如此），进程级天花板请用 `GOMEMLIMIT`。

### 优化器 Hint（`--query`）

`--query` 完整保留 SQL 中的优化器 hint，可用于控制 OceanBase / MySQL 的执行计划：

```bash
# OceanBase 并行 hint（放在 SELECT 之前）
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "/*+ parallel(8) */ SELECT id, name FROM users"

# hint 写在 SELECT 之后同样支持
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/orders.del \
  -q "SELECT /*+ INDEX(orders idx_status) */ * FROM orders WHERE status='active'"
```

单线程直接原样执行 `--query`，hint 天然保留；并行导出为注入分片条件会解析重写 SQL，重写前剥离语句最前面的 `/*+ ... */` hint，注入分片条件后再拼回，确保下发给数据库的分片查询（含分片键探测语句）仍携带 hint。

### 并行导入

```bash
# 4线程并行导入
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  --threads 4 \
  -t users
```

> 断线重放窗口是**进程级**的：每个 worker 保留「未提交批次」的值以便连接掉线后重放，历史上这个上限是每 worker 64 MiB，于是 `--threads 4` 实际可能留住 4×64 MiB（实测 4 线程宽行导入峰值 RSS 208 MB，几乎全是重放缓冲）。现在 64 MiB 是**总预算**，按 `--threads` 均分给各 worker（4 线程各 16 MiB、8 线程各 8 MiB），`--threads>8` 时改为各自保底 8 MiB——下限存在是因为再小就会比单批语句的常见宽度还窄，字节上限会批批触发、`--commit-count` 就此变成死选项；也就是说重放内存随线程数增长到 8 MiB×线程数封顶，`--threads 256` 最坏 2 GiB，仍只有旧语义（256×64 MiB = 16 GiB）的 1/8。窄行不受影响：行数上限（`--commit-count`）先触发，实测 100 万行窄表 4 线程导入的峰值 RSS 改动前后同为 28–29 MB。

### 列映射导入

```bash
# 方式1：位置映射（文件3列 → 表的name,age,email列）
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/data.del \
  --columns "name,age,email" \
  --no-col-names \
  -t users

# 方式2：名称映射（显式指定文件列到表列的映射）
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/data.del \
  --columns "user_name=name,user_age=age,user_email=email" \
  -t users
```

### 无包裹符模式

```bash
# 导出无包裹符文件（纯分隔符）
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.tsv \
  --col-del "," --no-char-del \
  -q "SELECT * FROM users"

# 文件内容示例：
# name,age,email
# 张三,25,zhang@example.com
# 李四,30,li@example.com

# 导入无包裹符文件
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.tsv \
  --no-char-del \
  -t users
```

> **注意**：无包裹符时空字段没有额外信息可依赖，`NULL` 与空字符串 `''` 会写成同一个空字段，回读一律按 `NULL` 处理（实测：源表里为空串的列，导入后变成 `NULL`）。需要区分二者时必须保留字符串定界符。

### 云环境连接加固

在云上（LB/NAT 网关、SNAT 空闲超时等），长时间没有网络往返的 TCP 连接可能被中间设备静默回收，工具在读写时会得到 `broken pipe` / `connection reset` / `invalid connection` / `gone away` 等瞬断错误。三个层面同时加固：

- **驱动层 socket 超时**（对应 JDBC `connectTimeout`/`socketTimeout`）：`--connect-timeout`（默认 5s）、`--socket-read-timeout`/`--socket-write-timeout`（默认 300s）。这三个参数以 DSN 字段（`timeout` / `readTimeout` / `writeTimeout`）形式传入 go-sql-driver，作用于**每次 socket 读/写**；由于是 DSN 级参数，`--session-var` 无法覆盖（同名会被拒绝）。
- **连接池 TTL**：导入连接池（主连接与每个并行 worker）设置 `SetConnMaxIdleTime(1m)` + `SetConnMaxLifetime(5m)`，让连接在 LB 空闲阈值前主动老化重建；并行 worker 另设 `SetMaxOpenConns(1)`。导出连接池**不**做老化（`ConnMaxIdleTime=0`、`ConnMaxLifetime=0`）：它用裸 `START TRANSACTION ... WITH CONSISTENT SNAPSHOT` 建立快照，连接在语句之间会归还空闲池，一旦被当作空闲连接关闭，快照就被静默回滚、后续读取失去一致性。
- **事务空闲提交**：导入的写入端（单线程管线与每个并行 worker）在 30s 内没有收到新批次时，主动 `COMMIT` 当前已写入的部分事务并开启下一个，缩短未提交窗口和重放集合；`--commit-count` 到期或重放缓冲达到 64 MiB 字节预算时同样提交。
- **瞬断自动重试**：对 `driver.ErrBadConn` / `invalid connection` / `io.EOF` / 网络超时 / `broken pipe` / `connection reset` / `server has gone away`(2006) / `lost connection`(2013) 等连接级错误，默认最多 3 次尝试，指数退避约 1s→2s（上限 4s，带 20% 向下抖动，避免多个 worker 同时重连）。重连后**先回放当前事务内所有已执行但未提交的批次，再重试失败批次**——连接断开时 MySQL 已回滚整个事务，只重试单批会静默丢掉同事务中先前的行。回放缓冲同时受 `--commit-count`（1000 行）与 64 MiB 字节预算约束，任一触顶即提交，避免宽 BLOB/TEXT 行撑爆内存。`COMMIT` 本身不重试（响应丢失时无法判断是否已生效，重放可能重复写入），直接报错。`context.Canceled`、语法/约束类错误以及 `1213` 死锁 / `1205` 锁等待超时不重试。

对导出而言，主连接与分片连接都保留 `SetConnMaxLifetime(0)` 且 `SetConnMaxIdleTime(0)`（不因 TTL 关闭，保证一致性快照期间连接稳定）；读写超时对每条连接独立生效，长时间零字节的响应（如大 result set 起始阶段）会命中 `socket-read-timeout`，请按需调大。

正因为导出的连接不做老化，"读完就放"成了它唯一的释放时机：并行导出的每个分片在自己的行全部读完后立刻结束快照并关闭该分片连接，之后才把剩余字节交给按序写出的那个 goroutine。反过来说，先前完成的分片不会一边占着连接和快照、一边等前面的分片腾出写出位置——云 LB/NAT 回收的空闲 socket 正是打在这种"不再产出任何数据、却仍打开"的连接上。导入侧不需要对应措施：并行 worker 由单个读取 goroutine 经共享队列动态派工，拿不到活就提交、关闭自己的连接后退出（实测 `--threads 5` 全程恰 6 条连接，结束时全部消失）。

### 参数边界校验

所有参数校验前置到真正读写文件/连库之前，越界或冲突一律以用法错误退出（退出码 `2`，判定口径见需求文档 3.3.4），不会留下半成品文件或副作用。主要边界：

| 选项 | 合法范围 | 越界行为 |
|------|----------|----------|
| `--port` | `1`–`65535` | `invalid --port`，退出码 2 |
| `--threads` | `1`–`256`（每任务一条物理连接+协程，防止打爆 FD/`max_connections`） | `invalid --threads`，退出码 2；静态范围之外的代价在连上库之后出声：worker 数 + 1 超过 `@@max_connections` 记 `warn`（`workers exceed the server connection limit…`），并行导入的重放缓冲按 worker 各自计算、总量超过进程预算时记 `warn`（`parallel import replay window scales with threads`），两条都在任何数据移动之前 |
| `--statement-size`（导出） | `>0` 且 ≤ `268435456`（256MiB，防止 SQL writer 拼接到 OOM） | `invalid --statement-size`，退出码 2 |
| `--statement-size`（导入） | `0`–`268435456`（`0`=不额外压低，只按 `max_allowed_packet` 预算；该值只能压低预算、不能抬高） | `invalid --statement-size %d: must be between 0 and 268435456 (0 means no extra cap)`，退出码 2；默认预算还按驱动侧 64 MiB 客户端插值上限钳到 `60397977` 字节（`limiting_factor=driver_packet_cap`）——这只在服务端值**大于** 67,108,864 B（64 MiB）时发生，按 OB V4.x 默认 16M 的床上是 `batch_bytes=15099494 limiting_factor=max_allowed_packet`；显式给出更大的数字不改写、只记 `warn` |
| `--batch-size` | `1`–`100000`（读→写交接每批行数） | `invalid --batch-size`，退出码 2；`export` 里与 `--threads>1` 同时显式给出也是用法错误，退出码 2 |
| `--pipeline-buffer` | `0`–`1024`（交接队列深度，批数；`0`=同步交接） | `invalid --pipeline-buffer`，退出码 2；`export` 里与 `--threads>1` 同时显式给出也是用法错误，退出码 2；`import` 侧深度 × 单批字节预算可能超过 1 GiB 时**不改深度**，只在数据移动前记 `warn`（`the hand-off queue may retain more than the memory envelope, importing at the configured depth`，实测 `requested=1024 batch_bytes=15099494 queued_bytes=15461881856 envelope_bytes=1073741824`，运行仍按深度 1024；床上预算为 15,099,494 B 时深度 71 及以下完全不记这条）；库调用方传入的负值按 `0`（同步交接）处理，不再让 `make` 因负容量崩溃 |
| `--connect-timeout` / `--socket-read-timeout` / `--socket-write-timeout` | 均 `>0` | 退出码 2 |
| `--progress-interval` | `>0` 且 `≥10ms`（定时进度日志间隔；`0` 不是"关闭进度"，那是 `--quiet`）| `invalid --progress-interval`，退出码 2：0 与负值报 `must be > 0`，`1ms`/`9ms` 报 `must be >= 10ms`（实测 `1ns` 在过去是合法值，一次 6400 行导出打出 1355 条进度、日志 175,557 字节）|
| `--commit-count` | `≥1` | `invalid --commit-count`，退出码 2 |
| `--skip-count` / `--row-count` | `≥0`（`row-count=0` 表示无限制） | 退出码 2 |
| `--log-level` | `debug`/`info`/`warn`/`error`（大小写与首尾空格容错） | `invalid --log-level`，退出码 2（此前非法值会被静默降级为 info） |
| `--log-format` / `IMEX_LOG_FORMAT` | `java`（默认）/ `text` | `invalid --log-format`，退出码 2（此前非法取值静默回落 java） |
| `--format` / `--mode` | 各自枚举白名单 | 退出码 2 |
| `--default-charset` | 字符集白名单；非 `utf8mb4` 不可用于 `parquet`；`gbk`/`gb18030` 不可用于 `sql`（按字节转义不安全） | `invalid --default-charset` / 格式冲突，退出码 2 |
| `--hex-blob` + `--no-hex-blob` | 二者互斥 | `mutually exclusive`，退出码 2 |
| `--char-del` `--col-del` `--row-del` `--escape` `--no-escape` `--timestamp-fmt` `--date-fmt` `--no-char-del` `--no-col-names` | 仅 `--format del` 生效（SQL/Parquet 写端不使用这些修饰） | `--xxx can only be used with --format del`，退出码 2 |
| DEL 分隔符两两同值（`--col-del`/`--row-del`/`--char-del`/`--escape`）| 五对组合必须互不相同：列=行、字符=列、字符=行、转义=列、转义=行；`--escape` 与 `--char-del` 同值**合法**（默认 `"` 即双写转义），`--no-char-del` 免除字符相关项、`--no-escape` 免除转义相关项；判定按固定顺序（char、col、row、escape）取第一条冲突 | `invalid --col-del ",": it is the same as --row-del, which makes the file layout ambiguous`，退出码 2。此前这类组合被接受：实测 200 行导出用 `--col-del , --row-del ,` 得到 14,501 B、**0 个行分隔符**、402 个逗号的文件，用同样参数回读时读出 401 行、**落库 0 行**（全部失败）却只报 `import aborted`|
| `--shard-key`（导出）指向的列 | 显式给出的分片键必须存在于目标表，但**校验只发生在并行路径**；自动检测失败仍回落单线程。`--threads 1` 或并行请求回落到单线程时，这个值既不被读取也不被校验，运行改记 `warn`（`single-threaded export ignores --shard-key`，带 `shard_key`/`threads`/`fallback`/`hint`），配置记录在写时就已确定用不上的场合（`--threads 1`）追加 `shard_key_ignored=true`；退出码仍为 0，产物与不带该选项逐字节一致 | `invalid --shard-key "nosuchcol" for table "t": <原因>`，退出码 2（并行路径；此前只记一条 `warn` 并按单线程跑完、报告成功）|
| `--hex-blob` / `--no-hex-blob` + `--format parquet` | Parquet 二进制列原生按 `binary` 存储，两个开关都不改变输出 | `--hex-blob cannot be used with --format parquet`，退出码 2 |
| `encrypt` 的位置参数 | 恰好 1 个（密码明文） | `imex encrypt accepts 1 argument(s), received N`，退出码 2 |

关联校验：`--create`/`--no-data`/`--statement-size`/`--extended-insert` 仅对 `--format sql` 生效（这一串是导出侧的 SQL 专属校验；导入侧的 `--statement-size` 与格式无关，DEL/SQL/Parquet 三种输入都生效），`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-escape`/`--timestamp-fmt`/`--date-fmt`/`--no-char-del`/`--no-col-names` 仅对 `--format del` 生效，显式传入且格式不符时一律报错而非静默忽略；`--hex-blob`/`--no-hex-blob` 对 SQL 与 DEL 有效，与 `--format parquet` 组合时同样报错（Parquet 二进制列已是原生 binary，开关无意义）；`--mode create` 仅支持 `--format sql`；导出侧 `-q`/`--query` 与 `--query-file` 互斥，且必须恰好给出一个（都不给或只给空语句都是用法错误，退出码 2）。这三类判据、报错文案、`--help` 里追加的范围标记（`(only with --format del)` / `(only with --format sql)` / `(not with --format parquet)`）以及配置回显要打印的字段集合，全部由 `internal/config/formatscope.go` 这一张登记表产出，因此不会出现帮助文本说能用、运行时却报错，或反过来回显了一堆被忽略的字段的读感；新增一个格式专属选项只需在表里加一行。另有一类**按执行路径判定**的越界：`export --threads>1` 时显式给出 `--batch-size` 或 `--pipeline-buffer` 即为用法错误（退出码 2），因为它对分片流式路径不改变任何输出；该校验排在 `--threads`→`--batch-size`→`--pipeline-buffer` 的顺序里，因此非法的 `--threads` 或非法的选项值仍先于冲突报出。另有两类**跨选项 / 跨服务器**的判定：DEL 的四个文本修饰 `--char-del`/`--col-del`/`--row-del`/`--escape` 之间必须互不相同（例外只有 `--escape` 与 `--char-del` 同值，即默认的双写转义；`--no-char-del`、`--no-escape` 各自免除相关项），同值写出的文件无法回读，因此按 char→col→row→escape 的固定顺序取第一条冲突报用法错误（退出码 2），同一条组合在导出与导入两侧判定一致；`--threads` 与目标库 `@@max_connections`、并行导入重放缓冲总量的关系则不是范围能表达的，连上库之后各记一条 `warn`（见上表）。SSL/TLS 选项已移除，`--ssl-*` 现为未知选项（退出码 2）。OceanBase 租户选项 `--tenant` 已移除（见下节）；`--strip-lzeros` 与 `--dec-pt`（数值列始终输出 MySQL wire text，小数点恒为 `.`）亦已移除。早期的 `--codepage` 已移除——它只描述文件码页且对二进制列有损；连接/文件字符集改由 mysqldump 惯用法的 `--default-charset` 掌管，二进制列的字符集安全由 `--hex-blob`（非 utf8mb4 自动开启）保证（见上文"字符集与二进制编码"）。

### OceanBase 租户连接

OceanBase 的租户通过**用户名**指定，OBProxy 在握手认证阶段据此路由，支持 `user@tenant`、`user@tenant#cluster` 等形式：

```bash
imex export -h oceanbase-host -u 'root@my_tenant#obcluster' -p password -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"
```

### 自定义分隔符

```bash
# 使用十六进制分隔符（制表符 0x09）
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.tsv \
  --col-del 0x09 --no-char-del \
  -q "SELECT * FROM users"

# 使用多字节分隔符
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  --col-del "||" --row-del "\r\n" \
  -q "SELECT * FROM users"
```

### 批量导入控制

```bash
# 跳过前 1 行数据，导入 1000 行，每 100 行提交一次
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  --skip-count 1 --row-count 1000 --commit-count 100 \
  -t users
```

> `--commit-count` 的默认值 `1000` 是量出来的、不是猜的：本轮把它与拐点候选 `20000` 在本地 MariaDB 上对跑（每组 3 次取中位数，落库行数每次都是 100 万）——1M 行窄表 `--threads 4`：1000 → 3.56 s（3.50/3.56/3.79），20000 → 3.66 s（3.66/3.71/3.49）；1M 行平均 168 B 的表 `--threads 1`：1000 → 8.52 s（8.52/9.04/8.46），20000 → 8.47 s（8.19/8.47/8.48）。两侧中位数之差 ≤ 0.1 s、小于同组内 ±0.6 s 的抖动，即加大提交窗口在这台机器上不构成拐点，因此**保持默认 1000**（更大的窗口只会拉长掉线重放的范围）。真正影响重放内存的是那条字节上限（见上节），它现在按 worker 均分。

### 日志文件

```bash
# 导出：命令行日志只写文件，终端不打印
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  -m /var/log/imex_export.log \
  -q "SELECT * FROM users"
```

导出/导入的运行日志同时打印**行数**与**文件字节数**，行数据全部来自服务端/驱动的真实反馈：导出侧行数按**实际写入文件的行数**统计；导入侧 `read` 按**进入写入管道的文件行数**统计（不含 `--skip-count` 跳过与被 `--row-count` 截断的行），并按每批的 `RowsAffected` 拆成互斥的 `inserted` / `updated` / `unchanged` 三个计数（`insert_update` 模式下一条新行贡献 1、一条被实际改动的已存在行贡献 2、一条写入相同值的行贡献 0，因此 `updated = max(0, affected - rows)`、`unchanged = max(0, rows - affected)`、`inserted = rows - updated - unchanged`，三者之和恒等于该批行数）；`insert`/`create`/`replace` 模式的 affected 恒等于行数，全部计入 `inserted`——`replace` 是先把目标表清空再插入，所以文件里每一行都是新插入。批次执行失败的行计入 `failed`。导入、导出均每隔 `--progress-interval`（默认 **5 秒**）打印一次进度：`rate` 是**自开始以来的总速率**（已处理 ÷ 已用时长，平滑，反映整体快慢），`interval_rate` 是**上一个进度点到这一个进度点之间的区间速率**（两次快照之差，反映此刻吞吐与抖动；两者无倍率关系，长跑时看 `interval_rate` 判断当前快不快、看 `rate` 判断整体多少），另打印已处理条数与 `elapsed`。所有耗时字段（进度里的 `elapsed` 与汇总里的 `duration`）固定以**秒**为单位、保留**三位小数**（`842ms` 写成 `0.842s`、90 秒写成 `90.000s`），不再随量级在 `ms`/`s`/`m` 之间切换，跨记录可以直接相减比较。结束日志打印总条数、`bytes`、`duration` 与总速率；运行中止时打印 `import aborted` 汇总（含 `failed`）。**汇总恒为本次运行的最后一条记录**：写汇总之前先停掉进度计时器，而停止会等待正在写的那条进度落笔，因此不会再出现 `completed` 之后跟一条 `progress` 的读感。反过来说，**运行时长不足一个间隔时一条进度也不会打印**：首个计时点要等满一个间隔才触发，停止只等在写的那条记录、不空等剩余间隔，所以一个 3 行的小表 6 ms 就结束，日志里只有横幅、配置与汇总（实测），把间隔调成 `200ms` 后同一类长跑（100 万行）则每 200 ms 一条、共 29 条进度，最后一条仍是 `export completed`。

版本横幅（`imex starting`，含 version/build_time/git_commit/go_version/command）始终作为运行的**首行**日志输出，随后是配置参数快照（`export/import config`，由 `NewExporter`/`NewImporter` 在**建立连接之前**打印，因此连不上服务端时也看得到参数；原先重复它的 `export started`/`import started` 已删除）。快照只打印**所选 `--format` 真正读取的字段**，判据与用法错误的拒绝逻辑、`--help` 里的范围标记同出自一张登记表 `internal/config/formatscope.go`：`-f sql` 不再打印 `char_del`/`col_del`/`row_del`/`escape`/`timestamp_fmt` 这些被忽略的 DEL 布局，`-f del` 不再打印 `statement_size`/`no_create`/`extended_insert`，`-f parquet` 两类都不打印（实测：`export` 的回显改动前对三种格式是**逐字节相同**的同一套 26 项，现在分别是 25/20/15；`import` 现在是 29/20/20）。字段按「连接 → 输入输出 → 并行与流水线 → 字符集 → 该格式的专属布局 → 日志」排列，且补齐了 DEL 写入器一直在读、快照却从未打印的 `date_fmt`、`no_escape`、`no_char_del` 三项；`hex_blob` 打印的是**生效值**（`--hex-blob`/`--no-hex-blob` 都没给时由字符集决定），与写入器用的是同一个判定函数；唯一例外是 `imex version` 子命令——它只往标准输出打印版本、Git commit、构建时间与 Go 版本四行，不产生任何日志记录（这些字段与横幅完全重复），因此可以安全地 `imex version | grep ...`。所有记录的消息统一为**小写、单行的 `<对象> <事件>`**，事件词固定为 `config`/`progress`/`completed`/`aborted`/`failed`（`started` 只出现在版本横幅 `imex starting` 里；如 `export config`、`shard export completed`、`insert batch failed`、`worker completed`），大小写敏感的一条 grep 就能覆盖全链路；同一事件只输出一行。默认 `java` 布局与 `text` 布局都保证**一条记录 = 一个物理行**：值为空或含空白/控制字符/`"`/`\`/`=` 时按 slog 规则加引号转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`），因此打印 DEL 定界符的配置行不会被真实换行拆开，切换 `--log-format` 也不改变字段切分；`--log-format` 只换布局、不改 `--log-level`。加 `--quiet`（或 `IMEX_QUIET=1`）可**禁止定时进度日志**，仅保留配置、结束统计与 ERROR 记录；`--messages` 文件同样没有进度行（早期版本让消息文件按 DB2 格式继续记 `SQL3105N`，该例外已随报告一起取消）。

`--messages` / `-m` 的文件内容**就是本次运行的命令行日志**，并且是它唯一的目的地：一条记录一个物理行，版本横幅是首行（文件在根命令打印横幅之前打开），`export/import config`（建连接之前）、定时进度、WARN 告警、`completed` 统计与收尾的 `command failed` 都在里面，stdout 与 stderr 一行也不打印；`--log-level`、`--log-format` 一样作用于文件，退出码不受影响（0 成功、1 运行时错误、2 用法错误），脚本照样能判断成败。实测同一条注定失败的导出：带 `-m` 时 stdout/stderr 均 0 字节、文件 5 条记录、退出 1，而不带 `-m` 那次的 stderr 与该文件去掉时间戳/PID 前缀后 `diff` 逐行相同。两类例外：未知选项在文件打开前就失败，错误只上 stderr 且不建文件；`-m` 路径无法创建时 `failed to create message file <path>: ...` 也只上 stderr。`--help` 与 `-m` 同时给不产生文件。早期版本按 DB2 LOAD/EXPORT 风格另写一份 `SQL3104N/3105N/3107N/3109N/3110N` 报告，那份重复的记录与 `MessageWriter` 已整体删除，告警与失败只保留一条日志记录。会出现 WARN 的场景：导出侧 `--no-hex-blob` 搭配非 utf8mb4 字符集、单行 INSERT 超过 `--statement-size`；导入侧读不到 `max_allowed_packet`（按保守值分批）、`--batch-size` 被 65535 占位符上限下调、无表头文件被当作表头丢弃首行、`insert_update` 命中无唯一键的表。

### 错误堆栈开关

命令失败时统一以 **ERROR 级日志记录**输出（`level=ERROR msg="command failed" error="..."`），与其它诊断信息格式一致，不再单独打印裸 `Error:` 行。默认只含错误链文本；开启错误堆栈后会附带 pkg/errors 记录的完整调用栈（每一层 wrap 都带自身栈帧），便于排障。两种开启方式：

```bash
# 方式1：命令行标志（持久标志，所有子命令可用）
imex export --error-stack -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del -q "SELECT * FROM nope"

# 方式2：环境变量（适合脚本/容器）
IMEX_ERROR_STACK=1 imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del -t users
```

输出示例（默认，无堆栈）：

```
time=... level=ERROR msg="command failed" error="failed to create exporter: Error 1146 (42S02): Table 'mydb.nope' doesn't exist"
```

开启 `--error-stack` 后，`error=` 字段内追加 pkg/errors 各层 wrap 的完整调用栈：

```
time=... level=ERROR msg="command failed" error="failed to execute query:
github.com/imex/internal/export.(*Exporter).exportSingle
	/path/exporter.go:185
...
export failed
github.com/imex/cmd.init.func2
	/path/export.go:111
..."
```

### 失败 SQL 回显与脱敏

导入/导出语句在服务端执行失败时（列不存在、类型不符、数值超长、约束冲突等），错误信息与日志（`--messages` 打开时在文件里）都会附带失败 SQL 的结构回显，便于定位。为防止敏感数据泄露，**默认不内联真实行数据值**，`?` 占位符只以类型/长度标签呈现：

```
Error: import failed: failed to insert batch (SQL: INSERT INTO `u` (`id`, `pw`) VALUES (?,?),(?,?) /* args: str(1), str(14), str(1), str(12) */): Error 1406 Data too long for column 'pw' at row 1
```

标签足以判断"哪一列、什么类型、多长、是否 NULL"，而不会把密码/个人信息写进长期留存的日志与日志文件。回显超过 2048 字节自动截断。确需内联真实值排障时显式开启（警告：真实数据将出现在 stderr，或出现在 `--messages` 文件）：

```bash
imex import --echo-sql-values ...
IMEX_ECHO_SQL_VALUES=1 imex import ...   # 环境变量方式
```

`--query` 的 SELECT 原文与 CREATE TABLE 语句不含行数据，失败时始终原文回显。

### 调试级 SQL 交互回显

```bash
# 查看导出在并行分片前到底发了哪些语句
imex export --log-level debug -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del --threads 4 -q "SELECT * FROM users" 2>&1 \
  | grep 'db interaction'
```

`--log-level debug` 会把工具与数据库之间的**交互语句**逐条记成 `db interaction`（导出、导入都适用），用于确认自动分片切了哪些边界、导入前跑了哪些校验。绑定参数按既有脱敏规则以 `num`/`str(长度)`/`bytes(长度)`/`NULL` 呈现，真实值仍需 `--echo-sql-values`。

覆盖范围是"语句数量不随行数增长"的那一类：导出侧的 `SHOW CREATE TABLE`、`INFORMATION_SCHEMA` 分片统计与列类型、`MIN`/`MAX`、`LIMIT 0` 列探测、采样探针、`version_comment`；导入侧的表/列/唯一键校验、表头判定、`SHOW VARIABLES max_allowed_packet`、`CREATE TABLE`、`DELETE`、`SET FOREIGN_KEY_CHECKS`。**逐行数据语句不打印**——攒批 INSERT/REPLACE 只在失败时经脱敏错误路径出现，因此 debug 日志不会因为导入百万行而膨胀。并行导出还会在每条分片记录上带 `worker=N`，分片查询失败的文案也以 `(worker N, SQL: ...)` 开头。

默认的 info 级别下这些记录一条都不会出现。

### 运行时 SQL 归因注释

默认情况下，工具在自己**主动执行**的语句前追加一段自描述注释 `/* imex:<commit7> */`（commit 为构建经 ldflags 注入的 git 短哈希前 7 位）。目的是在数据库 `general_log`、慢查询日志或审计流里，按该前缀即可过滤出"imex 执行的语句"，并据 commit 定位到具体二进制版本。

加注释的范围是**工具对数据/结构/元数据主动发起的语句**：

- 导出：主 `SELECT`、并行分片查询、列探测（`LIMIT 0`）、`SHOW CREATE TABLE`、主键/唯一键/列类型/min-max 探测、`SHOW VARIABLES version_comment`
- 导入：`INSERT` / `REPLACE` / `INSERT … ON DUPLICATE KEY UPDATE` / `DELETE` / `CREATE TABLE`、目标表与列的存在性/类型校验查询

**不加**注释的是工具的连接期与事务期内部控制语句：`SET NAMES` 与 `--session-var`（均由 DSN 在建连时发出、不经执行路径）、`SET FOREIGN_KEY_CHECKS`，以及 `START TRANSACTION` / `SET TRANSACTION` / `COMMIT` / `ROLLBACK`。

可用 `--no-sql-tag` 或 `IMEX_NO_SQL_TAG=1` 关闭该注释。dev 构建（`GitCommit=unknown`）或注入值非合法十六进制哈希时不加注释，保持日志零噪音；注释内容限定为 `[0-9a-f]`，不会提前闭合或注入。该注释**只作用于运行时发往服务端的语句，导出文件内容不受影响**（导出的 SQL 文件不写此注释）。若 `--query` 含优化器 hint `/*+ … */`，前导普通块注释不影响 hint 解析（hint 仍紧跟 `SELECT`）。

### 密码加密

为避免在 `ps` 命令中泄露明文密码，可以使用 PBKDF2-HMAC-SHA256 + AES-256-GCM 加密密码：

```bash
# 1. 设置加密密钥（环境变量）
export IMEX_KEY="your-secret-key"

# 2. 加密密码
imex encrypt "my-password"
# 输出: ENC:GWmxdaDgE9vyGSiE5SVPKTidlv5WX+Fmhhr79qvsPu39e9fGUuiUT9nfCDXtre0RToSljtNCdA==

# 3. 使用加密后的密码执行导出/导入
imex export -h 127.0.0.1 -u root -p "ENC:GWmxdaDgE9vyGSiE5SVPKTidlv5WX+Fmhhr79qvsPu39e9fGUuiUT9nfCDXtre0RToSljtNCdA==" \
  -D mydb -o backup.sql -q "SELECT * FROM users"
```

**自动化脚本示例：**

```bash
#!/bin/bash
# 设置密钥（从安全存储获取）
export IMEX_KEY="my-secret-key"

# 加密密码（只需执行一次，保存结果）
ENCRYPTED_PWD=$(imex encrypt "actual-password")

# 批量导出多个数据库
for db in db1 db2 db3; do
    imex export -h 127.0.0.1 -u root -p "$ENCRYPTED_PWD" -D "$db" -o "${db}.sql" -q "SELECT * FROM users"
done
```

**安全特性：**
- ✅ `ps` 命令看不到明文密码（只显示 `ENC:` 开头的密文）
- ✅ AES-256-GCM 加密，密钥经 PBKDF2-HMAC-SHA256（600000 次迭代）从 `IMEX_KEY` 与每条密文独立的 16 字节随机盐派生（相同密码两次加密结果必不同；离线尝试密钥的代价实测从约 94 万次/秒降到约 3 次/秒）
- ✅ 密钥通过环境变量传递（不暴露在命令行）
- ✅ 自动解密（export/import 命令自动识别 `ENC:` 前缀并解密）

**注意事项：**
- 加密密钥必须通过 `IMEX_KEY` 环境变量设置
- 密钥丢失将无法解密密码，请妥善保管
- 建议在自动化脚本中使用密钥管理服务（如 Vault）存储密钥，由运行环境注入 `IMEX_KEY`
- 工具只从 `IMEX_KEY` 环境变量读密钥，**不提供 `--key-file` 之类的密钥文件选项**：密钥托管属部署侧职责
- `IMEX_KEY` 是这套机制里唯一的秘密：PBKDF2 只抬高猜测成本，不会把人可记忆的口令变成强密钥
- 2026-10-26 之前的 `ENC:` 密文全部失效（布局改为 `salt ‖ nonce ‖ 密文+标签`，无版本位、不做兼容），需用当前版本重新执行一次 `imex encrypt`

## 文件格式示例

### DEL 格式

```
"id","name","email","created_at"
1,"Alice","alice@example.com","2026-01-01 10:00:00"
2,"Bob","bob@example.com","2026-01-02 11:00:00"
3,"Charlie","charlie@example.com","2026-01-03 12:00:00"
```

### Parquet 格式

Parquet 文件为二进制列式存储，无法直接文本预览。所有列统一存储为可空 UTF-8 字符串，NULL 保留为 Parquet NULL，行序与 `--query` 结果一致。

> 二进制列（BLOB/BINARY 等）在 Parquet 中存为不带 UTF-8 标注的原始 BYTE_ARRAY，非 UTF-8 内容可完整往返（实测 HEX 指纹一致）。

```bash
# 查看 schema（需要 parquet-tools 或 DuckDB 等工具）
parquet-tools schema /backup/users.parquet
# message schema {
#   optional binary id (STRING);
#   optional binary name (STRING);
#   optional binary email (STRING);
# }
```

### SQL 格式

```sql
-- Table structure for users
CREATE TABLE `users` (
  `id` int NOT NULL AUTO_INCREMENT,
  `name` varchar(100) NOT NULL,
  `email` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Data for users
INSERT INTO `users` (`id`,`name`,`email`,`created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00');
INSERT INTO `users` (`id`,`name`,`email`,`created_at`) VALUES (2,'Bob','bob@example.com','2026-01-02 11:00:00');
```

SQL 文件的默认布局是**每行一条 INSERT**（mysqldump `--skip-extended-insert` 的布局），`--extended-insert` 才把多行合并进一条语句。攒批交给导入侧是刻意的分工：导入器把语句拆成行后按 `--batch-size` 重新组批，文件是单行还是合并布局，服务端收到的语句一模一样（实测 20,000 行、`--batch-size 1000`：单行文件 20,000 条语句与合并文件 1 条语句都只产生 20 条服务端 INSERT）。合并布局在导出侧换到的只有体积——100 万行窄表实测：单行 2.5 s / 峰值内存 33 MB / 278,790,434 B（gzip 28,247,005 B），合并 2.7 s / 70 MB / 153,792,809 B（gzip 27,050,101 B，19 条语句）；代价是导入侧要一次性解析整条语句，同一份数据在 `--batch-size 1000` 下峰值内存 44 MB（单行）对 216 MB（合并）。

### 三类格式共有的值处理

- **`YEAR` 零值**：MySQL/MariaDB 文本协议把 `YEAR(0000)` 发回裸字符串 `0`，直接回导会被服务端两位 YEAR 规则解释成 2000。导出时识别 YEAR 列并统一写成 `0000`，DEL/SQL/Parquet 一致（实测 `--format del` 得 `"1","0000",`，`--format sql` 得 `'0000'`）。
- **NULL 与空字符串**：DEL 默认模式下 NULL 是不加定界符的空字段、空串是 `""`，可区分；`--no-char-del` 下两者形态相同，回读一律按 NULL。SQL 用 `NULL` 与 `''` 区分。Parquet 用列式 NULL 区分。
- **二进制列**：DEL/SQL 在 `--hex-blob` 生效时写 `0x…`，空值写 `0x`；Parquet 存原始 BYTE_ARRAY，不做 UTF-8 标注。
- **日期与时间戳**：`--date-fmt`/`--timestamp-fmt` 仅作用于 DATE 与 DATETIME/TIMESTAMP 列，其余列不受影响。

## 修复记录（v0.2.2）

对照需求文档的系统自测发现以下缺陷，均已修复并通过端到端回归（HEX 字节指纹 + 单位/整表往返校验）：

| 缺陷 | 修复 |
|------|------|
| 日志的耗时字段单位随量级漂移（`842ms`/`4.3205s`/`1m2.3s` 混用），正常跑完看不到工具向库发过哪些语句，并行日志分不清哪个 worker 出错 | ① 新增 `common.Seconds`，单线程/并行导出的 `duration`、导入结构-only/单线程/并行/中止的 `duration` 与 `elapsed`、以及两个进度 `elapsed` 共 8 处统一为秒、三位小数（`842ms`→`0.842s`、`90s`→`90.000s`）。② `--log-level debug` 下新增 `db interaction` 记录，回显导出/导入与库交互的语句（`SHOW CREATE TABLE`、information_schema 校验、分片统计与采样探针、`max_allowed_packet`、`CREATE TABLE`/`DELETE`/`SET FOREIGN_KEY_CHECKS` 等），绑定参数按 `num`/`str(len)`/`bytes(len)`/`NULL` 脱敏；攒批 INSERT/REPLACE 一律不打，因此记录条数只随表/分片/批次规模增长、与行数无关。③ 并行导出的分片完成/失败记录带 `worker` 与 `shard`，分片查询失败的错误前缀为 `(worker N, SQL: ...)`，并行汇总补 `workers`（并行导入沿用既有的 `thread`）。实测 100 万行表 `--threads 4 --log-level debug` 得到 22 条交互记录、`worker=0` 到 `3`、`duration=4.320s`；同一命令降到 info 级后交互记录为 0。新增 `internal/common/logsql_test.go`（3 用例）与 `TestOptionMatrixDebugEchoesInteractionSQL`；无 flag/帮助/退出码/文件格式/env 变化，`cli-oracle` 无需重新基线 |
| create 模式省略 `-t` 时 INSERT 因空表名失败并遗留脏表 | 自动取 SQL 文件首条 CREATE 语句表名回填，再按 `-t`（若提供）重写 |
| 导出侧 `--date-fmt`/`--timestamp-fmt` 解析但不生效 | DEL 写入按列类型（`common.ColumnKind`）应用文本修饰，默认值下输出与旧行为逐字节一致（`--dec-pt` 后来随参数精简移除） |
| 导入侧 `--date-fmt`/`--timestamp-fmt` 未回转换 | 查询目标表列类型后，仅对日期/时间戳列做反向转换，字符串与数值列不受影响（`--dec-pt` 后来随参数精简移除） |
| `--no-col-names` 不带 `-c` 报错晦涩 | 新增前置校验，给出清晰错误并提示使用 `--columns` |
| Parquet 非 UTF-8 二进制回导失败 | 二进制列存为原始 BYTE_ARRAY（去 UTF-8 标注），非 UTF-8 内容完整往返 |
| DEL/Parquet 导入未校验目标列，表达式列头（如 `SELECT 1` 产生的 `"1"`）报 `Unknown column '1' in 'field list'`；replace 模式还会先清空表再失败 | 导入前用 `information_schema` 校验目标列存在性（大小写不敏感），缺失时立即给出含修复建议的错误（起别名或用 `--columns`），表不存在同样明确报错；校验在 mode 处理之前执行，replace 模式不再发生"先清表后报错"的数据丢失 |
| 语句执行失败时错误信息不含 SQL，难以定位 | 失败 SQL 随错误回显到日志（stderr，或 `--messages` 文件）；默认脱敏（占位符保留，值以 `str(14)/num/NULL/bytes(256)` 类型标签呈现），2048 字节截断并保持引号配平；`--echo-sql-values` / `IMEX_ECHO_SQL_VALUES=1` 可显式内联真实值 |
| 失败时以裸 `Error:` 行输出、与其它日志格式不一致；版本信息不在首行；进度日志无法关闭 | 命令失败统一走 `level=ERROR msg="command failed"` 日志记录（`--error-stack` 追加调用栈）；版本横幅 `imex starting` 提前为运行首行；新增 `--quiet` / `IMEX_QUIET=1` 禁止定时进度日志（当时消息文件仍继续记进度，该例外已由后面的 `--messages` 改造取消） |
| 导入行数统计把"写入相同值的行"当成新增（`insert_update` 重复导入同一文件时报 `inserted=N updated=0`）| 每批按服务端 affected 拆成互斥的 `inserted`/`updated`/`unchanged`，三者之和恒等于批行数；`replace` 模式（先清空目标表）仍全部计新增。实测：相同内容重导 → `inserted=0 updated=0 unchanged=4`，全部改动 → `updated=4`，删 2 行后重导 → `inserted=2 unchanged=2` |
| 单线程导出失败只报错不清理，磁盘上留下半截产物（实测 20 万行表被 `--no-char-del` 拒于末行时残留 21 MB，文件名与成功导出一致）| 两条导出路径统一"要么完整、要么没有"：`exportSingle` 在创建输出文件之后注册与并行路径共用的 `removeIncompleteOutput`，任何后续失败都删除该文件；注册点保证创建之前的失败（查询报错、`--no-replace` 命中已有文件）不会误删既有文件 |
| `--no-col-names` 导出的文件只用 `-c` 导入时静默丢掉首行（3 行只进 2 行、退出码 0）| 文件本身无法区分"有表头 + 位置映射"与"无表头"，两种情形都必须能导入，因此不改结果、只补可见性：被当作表头丢弃的那一行若与目标表列名全不匹配，记 WARN 日志（`--messages` 打开时只写进该文件），提示补 `--no-col-names`；`--no-col-names` 是成对选项，导出加了导入也要加 |
| `--messages` 写的是另成一套的 DB2 风格报告（`SQL3104N/3105N/3107N/3109N/3110N` 与 `# Completed/# Duration/# Rate`），既与命令行日志重复，又漏掉日志里的多数记录 | `--messages` / `-m` 改为**命令行日志的文件副本**：`internal/common/log.go` 里加带锁的日志 sink（两种布局都经它，`os.Stderr` 在写入时解析），根命令在打印版本横幅之前打开文件、`cmd.Execute()` 在记完 `command failed` 之后关闭，因此文件首行是横幅、失败收尾也在文件里；`MessageWriter` 与导出/导入侧 35 处消息调用整体删除（告警与失败本来就各有一条日志记录，只去掉重复的第二份）；`--log-level`、`--log-format`、`--quiet` 一并作用于文件。实测 `-m` 导出失败时文件与 stderr `diff` 逐字节一致。**注：这一版仍同时写 stderr 与文件，下一行改为只写文件、终端静默 |
| 带 `--messages` 时终端与文件各打一份，`-m` 语义仍像“另存一份”而不是“交给文件” | `sink.Write` 改为单一目的地：文件挂载期间**只写文件**，未挂载时照旧写 stderr，因此带 `-m` 的运行终端完全静默，文件是这次运行唯一的记录，退出码不变（实测失败导出：stdout/stderr 各 0 字节、文件 5 条、退出 1；同一命令不带 `-m` 的 stderr 与该文件去掉时间戳/PID 后逐行相同）。`CloseLogFile` 先脱钩再关闭，所以“关闭失败”这条 ERROR 是唯一回到 stderr、不落进文件的记录。帮助文本改为「Redirect the command log to this file; with it, nothing is printed to stderr」；未知选项在 `PersistentPreRunE` 之前失败，文件尚未打开，故只上 stderr 且不建文件，`--help` 与 `-m` 同时给也不建文件。测试同步改为断言终端为空：`internal/common/log_test.go` 的镜像用例改名 `TestLogFileTakesOverFromStderr`（stderr 0 字节、文件恰 3 条、关闭后记录回 stderr），CLI 矩阵的 5 种格式/并行组合 + `-m` 短选项 + 失败记录 + 级别过滤各加“stdout+stderr 为空”断言，`--no-col-names` 对称用例的 WARN 断言移进文件 |
| 长跑到结束时日志最后一行是 `export progress`，读起来像任务还没完 | 计时器的停止方式导致：`StartMonitor` 返回的 cancel 只发信号，goroutine 里已开始的那次 report 收不回来，而汇总先写好、`defer stopMon()` 在函数返回后才执行。现 stop 改为**排空**（等待 monitor goroutine 退出，并在 tick 与 cancel 同时就绪时再确认一次取消），导出/导入的 6 处终点汇总（`export completed`、`parallel export completed`、结构-only 与常规 `import completed`、`parallel import completed`、`import aborted`）在写汇总**之前**显式停止，停止句柄提为结构体字段；间隔从硬编码 5 s 改为 `common.MonitorInterval` 变量（默认值不变），使回归测试能把计时器压到毫秒级。**注：该变量下一行升级为命令行选项 `--progress-interval`。**回归：`internal/common/stats_test.go` 把一条 report 钉在"正在写"的状态再 stop，旧实现必然产出 `[completed progress progress]`；`TestLive_ProgressNeverOutlivesCompletion` 以 8192 行表在 1 ms 间隔下跑单线程/并行 × 导出/导入四段，断言日志里确有进度记录且最后一条是对应汇总 |
| `export started` 与紧随其后的 `export config` 字段重复（output/input、format、host、port、database 两个都打），而配置回显打在连接之后，连接失败时一个参数都看不到 | 删掉两侧 `started` 记录，并把 `export/import config` 上移到 `NewExporter`/`NewImporter` 的**建连接之前**，回显同时补 `progress_interval`。实测：3 行小表导出 3 条记录、6 ms 结束；查不存在的表失败导出文件 4 行（横幅、config、`query failed`、`command failed`）退出 1；密码写错那次 6 行（横幅、config、3 条 ping WARN、`command failed`）退出 1 且不创建输出文件。同时新增 `--progress-interval`（默认 5s，必须 > 0，`0`/负值为用法错误退出 2；不解释为"关闭进度"，那是 `--quiet`），包级变量 `common.MonitorInterval` 删除、改由 `config.Config.ProgressInterval` + `EffectiveProgressInterval()` 承担（零值与负值回落默认值，避免库调用方把非正间隔传给会 panic 的 `time.NewTicker`）。测试：`internal/config/config_test.go`（2 用例）、`cmd/cli_matrix_test.go:TestCLIProgressInterval`、`internal/common/stats_test.go:TestMonitorSilentBeforeFirstTick`（间隔未到不产出记录且 stop 立即返回），`TestLive_ProgressNeverOutlivesCompletion` 增加一段真实二进制 `--progress-interval 1ms --messages` 的端到端断言 |
| SQL 文件默认把多行合并进一条 INSERT，而导入侧无视文件布局按 `--batch-size` 重新攒批——合并只在导出侧换来体积，代价是导出与导入都要把整条语句留在内存 | `--format sql` 的默认布局改为**每行一条 INSERT**（mysqldump `--skip-extended-insert` 的形态），攒批统一交给导入侧；新增 `--extended-insert` 显式恢复合并，`--single-row-insert` 保留为已废弃别名（与 `--extended-insert` 同时给出为用法错误，退出码 2），`--statement-size` 在默认布局下退化为告警阈值，配置回显补 `extended_insert`。实测（本地 MariaDB，100 万行窄表，热缓存，`/usr/bin/time -v` 峰值 RSS）：导出单行 2.43–2.57 s / 35.5–38.5 MB / 278,790,434 B（gzip 28,247,005 B）/ 1,000,000 条语句，合并 2.65–2.69 s / 71.3–74.1 MB / 153,792,809 B（gzip 27,050,101 B）/ 19 条语句，且 `--extended-insert` 的产物与旧默认**逐字节一致**；导入同一份数据（`--batch-size 1000` 单线程）单行 12.49–12.87 s / 42–44 MB，合并 12.96–13.19 s / 215–224 MB（`--threads 4 --batch-size 20000`：10.0–10.2 s / 67–69 MB 对 8.9 s / 273–276 MB）。目标库 general log 复核（20,000 行、`--batch-size 1000`）：20,000 条语句的单行文件与 1 条语句的合并文件都只产生 20 条服务端 INSERT。结论：内存差是结构性的（导出减半、导入降到约 1/5），速度差在 ±10% 以内，代价是文件体积 +81%（gzip 后 +4.4%）。顺带登记一处既有缺陷（待用户定级）：SQL 文件把 `LOCK TABLES` 写在 `CREATE TABLE` 之前，用标准 `mysql` 客户端回放空库会在第 2 行 `ERROR 1146 (42S02)` 中止，imex 自身导入会跳过这些前导语句故不受影响 |
| SQL 导入把每条语句先物化成字符串、再拷一份大写副本来判关键字，每个 VALUES 元组还要经 `strings.Builder` 重打一遍；跨物理行的语句每追加一行都从头重扫已积累的文本，代价随语句长度平方增长 | 语句级改为全程 `[]byte`：`nextStatement` 直接交出记录缓冲里的切片（只有跨行语句才复制进 reader 复用的 `stmt` 缓冲），引号状态 `stmtState` 跨行携带使一条语句只扫一遍，关键字与 `NULL` 用只折叠 ASCII 的 `hasPrefixFold`/`indexFold`/`equalFoldNull` 在字节上比较，`extractRows` 把元组窗口直接交给 `parseValueList([]byte)`，攒批语句文本改为按精确长度 `Grow` 一次成型、归属注释作为前缀字节写入（不再由 `common.TagSQL` 把整批文本再拼一遍）。值的类型仍是 `string`——装箱到 `driver.Value` 必然分配，只减少拷贝次数。实测（同一份 2 万行 × 8 列数据三种布局的微基准，`-benchmem`）：默认单行 90.0→42.3 ms / 36.6→9.3 MB/op / 728,986→340,009 次分配，`--extended-insert` 69.1→32.8 ms / 32.1→10.2 MB / 629,507→320,368，跨行折行 1839.3→32.1 ms（57×）/ 40.8→10.4 MB / 650,226→320,387；每条批次语句文本的构造 131→21 µs / 138,160→24,792 B / 20→3 次分配。端到端（本地 MariaDB，20 万行，`--batch-size 1000` 单线程，注入归属注释的构建，峰值 RSS）：单行布局 2.61→2.29 s / 41.6→34.6 MB，合并布局 2.85→2.44 s / 187.8→137.6 MB，148 万字节折行文件 5.98→0.184 s；12 次导入（新旧二进制 × 三种布局 × 注释开关）目标表 `CHECKSUM TABLE` 逐组相同，general log 复核服务端仍只收到 10 条约 138,556 字节的 INSERT，下发内容不变 |
| 并行导出中已经读完的分片仍持有连接与一致性快照，直到把尾部字节交给按序写出的 goroutine（云环境里这段空等正对上 LB/NAT 的空闲 socket 回收，实测倾斜表下 `Writing to net` 约 5 s、`INNODB_TRX` 快照年龄涨到 9 s）| `exportShard` 在行迭代结束（`rows.Err()` 判过）后立刻 `releaseDB()` 关闭 `rows` 与分片连接，页脚写出与 `pipe.Close()` 移到释放之后；`releaseDB` 幂等，`defer` 注册在输出 `Close` 之前，保证查询失败、页脚失败等路径下数据库仍是最先被放弃的资源。为在无数据库条件下回归，`Exporter` 增加未导出的 `openShard` 连接器钩子，新增 `internal/export/exporter_shard_test.go`（2 用例）：一个用例起故意没有消费者的 `shardPipe`，断言连接已关闭而 `exportShard` 尚未返回（把改动前的 `defer` 顺序还原回去必然在 5 s 超时失败，已实测），另一个断言空分片不留连接也不挂队列。实测这条**是不变量而不是吞吐**：A/B 同一份 400 万行倾斜表产物逐字节一致（DEL 587,806,929 B md5 `2c9834af64e6`、SQL `--extended-insert` 642,102,879 B md5 `503ae9219565`），耗时持平（DEL 8.334/8.167 s 对 8.140/8.050 s，SQL 12.284/12.674 s 对 12.321/13.384 s）——本地写普通文件时尾部只有 1~2 个 256 KiB 块（写入器即 `bufio.NewWriterSize(w, 256*1024)`）；差别在连接状态：旧二进制采样抓到过 `[3 2:270=Sleep:0 268=Query:6 267=Sleep:6]`（一个读完但仍打开的分片），新二进制再没有"已完成但仍打开"的连接。导入侧本条不适用：并行导入是单读取 goroutine + 共享队列的动态派工，worker 退出即 `commit` 后 `db.Close()`，实测 `--threads 5` 全程恰 6 条连接且结束全消失 |
| 帮助文本与实际语义不符，且并行导出会静默忽略 `--batch-size`/`--pipeline-buffer`：`--statement-size` 在默认单行布局下已不是攒批上限、只是告警阈值，help 仍写 "Max bytes per INSERT statement"；这两个管道选项只有 `exportSingle` 读取，`exportParallel` 的前瞻另由 64 MiB ÷ 分片数决定，于是 `--threads 8 --batch-size 50000` 静默等价于默认值 | 三条 help 改述实际语义（`--statement-size` 加 "only bounds merged statements: without --extended-insert a longer row just warns"，两个管道选项加 "single-threaded export; --threads>1 ignores it and says so"），并在 `exportParallel` 确认按分片并行执行之后（放在分片数判定之后，降级为单线程时不误报）经 `pipelineOptionsIgnoredByParallel()` 记一条 `info`：`parallel export ignores pipeline options`，带生效值与每分片实际前瞻。判定基准是"与出厂默认值不等"而不是"是否传了该 flag"，因此常见运行完全静默。实测（100 万行表，`--threads 4`）：`--batch-size 5000` 那次出现该记录且 `look_ahead_per_shard="16.0 MiB"`；不带该选项的并行导出与 `--threads 1 --batch-size 5000`（真正生效的路径）出现次数均为 0。顺带删除死常量 `config.DefaultMaxAllowedPacketCap`（全仓库仅定义处一处；`--statement-size` 的上界在参数校验里独立成立，导入侧的包上限另有 `batchcap.go` 的 4 MiB 保守值）。新增 `TestParallelExportPipelineOptionNotice`（无库依赖）与 `TestLive_ParallelExportReportsIgnoredPipelineOptions`（真实库三段式：并行+改动必须出现、并行+默认必须静默、单线程+改动不得报警）；`docs/cli-oracle.txt` 头注登记三处 reword（导出侧有意差异 6→9 条） **注：本行提到的「记一条忽略 info」已被变更记录 64 升级为用法错误（显式传入即退出码 2），库调用方仍只记 info。** |
| 导入侧批次字节预算完全由目标库 `max_allowed_packet` 决定，用户无法主动把批次切小：`--statement-size` 只在导出侧有意义，`import` 根本没有该选项 | 给 `import` 补上同名选项，语义为「单条生成语句的字节上限」：与 `max_allowed_packet × 9/10` 预算取较小值，**只压低不抬高**（抬高等于让一批在服务端中途触发 `error 1153`），默认 `0`=不额外压低，低于 64 KiB 下限的显式数字照原值执行。`applyStatementCap()` 挂在 `batchBudget()` 上，故单线程/并行导入与 DEL/SQL/Parquet 三种输入一并生效；单行大于上限时仍独立成批（不拆行）。参数校验与导出侧刻意分列：导入用 `validateImportStatementSize`（接受 `0`，`<0` 或 `>268435456` 为用法错误退出 2），导出仍用 `validateStatementSize`（拒绝 `0`）。日志：`import batch byte cap resolved` 增 `statement_size` 与 `limiting_factor`（`max_allowed_packet` / `statement-size`）两个字段，`import config` 回显增 `statement_size`。实测（本地 MariaDB，`max_allowed_packet=134217728` → 预算 120,795,955 B；2048 行 × 约 200 B；`--batch-size 20480` 使行数不触顶；目标库 `Com_insert` 差值）：默认 **1** 条语句，`--statement-size 65536` **7** 条（`--threads 4`、SQL 文件、Parquet 文件同样 7 条），`--statement-size 1024` **342** 条，`--statement-size 268435456`（高于预算）仍 **1** 条且记 `limiting_factor=max_allowed_packet`；各次落库行数与 `CRC32` 校验和全部一致（2048 / 4397778945034）。测试：`internal/import/batchcap_test.go` 新增 `TestApplyStatementCap`、`TestBatchBudgetAppliesStatementCapWithoutConnection`（读不到 `max_allowed_packet` 的分支同样压低），`cmd/cli_matrix_test.go:TestCLIImportStatementSize`（导入接受 `0`、导出仍拒绝、两侧共享上界），`test/integration/live_mysql_test.go:TestLive_ImportStatementSizeCapsStatements`（真实库以 `Com_insert` 断言语句数变化并复核落库行）；`docs/cli-oracle.txt` 头注登记导入侧新增该选项（有意差异 4→5 条） **注：本行引的 `max_allowed_packet=134217728`、预算 120,795,955 与 `1024` 档的 342 条是 128 MB 测试床上的值；变更记录 70 把床按 OceanBase V4.x 默认调成 16M（预算 15,099,494）并以 207 B 行宽的 fixture 重测，同一矩阵为 1 / 7 / 410 / 1 条、`CRC32` 和 4463837020299，342 与 410 之差来自行宽而非床。** |
| 配置回显不看 `--format`：三种格式打的是**同一套 26 个字段**（实测把 HEAD 二进制的三条 `-f del/sql/parquet` 回显去掉 `format=` 后逐字节比对完全相同），既包含所选格式根本不读的字段（`-f del` 打 `statement_size`/`no_create`/`extended_insert`，`-f sql` 打 `char_del`/`col_del`/`timestamp_fmt`，`-f parquet` 两类都打），又漏打 DEL 写入器一直在读的 `date_fmt`/`no_escape`/`no_char_del` 三项，`hex_blob` 打的还是 `--hex-blob` 原始标志位而非生效值（非 utf8mb4 自动开启时回显 `false`）；`--help` 也不说这些选项只对某种格式有效 | 建立唯一一张格式适用范围登记表 `internal/config/formatscope.go`（`ExportOptionScopes`/`ImportOptionScopes` + `AppliesTo`/`Violation`），三处共用：① `cmd/validate.go` 把手写的 `validateSQLOnlyOptions`/`validateDELOnlyOptions`+`delOnlyFlags`/`validateParquetBinaryOptions` 合并为一个 `validateOptionScopes`，**拒绝顺序与文案逐字不变**；② `cmd/option_scope.go:annotateOptionScopes` 在两个子命令注册 flag 的末尾给每个受限选项追加范围标记；③ 回显移到 `internal/export/configlog.go`/`internal/import/configlog.go`，按 `AppliesTo` 过滤、补齐漏打的三项，`hex_blob` 改用新抽出的 `effectiveHexBlob()`（原先单线程路径与并行分片各写一遍的重复表达式一并收敛）。实测：`export` 回显 `-f del/sql/parquet` 为 25/20/15 个字段、`import` 为 29/20/20（DEL 布局 9 项只在 `-f del` 出现）；`-f del --charset gbk` 回显 `hex_blob=true` 而 `-f del` 默认 `false`；`--format sql --char-del '!'` 仍报 `--char-del can only be used with --format del` 退出 2；`export --help` 有 16 行、`import --help` 有 9 行带标记。测试：`internal/config/formatscope_test.go`（4 用例：`AppliesTo` 矩阵、`Violation` 文案与首条顺序、登记表自洽性、两侧 DEL 组必须一致）、`cmd/option_scope_test.go`（3 用例：标记进入 `Usage` 与渲染后的 help、九个 DEL 选项不得漏登记、重复调用不重复追加）、`internal/export/configlog_test.go`（4 用例）、`internal/import/configlog_test.go`（3 用例，含导入侧 `statement_size` 不受格式过滤）；`docs/cli-oracle.txt` 头注把标记带来的帮助文本差异登记为有意差异（export 9→21 行、import 5→14 行）；**注：本行提到的 SQL 布局字段 `no_create` 已被下一行的 `--create` 取代** |
| `--query` 写成带 schema 的限定名（形如 db.tbl，两段各带反引号）时 SQL 导出必然失败：表名解析交回的是**已带反引号的限定串**，`SHOW CREATE TABLE` 又把它当裸名包一层反引号并前置连接自己的 `--database`，服务端把整串当作默认库里的单个标识符（HEAD 构建的二进制实测退出 1：`Error 1146 (42S02): Table 'imex_test.imex_test.zzq_evi' doesn't exist`）。另有两点不便：SQL 导出默认总带 `CREATE TABLE`，想只要数据没有开关；长 SQL 只能写在命令行上 | 三项：① 表名解析统一交回**裸表名**（`ExtractTableName`/`SimpleTableName` 断言 `sqlparser.TableName` 后取其 `Name`），新增 `ExtractTableRef()` 把 schema 与表名拆开、只供 `SHOW CREATE TABLE` 使用（`getCreateTableStatement(schema, tableName)`，schema 为空不加前缀），单线程与并行分片两条预取路径同一口径——产物里的 `CREATE`/`INSERT`/`LOCK TABLES` 因此永远引用裸表名，同一个文件可以导进任意库；分片键探测本就按 `TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?` 过滤，带前缀反而匹配不到主键。② SQL 导出改为**默认仅数据**，新增 `--create` 显式带回建表，并**删掉 `--no-create`**（结构已是 opt-in，反向开关唯一的作用就是否决 `--create`，留着只多一个状态组合；传入它现在报 `unknown flag: --no-create` 退出 2），该选项登记为 SQL 专属（`--create -f del` 与其它格式越界同口径退出 2），配置回显由 `no_create` 改打 `create`；要用 `import --mode create` 建表，导出就必须加 `--create`。③ 新增 `--query-file` 从文件读 SQL（首尾空白与行尾分号自动去掉），与 `-q` **互斥**：两者都给、两者都不给、文件为空、文件不存在均为用法错误退出 2，回显只在真正走文件时打 `query_file`。实测（本地 MariaDB，HEAD 与工作区两个二进制跑同一条命令）：工作区默认导出退出 0、产物 `CREATE TABLE` 0 行 / `INSERT` 3 行；`--create` 退出 0、产物首块即该表的建表语句、全文件库名出现 **0** 次；删掉源表后用 `import -f sql --mode create` 回灌同一文件 `inserted=3` 且 `COUNT(*)=3`；`--no-create` 无论单独给还是跟着 `--create` 给，都是 `unknown flag` 退出 2。测试：新增 `TestSQLParser_ExtractTableRef`，`TestSQLParser_SimpleTableName` 的限定名期望由 `db.users` 改为 `users`（钉住缺陷本身），`option_matrix_test.go` 的 `TestOptionMatrixNoDataNoCreate` 重写为四个子用例（默认仅数据可导入、`--create` 结构+数据、`--create --no-data` 配 `--mode create`、默认仅数据配 `--mode create` 必须失败），新增 `test/integration/export_cli_test.go`（4 用例：schema 限定查询导出回灌、`--create` 默认矩阵、`--query-file` 正例与四条负例、parquet 走 `--query-file`）；`docs/cli-oracle.txt` 的 `## export` 段按新帮助文本整体重新基线（实测逐字节相同） |
| 工程名 `gomydump` 与工具定位（导入 + 导出）不再贴合，且各处叫法混用：模块名、二进制名、环境变量、帮助与日志回显、文档、测试库名与表前缀（实测表前缀还写成 `gmydump_it_`，少一个 `o`）不完全一致 | 全线改名为 **imex**（ImEx = Import + Export）：`go.mod` 模块路径 `github.com/imex`，命令词与二进制 `imex`，环境变量前缀 `IMEX_`（`IMEX_KEY`、`IMEX_QUIET`、`IMEX_LOG_FORMAT`、`IMEX_ERROR_STACK`、`IMEX_ECHO_SQL_VALUES`、`IMEX_NO_SQL_TAG`），SQL 归因注释 `/* imex:<commit> */`，启动横幅 `imex starting`，版本行 `imex version 0.1.0`，`Makefile` 的 `BINARY` 与 ldflags 注入路径、`Dockerfile` 的 `ENTRYPOINT ["/app/imex"]`，集成测试改用 `IMEX_TEST_DSN`、测试库 `imex_test` 与表前缀 `imex_it_`。归因注释**只写不读**（导入侧不反向识别它），旧导出文件里的 `/* gomydump:... */` 仍按普通 SQL 注释忽略，**旧产物照常导入**；本轮无 flag、退出码、文件格式变更，也不留旧名兼容层。实测：`imex version` 输出 `imex version 0.1.0`，`--log-level debug` 首条记录 `INFO imex starting ... command=export`，MariaDB general log 收到 `/* imex:9389a88 */ SHOW CREATE TABLE ...` 与 `/* imex:9389a88 */ SELECT ...`；代码、`Makefile`、`Dockerfile`、`go.mod` 对 `gomydump`/`GOMYDUMP`/`gmydump` 三种写法命中 **0**（文档里仅剩本条与需求文档变更记录 56 对旧名的引述）。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七包全通过，真实库 `./test/integration` 104 条 PASS（含子用例）、0 SKIP，测试资产数量不变（39 个 `_test.go`、201 个 `Test*`、6 个 `Fuzz*`、12 个 `Benchmark*`）。同日收尾把仓库外也统一：工作副本目录名与产物前缀一并改为 `imex`（发布与保留脚本同时识别改名前的 `gomydump-` 前缀，否则旧批次清不掉），仓库内旧名产物 `bin/gomydump*` 与 `dist/gomydump-*` 删除。详见需求文档变更记录 56、57 |
| 导出侧「拒绝覆盖已有输出文件」的选项名 `--no-replace` 与语义不符：它不涉及 REPLACE，却与 `--format sql` 的 REPLACE 语句、`import --mode replace` 三个 replace 撞名 | 改名为 `--no-overwrite` 并全线同步（flag 注册名、配置字段 `NoOverwrite`、配置回显键 `no_overwrite`、报错文案、集成用例名、README 与需求文档选项表、`cli-oracle.txt` 的 `## export` 段重新基线）；不留别名——实测旧名报 `unknown flag: --no-replace` 且退出 2，行为不变——实测带该选项命中已存在文件退出 1 且既有文件原样存活，不带则正常覆盖。详见需求文档变更记录 58 |
| 口令加密的密钥派生是单次 `sha256(IMEX_KEY)`，无盐、无工作因子：实测拿到一条 `ENC:` 密文就能以 **939,822 次/秒** 离线尝试密钥候选（GCM 标签可当场验真伪），且同一密钥下所有密文共用同一把派生密钥 | 改为 stdlib `crypto/pbkdf2`（HMAC-SHA256、600000 次迭代、go 1.26.10 内置无新依赖）+ 每条密文独立的 16 字节随机盐，密文布局 `salt ‖ nonce ‖ 密文+标签`。按用户口径不加版本位、不兼容旧串：旧 `ENC:` 一律报 `wrong key or corrupted data` 并退出 1，且实测不创建输出文件。代价为每次进程一次性约 0.3 s（实测带密文口令连真实库导出 0.35–0.42 s，明文口令 0.00 s）。新增 `internal/common/password_test.go`（9 个用例，此前该路径零测试）。详见需求文档变更记录 60 |
| `--extended-insert` 配一个低于单行语句宽度的 `--statement-size` 时静默退回逐行布局：合并判定是「当前批非空且加入下一元组将超上限才 flush」，于是每条语句永远只有 1 个元组，产物与默认单行布局逐字节相同，用户要求的合并一条也没发生却**零告警**（同一尺寸下默认单行布局反而会告警一次） | `internal/export/sql_writer.go` 的写入器补记两个状态——批量化布局累计的行数，以及是否曾把两个元组放进同一条语句；`WriteFooter` 在累计行数达到 2 且从未合并时记一次 WARN（`--extended-insert never merged rows: every row alone exceeds --statement-size, so the dump is one INSERT per row`，带 `statement_size` 与 `rows`），两种布局的告警由此对称，单行布局原有的「单行超限只告警一次」不变。单行的产物无法再拆，因此这里只出声、不改写行为。**实测**（本地 MariaDB，`bench_lay` 20 万行 3 列，`-f sql`）：`--extended-insert --statement-size 64` 产物 17,224,453 B / 200,000 条语句，与默认单行布局**逐字节相同**（md5 `01cfcd0ba049a7194be925f1a490e389`），改动前告警 **0** 条、改动后 **1** 条；`--statement-size 400` → 8,690,869 B / 22,217 条（无告警），`1000` → 8,015,845 B / 8,154 条，默认 `8388608` → 7,624,501 B / 1 条（体积 -55.8%，gzip 后 1,859,386 B 对 1,677,322 B）。同一份数据导入：单行文件 766 ms 对合并文件 814 ms（差异在噪声内），即文件布局不影响导入速度（`internal/import/sql_reader.go` 把元组拆回行后按 `--batch-size` 重新攒批），`--extended-insert` 的收益只在体积与第三方工具直接消费（例如管道给 `mysql` 客户端）。`--batch-size` 侧同样复核过：导入 100/200/500/1000/5000/10000/21845 分别为 1022 / 901 / 821 / 813 / 739 / 756 / 807 ms（20 万行逐次重导，每次行数都对得上），500 以上进入平坦区，只有病态小值（100）慢约 38%，默认 1000 无需调整；导出侧 1000/50/50000 为 109/114/166 ms，`--threads 4 --batch-size 50000` 为 122 ms 且该路径根本不读这两个选项（会记一条忽略 info）。测试：`internal/export/sql_writer_test.go` 新增 `captureWriterWarnings`（把 `common.Logger` 临时换成 `slog` 的 warn 级内存 handler）与 4 个用例——合并不可能的尺寸必须恰好 1 条合并告警且产物逐行、默认尺寸真正合并必须静默、单行文件不得误报合并失败、单行布局仍只告警一次且不出现合并告警；`test/integration/option_matrix_test.go:TestOptionMatrixSQLBatching` 增加`--extended-insert --statement-size 64` 退回逐行布局的产物形态断言 **注：本行末的「该路径根本不读这两个选项（会记一条忽略 info）」同被变更记录 64 改为显式传入即用法错误、退出码 2。** |
| `--single-row-insert` 自第 47 项把「每行一条 INSERT」变成默认布局后就是纯 no-op：`legacySingleRowInsert` 由 cobra 写入后全仓库无人读取，唯一读它的 `validateInsertLayout` 判的仍是它自己与 `--extended-insert` 的互斥 | 删除该选项，不做兼容层（与 `--no-create` 直接删除、`--no-replace` 不留别名同口径）：`cmd/export_flags.go` 的死变量与注册、`cmd/validate.go` 的 `validateInsertLayout` 及其调用点、`internal/config/formatscope.go` 的 `sqlOnly("single-row-insert")` 登记四处同时消失，SQL 专属组由五项减为四项，`--help` 的范围标记也少一行。内部 API `SQLWriter.SetSingleRowInsert` **保留**——它由 `internal/export/exporter.go` 按 `!cfg.ExtendedInsert` 真实驱动默认布局，与同名 CLI 选项不是一回事。**实测**：`imex export ... -f sql --single-row-insert` 报 `unknown flag: --single-row-insert` 且退出 **2**；与 `--extended-insert` 同时给出同样退出 **2**（文案由互斥改为未知选项）；`imex export --help` 与 `docs/cli-oracle.txt` 的 `## export` 段重新基线后 diff 为空（只少那一行，最长名仍是 `--socket-*-timeout`，列对齐未变）；全仓库 `single-row-insert` 在代码与构建脚本中命中 **0**，只剩「已删除选项是未知选项」这一条用例名与该用例的 argv。测试：`cmd/cli_matrix_test.go` 删掉旧选项合法 / 与 `--extended-insert` 互斥 / 配 `--format parquet` 越界三条矩阵用例，改为一条「已删除选项即未知选项」，parquet 越界的覆盖转由 `--extended-insert` 承担，`TestCLIInsertLayout` 去掉废弃项段落；`internal/config/formatscope_test.go` 的 `AppliesTo` 矩阵把该行换成 `extended-insert` 两行（SQL 为 true、parquet 为 false）；`test/integration/roundtrip_test.go` 的注释不再点名已删除的选项；`test/integration/option_matrix_test.go` 的旧选项用例换成上面那条退化布局用例。文档：README 导出标志表与参数校验表各删一行、SQL 输出说明补删除理由与未知选项后果、关联校验段更新 SQL 专属清单；requirements.md 选项表与适用性段、DB2 映射行、选项演进表第 4 行的注、10 节测试场景行、11.1 三行计数、`docs/cli-oracle.txt` 头注（新增 Re-baselined note 3，nine DEL plus five SQL / fourteen marked 改为 nine plus four / thirteen）同步。测试资产 40 个 `_test.go` / 214 个 `Test*`（`internal/export` 70→74），6 个 `Fuzz*`、12 个 `Benchmark*` 不变 |
| 并行导出的分片边界只按 `MIN`/`MAX` 等宽切，三处后果都有实测代价：① 分片首列非数值（CHAR/VARCHAR/UUID 主键）时**整表降级为单线程**并记一条 `only 1 shard range` 告警，`--threads 8` 也一样；② 稀疏或倾斜的整数键被切成空段与巨段——1M 行、`id` 跨度 1…2 亿的表在 `--threads 4` 下切出 200,000 / 0 / 0 / 800,000 行，两个 worker 全程空转、最慢段占全表 80%；③ `--threads` 与段数 1:1，收尾段决定总时长。另有两处静默：`export --threads>1` 显式给出的 `--batch-size`/`--pipeline-buffer` 只记一条 info 就丢弃；并行导入的断线重放字节上限是**每 worker** 64 MiB，`--threads 4` 实测峰值 RSS 208 MB 几乎全是重放缓冲 | 分片边界改为采样，段数与线程数解耦，两处静默改为拒绝或按进程计。① **采样**（新文件 `internal/export/shard_sample.go`）：读 `INFORMATION_SCHEMA.TABLES` 的 `TABLE_ROWS`/`AVG_ROW_LENGTH` 估行数，再以 `WHERE 键 > ? ORDER BY 键 LIMIT 1 OFFSET 每段行数-1` 逐段定位（边界走绑定参数、偏移内联为整数，因此不需要窗口函数、不依赖 MySQL/OceanBase 特有语法），非数值键同样可切；两条捷径直接走等宽——整数键且 `TABLE_ROWS × AVG_ROW_LENGTH < 256 MiB` 的小表不花探针，值域跨度 ≤ 行数 × 3/2 的可证密度表也不花探针（判据用 `big.Rat` 精确比较，`BIGINT` 满跨度不误判）；统计缺失、查询报错、取到 NULL、段数不足一律 WARN 回落原行为，绝不因采样而失败。实测：字符串主键表 `--threads 4` 由 1 段变 8 段（7 次探针 + 1 条统计查询），稀疏整数表 8 段为 123,749×7 + 133,757 行，最慢段由 800,000 降到 133,757；稠密小表边界与等宽**完全一致**，代价只是那 1 条统计查询。② **产物不变**是硬约束：DEL 与默认单行 SQL 下 3 张表 × `--threads 1/4/8` 与改动前二进制 md5 全同，只有 `--extended-insert` 每边界多一条 INSERT 头（实测 +192 B / +384 B）。③ **段数与 worker 解耦**：`min(2×threads, 64)` 段由恰好 `threads` 个 worker 经 `atomic` 游标**按序领取**（乱序领取会与按段序消费写出侧死锁）；64 MiB 前瞻预算改为按段均分，实测 `--threads 8` 每段 4 MiB 无可测损失（0.86 s 对 0.91 s，差额即探针成本）。④ **两处静默**：`cmd/validate.go:validateParallelExportOptions` 以 `cmd.Flags().Changed()` 判定，显式给出即 `common.Usagef` 退出 2（给默认值同样拒绝，因为「看起来被采纳」才是问题），`import` 侧不受影响，库调用方仍收 info；`internal/import/session.go:replayBudget` 把 64 MiB 改为进程预算按 `--threads` 均分、下限 8 MiB。⑤ **一条实测否决**：原拟把 `--commit-count` 默认值抬到 20000，对跑 3 次取中位数后差值 ≤ 0.1 s 且小于组内抖动（`--threads 4` 窄表 3.56 对 3.66 s，单线程 168 B 行表 8.52 对 8.47 s），故**不改默认值**、导入 CLI 无需重新基线。⑥ **测试**：新增 `internal/export/shard_sample_test.go`（11 个 `Test*`，进程内假驱动按查询形态应答，断言探针 SQL 形状、绑定参数链、跳过重复边界、稠密小表零探针、5 类失败回落、NULL 行仍被覆盖、复合键拒绝采样）与 `cmd/cli_matrix_test.go` 的 `TestCLIParallelExportRejectsPipelineOptions`（9 用例，含默认值也拒、非法值先报、必须 `common.IsUsageError`）、`TestCLIImportKeepsPipelineOptionsWithThreads`，`internal/import/session_test.go` 3 个用例钉住预算均分与自身上限触发提交；`test/integration/option_matrix_test.go` 新增 `TestOptionMatrixTextShardKeyParallelExport`（240 行 md5 十六进制 CHAR(32) 主键，串行与 `--threads 4` 的 DEL/SQL 产物互比、日志不得出现 `only 1 shard range` 且须有 ≥2 条分片完成记录、并行产物回灌后按键序逐行比对），并把 `TestOptionMatrixParallelOutput` 的头注与上界改为按段数表达（`extra ≤ 2×threads-1`）。⑦ **文档**：`docs/cli-oracle.txt` 的 `## export` 段按新帮助重新基线（逐字节相同，新增 Re-baselined note 4 记录「忽略 info → 用法错误」）；README 并行导出说明、流水线生效范围、参数校验表、关联校验段、并行导入与批量导入控制各按上述改写。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过，真实库 105 条 PASS（38 顶层 + 67 子用例）、0 SKIP、0 FAIL，`statementCapSlack` 标记 = 3。详见需求文档变更记录 64 |
| 参数边界审计查出七项，代价都有实测现场：① 并行导入的 worker 报错时进程直接崩溃（读端 goroutine 在 `errCh` 关闭后仍向其发送，`panic: send on closed channel`，退出码 **2** 掩盖了真实的 `1062`，日志半截、部分行已提交）；② 导入默认字节预算只看服务端 `max_allowed_packet`，本机 128 MB 给出 120,795,955 B，而 go-sql-driver 的客户端插值只到 64 MiB，超过就静默退回服务端预处理语句（同一份 80 MiB 数据 2.078 s 对 1.195 s）；③ `--pipeline-buffer` 的单位是批数、单批可达整个预算，于是队列驻留没有上界（200 MB / 10 万行文件在深度 1024 下峰值 RSS **280,288 KB**）；④ `--progress-interval` 只要求为正，`1ns` 合法——一次 6,400 行导出记 **1,355 条**进度、日志 175,557 字节；⑤ `--threads` 的静态范围（1–256）不知道目标库：`@@max_connections=151` 时 200 个 worker 会连不上并把整跑失败在半途，重放缓冲又是**每 worker** 一份（256 线程实际索要 2 GiB），两处都无人出声；⑥ DEL 的 `--col-del` 与 `--row-del`（及 `--char-del`/`--escape` 之间）可以取同一个值，200 行导出写出 14,501 B、**0 个行分隔符**、402 个逗号的文件，同参数回读读出 401 行、**落库 0 行**；⑦ 显式 `--shard-key` 指向不存在的列时只记一条 warn 就回落单线程，跑完还报告成功 | 七项全修，原则是「默认值按机理解释清楚、用户显式写下的数字只出声不改写、结果错误而非只是慢的取值改为拒绝」。① 读端纳入同一个 `WaitGroup`（`internal/import/importer.go`），通道只能在两阶段都结束后关闭；② 新增 `driverInterpolationCap`/`driverBatchBudget()` 与纯函数 `batchCeilings()`（`internal/import/batchcap.go`），默认预算取 `min(服务端预算, 64 MiB × 9/10)` = **60,397,977 B** 并在记录里点名 `limiting_factor=driver_packet_cap`，显式超限值原样执行另记一条带 `driver_ceiling_bytes` 的 warn；③ 新增 1 GiB 队列包络（`effectiveQueueDepth()` + `Importer.queueDepth`，单线程 `rowCh` 与并行 `batchCh` 都改用它），实测下调为 `requested=1024 effective=17` 后峰值 RSS **112,584 KB**、耗时 4.11 s 不变，默认深度 10（80,480 KB）永不被下调；④ 新增 `config.MinProgressInterval = 10ms` 与 `validateProgressInterval`（两侧共用），0 与负值报 `must be > 0`、低于下限报 `must be >= 10ms`，均为退出 2；⑤ 新增 `common.MaxConnections`，导入与导出在数据移动前各比较 `threads+1` 与 `@@max_connections` 并记 warn（`connections_needed=201 max_connections=151`）、并行导入再报重放窗口包络（`total_bytes=2147483648` 对 `budgeted_total_bytes=67108864`），只出声不拒绝——DBA 现场调高上限后运行仍然正确，同时纠正 `session.go` 里与实现不符的注释；⑥ `validateDelimiters` 重写为固定顺序 char→col→row→escape 的五对冲突表（此前遍历 map，同一条命令可能报出不同冲突），`--no-char-del`/`--no-escape` 各自免除相关项、`--escape` 与 `--char-del` 同值这条默认组合仍合法，命中即退出 2 并点名两个选项；⑦ `internal/export/exporter.go` 在 `ShardKey != ""` 时把检测失败包成 `common.Usagef`（退出 2、点名选项与表名），自动检测失败照旧回落单线程。**测试**：新增 `internal/import/batchcap_ceiling_test.go`（5 个纯函数用例，含「显式值大过服务端预算时不能反过来绕过钳制」）、`cmd/validate_boundary_test.go`（1 个用例 × 11 组取值 × 两条命令）、`test/integration/boundary_test.go`（6 条真实库边界用例，失败路径重跑三次断言退出 1 且 `panic:`/`send on closed channel`/`goroutine ` 命中为 0）；`cmd/cli_matrix_test.go` 的间隔矩阵与 `test/integration/live_mysql_test.go` 的两处断言按新契约改写（`limiting_factor` 现读服务端值，不写死本机）。**契约面**：三行帮助文本改写（两侧的 `--progress-interval`、import 的 `--pipeline-buffer` 与 `--statement-size`），`## export` 按新帮助重新基线后 diff 为空（87 行逐字节相同，新增 Re-baselined note 7），`## import` 有意 delta 十五行改为十六行；三类此前被接受且结果错误的取值改为退出 2，并行导入数据失败由错误的 2 修正为契约里的 1；文件格式、`IMEX_*` 环境变量不变；发往服务端的语句仅导入默认批次边界收窄（同一文件实测语句条数与 `CRC32` 落库校验和不变），`--threads>1` 多一条 `SELECT @@max_connections`。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过，真实库 **112 条 PASS（45 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**，`statementCapSlack` 标记 = 3。详见需求文档变更记录 67 **注：本行 ③ 的「1 GiB 队列包络下调深度」已被变更记录 69 撤掉（那是把 `--pipeline-buffer` 换算成内存数字），本行引的 RSS 对照值（1024 → 280,288 KB、下调后 17 → 112,584 KB）随下调逻辑一并失效、不可复现；同一轮 ② 的驱动插值钳制保留不动，另按 69 撤掉了 9/10 预算下面那条 64 KiB 下限。** |
| `--shard-key` 的生效面与回显面不一致：交叉测试实测 `--threads 1 --shard-key nosuchcol` 退出 0、产物与不带该选项逐字节相同（111,692 B，md5 `bc78091aa71e…`），配置记录却照样回显 `shard_key=nosuchcol`，`--threads 1 --shard-key '!!!'` 同样回显；`--threads 4` 配 JOIN 回落后也是退出 0 且键无人读取；而同一个非法键在 `--threads 2` 下退出 2——合法性取决于运行走了哪条路，日志说的却是另一回事 | 按「出声」而非「拒绝」修（拒绝会把此前返回 0 的调用改成 2，属不兼容变更）：① `exportSingle` 入口新增 `warnIfShardKeyIgnored`，它是全部单线程路由（`--threads 1`、复杂特性、表名解析失败、检测失败、只切出一段）的唯一汇聚点，实测 `WARN single-threaded export ignores --shard-key shard_key=… threads=… fallback=… hint=…`；② 配置记录仅在写时就确定用不上（`threads<=1` 且给了键）时追加 `shard_key_ignored=true`，回落路径不出字段以免出一条会被推翻的断言；③ `--shard-key` 帮助文本改写为"只有并行导出读取并校验它"。退出码、产物、发往服务端的语句均不变。测试：新增 `internal/export/shardkey_ignore_test.go`（2 用例：五组路由 + 三种格式的记录字段）、`test/integration/boundary_test.go:TestBoundarySingleThreadedExportNamesAnIgnoredShardKey`（512 行表上跑三条路由并断言字段与退出码）；`TestBoundaryExplicitShardKeyIsRefused`、`TestOptionMatrixTextShardKeyParallelExport` 未改一行仍通过。契约面：`docs/cli-oracle.txt` 新增 Re-baselined note 8、`## export` 按新帮助重新基线（实测 diff 为空，87 行逐字节相同）、`## import` 有意 delta 仍十六行；无 flag 增删、无 env 变化。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全过、真实库 **113 条 PASS（46 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**。同一轮交叉测试另查出两项未修、待定级：1 GiB 队列包络可把 `--pipeline-buffer` 压到低于 `--threads`（`--threads 32` 实测 `requested=1024 effective=17`，与本文「并行导入建议 ≥ `--threads`」矛盾）；导出侧交接队列仍按批数定深、无字节包络。详见需求文档变更记录 68 **注：本行末尾两项已由变更记录 69 处置——队列包络不再改写深度、只出声报价，本行引的那句「并行导入建议 ≥ `--threads`」随下调逻辑一并撤掉（队列由全部 worker 共享，深度是驻留、线程是并发，两者正交）；导出侧那条按同一原则保持"按给定值执行"，只补文档不改代码。** 同一轮末尾缺口 C 引的 140,416 KB / 150,464 KB 也取自那张 128 MB 床（变更记录 70 之后本床为 16M）。** |
| 用户指令「`--threads` 是并发、`--pipeline-buffer` 是队列深度，两者并不矛盾，不要做转换，要根据用户输入来。其他参数也是类似」：审计上一轮加的两处换算，它们都把一个用户数字改写成另一个用户数字，而队列实为全部 worker 共享（`internal/import/importer.go` 单读 goroutine 填一条共享 `batchCh`，worker 消费），深度只决定驻留批数 | 原则改为「协议/服务端事实保留，用户选项之间的换算撤掉」：① **撤 1 GiB 队列夹紧**——`effectiveQueueDepth()` 删除，新增纯函数 `queuedBytes(depth, batchBytes)`（饱和到 `MaxInt64`），`resolveQueueDepth` 照给定深度执行、只在超线时记 `warn`（`the hand-off queue may retain more than the memory envelope, importing at the configured depth`，字段 `requested=1024 batch_bytes=60397977 queued_bytes=61847528448 envelope_bytes=1073741824` 加出路 hint），负深度按 `0` 处理而不是让 `make(chan, -n)` panic；实测 `--threads 32 --pipeline-buffer 1024` 记录回显 `pipeline_buffer=1024`、`lowering` 命中 0、退出 0、5,000 行落库。② **不加线程下限**——曾拟把深度压到不低于 `--threads`，那是同一个毛病的反方向，未实现。③ **撤 64 KiB 字节预算下限**（`batchByteLimit` 现为纯 `packet×9/10`）：下限会把预算抬过服务端刚说出的值，实测 `max_allowed_packet=40000`（生效 39936）配 `--batch-size 20480` 时旧实现 `batch_bytes=65536` → 退出 1、`Error 1153 (08S01): Got a packet bigger than 'max_allowed_packet' bytes`、`failed=2870`、0 行落库且记录误写 `limiting_factor=max_allowed_packet`；改后 `batch_bytes=35942` → 退出 0、5,000 行全部落库。④ **其余上限按性质分类**：`max_allowed_packet×9/10`（服务端事实）、`maxPlaceholders 65535`（协议 2 字节参数计数，压行数并出声）、驱动 64 MiB 插值上限（只钳默认值，显式值原样执行另记 warn）、并行导出 `min(2×threads,64)` 段数计划与按段均分的重放预算（内部规划，不读用户的批数/深度）——都不算换算；导出侧交接队列同样按给定深度执行、不设字节包络（缺口 C 按此原则只补文档）。测试：`internal/import/batchcap_ceiling_test.go` 的 `TestEffectiveQueueDepth`/`TestResolveQueueDepthReportsReduction` 改为 `TestQueuedBytesIsThePriceOfTheDepth`（7 组含饱和）与 `TestResolveQueueDepthHonoursTheDepthAndReportsItsPrice`（断言 `queueDepth==1024`、六个字段齐全、默认深度静默、`-1→0`），`TestBatchByteLimit` 换掉两条下限用例并钉住 `65536→58982`/`1024→921`，`test/integration/boundary_test.go` 的 `TestBoundaryPipelineBufferIsLoweredToBoundQueuedBytes` 改为 `TestBoundaryPipelineBufferIsHonouredAndPriced`（`pipeline_buffer=1024` 回显 + 无 `lowering` + 用 `logField` 从本次运行读回 `batch_bytes` 再断言 `queued_bytes`）。契约面：仅 import 的 `--pipeline-buffer` 一行帮助文本改写，无 flag 增删、退出码变化、文件格式变化、env 变化；发往服务端的语句只在一种情形变化——此前被包络压低深度的组合现在真按用户深度跑（批次边界本身不变，`batch_bytes` 仍由服务端与驱动定），以及此前服务端值 ≤ 72,817 B（9/10 预算不足 64 KiB）的库上、下限把预算抬到高于用户库实际包上限的场景改为按真实预算成批。`docs/cli-oracle.txt` 的 `## export` diff 仍逐字节为空（87 行），`## import` 有意 delta 仍十六行（`--pipeline-buffer` 那行内容更新、行数不变），新增 Re-baselined note 9 并声明 note 7 对该行的描述已被取代。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过，真实库 **113 条 PASS（46 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**，`statementCapSlack` 标记 = 3。详见需求文档变更记录 69 **注：本行的 `batch_bytes`/`queued_bytes`/RSS 数字是在 128 MB 的 `max_allowed_packet` 床上取的；变更记录 70 把床按 OceanBase V4.x 默认调成 16M 后已重新实测并改写正文数字。** |
| 测试床的 `max_allowed_packet=128M` 是这台沙箱自己的值，OceanBase 租户从来不是这个数：官方文档里 V2.1.x/V2.2.x/V3.1.x/V3.2.x 的租户变量默认值是 4,194,304，V4.x 的调优文档写的是 16,777,216，诊断文也承认「有的版本为 4M，有的版本为 16M」。用 128 MB 测出来的 `batch_bytes=120,795,955 → 钳到 60,397,977`、`limiting_factor=driver_packet_cap`、深度 1024 峰值 RSS 最高 292,356 KB 这些数字，拿到真实 OB 租户上一条都不成立，而 README/需求文档/oracle 里引用它们的句子有十几处 | 按用户指令「b 16m」：**代码逻辑一行不改，把测试床按 OB V4.x 默认设成 `max_allowed_packet=16M` 并重新实测，正文数字全部换成 OB 租户上真正会出现的那一组**。① 床：`/tmp/mysqld/my.cnf` 改 `max_allowed_packet=16M` 后重启，`SELECT @@global.max_allowed_packet` = 16777216，`@@max_connections` = 151。② 预算：默认预算变成 `batch_bytes=15099494 limiting_factor=max_allowed_packet`——驱动侧 64 MiB 插值上限在本床**根本轮不到**，因此给"谁在决定"补了一条边界实测：服务端 67,108,864 B（恰为 64 MiB）时预算 60,397,977 仍记 `max_allowed_packet`，67,109,888 B 时才改记 `driver_packet_cap`；这条钳制只在服务端值 > 64 MiB 时生效，故 128 MB 床上的历史数字仍作为「那种床上会发生什么」保留。③ 报价线：`⌊1 GiB / 15,099,494⌋ = 71`，实测深度 71 静默、**72 起出声**（`queued_bytes=1087163568`），深度 1024 记 `requested=1024 batch_bytes=15099494 queued_bytes=15461881856 envelope_bytes=1073741824`；默认深度 10 的名义驻留从 576 MiB 降到 144 MiB。④ 内存：200,688,900 B / 100,000 行、`--threads 32` 各重复三轮——深度 1024 峰值 RSS **76,616–152,040 KB**（3.78–5.07 s），深度 10 为 **122,740–149,412 KB**（4.45–4.83 s），两组重叠；结论比 128 MB 床更清楚：预算收窄到 15 MB 之后，深度不再决定峰值内存，因为单个批次就小了，读侧供不满那么深的队列。旧对照（280,288 KB / 下调后 112,584 KB / 本轮早些时候的 147,340–292,356 KB）全部标注为 128 MB 床的历史测量并停止引用。⑤ `--statement-size` 矩阵重测（新 fixture 424,882 B / 2,048 行 × 约 207 B，`--batch-size 20480`，`Com_insert` 全局计数差）：不加 1 条、`65536` 7 条（`--threads 4`、SQL 输入、Parquet 输入各 7 条）、`1024` **410** 条、`268435456` 1 条且 `limiting_factor=max_allowed_packet`；四种落库 2,048 行、`CRC32` 和 4463837020299 全同。342 → 410 的差来自行宽（每批 6 行 vs 5 行），不是床变了，正文写明。⑥ 真实库用例修好一条：`TestBoundaryExplicitStatementSizeAboveCeilingIsHonouredAndReported` 在 16M 床上**必须跳过**（它要求服务端预算能容下 100 MiB 这个显式值），1 SKIP 会让"全绿"失去意义，故改写为 `TestBoundaryExplicitStatementSizeIsLoweredOnly`——同一张表先跑「预算内的显式值原样生效且不出驱动告警」（`packetBudget/4`，本床 3,774,873），再跑「CLI 接受的最大显式值 268435456 绝不高抬预算」，后者按本床实际决定者分三支断言（`packetBudget > 268435456` / `> 60,397,977` / 其余），大床走第一支、128 MB 走第二支、OB 默认床走第三支，谁都不必 skip；两次运行用两张表，否则第二次会撞 `1062`。断言全部从 `SELECT @@max_allowed_packet` 现读，不写死本机。⑦ 文档：README 与需求文档正文的每处 `batch_bytes`/RSS/语句条数按新床改写或标注来源床，oracle 的 `## export` diff 仍逐字节为空、`## import` 有意 delta 仍十六行（**没有任何帮助文本变化**），新增 note 10 说明本轮只换测量环境并给出新记录取值。验证：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过（`cmd` 31.072 s、`internal/common` 54.353 s，末次实跑），真实库 **113 条 PASS（46 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**，`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3。契约面五面全不变：这是测量环境与其产出的文档数字，`max_allowed_packet` 从来不是 imex 的选项；兜底常量 `defaultMaxAllowedPacket = 4 MiB` 也不改，它等于 OB V2/V3 的默认值，是"读不到值时"最保守的那个假设，且检测服务端类型走的就是同一条 `SHOW VARIABLES`，被屏蔽时工具无从知道对面是哪个 OB 版本。详见需求文档变更记录 70 |

## 项目结构

```
imex/
├── main.go                    # 程序入口
├── main_test.go               # 日志消息风格静态守卫（go/ast）
├── cmd/
│   ├── root.go                # 根命令和版本命令
│   ├── export.go              # export 子命令（RunE 与参数校验）
│   ├── export_flags.go        # export 子命令 flag 注册
│   ├── import.go              # import 子命令（RunE 与参数校验）
│   ├── import_flags.go        # import 子命令 flag 注册
│   ├── validate.go            # 共享参数边界校验（port/threads/statement-size/log-level/分隔符解析/格式适用性）
│   ├── option_scope.go        # 把格式适用范围写进 flag 自己的帮助文本（与拒绝逻辑同一张表）
│   ├── cli_matrix_test.go     # 参数组合矩阵与退出码契约测试
│   ├── validate_fuzz_test.go  # 校验函数 fuzz 测试
│   └── encrypt.go             # encrypt 子命令（密码加密）
├── internal/
│   ├── config/
│   │   ├── config.go            # 配置定义与默认值（连接/超时/字符集/DelModifier/导出导入选项）
│   │   └── formatscope.go       # 格式适用范围登记表（校验拒绝文案、帮助文本标记、配置回显字段三处共用）
│   ├── common/
│   │   ├── util.go              # DSN 构建（超时/字符集/会话变量注入）与工具函数
│   │   ├── colkind.go           # 列类型分类（DEL 文本修饰与回转换、二进制列判定）
│   │   ├── sessionvars.go       # 会话变量解析与 DSN 注入（--session-var）
│   │   ├── sqlpreview.go        # 失败 SQL 脱敏回显（--echo-sql-values）
│   │   ├── sqltag.go            # 运行时 SQL 归因注释 /* imex:<commit> */（--no-sql-tag）
│   │   ├── dberr.go             # 瞬态错误判定（连接级 MySQL 错误码/网络错误/文本兜底）
│   │   ├── retry.go             # 重试策略（次数、指数退避、抖动、ctx 取消）
│   │   ├── pool.go              # 连接池老化（ConnMaxIdleTime / ConnMaxLifetime）
│   │   ├── usage.go             # 用法错误分类（退出码 2）
│   │   ├── log.go               # 日志模块（java/text 两种格式、版本信息、启动日志）
│   │   ├── stats.go             # 吞吐统计、速率计算与定时进度计时器（停止时排空在写的记录）
│   │   ├── password.go          # 密码加密/解密（PBKDF2-HMAC-SHA256 派生 + AES-256-GCM）
│   │   └── detect.go            # 服务器类型检测
│   ├── export/
│   │   ├── exporter.go          # 导出调度器（单线程流水线、并行分片与合并、hex-blob 判定）
│   │   ├── configlog.go         # 配置回显（按 --format 过滤字段、hex-blob 取生效值）
│   │   ├── del_writer.go        # DEL 格式写入器（双写/反斜杠转义、字段缓存优化）
│   │   ├── sql_writer.go        # SQL 格式写入器（默认每行一条 INSERT、`--extended-insert` 才按字节攒批、快速路径优化）
│   │   ├── parquet_writer.go    # Parquet 格式写入器（列式存储）
│   │   ├── shard.go             # 分片逻辑（主键/唯一键检测）
│   │   └── sql_parser.go        # SQL 解析器（JOIN 检测）
│   └── import/
│       ├── importer.go          # 导入调度器（单线程流水线、并行 worker、列映射、攒批）
│       ├── configlog.go         # 配置回显（按 --format 过滤 DEL 字段）
│       ├── batchcap.go          # 攒批上限（max_allowed_packet 字节预算 + --statement-size 压低 + 65535 占位符）
│       ├── session.go           # 写入会话（事务、重连回放、空闲提交、commit-count）
│       ├── record_reader.go     # 无上限记录读取器（自定义 SplitFunc，零拷贝别名）
│       ├── del_reader.go        # DEL 格式读取器
│       ├── sql_reader.go        # SQL 格式读取器
│       └── parquet_reader.go    # Parquet 格式读取器
├── test/
│   └── integration/             # 集成测试
│       ├── roundtrip_test.go    # 离线往返（导出写入器→导入读取器：转义/换行/0x/NULL 与空串/单行 INSERT/DEL 修饰符）
│       ├── live_mysql_test.go   # 真实 MySQL 端到端（默认跳过，需 IMEX_TEST_DSN）
│       ├── option_matrix_test.go # 真实 CLI 交叉选项矩阵（argv 驱动编译出的二进制，同上门控）
│       ├── lock_contention_test.go # 重复主键 × 导入并发的锁冲突回归（真实二进制 + 真实库）
│       └── export_cli_test.go   # 导出 CLI 三项：schema 限定查询、`--create` 默认、`--query-file` 互斥
├── docs/
│   ├── requirements.md          # 需求文档
│   └── cli-oracle.txt           # 已发布二进制的 --help 契约快照（回归基线）
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile
└── README.md
```

## 技术栈

- **开发语言**：Go 1.26+
- **MySQL 驱动**：github.com/go-sql-driver/mysql v1.9.3
- **命令行框架**：github.com/spf13/cobra v1.10.2
- **SQL 解析**：vitess.io/vitess（JOIN 智能检测）
- **Parquet 支持**：github.com/parquet-go/parquet-go v0.26.4
- **错误与堆栈**：github.com/pkg/errors（错误链 + 可选调用堆栈，`--error-stack` 开启）
- **日志**：标准库 log/slog

## 许可证

MIT License
