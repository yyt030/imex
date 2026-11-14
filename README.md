# imex

名称取自 **Im**port + **Ex**port（ImEx）。基于 Go 语言开发的表粒度 MySQL 逻辑备份与恢复工具，功能设计参考 IBM DB2 的 EXPORT/IMPORT，同时支持 MySQL 和 OceanBase MySQL 模式。

> **当前版本 `v0.38.1`**（2026-11-14，**纯文档轮**：文档瘦身与历史记录外迁，无功能／选项／产物／错误文案／下发 SQL 变更）。从「特性」到「文件格式示例」描述**当前行为**；逐版「修复记录／功能记录」是**历史版本存档**，全文已外迁至 [`docs/CHANGELOG.md`](docs/CHANGELOG.md)（本文末尾只留最近三版摘要），不代表当前行为。逐版功能记录的末版仍为 `v0.38.0`——`v0.38.1` 无功能变更，故不新增记录节，本轮内容见 `docs/requirements.md` 变更记录 **172**。完整设计理由与实测取证见 `docs/requirements.md` 十四节「变更记录」。

## 目录

**使用文档（当前行为）**

- [特性](#特性) — 能力与三引擎口径
- [安装](#安装) — 源码构建、交叉编译、发布矩阵
- [快速开始](#快速开始) — 最短上手路径
- [命令说明](#命令说明) — 命令、全部选项、环境变量、输出通道
- [高级用法](#高级用法) — 并行分片、列映射、分隔符、口令加密、SQL 回显
- [文件格式示例](#文件格式示例) — DEL / SQL / Parquet 产物样例

**历史版本记录**（v0.6.0 → v0.38.0，共 22 节，**不代表当前行为**）

- [最近三版摘要](#历史版本记录) — 本文末尾
- [全部 22 节全文](docs/CHANGELOG.md) — 逐版修复／功能记录，完整理由见 `docs/requirements.md` 十四节「变更记录」

**仓库信息** — [项目结构](#项目结构) · [技术栈](#技术栈) · [许可证](#许可证)

## 特性

- **多种导出格式**：支持 DEL（定界 ASCII）、SQL 和 Parquet 格式
- **并行分片导出**：基于主键/唯一索引的多线程并行导出，大幅提升大表导出性能；分片结果流式写出，不产生临时文件，磁盘写入量等于输出体积
- **并行导入**：支持多线程并行导入数据
- **Pipeline 模式**：导入导出采用 goroutine → channel → goroutine 流水线架构，提升吞吐量
- **联合主键支持**：完整支持复合主键的并行分片导出
- **复杂 SQL 智能检测**：集成 Vitess SQL 解析器，自动检测 JOIN 查询并降级为单线程导出，单表查询自动分片并行
- **灵活的导入模式**：INSERT / INSERT_UPDATE / REPLACE
- **列映射导入**：支持文件列到表列的自定义映射（DB2 风格），可按位置或按名称映射
- **元数据查询**：`imex list` 只读查看库、表、视图、列、索引、触发器、例程与事件，结果按对齐表格输出，可写 `--output` 文件
- **对象定义导出**：`imex show sch|tab|vws|trg|evt|prc|fcn <名称>` 打印单个对象在服务器上的 `CREATE` 语句原文，只补终止符，不重写文本
- **自定义分隔符**：支持单字符、多字节、十六进制格式
- **无包裹符模式**：支持导入导出不使用字符串定界符的纯分隔符文件
- **OceanBase 兼容**：MySQL 模式共用同一套协议、语句与文件格式，不做服务端类型探测（探测的唯一消费者随 `LOCK TABLES` 一起删除，见变更记录 72/73）
- **云环境连接加固**：驱动层 socket 超时 + 连接池 TTL + 事务空闲提交 + 瞬断自动重试，规避 LB/NAT 静默回收
- **日志文件**：`--messages` / `-m` 把本次运行的命令行日志与错误追加到该文件（终端不再打印；文件可保留多次运行的记录）；它**不接管结果**——`list`/`show`/`exec` 的结果仍去 `stdout`，或去 `--output` 指定的文件
- **可调进度节奏**：`--progress-interval` 控制定时进度日志的间隔（默认 5 秒，下限 100ms；进度记录没有关闭开关）
- **流式处理**：支持 GB 级大文件，不在内存中缓存全量数据
- **批量优化**：导入使用批量 INSERT（1000 行/批次），提升写入性能
- **单行 SQL 布局**：`--format sql` 默认每行一条 INSERT，文件逐行可切分；攒批统一由导入侧按 `--batch-size` 完成，导出侧的 `--extended-insert` 只用于换取更小的文件体积
- **低开销 DEL 解析**：定界符预计算、读取路径不做无效字节↔字符串转换，单线程解析开销降低后并行导入才有效扩展
- **密码加密**：PBKDF2-HMAC-SHA256 派生密钥 + AES-256-GCM 加密密码，避免在 ps 命令中泄露明文
- **连接参数可走环境变量**：`IMEX_HOST`/`IMEX_PORT`/`IMEX_USER`/`IMEX_PASSWORD`/`IMEX_DATABASE` 为五个连接选项供默认值，命令行显式写下的选项始终优先；日志只记变量名、不记取值
- **三引擎**：`--engine` 取 `mysql`（默认）、`pg` 或 `dm`。PostgreSQL 走 `github.com/jackc/pgx/v5`，`export`/`import` 支持 `-f del|sql|parquet`（`-f sql` 由 `--sql-dialect pg` 产出方言匹配的 SQL）、`--session-var`（与 MySQL 同一口径）、导入侧 `--mode insert|replace|insert_update` 与 `--threads`，导出侧 `--threads 1–256` 并行分片导出（见「PostgreSQL 引擎」），`list` 给七个对象；`show` 现从系统目录重建对象定义（`tab`/`vws`/`trg`/`prc`/`fcn` 五类可用，`evt` 因 PostgreSQL 无事件调度器仍在连库前退出 `2`）；`--no-hex-blob` 在连库前退出 `2`。达梦 DM8 走 `gitee.com/chunanyong/dm`、恒以 Oracle 兼容模式连接，`export` 支持 `-f del|sql|parquet`（`--threads 1–256` 并行分片导出，见「达梦引擎」）、`import` 支持 `-f del|sql|parquet` 的 `--mode insert|replace` 与 `--threads`，`list` 给八个对象（除 `events` 外全部），`show` 现走 Oracle 模式的 `DBMS_METADATA.GET_DDL`（`tab`/`vws`/`trg`/`prc`/`fcn` 五类可用，`evt` 因无事件调度器退出 `2`），`--session-var` 在连库之前退出 `2` 并说明原因。**`--engine` 没有对应的环境变量**，引擎只能由命令行选定
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

覆盖 DEL/SQL/Parquet 往返、insert/insert_update/replace 模式、列映射、并行导出与并行导入、日志文件。

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

# 自带结构的 SQL：建表语句由 `imex show tab` 取，数据由 export 取，拼起来是一份完整 dump
imex show tab users -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users_ddl.sql
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users_data.sql -f sql -q "SELECT * FROM users"
cat /backup/users_ddl.sql /backup/users_data.sql > /backup/users.sql
# 这份拼接文件交给标准 mysql 客户端可一次建表并灌数据；imex import 不重放 DDL，
# 它跳过文件里的 CREATE 语句，只导入 INSERT，因此要先用上面的 DDL 把表建好，再以 insert 模式导入
imex import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.sql -f sql \
  --mode insert -t users
```

## 命令说明

### export - 数据导出

```bash
imex export [连接选项] [导出选项] [修饰选项] --query "<SELECT SQL>"
```

**连接选项：**

| 选项 | 短选项 | 说明 | 环境变量 | 默认值 |
|------|--------|------|----------|--------|
| `--engine` | | 数据库引擎：`mysql`（默认）、`pg`（PostgreSQL，经 `jackc/pgx/v5`）或 `dm`（达梦 DM8，经 `gitee.com/chunanyong/dm`，恒以 Oracle 兼容模式连接；`dameng` 是同一个取值的别名），取值不区分大小写、裁两端空白；`pg` 的能力面与被拒选项见「PostgreSQL 引擎」一节，`dm` 见「达梦引擎」一节 | 无（刻意不设） | `mysql` |
| `--host` | `-h` | 数据库主机地址 | `IMEX_HOST` | `127.0.0.1` |
| `--port` | `-P` | TCP/IP 端口（`--engine pg` 时默认 `5432`，`--engine dm` 时默认 `5236`） | `IMEX_PORT` | `3306` |
| `--user` | `-u` | 数据库用户名（支持 OceanBase 格式，见下方说明）；`--engine dm` 时它就是达梦的账号，不能为空，为空在建连之前退出 2 | `IMEX_USER` | `root` |
| `--password` | `-p` | 数据库密码 | `IMEX_PASSWORD` | 空 |
| `--database` | `-D` | 数据库名；`--engine dm` 时它是模式名，既写进驱动的 `schema` 参数，也是目录查询过滤的 `OWNER` | `IMEX_DATABASE` | 空 |
| `--session-var` | | 导出前设置的会话变量（`name=value`，可重复，见下方说明）；`--engine pg` 与 MySQL 同一口径生效（每条新物理连接执行 `SET name = value`），`--engine dm` 下整个选项被拒（退出 2） | | 空 |
| `--connect-timeout` | | 建连（TCP dial）超时，落到驱动的 `timeout` | | `5s` |
| `--socket-read-timeout` | | 单次 socket 读超时，落到驱动的 `readTimeout` | | `300s` |
| `--socket-write-timeout` | | 单次 socket 写超时，落到驱动的 `writeTimeout` | | `300s` |

`--password` 与 `--database` 没有默认值，任一缺失即是用法错误、退出码 `2`；缺口令时的提示同时点名两个来源（`--password is required (or set IMEX_PASSWORD)`）。`--host` 与 `--user` 有内置默认值（`127.0.0.1`、`root`），只有把它们**显式写成空值**（`-h ""`、`-u ""`）才会被判为缺失并以退出码 `2` 拒绝，此时不会回落到 `IMEX_HOST`/`IMEX_USER`（见「连接信息来自环境变量」）。

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

- 变量在**每一条新建物理连接**上生效，不受连接池复用影响：MySQL 侧由驱动把这些值放进 DSN 附加参数、在建连时自动执行对应 `SET`；`--engine pg` 走同一语义的另一条通道——pgx 的 AfterConnect 钩子在每条新物理连接上依次执行 `SET name = value`。两者都是「每连接一条 SET」，因此主连接、并行导出的每个分片连接、并行导入的每个工作线程连接都一致生效。`--engine dm` 仍不支持（见下方达梦一节）。
- 值按 SQL 字面量自动处理（MySQL 与 pg 共用这一份解析）：纯数字与 `ON/OFF/TRUE/FALSE/DEFAULT/NULL` 原样传入，其余加单引号（内部单引号双写）。值中禁止反引号、注释序列（`/*`、`--`）与反斜杠，防止破坏或注入 SET 语句。
- 变量名须匹配 `[A-Za-z][A-Za-z0-9_]*`；与工具/驱动内部参数同名的会被拒绝，且**保留清单按引擎不同**：MySQL 侧是 go-sql-driver 的连接串词汇（`charset`、`parseTime`、`tls`、`timeout` 等，由方言层交出，`internal/backend/dsn_mysql.go`），pg 侧是 libpq/pgx 的连接关键字（`host`、`port`、`dbname`、`user`、`password`、`sslmode`、`connect_timeout` 等）加上工具自己掌控的 GUC（`client_encoding`、`statement_timeout`），清单由 `internal/backend/postgres.go` 交出。这些保留名在连库之前即被 `parseSessionVars` 拒绝，因此解析器本身不认识引擎——它向方言层要一张表。连接/文件字符集由 `--default-charset` 统一掌管，会话级字符集变量 `character_set_client` / `character_set_connection` / `character_set_results` 在 MySQL 侧同样被拒绝——否则会以会话变量绕过该不变量（如 `character_set_results=gbk` 会转码二进制列造成损坏）。未知变量名会在连接阶段由服务端报错，便于尽早发现拼写错误。
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

这 16 个名字是 **MySQL 方言自己的词表**（`internal/backend/charset_mysql.go`，134 轮自 `cmd/validate.go` 下沉），`cmd` 只保留「不认识的字符集」那句文案与它在校验顺序里的位置；`--engine pg` 用的是另一张表，映射到 `client_encoding` 的五个名字（见下方「PostgreSQL 引擎」一节）；`--engine dm` 沿用这 16 个名字做**文件编码**的判定，但连接串里没有对应的字符集参数，服务端一侧的字符集由实例与角色配置决定（见下方「达梦引擎」一节）。

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
| `--query` | `-q` | SQL SELECT 语句（支持 OceanBase / MySQL 优化器 hint）；写 `@文件` 时从该文件读取（读入后去掉首尾空白与结尾分号），要传以 `@` 开头的字面 SQL 用 `@@` 转义；必填 | 无 |
| `--messages` | `-m` | 把全部日志记录**追加**到该文件，终端不再打印（文件内容就是命令行日志本身；已存在的内容保留） | 空 |
| `--progress-interval` | | 定时进度日志的间隔（必须 > 0 且 ≥ 100ms，否则用法错误退出码 2） | `5s` |
| `--no-overwrite` | | 拒绝覆盖已存在的输出文件（默认允许覆盖；原名 `--no-replace`，已改名且不留别名） | `false` |
| `--threads` | | 并行导出任务数（1=不并行；上限 256）。每个 worker 全程一条连接，worker 数 + 1 超过服务端 `@@max_connections` 时在数据移动前记一条 `warn` | `1` |
| `--shard-key` | | 分片键列名（空=自动检测主键/唯一键）；**只有并行导出读取并校验它**——显式给出的列在表里不存在时为用法错误、退出码 2，自动检测失败仍回落单线程；`--threads 1` 或并行请求回落到单线程时该值不起作用，运行记一条 `warn` 并在配置记录标 `shard_key_ignored=true`（不改变退出码与产物） | 空 |
| `--batch-size` | | 读→写交接每批的行数（仅单线程导出流水线；与 `--threads>1` 同时显式给出即用法错误，退出码 2） | `1000` |
| `--pipeline-buffer` | | 读→写交接队列深度（批数；0=同步交接；仅单线程导出，与 `--threads>1` 同时显式给出即用法错误，退出码 2） | `10` |
| `--default-charset` | | 连接与输出文件字符集（见上方说明；utf8mb4/utf8/latin1/ascii/gbk/gb18030。`--engine pg` 时它映射为 `client_encoding`，只认五个名字，见「PostgreSQL 引擎」；`--engine dm` 时它只管产物文件的文本编码，连接串里没有对应参数，见「达梦引擎」） | `utf8mb4` |
| `--hex-blob` | | 二进制列强制写为 `0x` 十六进制字面量（非 utf8mb4 时自动开启）；仅 DEL/SQL 文本格式，`--format parquet` 下传入报错 | `false` |
| `--no-hex-blob` | | 强制二进制列原样透传（非 utf8mb4 下可能损坏，会打印告警）；与 `--hex-blob` 互斥；`--format parquet` 下传入报错 | `false` |
| `--parquet-compression` | | Parquet 页压缩算法（none/snappy/zstd）；仅 `--format parquet`，其它格式下传入报错（退出码 2）。导入无需指定：编解码器记录在文件 footer 里 | `none` |

> Parquet 压缩取舍（30 万行实测）：`snappy` 导出耗时约 +4%，文本类数据体积约为原来的 1/10；`zstd` 更小（同一份数据约为 1/58）但耗 CPU 与内存明显更多，**不可压缩数据**（已随机的二进制）三者体积接近，压缩纯属浪费。导入侧三种编码耗时与内存几乎一样（编解码器从 footer 读，无需选项）。

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
| `--no-sql-tag` | 不在工具主动执行的语句前加 `/* imex:<commit> */` 归因注释（默认加，可用 `IMEX_NO_SQL_TAG=1`） | `false` |

**SQL 格式修饰选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--statement-size` | 单条 INSERT 语句最大字节数（只有 `--extended-insert` 时是攒批上限；默认单行布局下一行无法再拆，超过它只记一次告警） | `8388608`（8MiB） |
| `--extended-insert` | 把多行合并进一条 INSERT（按 `--statement-size` 分组，等价 mysqldump 默认行为）；若 `--statement-size` 低于单行语句宽度则一条也合不成，产物退回每行一条 INSERT 并记一次告警 | `false` |
| `--sql-dialect` | 指定 `-f sql` 产物写哪种 SQL 语法：`auto`（默认）跟随所连引擎——MySQL 引擎逐字节保持既有 MySQL 语法、PostgreSQL 引擎写 pg 语法；`mysql`/`pg` 显式指定，**与连的哪个引擎无关**（一个 MySQL 库可以刻意产出 pg dump）。pg 语法用双引号标识符、`'\x..'::bytea` 表二进制、含控制字符的串写 `E'..'`、布尔写 `true`/`false`；MySQL 语法用反引号、`0x` 十六进制、反斜杠转义；dm 语法用双引号标识符、二进制 `HEXTORAW('..')`、串字面量只翻倍单引号且反斜杠与控制字符原样、布尔写 `1`/`0`。仅 `--format sql` 生效（其它格式下传入是用法错误退出 2）；非法值 `invalid --sql-dialect "x": must be one of auto/mysql/pg/dm` 退出 2。`--engine dm` 连接时 `auto` 落 dm 语法，`export -f sql` 与 `import -f sql` 双向可用（见「达梦引擎」） | `auto` |

> SQL 导出只写数据语句：逐行 `INSERT`，**不含任何前导，也不含任何 DDL**（历史上的 `SET NAMES` 前导已移除，以便同一份文件跨 MySQL/PostgreSQL/达梦 通用回放）。文件的另一半——建表语句——由 `imex show tab` 交回，它是本工具唯一读 `SHOW CREATE TABLE` 的地方，两者拼起来就是自带结构的 dump（见上文自带结构 SQL 示例与「show - 对象定义导出」一节）。`--create` 与 `--no-data` 已直接删除、不留别名，传入即 `unknown flag: --create` / `unknown flag: --no-data` 并退出 2（`--no-create`、`--single-row-insert` 此前已按同一口径移除）。因为产物只描述数据，`--format sql` 仍只接受单表查询：JOIN、`FROM a, b`、派生表 `FROM (SELECT …) t`、`WITH` CTE 都会在解析阶段报错并提示改用 `--format del`/`--format parquet`，报错发生在输出文件创建之前。`-q`（含 `-q @file` 从文件读入的语句）里若表名带了 schema（`db.tbl`），导出只按裸表名写 `INSERT`，并行路径的分片键探测、统计读取与采样边界仍以查询点名的 schema 为准（见「并行导出」一节），因此不会再出现把连接的 `--database` 二次拼接导致的 `Error 1054/1146`。导入侧 `--mode create` 已连同其建表语义一并移除（传入即 `invalid --mode "create"`，退出码 2）：imex 不重放 DDL，导入 SQL 文件时它跳过 `CREATE` 语句只灌 `INSERT`，要建表就先用 `show tab` 产出的 DDL 在目标库建好、再以 `--mode insert` 导入；`mysqldump` 等外部工具含 `CREATE TABLE` 的文件同样照此处理，建表交给标准客户端、imex 只负责数据。

### import - 数据导入

```bash
imex import [连接选项] [导入选项] [修饰选项]
```

连接选项与 export 子命令相同（`--engine`/`--host`/`--port`/`--user`/`--password`/`--database`/`--session-var`/`--default-charset` 以及 `--connect-timeout`/`--socket-read-timeout`/`--socket-write-timeout`），五个连接选项的环境变量形式同样可用（见「连接信息来自环境变量」）；`--password` 与 `--database` 必填，`--host` 与 `--user` 走内置默认值，口径与 export 一致。导入时 `--default-charset` 须与当初导出所用的字符集一致（文件编码 == 连接字符集），否则文本列会被误解码；现行 SQL 产物已不含 `SET NAMES` 前导，imex 导入仍以本参数为准，读旧文件时那一行会被跳过。`--mode create` 已从三个引擎整体移除（传入即 `invalid --mode "create"`，退出码 2），建表交给你自己的客户端，imex 只灌数据。`--engine pg` 时 `--session-var` 与 MySQL 同一口径生效（每条新物理连接上执行 `SET name = value`，见「PostgreSQL 引擎」），`-f sql` 会被拒绝（退出码 `2`）；`--mode replace` 与 `--mode insert_update` 与 `--threads` 可用，但 replace 在 pg 侧只是 `DELETE FROM`（没有可关的引用检查）、insert_update 要求表上有一把唯一的键（见下）。`--engine dm` 时 `--session-var` 仍被拒绝（退出码 `2`，`SET <name>=<value>` 是会话语法翻译不过去）且 `--mode insert_update` 同样退出 `2`，可用的是 `-f del|sql|parquet` 的 `--mode insert|replace`——`-f sql` 由方言感知的读取器回放同引擎的达梦 dump（见「达梦引擎」）。

**导入选项：**

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--input` | `-i` | 输入文件路径 | 无（必填） |
| `--format` | `-f` | 输入格式（del/sql/parquet） | `del` |
| `--mode` | `-M` | 导入模式 | `insert` |
| `--table` | `-t` | 目标表名（`insert`/`replace`/`insert_update` 三种模式均必填；`--mode create` 已移除，不再有"省略则取 SQL 文件建表语句表名"的分支） | 无 |
| `--columns` | `-c` | 列映射（见下方说明） | 空 |
| `--messages` | `-m` | 把全部日志记录**追加**到该文件，终端不再打印（文件内容就是命令行日志本身；已存在的内容保留） | 空 |
| `--progress-interval` | | 定时进度日志的间隔（必须 > 0 且 ≥ 100ms，否则用法错误退出码 2） | `5s` |
| `--threads` | | 并行导入任务数（1=不并行；上限 256）。worker 数 + 1 超过服务端 `@@max_connections`、或 `--threads` 让重放缓冲总量（每 worker 一份，下限 8 MiB）超过进程预算时，在任何数据移动之前各记一条 `warn` | `1` |
| `--batch-size` | | 读→写交接每批的行数（单线程与并行导入均生效；实际还受字节上限与占位符上限约束，见下） | `1000` |
| `--pipeline-buffer` | | 读→写交接队列深度（批数；0=同步交接），**按给定值执行**。该队列由全部 worker 共享，深度与 `--threads` 是两件正交的事（深度是驻留内存，线程是并发）。深度 × 单批字节预算可能超过 1 GiB 时不改写深度，只在数据移动前记一条 `warn` 报出这个价（`requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes`/`hint`） | `10` |
| `--statement-size` | | 单条 INSERT 语句的字节上限，只能**压低**下面「导入攒批的硬上限与可选压低」里由 `max_allowed_packet` 导出的预算，不能抬高；`0`=不额外压低。默认预算还受驱动侧 64 MiB 客户端插值上限钳制——只有服务端报出**大于** 67,108,864 B（64 MiB）的包时才轮到它——实测 67,108,864 时 `batch_bytes=60397977 limiting_factor=max_allowed_packet`，67,109,888 时同一数字改记 `limiting_factor=driver_packet_cap`，测试床按 OceanBase V4.x 默认的 `max_allowed_packet=16M` 时决定者是 `max_allowed_packet`（`batch_bytes=15099494`）。单行大于该预算时仍独立成批（一行无法再拆分） | `0` |
| `--default-charset` | | 连接与输入文件字符集，须与导出时一致（见导出侧说明） | `utf8mb4` |

**`--columns` 列映射说明：**

支持两种格式：
- **位置映射**：`--columns "name,age,email"` - 文件列按位置映射到表列，条目数必须等于文件列数
- **名称映射**：`--columns "user_name=name,user_age=age"` - 按列名指定文件列到表列的映射，**只要求写出来的映射有效**：可以只写文件中的部分列（其余字段不导入），书写顺序不影响结果

通过检测 `=` 符号自动识别格式。名称映射写出的每一条都在读取数据前校验：文件列必须存在于文件的列名行（大小写不敏感）、同一文件列不得写两次、同一目标列不得被两列写入；未写进映射的文件列不导入（启动时记一条 `unmapped file columns are not imported`），未写进映射的目标列取 DEFAULT/NULL/自增值。

**导入模式：**

| 模式 | 说明 |
|------|------|
| `insert` | 追加数据到表 |
| `insert_update` | 插入新行或更新主键匹配的行 |
| `replace` | 删除表所有数据后**普通 INSERT**（不是 MySQL 的 `REPLACE INTO`） |

> 历史上的 `--mode create`（建表后导入）已从三个引擎整体移除——imex 导入不重放 DDL。给出 `--mode create` 在校验阶段（建连之前）即 `invalid --mode "create": must be one of insert/insert_update/replace`、退出 2。SQL 文件里的 `CREATE TABLE` 会被读取器跳过、只导 `INSERT`；要建表先用 `imex show tab` 取 DDL、用标准客户端执行，再以 `--mode insert` 导入。

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
| `--no-sql-tag` | 不在工具主动执行的语句前加 `/* imex:<commit> */` 归因注释（默认加，可用 `IMEX_NO_SQL_TAG=1`） | `false` |

### list - 元数据查询

```bash
imex list <object|缩写> [连接选项] [查询选项]
```

`list` 读取 `information_schema`。结果、日志、错误各走自己该走的通道（见下表与「输出通道」段），因此可以直接接管道。连接选项与 export 相同（含 `--engine` 与五个 `IMEX_*` 连接变量，见「连接信息来自环境变量」），但 `--database` 对大多数对象**不是必填**；`--password` 始终必填（缺失时提示 `--password is required (or set IMEX_PASSWORD)` 并退出码 `2`），`--host` 与 `--user` 同样走内置默认值。`--engine pg` 时九个对象里八个可用，`evt`（事件）退出 `2`；`-D` 是连接的库，对象范围取 `current_schema()`，系统 schema（`pg_catalog`、`information_schema`、`pg_toast`）默认排除，`--all` 取消排除。`--engine dm` 时八个对象可用（除 `events` 外全部：`databases`、`schemas`、`tables`、`views`、`columns`、`indexes`、`triggers`、`routines`），`events` 退出 `2`；`-D` 对 tables/views/triggers/routines/columns/indexes 是模式名（进 DSN 并过滤 `OWNER`），对 `databases` 过滤库名、对 `schemas` 过滤模式名且都不进 DSN；五个系统模式在 `schemas`、`tables`、`views`、`triggers`、`routines` 上默认排除，`--all` 取消排除（见「达梦引擎」）。子命令以三字母短名为主名、英文全名为等价别名（`imex list tab` 与 `imex list tables` 解析到同一子命令，与 `show` 同形；下表用全名列举对象）：

| 对象 | 需要的范围 | 输出列 |
|------|------------|--------|
| `databases` | 无；`-D` 只把结果缩小到该库 | `database charset collation` |
| `tables` | 无；`-D` 限定库，`--type base\|view\|all` 选类型 | `database table type engine rows data_length index_length comment` |
| `views` | 无；`-D` 限定库 | `database view check_option is_updatable definer security_type` |
| `columns` | **必须** `-D` 且 `-t` | `database table column ordinal type nullable default key extra comment` |
| `indexes` | **必须** `-D` 且 `-t` | `database table index non_unique seq column cardinality index_type` |
| `triggers` | 无；`-D` 限定库，`-t` 只列该表的触发器 | `database trigger event table timing statement definer` |
| `routines` | 无；`--type procedure\|function\|all` | `database routine type definer security deterministic data_access` |
| `events` | 无；`-D` 限定库 | `database event status execute_at interval_value interval_field definer last_executed` |

不给 `-D` 时，除 `databases` 外的对象只列非系统库（`information_schema`/`mysql`/`performance_schema`/`sys`），`--all` 把它们也纳入；显式给了 `-D mysql` 则照常只列 `mysql`。`--pattern` 对名称列做 `LIKE` 匹配，`%`、`_` 为通配符，反斜杠转义，例如 `--pattern 'tmp\_%'`。

对象名支持三个字母的固定简写，大小写敏感（`imex list <简写> --help` 与全名完全等价）：

| 对象 | 简写 |
|------|------|
| `databases` | `dbs` |
| `tables` | `tab` |
| `views` | `vws` |
| `columns` | `col` |
| `indexes` | `idx` |
| `triggers` | `trg` |
| `routines` | `rtn` |
| `events` | `evt` |

**输出选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--no-col-names` | 不打印表头行 | `false` |
| `--pattern` | 名称列 `LIKE` 过滤 | 空 |
| `--all` | 不给 `-D` 时也包含系统库 | `false` |
| `--type` | `tables` 选 `base`/`view`/`all`；`routines` 选 `procedure`/`function`/`all` | `base` / `all` |
| `--table` | `-t`，`columns`/`indexes` 必填，`triggers` 用作过滤 | 空 |
| `--output` | `-o`，把结果写入该文件（覆盖已存在的同名文件） | 空 |
| `--no-overwrite` | 配合 `--output`：目标已存在则拒绝并退出码 `1`，不改写 `--messages` 文件 | `false` |
| `--messages` | `-m`，把日志与错误**追加**写入该文件、屏幕完全安静；**结果不受它影响**，仍去 `stdout` 或 `--output` | 空 |

**输出通道**（`list`、`show`、`exec` 共用同一张表）：

| `--output` | `--messages` | 结果 | 日志与错误 |
|------------|--------------|------|------------|
| 有 | 无 | 该文件 | stderr |
| 有 | 有 | 该文件 | `--messages` 文件，屏幕安静 |
| 无 | 无 | stdout | stderr |
| 无 | 有 | **stdout**（不受 `--messages` 影响） | `--messages` 文件，屏幕安静 |

`--messages` 只接管日志与错误，**从不接收结果**；要把结果落成文件必须显式给 `--output`。两者指向同一个路径是用法错误，退出码 `2`（结果按覆盖写、日志按追加写，同一个文件无法两者兼得）。结果先缓冲、成功结束后才投递，因此失败的运行不会在文件里留下半截结果。缓冲的上限由命令输出规模决定：本机实测最大的一次普通运行（全服务器 `list tables`）为 12,848 字节 / 141 行（2026-11-02 复测，行数随服务器上的表数变化；同一命令在设计轮为 12,747 字节 / 140 行）。

`table` 是唯一的输出格式：缓冲全部行后按 rune 宽度对齐，列间两个空格，NULL 输出大写 `NULL`（与字面字符串 `NULL` 无法区分），值里的反斜杠、制表符、回车、换行转成 `\\`、`\t`、`\r`、`\n`。空结果不算错误：退出码 0，仍打印表头。

```bash
imex list databases -u root -p pass
imex list schemas -u root -p pass
imex list sch -u root -p pass -D shop
imex list dbs -u root -p pass -D shop
imex list tab -u root -p pass -D shop
imex list columns -u root -p pass -D shop -t orders
imex list indexes -u root -p pass -D shop -t orders
imex list routines -u root -p pass --type procedure --pattern 'etl\_%'
imex list tables -u root -p pass -D shop -o /backup/shop_tables.txt
```

用法错误（未知对象、缺少 `-D`/`-t`、非法 `--type`、`--output` 与 `--messages` 同路径）退出码 2，错误文本在 stderr；连接或查询失败、`--no-overwrite` 命中已存在文件退出码 1。

### show - 对象定义导出

```bash
imex show <kind> <对象名> [连接选项] [-D 库名] [-o 文件]
```

`show` 打印**一个**对象在服务器上保存的定义，与 `list` 的「枚举一批」分工互补：批处理由 `list` 出名字，逐个取定义交给 `show`。表的建表语句也只在这里取得——`export -f sql` 的产物是纯数据，全工具取建表文本的地方只有 `show tab`。七个 kind 一律三个字母。三个引擎都取，只是取法不同：MySQL 走 `SHOW CREATE` 语句族；PostgreSQL 没有 `SHOW CREATE`，定义由系统目录重建（列、类型、默认值、约束经 `format_type`/`pg_get_constraintdef` 拼回，视图/函数/存储过程/触发器经 `pg_get_viewdef`/`pg_get_functiondef`/`pg_get_triggerdef`，模式拼成 `CREATE SCHEMA … AUTHORIZATION …;`）；达梦走 Oracle 模式的 `DBMS_METADATA.GET_DDL`，模式这一层取 `GET_DDL('USER',…)`——达梦的模式与账号是同一个对象，`GET_DDL('SCHEMA',…)` 会报 `-20001`，所以输出是 `CREATE USER … IDENTIFIED BY <PASSWORD> …`，口令由服务端元数据层脱敏成占位符。`evt`（事件）只有 MySQL 有——`--engine pg` 与 `--engine dm` 下 `show evt` 在建库连接之前退出 `2`，其余六个 kind 两引擎均可打印。`sch` 是唯一不带 schema 限定的 kind：模式名本身就是完整引用，`imex show sch appdb` 不需要 `-D`。下表是 MySQL 一侧的语句与结果列（`internal/backend/show_mysql.go`）：

| kind | 别名（英文全名） | 语句 | 定义所在列 |
|------|------------------|------|------------|
| `sch` | `schema` | `SHOW CREATE DATABASE` | `Create Database` |
| `tab` | `table` | `SHOW CREATE TABLE` | `Create Table` |
| `vws` | `view` | `SHOW CREATE VIEW` | `Create View` |
| `trg` | `trigger` | `SHOW CREATE TRIGGER` | `SQL Original Statement` |
| `evt` | `event` | `SHOW CREATE EVENT` | `Create Event` |
| `prc` | `procedure` | `SHOW CREATE PROCEDURE` | `Create Procedure` |
| `fcn` | `function` | `SHOW CREATE FUNCTION` | `Create Function` |

三个字母与全名等价（`imex show table x` 与 `imex show tab x` 逐字节相同），大小写敏感。MySQL 里 schema 就是 database，所以 `show sch` 读的是 `SHOW CREATE DATABASE`；PostgreSQL 与达梦的 schema 在 database 里面，重建/取定义的方式见上一段。上表的「语句」与「定义所在列」两列是 MySQL 一侧的方言数据（`internal/backend/show_mysql.go`，经 `Backend.ShowDefinition` 取得）；pg 侧由 `internal/backend/show_postgres.go` 交回目录重建查询、达梦侧由 `internal/backend/show_dameng.go` 交回 `DBMS_METADATA.GET_DDL` 调用，各引擎取不到定义的那个 kind（pg/dm 的 `evt`）在建连前被 `Refusals(CmdShow)` 拒掉，不会拨号。每个查询都只回一行一列，`show` 按列名取值、取不到时退化为「第一个以 `CREATE` 开头的值」，绝不把 `sql_mode` 或 `time_zone` 当定义输出。MySQL 的 `SHOW CREATE` 不返回终止符，工具补一个 `;\n`；自带结尾 `;` 的形态（Gauss/达梦 `GET_DDL` 的表与 PL/SQL、pg 的目录重建表）按 `hasTerminator` 回报，不再追加，避免出现 `;;`。

`information_schema` 被刻意排除：`TRIGGERS.ACTION_STATEMENT` 只有函数体没有触发器头，`ROUTINES.ROUTINE_DEFINITION` 丢参数列表与 `RETURNS`，`VIEWS.VIEW_DEFINITION` 是解析后的规范化文本——都不足以重建对象。

对象名可以自带库限定（`mydb.users`、`` `mydb`.`users` ``），此时不需要 `-D`；否则必须给 `-D`，两者都没有是用法错误退出码 `2`。引号内的双写反引号是转义，所以 `` `a.b`.`c` `` 只在第二个点处切分。输出通道与 `list` 完全同一张表（含 `--output`/`--no-overwrite`/`--messages` 与同路径拒绝）。

```bash
imex show tab users -D mydb                     # 打到 stdout
imex show vws v_orders -D mydb -o /backup/v_orders.sql
imex show prc build_etl -D mydb -o /backup/etl.sql
imex show tab mydb.users                        # 名字自带库，不需要 -D
imex list tab -D mydb --no-col-names | awk '{print $2}' | while read -r t; do
  imex show tab "$t" -D mydb -o "/backup/ddl/$t.sql" -m /backup/ddl/run.log
done
```

对象定义（DDL）的回放不走 `imex import`——`import` 只重放 `INSERT`，把 `CREATE TABLE` 与一切其它语句一律跳过。`show` 存的建表文本请交给下一节的 `imex exec`：它按顶层 `;` 逐条执行任意 SQL，`imex show tab users -D mydb \| imex exec -D otherdb` 就把建表语句原样重放到另一张库里（自 `v0.28.0`，见「exec - 纯执行 SQL」一节）。

### exec - 纯执行 SQL

自 `v0.28.0`。`exec` 把任意 SQL 直接发往目标并回报每条做了什么，独立于 `export`/`import` 的数据导入导出路径——`show` 存的建表文本、临时的一条 `ALTER`、一段脚本，都从这里执行。

```bash
imex exec [连接选项] [-D 库名] [-f 文件 | '单条 SQL']     # 不给 SQL 时从标准输入读
```

SQL 来源三选一，互斥：位置参数给一条、`-f` 指定脚本、或从 stdin 读（`show` 产物直接管道进来）。文本按**引擎语法**在顶层 `;` 处切分，逐条执行、遇到第一条被服务端拒绝的即停：返回值语句打成 `list` 同款对齐表，写入语句报 `OK`／`N rows affected`／`last_insert_id`。

| 选项 | 作用 |
|------|------|
| `-f, --file` | 从该文件读 SQL（与位置参数、stdin 互斥） |
| `-D, --database` | 语句运行的默认库（亦 `IMEX_DATABASE`） |
| `--engine` | `mysql`／`pg`／`dm`，决定顶层 `;` 的切分规则 |
| `--no-col-names` | 结果集不打表头行 |
| `-o`/`-m`/`--no-overwrite` | 与 `list`/`show` 完全同一套输出通道 |

```bash
imex exec -D mydb 'CREATE TABLE t (id INT PRIMARY KEY, name TEXT)'   # 内联一条
imex exec -D mydb -f schema.sql                                       # 跑一份脚本
imex show tab users -D mydb | imex exec -D otherdb                    # 重放 show 存的 DDL
```

例程定义体内自带 `;` 者须用带 `DELIMITER` 指令的脚本经 `-f` 执行——体内裸 `;` 会被读成语句结束。完整帮助见 `imex exec --help`，逐字节契约见 `docs/cli-oracle.txt` 的 `## exec` 段；设计与取证见需求文档 2.7。

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

### 批量导出所有表

仓库不附带批量脚本（原 `scripts/export-all-tables.sh` 已于 2026-11-01 移除，见 `docs/CHANGELOG.md` 的「功能记录（v0.15.0）」）。多表场景由调用方自己循环 `imex export`，表名用 `imex list tab` 枚举——`list` 的日志与错误走 stderr，stdout 只有对齐的结果，首行是表头、第 2 列是表名：

```bash
imex list tab -D shop -u root -p pass 2>/dev/null | awk 'NR > 1 { print $2 }' |
  while read -r t; do
    imex export -D shop -u root -p pass -q "SELECT * FROM \`$t\`" \
      -o "export/shop/$t.del"
  done
```

- 换成 SQL 产物就把 `-o` 的后缀改为 `.sql` 并加 `-f sql`；建表语句另用 `imex show tab $t -o …` 取，两个文件 `cat` 起来才等价于过去带 `--create` 的单个文件；`--pattern 'order%'` 可以直接加在 `imex list tab` 上做名字过滤。
- 每条 `imex export` 各开一次 `START TRANSACTION WITH CONSISTENT SNAPSHOT`，**表与表的时点不一致**（v0.14.0 的 `general_log` 取证见变更记录 112 ⑦）。要整库一致快照属于尚未实现的数据库级批量导出，见变更记录 111 之后的功能差距讨论。
- 表名或库名本身含连续空格时，`awk` 按「两个及以上空格」切列无法把它与列分隔区分——这是 `list` 对齐输出的固有性质，这类对象请改用 `--pattern` 分批或直接调用 `imex export`。
- 想把枚举结果落成文件再慢慢用：`imex list tab -D shop -o names.txt`（重跑默认覆盖，怕踩到已有文件就加 `--no-overwrite`，命中时退出 1 且原文件不动）。取每张表的建表语句用 `imex show tab`，见「show - 对象定义导出」的循环示例。

### 连接信息来自环境变量

export、import、`list`、`show` 四条需要连库的命令，五个连接选项都可以由环境变量供值，容器与 CI 里设一次即可，不必每条命令重复写地址与口令。`--engine` 不在其中——引擎只能由命令行显式选定，没有 `IMEX_ENGINE`：

```bash
export IMEX_HOST=127.0.0.1 IMEX_PORT=3306 IMEX_USER=app IMEX_PASSWORD=pass IMEX_DATABASE=mydb
imex export -q "SELECT * FROM users" -o /backup/users.del   # 一个连接参数都没写
imex import -i /backup/users.del -t users
imex list tables
```

| 选项 | 环境变量 | 空白取值 | 说明 |
|------|----------|----------|------|
| `--host` | `IMEX_HOST` | 按未设置 | 主机地址 |
| `--port` | `IMEX_PORT` | 按未设置 | 端口；非数字或超出 1–65535 是用法错误、退出码 `2` |
| `--user` | `IMEX_USER` | 按未设置 | 用户名 |
| `--password` | `IMEX_PASSWORD` | 按未设置 | 口令；`ENC:` 密文照常按 `IMEX_KEY` 解密 |
| `--database` | `IMEX_DATABASE` | 按未设置 | 数据库名 |

- **优先级：命令行显式写下的选项 > 环境变量 > 选项的内置默认值。** 判据是这个选项在命令行上被写过没有，不看取值，因此 `IMEX_USER=svc` 时 `imex export -u root` 用的仍是 `root`，`IMEX_PASSWORD=pass` 时 `imex export -p ""` 用的仍是空口令。
- **空白等于没设**：变量不存在、为空、或去掉首尾空白后为空，都退回内置默认值；供值的变量两端空白会被去掉。
- **缺值提示点名两个来源**：`--password` 是唯一既没有默认值、又最常被放进环境的必填项，缺失时的用法错误文案为 `--password is required (or set IMEX_PASSWORD)`，export、import、`list`、`show` 四条命令一字不差（`cmd/conn_env_test.go` 钉前两条，`cmd/list_test.go`/`cmd/show_test.go` 钉后两条）。它是一句静态文案，只说明「这个值有两个来源」：写了 `-p ""` 而 `IMEX_PASSWORD` 有值时，按优先级用的仍是那个空值、打出的仍是这句提示，此时该改的是命令行上的空值而不是环境变量。
- **日志只出现变量名，绝不出现值**：有变量供值时记 INFO `connection values supplied by environment variables="[IMEX_USER IMEX_PASSWORD]"`。这条记录同样进 `--messages` 文件，所以口令（含 `ENC:` 密文）不会落到任何产物里。
- 这与四个全局开关的语义**相反**，是刻意的：`IMEX_ERROR_STACK`/`IMEX_ECHO_SQL_VALUES`/`IMEX_NO_SQL_TAG`/`IMEX_LOG_FORMAT` 是**覆盖**（布尔与布局只有「打开/选一个」这一维），连接变量是**默认值的来源**（让 env 压过命令行写下的地址，同一条命令就会连到操作者没写的那个库）。

### PostgreSQL 引擎（`--engine pg`）

加 `--engine pg` 即连 PostgreSQL（驱动 `github.com/jackc/pgx/v5`，直连明文 TCP，连接 URL 固定 `sslmode=disable`）。默认端口随之变成 `5432`——但只在命令行没写 `-P`、环境也没给 `IMEX_PORT` 时生效。本轮在 PostgreSQL 14.24 上实测。取值不区分大小写、裁两端空白，`--engine PG`、`--engine " pg "` 与 `--engine pg` 是同一台服务器、同一套语句、同一份产物（131 轮统一）；配置回显仍照你写的拼写打印，那行记的是「这次给了什么」。

```bash
imex export --engine pg -h 127.0.0.1 -u pgprobe -p pass -D pgprobe \
  -q "SELECT * FROM users" -o /backup/users.del
imex import --engine pg -h 127.0.0.1 -u pgprobe -p pass -D pgprobe \
  -i /backup/users.del -t users
imex list --engine pg -h 127.0.0.1 -u pgprobe -p pass -D pgprobe tables
```

**能用的**：`export -f del|parquet|sql`（单线程，或 `--threads 1–256` 并行分片导出）、`import -f del|parquet` 的 `--mode insert|replace|insert_update` 与 `--threads 1–256`、`list` 的七个对象（`dbs tab vws col idx trg rtn`）。MySQL 一侧的行为与产物一字未变，`--engine` 不给就是 `mysql`。

`--mode replace` 在这一引擎上是「`DELETE FROM 表`，不带任何会话开关」：MySQL 侧那句 `SET FOREIGN_KEY_CHECKS=0` 是 MySQL 语法，pg 没有等价设置，工具不发也不编造。代价是引用完整性关不掉——被别的表引用的表在清空时报 `SQLSTATE 23503`、退出 `1`，且原有行一条不少（实测 `parent` 两行仍在）。这是两引擎的真实行为差异，不是缺陷。

`--mode insert_update` 的冲突键由目录表读出而不是猜：有主键用主键（其余唯一键另记一条 warn，因为 `ON CONFLICT (a)` 不会在 `b` 的冲突上触发而 MySQL 会）；无主键但只有一把唯一键就用它；无主键且有**两把以上**唯一键则退出 `1`，并且发生在写入任何数据之前。没有主键也没有任何唯一键时语句退回普通 `INSERT` 并给出 warn（重复导入会翻倍），与 MySQL 侧同口径。PostgreSQL 对新行和被改行都记 1 行受影响，`RowsAffected` 分不出插入与更新，所以这条语句带 `RETURNING (xmax = 0)` 逐行回执，`inserted`/`updated` 按回执计数、`unchanged` 由「文件行数 − 回执行数」得出；子句末尾另有 `IS DISTINCT FROM` 守卫，值已经完全相同的行不改写，实测那一行的 `xmin` 与 `ctid` 都不动，与 MySQL 记 0 受影响行的口径一致。文件里**同一个冲突键出现两次**时两引擎不同：这些行落进同一条语句时 PostgreSQL 拒绝执行（`ON CONFLICT DO UPDATE command cannot affect row a second time`，`SQLSTATE 21000`）、整个导入回滚并退出 `1`，MySQL 则按文件顺序逐行应用、最后一条生效；给同一份文件加 `--batch-size 1` 后两侧结果与计数一致，工具不替你重排或去掉文件里的行。

**不能用的**（连库之前就被拦下，退出码 `2`，文案点名原因；这张表就是方言层的数据，见 `internal/backend/refusals_postgres.go`——按 export / import / list / show 四条命令各存一份有序规则，MySQL 那份为空，`cmd` 只负责把命中的规则套进同一句模板）。**导入侧 (`--engine pg`) 已无任何拒绝规则**，下表只是导出侧、list、show：

| 命令 / 选项 | 拒绝理由 |
|-------------|----------|
| `show evt` | PostgreSQL 没有事件调度器，没有可打印的事件定义；定时作业在 `cron` 或 `pgAgent` 里 |
| `list evt` | PostgreSQL 没有事件调度器，定时作业在 `cron` 或 `pgAgent` 里 |
| `export --no-hex-blob` | `bytea` 走文本协议时裸 NUL 报 `SQLSTATE 22021`，pg 侧恒用 `0x` 十六进制，关不掉 |

曾经被拒、现已放行的：`export --threads > 1` 的并行分片导出——分片元数据改读 `pg_catalog`（主键／唯一键／列类型／行数估计）、标识符用双引号、占位符用 `$n`、边界采样与 `MIN/MAX` 也按 pg 方言，每个 worker 各自开 `BEGIN ISOLATION LEVEL REPEATABLE READ`；产物与 `--threads 1` 逐字节一致（含 `BOOL`／`DATE`／`TIMESTAMP`／`FLOAT` 的文本改写），**PG 15／16／17 真库实测通过**；`-f sql` 随 `--sql-dialect` 增量打通（导出按 pg 方言写、导入由方言无关读取器解析后重新绑定，不再整体拒绝）；`--session-var` 与 MySQL 同一口径生效（每条新物理连接执行 `SET`，见上「会话变量」）；`--mode create` 则不是引擎级拒绝，而是从三个引擎整体移除的非法值（任何引擎传入都 `invalid --mode "create"`、退出 2）。

**取值写法与 MySQL 不同**（这是 pg 的原生文本，不是转译成 MySQL 样式）：`boolean` → `t`/`f`；`timestamp` → `2026-01-31 09:26:15.123456`；`timestamptz` → `2026-01-31 09:26:15.123456+08`；`date` → `2026-01-31`；`real`/`double precision` → Go 最短往返写法；特殊值 `infinity`/`-infinity` 原样透传，`±Inf`/`NaN` 写成 `Infinity`/`-Infinity`/`NaN`；`bit(n)`/`bit varying(n)` → 比特串本身（`10100101`、`101`，与 `psql -t -A` 一致；变更记录 129 之前它被当成二进制列打成 `0x3130313030313031`，即那串 ASCII 的十六进制）；NULL 是一个**空字段**（两个分隔符之间没有内容，与 MySQL 侧同一套 DEL 布局规则，不是 `-`）；`bytea` 恒为 `0x…`。

**`--default-charset` 在 pg 侧是 `client_encoding`**，只认五个名字：`UTF8`、`LATIN1`、`GBK`、`GB18030`、`SQL_ASCII`（写 `utf8mb4`/`utf8` 会落到 `UTF8`，`latin1`/`iso-8859-1` 落到 `LATIN1`，`gbk`/`936` 落到 `GBK`，`gb18030`/`5488` 落到 `GB18030`，`ascii`/`us-ascii` 落到 `SQL_ASCII`）。表外取值是用法错误、退出 `2`：`invalid --default-charset "nonsense" with --engine pg: PostgreSQL has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)`。

**两级命名空间**：PostgreSQL 是「库 + schema」，而一个连接只能进一个库，所以 `-D` 归连接用，对象范围取该角色 `search_path` 的第一个 schema（`current_schema()`）。要换 schema，由连接角色的 `search_path` 决定，工具不加新选项。不给 `-D` 时默认排除 `pg_catalog`、`information_schema`、`pg_toast` 三个系统 schema，`--all` 取消排除。`list` 输出里那一列的名字仍叫 `database`，pg 下它装的是 schema 名。`list columns`/`list indexes` 仍要求 `--database`，与 MySQL 侧同口径。

### 达梦引擎（`--engine dm`）

加 `--engine dm` 即连达梦 DM8（驱动 `gitee.com/chunanyong/dm` v1.8.23，纯 Go、无 CGO，连接串是驱动自己的 `dm://user:password@host:port?k=v`）。这一引擎**恒以 Oracle 兼容模式连接**——DSN 里固定带 `compatibleMode=oracle`，不给开关，也不提供达梦原生模式。默认端口随之变成 `5236`——同样只在命令行没写 `-P`、环境也没给 `IMEX_PORT` 时生效。取值不区分大小写、裁两端空白，`dm`、`DM`、`" dm "` 与别名 `dameng` 是同一台服务器、同一套语句；`postgres`、`oracle`、`dm8` 这类拼法是用法错误退出 `2`（`invalid --engine "dm8": must be one of mysql/pg/dm`）。

`-u` 与 `-p` 是**原样**写进连接串的，不做 URL 转义（只有 `?` 后面的参数部分才转义）。这是驱动自己的读法决定的：它手写解析这条串——最后一个 `?` 之后是参数、最后一个 `@` 之前是账号、第一个 `:` 分账号与口令——并且**从不解码**，尽管它自己拼串时是转义的。于是转义过的口令到服务器那儿成了字面的 `%23`，达梦回 `Error -2501: Invalid username or password`。实测有 22 个 ASCII 字符会被 `net/url` 转义（`` !"#%'()*/:<>?@[\]^`{|} ``），按同一处「从不解码」的实现它们都读不回来；其中 `#`、`%`、`@` 三个在真实 DM8 上取过样本——141 轮建了三个分别含这三字的账号，修复前一律 `-2501`，修复后登录、导出、导入全部走通。唯一仍然无法表达的取值是口令里含字面 `?`：它会把参数分界挪走，驱动也没给转义的办法（转义版同样读不回来，两种写法都不行）。

> **这一节已有真库往返**（2026-11-05，沙箱内自装的独立 DM8 实例：`DM Database Server 64 V8`、构建号 `03134284604-20260707-335949-20228`、`DB Version 0x7000d`、x64、UTF-8 字符集、`CASE_SENSITIVE=1`、`COMPATIBLE_MODE=0`（服务端）——`compatibleMode=oracle` 是客户端参数，见 2.1.12）。26 列 × 8 行的类型矩阵在 dm→dm 的 DEL 与 parquet 两条路径上各自往返，逐列 NULL 安全比对差异 **0**；dm→MySQL 与 MySQL→dm 也各跑一遍。仍未实测的只剩少数几项（列存储表上的攒批插入、v8.1 这条版本下界、`±Inf`/`NaN` 在其它 DM 版本上的写法、多行 VALUES 的失败形态），逐条见 `docs/requirements.md` 11.2。

```bash
imex export --engine dm -h 127.0.0.1 -u dmprobe -p pass -D DMPROBE \
  -q "SELECT * FROM users" -o /backup/users.del
imex import --engine dm -h 127.0.0.1 -u dmprobe -p pass -D DMPROBE \
  -i /backup/users.del -t users
imex list --engine dm -h 127.0.0.1 -u dmprobe -p pass -D DMPROBE tables
```

**能用的**：`export -f del|parquet|sql`（单线程，或 `--threads 1–256` 并行分片导出；任意 `--query`，DEL 布局修饰项照常生效；`-f sql` 按达梦（Oracle 兼容）语法写：双引号标识符、二进制 `HEXTORAW('..')`、串字面量只翻倍单引号且反斜杠原样）、`import -f del|parquet|sql` 的 `--mode insert`（出厂默认值）与 `--mode replace`（`-f sql` 由方言感知的读取器回放，按目标引擎取达梦语法——`HEXTORAW` 解码为字节、串里的反斜杠当字面量，因此一份达梦 `-f sql` dump 能导回达梦；它是同引擎回放解析器，不读任意外部 Oracle/dexp dump，跨引擎搬运仍走 del/parquet）、`list` 的八个对象（除 `events` 外全部：`databases`、`schemas`、`tables`、`views`、`columns`、`indexes`、`triggers`、`routines`，`tables` 的 `--type base|view|all` 可用）。`list` 八个对象的输出列名与 MySQL 侧逐列相同，解析表头的下游脚本不必为引擎分叉。导入侧的 `--threads` 不在拒绝之列——多 worker 各自攒批发的是同一句方言 INSERT，与引擎无关；导出侧的并行也已放行：分片度量改读达梦的 Oracle 兼容目录（`ALL_CONS_COLUMNS`＋`ALL_CONSTRAINTS` 取主键、`ALL_TAB_COLUMNS` 取列类型与可空性、`ALL_TABLES` 取 `NUM_ROWS`／`BLOCKS`，平均行长按 `BLOCKS × SF_GET_PAGE_SIZE() / NUM_ROWS` 折算），分片语句按达梦语法下发（下一段）。MySQL 一侧的行为与产物一字未变，`--engine` 不给就是 `mysql`。

**并行导出在达梦上与 MySQL 有三处不同**（2026-11-14 真 DM8 实测）：① 达梦没有**行值比较**——`WHERE (A, B) >= (15, 1)` 报 `Error -2007: nearby [15] has error`，所以复合键的分片边界改写成同一序的**字典序展开**（`A > 15 or (A = 15 and B >= 1)`：前缀项一律严格比较、只有最后一项取含等号，否则 `(15, 0)` 这样的行会同时落进相邻两片）。单键边界与 MySQL／PG 同形。② 分片语句是**按目标引擎的语法重新渲染**的：`--query` 过 MySQL 解析器再序列化回文本时，会带上反引号与 MySQL 的反斜杠转义（`'O\'Brien'`）——这两种形态达梦和 PG 都读作语法错误，而单线程路径把用户原文照发、从不遇到。规则是「用户怎么写就怎么发」：标识符裸写（让服务端自己的大小写折叠照常生效），只有该引擎不接受裸写的保留字才加引号（达梦实测 53 个词、PG 15 个词，名单取自解析器关键字域在两个语法位置上真库报错码的逐一分类，不是抄文档）；串字面量解码后按 SQL 标准翻倍单引号重发。MySQL 一侧整个渲染跳过，发出的语句一字未变。③ worker 数 + 1 与 `MAX_SESSIONS` 的预检读 `V$DM_INI`，只持 `RESOURCE` 的普通账号在那里拿 `Error -5504: 没有 […] 对象的查询权限`，因此这条预检对这类账号是惰性的（记 `debug`、不记 `warn`，并行照常跑）。`-f parquet` 与 MySQL／PG 同规则不并行（分片文件无法拼接），`--threads>1` 时自动降级单线程并记一条 info。

`list databases` 回答**库**这一层：达梦一个实例只有一个库，所以它读 `V$DATABASE`、只回一行（本机是 `DAMENG`）；`-D` 在这条命令上过滤库名，并且不写进 DSN——库名不是模式，写进去会在登录时被当成模式名拒掉（`Error -2103`）。**模式**是另一层，用 `list schemas`（别名 `sch`）看：`SELECT DISTINCT OBJECT_NAME FROM ALL_OBJECTS WHERE OBJECT_TYPE = 'SCH'`，不带 `--all` 回 `DMPROBE`、`VPROBE` 两行，`--all` 回全部 7 个（`CTISYS`、`DMPROBE`、`SYS`、`SYSAUDITOR`、`SYSDBA`、`SYSSSO`、`VPROBE`）。选 `ALL_OBJECTS` 而不是 `DBA_USERS`，是因为它不丢 `CTISYS`（内置全文检索模式、无登录账号但 `-D CTISYS` 可用）；也没用 `V$DATABASE`，因为库名不是模式名。它同样不需要 DBA 权限——只持 `RESOURCE` 的普通账号也读到 7 行，而 `SYS.SYSOBJECTS` 对它会报 `No select privilege`。`show sch` 取 `DBMS_METADATA.GET_DDL('USER',…)`：达梦的模式与账号是同一个对象，`GET_DDL('SCHEMA',…)` 报 `-20001 invalid object type`，所以输出是 `CREATE USER … IDENTIFIED BY <PASSWORD>`——口令是服务端元数据层自己的占位符，不能直接重放；`CTISYS` 这类没有账号的内置模式会报 `-20008 object is not found`。

`-D` 在达梦上对表、视图、触发器、例程、列、索引是**模式名**（达梦是「一个角色一个模式」的结构）：它既写进驱动的 `schema` 参数，也是目录查询过滤的 `OWNER`。**这一项区分大小写，且是登录这一关定的**：Oracle 模式把未加引号的名字存成大写，`-D dmprobe` 在 ping 阶段就报 `Error -2103: 无效的模式名[dmprobe]`（实测，六类对象一致），得写 `-D DMPROBE`；目录查询里虽然用 `UPPER(OWNER) = UPPER(?)` 比较，但那层容错发生在登录之后，够不着这个错。`list schemas` 上 `-D` 过滤 `OBJECT_NAME`、`list databases` 上过滤库名 `NAME`，这两条命令不把 `-D` 写进 DSN（不存在的名字回 0 行，而不是登录失败），也只有在这两条上小写是认的。不给 `-D` 时 `list schemas`/`list tables` 默认排除服务端自带的系统模式 `SYS`、`SYSDBA`、`SYSSSO`、`SYSAUDITOR`、`CTISYS`（实测名单：前四个在 `DBA_USERS` 里，`CTISYS` 是全文检索用的内置模式、无登录账号但持有 `SYSCONTEXTINDEXES`），`--all` 取消排除。`-u` 不能为空，为空在建连之前退出 `2`（`--user is required for --engine dm`）。标识符用双引号包裹、内部双引号翻倍（`a"b` → `"a""b"`）；Oracle 模式把未加引号的名字折成大写，要保住原名大小写唯有加引号这一条路。

`list views`／`list triggers`／`list routines` 读 `ALL_VIEWS`／`ALL_TRIGGERS`／`ALL_PROCEDURES` 而不是 `DBA_*`：实测同一台服务器上，只持 `RESOURCE` 的普通账号查 `SYS.DBA_VIEWS`／`DBA_TRIGGERS`／`DBA_PROCEDURES` 一律 `Error -5504: 没有 […] 对象的查询权限`，而三张 `ALL_*` 视图正常返回它有权看到的行。三者的列名与 MySQL 侧逐列相同，但达梦目录缺的信息印成空格子、不猜：`list views` 的 `check_option` 无处可读——`ALL_VIEWS.TEXT` 只有查询文本，`CREATE VIEW … WITH CASCADED CHECK OPTION` 只在 `DBMS_METADATA.GET_DDL` 的产物里（`show vw` 打的就是它）；`is_updatable` 只有一个候选列 `READ_ONLY`，本构建 244 行全是 `NULL`，映射写好但当前恒空；`security_type` 是实测常量 `DEFINER`（普通账号拿到视图的 `SELECT` 授权后能读视图、读基表却报 `-5504`，即按属主权限执行）。`list triggers` 的 `timing` 由 `TRIGGERING_TYPE` 折掉行模式（`BEFORE ROW`→`BEFORE`、`INSTEAD OF ROW`→`INSTEAD OF`），`event` 原样输出——达梦允许一个触发器挂多个事件，存成 `DELETE OR INSERT OR UPDATE` 这样的一格，MySQL 的一触发器一事件表达不了；`statement` 是 `TRIGGER_BODY`——它在本构建里存的是整段触发器源码（含 `CREATE OR REPLACE TRIGGER …` 头），与 MySQL 的 `ACTION_STATEMENT` 只放 body 不同；达梦没有 body-only 的列，故原样输出、不在 SQL 里猜切点。`list routines` 没有 `DBA_ROUTINES` 可用（`SELECT COUNT(*) FROM SYS.DBA_ROUTINES` 报 `-2106 无效的表或视图名`），改读 `ALL_PROCEDURES` 并只取顶层例程（`PROCEDURE_NAME IS NULL`）：包本身是 `OBJECT_TYPE='PACKAGE'`、包子程序各占一行并带自己的 `PROCEDURE_NAME`，不过滤会把一个包当成一个例程；`security` 由 `AUTHID` 映射成 `DEFINER`／`INVOKER`，`deterministic` 目录里就是 `YES`／`NO`，`data_access` 没有对应列、留空。

`list indexes` 只列**声明过的**索引，达梦给每张表自动建的那一个聚簇索引不在其中：它叫 `INDEX335554xx`、`INDEX_TYPE` 是 `CLUSTER`，`DBMS_METADATA.GET_DDL` 的产物里根本没有列清单（`CREATE OR REPLACE CLUSTER INDEX … ON "DMPROBE"."FX_DST"( STORAGE(ON "MAIN", CLUSTERBTR) ;`），`DBA_IND_COLUMNS` 因此没有它的行，与 `DBA_IND_COLUMNS` 的内连接自然把它挡在外面。这样才对得上 MySQL：`information_schema.STATISTICS` 也不会为一张没声明索引的表虚构一行（实测 MySQL 8.0.46 同形状的表只回表头）。所以 `list indexes -D DMPROBE -t FX_DST` 回 0 行不是漏行——那张表确实只有聚簇索引，没有主键也没有二级索引；建表时写了 `CLUSTER PRIMARY KEY` 的另算，它的索引有列清单（一行，`UNIQUE`／`CLUSTER`），照常出现。`seq` 原样输出，函数索引在达梦里是 `-1`、`index_type` 是 `FUNCTION-BASED NORMAL`，不折成 MySQL 的从 1 开始。

**连接超时（156 轮修复）**：达梦驱动 `gitee.com/chunanyong/dm` v1.8.23 在建连之后**不设任何 socket 超时**，且其单机连接路径把调用方 context 丢成 `context.Background()`，所以服务端「TCP 接了但不回应」（容器休眠、服务僵死、半开防火墙）时旧版会**永久挂起**，日志停在 `list config` 之后、没有任何输出。现在 `--connect-timeout`（默认 5s）真正约束 TCP 拨号与登录握手，`--socket-read-timeout`／`--socket-write-timeout`（默认 5m）约束握手之后的每次读写，`list`／`show`／`exec` 的语句另有 context deadline 兜底。冻结 DM8 容器实测：旧 `v0.31.0` 12 秒外层强杀仍卡住，修复后 **4.10 秒**退 1 并报 `i/o timeout`／`context deadline exceeded`，恢复容器后 `list dbs` 秒回 5 行。想更快报错就调小这两个时长（如 `--connect-timeout 3s --socket-read-timeout 10s`）。注意 `export`／`import` 的数据读写阶段仍是裸查询，只有建连与 ping 受这层保护。

**不能用的**（连库之前就被拦下，退出码 `2`，文案点名原因，形状与 pg 侧一致；这张表同样是方言层的数据，见 `internal/backend/refusals_dameng.go`）：

| 命令 / 选项 | 拒绝理由 |
|-------------|----------|
| `show evt` | DM8 Oracle 模式没有 MySQL 式的事件调度器，没有可打印的事件定义；定时任务在 DM 的 job 包里 |
| `list events` | DM8 Oracle 模式没有事件调度器，没有可列的事件；定时任务在 DM 的 job 包里。其余八个对象都可用（`views`／`triggers`／`routines` 读 `ALL_*`，见上） |
| `--mode insert_update` | 方言层的 upsert 在达梦上根本没实现，绕过 CLI 直接调库的调用方同样失败 |
| `--session-var` | `SET <name>=<value>` 是 MySQL 会话语法，本工具不翻译，请写进服务器或角色的配置 |
| `--no-hex-blob` | 达梦拒绝文本值里出现 NUL 字节，二进制列恒以 `0x` 十六进制写出，关不掉 |

> `--mode create` 不在上表：它不是达梦级拒绝，而是从三个引擎整体移除的非法值（任何引擎传入都 `invalid --mode "create"`、退出 2）。`--session-var` 在 pg 侧已随本版本放行，达梦侧仍未翻译、保持拒绝。

**取值写法**：驱动对每一个日期时间类型都交回 Go 的 `time.Time`，工具把它重排成达梦自己打印的样子——Oracle 模式的 `DATE` 带时分秒，写成 `2026-01-31 09:26:15`（按日期处理就会把时间部分丢掉）；`TIMESTAMP`/`DATETIME`/`DATETIME2` 同形后跟最多六位小数、尾零不印；`TIME` 写成 `15:04:05`；四个 `WITH TIME ZONE` 类型在各自形态后**保留偏移、不折成 UTC**，服务器给 `2024-05-06T07:08:09.123456+08:00`，文件里就是 `2024-05-06 07:08:09.123456+08:00`。`BIT`/`BOOLEAN` 一律写成一个数字（`1`/`0`）。二进制列（`BLOB`/`RAW`/`LONG RAW`/`BINARY`/`VARBINARY`/`LONGVARBINARY`/`IMAGE`）恒为 `0x…`。`+Inf`/`-Inf`/`NaN` 照达梦存的样子写出，不做归一。NULL 是一个空字段、空串是 `""`，与 MySQL 侧同一套 DEL 布局规则。

**导回达梦时的偏移量写法**：带时区的三个时间戳类型（`TIMESTAMP WITH TIME ZONE`、`DATETIME WITH TIME ZONE`、`TIMESTAMP WITH LOCAL TIME ZONE`）在导入侧会被补一个空格再发出去——文件里是 `…:09.123456+08:00`，绑进语句的是 `…:09.123456 +08:00`。达梦 Oracle 模式的字面量解析只认后者，前者一律 `Error 6015: Invalid datetime type format`、整批一行不进（141 轮实测：`+08:00`、`-07:00`、`+00:00` 三种偏移、0/3/6 位小数、`0001-01-01` 都在加空格后按同一时刻入库；`+0800` 这种没冒号的写法与 `[]byte` 绑定都读不进去，各报 `6015` 与 `6007`）。这条改写只认「偏移量直接贴在秒后面」的形状，已经带空格的、不带偏移的、以及 varchar 里恰好长成这样的值都原样发出；它由目标列的类型判定，因此 MySQL 与 PostgreSQL 两侧的行为一字不变，产物文件的字节也不变（同一张 26 列表用修复前后两个二进制各导一次，产物逐字节相同）。`TIME WITH TIME ZONE` 是另一回事：达梦对它的字符串绑定无论加不加空格都报 `Error 6103: Invalid datetime value`，只有 Go 的 `time.Time` 才收，因此这一列的 DEL/parquet 往返在达梦侧走不通，工具不伪造。

**绑进达梦时的取值形状**（142 轮）：达梦驱动会把交给它的**文本**再用 64 位浮点解析一遍，所以 15 位以上的数字会漂、超过 int64 的整数会**静默回绕**、而 `BIT`/`BOOLEAN` 列只认 `"0"`/`"1"`/`"true"`/`"false"` 四个词，别的一律落 `0`。导入侧因此按目标列的类型改绑定形态（不是改文件）：`NUMBER`/`DECIMAL`/`NUMERIC` 走驱动的精确小数（实测 `899999999999999.99999` 原样落库，旧二进制落 `900000000000000`；`99999999999999999999` 进 BIGINT 现在是服务器的 `Error -6102: Digital overflow`，旧二进制悄悄存成 `7766279631452241920`），`TINYINT`/`SMALLINT`/`INT`/`BIGINT` 先按四舍五入远离零取整再精确绑定（与今天文本路的落点一致，只把越界的那些从回绕换成报错），`BIT`/`BOOLEAN` 归一到「非零即 1」，parquet 与 `--hex-blob` 转储里的原始字节进数值列时先变成它承载的无符号整数文本（`0xaa`→170，旧二进制整批 `Error 6007: Data convertion error`）。MySQL 与 PostgreSQL 一侧什么都不改：两侧对每一列都交回「原样绑定」，同一份夹具导进 MySQL 的 `DECIMAL(20,5)` 在修复前后逐字节同值。

**跨引擎导入达梦时，二进制与 `BIT` 列请用 `--hex-blob` 或 parquet**：MySQL 的 DEL 转储默认把 `BIT`/`BLOB` 的存储字节**原样写在字段的引号里**（只有字符集装不下这些字节时才强制 `0x…`），那种串到读取器手里已经是文本、没法和真正的列内容区分。工具只把「不含任何可打印字节」的短串（≤ 8 字节）按载荷处理——`BIT(1)` 的 `0x01`/`0x00` 因此能正确落 `1`/`0`；而像 `BIT(8)` = `0x41`（恰是字符 `A`）这种含可打印字节的载荷**故意不猜**，它仍按文本走，结果是 BIT 目标落 `0`、数值目标报 `6007`。要可靠地搬这些列，导出时加 `--hex-blob`（`0x…` 记法），或改用 `-f parquet`（按文件自己的 schema 认字节）：实测这两种写法在达梦的 `BIT`/`BOOLEAN`/`INT`/`NUMBER`/`FLOAT` 五类目标上逐格正确落库。

**把这份文件导进 MySQL/PostgreSQL 时怎么建列**（现测，MariaDB 10.6.23 严格模式与 PostgreSQL 14.24）：`DATE`/`TIMESTAMP`/`DATETIME` → `DATETIME`/`DATETIME(6)`（pg `timestamp`），**不要**建 `DATE`——那一列会把时分秒静默截掉，导入退出码仍是 `0`；带 `WITH TIME ZONE` 的列 pg 建 `timestamptz` 直接可用，MySQL 没有任何日期时间列能接 `'…+08:00'`（一律 `Error 1292`），要么建 `VARCHAR`/`TEXT`，要么在导出语句里由服务端先剥掉偏移；`BIT`/`BOOLEAN` → MySQL 建 `TINYINT(1)` 总可用；`BIT(1)` 自 `v0.23.0` 起也能直收 DEL 里的对端布尔文本 `'1'`/`'0'`（旧的 `Error 1406` 在 DEL 路径已消除，见变更记录 144），惟 parquet 的 BIT 载荷仍建议 `TINYINT(1)` 或按上一条加 `--hex-blob`；pg 的 `bit(1)`/`boolean`/`smallint` 都可用；`FLOAT`/`DOUBLE` 若可能存 `±Inf`/`NaN`，MySQL 侧建 `TEXT`/`VARCHAR`（`DOUBLE` 报 `Error 1366`），pg 侧 `double precision` 认这三种拼法；零值时间 `0001-01-01 00:00:00` 超出 MySQL `TIMESTAMP` 的范围，用 `DATETIME`；`VARCHAR2` 里的长数字串照字符落库，别默认建 `BIGINT`（26 位就 `Error 1264`）。逐列细节见 `docs/requirements.md` 的 2.1.12 与 11.2。

**批次与超时**：占位符是位置 `?`，与 MySQL 同形，攒批 INSERT 每行重复 `(?,?)`；单条语句参数上限 65535。批次字节预算这一维在达梦没有可证的等价物（Oracle 模式没有 `max_allowed_packet` 那种会话可见的语句字节上限），因此取刻意保守的 **4 MiB** 回退值，日志直说按 4 MiB 估——宁可批次小，也不赌服务端会收下更大的语句。超时按驱动自己的单位换算：`--connect-timeout` 进 `connectTimeout`（**毫秒**），`--socket-read-timeout` 向上进位进 `socketTimeout`（**秒**）；`--socket-write-timeout` 在驱动侧没有对应参数，它不改变达梦连接的写超时。

`--mode replace` 在达梦上就是一条 `DELETE FROM "表"`：达梦没有会话级「关掉外键检查」的开关，工具不发也不编造那两句。代价与 pg 侧同性质——被别的表引用的表在清空时由服务端报引用完整性错误、退出 `1`，原行一条不少。

瞬断重试只认驱动自己那四个连接层错误码（`6001` 通信异常、`9007` 未知网络、`9008` 无效连接、`20001` 连接切换失败），其余一律终态；`6001` 这一条是在沙箱里对着无人监听的 5236 端口实测出来的，其余三个照驱动源码收录。负数码与超出 16 位的正码不参与匹配，免得截断后把一条注定失败的语句误判成可重试。

**已知风险**：① 达梦手册言明多行插入「不支持列存储表」，向列存表导入时攒批 INSERT 可能报错——第一版不特化，遇到就原样透传服务端错误；② `V$DM_INI` 与 `DBA_*` 的可见性依角色权限而定，读不到时连接上限与探列各走自己既有的失败路径。

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
> 降级的全部触发点（命中即记一条 warn）：查询含 JOIN/派生表等复杂特性、无法从语句提取表名、检测不到可用分片键、分片只能切出一段。最后一种现在只剩 `MIN()==MAX()`（以及采样失败后回落成一段）——**字符串/UUID 主键已不再降级**，见下条。复合主键/唯一键一直是支持的，但只对首列切区间，其余列保持全范围，因此每个元组仍恰好覆盖一次；键选取过程可用 `--log-level debug` 看到（`using primary key for sharding columns=…`）。
>
> 分片边界是**采样**出来的，不再只是按 `MIN`/`MAX` 等宽切：先从 `INFORMATION_SCHEMA.TABLES` 读整表的 `TABLE_ROWS` 与 `AVG_ROW_LENGTH`，再用 `WHERE 键 > ? ORDER BY 键 LIMIT 1 OFFSET 每段行数-1` 逐段定位边界（边界走绑定参数，段行数直接内联为整数），因此稀疏键、倾斜键、CHAR/VARCHAR/UUID 这类非数值键都能切出等量的段。实测一张 100 万行、`id` 跨度 1…2 亿的稀疏表：等宽切出的 4 段是 200,000 / 0 / 0 / 800,000 行（两个 worker 全程空转，最慢段占全表 80%），采样切出的 8 段是 123,749×7 + 133,757 行。等宽仍然保留，且在两种情况下直接采用：`TABLE_ROWS × AVG_ROW_LENGTH < 256 MiB` 的稠密整数键（小表不值得为采样多跑查询，实测 `bench_lay` 20 万行的边界与等宽结果**完全一致**，代价只是多一条元数据查询），以及键值域密度可证地接近均匀时（值域跨度 ≤ 行数 × 3/2 时等宽与理想切分的偏差不超过 1.5 倍，判据用 `big.Rat` 精确算，`BIGINT` 跨度 2^63 也不会误判）。采样的全部成本可测：8 段需要 7 次探针查询，同一张稀疏表上端到端导出由 0.84 s 变为 0.90 s（3 次中位数，`--threads 4`），增量即探针开销，与一次全表扫描（0.128 s）同量级，且只在真正并行时发生。任何一步失败（统计缺失、权限不足、探针报错、取到 NULL）都记一条 WARN 并**原样回落**到等宽/单段行为，绝不因为采样而让导出失败或改变产物内容。
>
> 并行导出按分片序号顺序拼接结果，**不保证**与 `--query` 的全局行序一致；需要稳定全局顺序时使用单线程导出。分片边界使用精确整数/十进制运算，`BIGINT`/`DECIMAL` 超过 2^53 也不会丢行；每个分片有各自的一致性快照，分片之间不是同一时间点。带 `--query` 的 `WHERE` 过滤时边界仍按**整表**统计来采样，可能被过滤后的实际分布误导（只会不均，不会丢行或重复），需要精确均衡时用单线程导出或把键条件写成分段查询。
>
> **`--query` 点名的 schema 优先于 `-D`**：`-q "SELECT * FROM otherdb.tbl"` 时，分片键探测、`INFORMATION_SCHEMA` 统计、`MIN`/`MAX` 与边界采样全部打在 `otherdb.tbl` 上（information_schema 的过滤条件为 `TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())`，查询没点 schema 时参数为空串、仍解析为连接的库）。这一点必须整个链路一致：只让探测走别的库、统计与采样仍走连接的库时，若连接库里恰好有同名表，段边界会来自那张表的键空间，产物静默少行（v0.12.1 实测 8 行跨库表导出 `rows=0`、退出码 0）。日志与错误里的表名同样带库名（`table=otherdb.tbl`），产物仍只写裸表名。
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

- **驱动层 socket 超时**（go-sql-driver 的 `timeout`/`readTimeout`/`writeTimeout` 三个字段）：`--connect-timeout`（默认 5s）、`--socket-read-timeout`/`--socket-write-timeout`（默认 300s）。这三个参数以 DSN 字段（`timeout` / `readTimeout` / `writeTimeout`）形式传入 go-sql-driver，作用于**每次 socket 读/写**；由于是 DSN 级参数，`--session-var` 无法覆盖（同名会被拒绝）。
- **连接池 TTL**：导入连接池（主连接与每个并行 worker）设置 `SetConnMaxIdleTime(1m)` + `SetConnMaxLifetime(5m)`，让连接在 LB 空闲阈值前主动老化重建；并行 worker 另设 `SetMaxOpenConns(1)`。导出连接池**不**做老化（`ConnMaxIdleTime=0`、`ConnMaxLifetime=0`）：它用裸 `START TRANSACTION ... WITH CONSISTENT SNAPSHOT` 建立快照，连接在语句之间会归还空闲池，一旦被当作空闲连接关闭，快照就被静默回滚、后续读取失去一致性。
- **事务空闲提交**：导入的写入端（单线程管线与每个并行 worker）在 30s 内没有收到新批次时，主动 `COMMIT` 当前已写入的部分事务并开启下一个，缩短未提交窗口和重放集合；`--commit-count` 到期或重放缓冲达到 64 MiB 字节预算时同样提交。
- **瞬断自动重试**：对 `driver.ErrBadConn` / `invalid connection` / `io.EOF` / 网络超时 / `broken pipe` / `connection reset` / `server has gone away`(2006) / `lost connection`(2013) 等连接级错误，默认最多 3 次尝试，指数退避约 1s→2s（上限 4s，带 20% 向下抖动，避免多个 worker 同时重连）。重连后**先回放当前事务内所有已执行但未提交的批次，再重试失败批次**——连接断开时 MySQL 已回滚整个事务，只重试单批会静默丢掉同事务中先前的行。回放缓冲同时受 `--commit-count`（1000 行）与 64 MiB 字节预算约束，任一触顶即提交，避免宽 BLOB/TEXT 行撑爆内存。`COMMIT` 本身不重试（响应丢失时无法判断是否已生效，重放可能重复写入），直接报错。`context.Canceled`、语法/约束类错误以及 `1213` 死锁 / `1205` 锁等待超时不重试。 能挺过的停机窗口是**有量值的**：三次尝试之间只有两次退避（1s、2s，各带最多 20% 向下抖动），所以**从第一次失败到放弃实测约 2.4–3 秒**（`-P 3399` 无人听那次整跑 2.675 s），目标库重启或 LB 切换只要在这个窗口内恢复，本次运行就自行接上，超过则整跑失败、退出 1；两个退避参数是编译期常量，**不做成选项**——每次重试都要回放未提交事务，窗口越长重复写入的风险面越大。实测：停 1.5 s 后拉起的一次 10 万行并行导入 exit 0、8 条 retrying、最高只到 attempt 2、落库 10 万行且不重不漏。 同一条判据也管住**开局建连的那次 ping**（`common.PingWithRetry`，导出与导入共用）：以前是两个构造器里各写一遍的 `for i := 0; i < 3`，不看错误性质，于是密码写错（1045）、库不存在（1049）、`--session-var` 写了服务端不认识的变量（1193）都要跑满三次尝试、固定等 6.0 s，末条还写成 `failed to ping database after 3 attempts`，把服务端给的真实原因挤到句尾。现在永久拒绝一次即返回（实测 5–6 ms）、文案按实际尝试次数写（`after 1 attempt`），连接级故障照旧重试到三次，而 `database ping failed, retrying` 只在还有重试时出现（三次尝试配两条告警，最后一次失败不再谎称 retrying）。上面那些 `2006`/`2013` 一类的服务端错误码表属于 MySQL 方言（`internal/backend/dberr_mysql.go`，132 轮自 `internal/common` 下沉），由连接建立时选出的方言以函数值交给重试层；`--engine pg` 自 134 轮起也有一张 SQLSTATE 表（`internal/backend/dberr_postgres.go`）：`08001`/`08003`/`08006`/`08P01`/`53300`/`57P01`/`57P02`/`57P03`/`57P05` 判为瞬态，`08007`（提交结果未知）、`08004`（服务端拒绝建连）、`57P04`（库被删）、`58030`（服务端 I/O）、`57014`/`40001`/`40P01`（取消与并发争用）以及 `28xxx`（认证）刻意不重试——理由与 MySQL 侧 `1213`/`1205` 相同：盲目重放可能重复写入。实测：`ALTER ROLE … CONNECTION LIMIT 2` 下第三条连接报 `SQLSTATE 53300`，旧行为 1 次尝试 0.01 s 失败，新行为 3 次尝试 2.68 s 后失败；导入 50 万行途中 `pg_terminate_backend` 回报 `SQLSTATE 57P01`，旧行为退出 1（27.9 万行已落库），新行为重连回放后 3.011 s 完成 50 万行、退出 0。

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
| `--progress-interval` | `>0` 且 `≥100ms`（定时进度日志间隔；`0` 不是"关闭进度"，进度记录没有关闭开关）| `invalid --progress-interval`，退出码 2：0 与负值报 `must be > 0`，`10ms`/`1ms` 报 `must be >= 100ms`（2026-10-30 由 10ms 提到 100ms：实测 200,000 行导入分 200 批、发送段 1.391 s，约 7 ms 一批，10ms 的下限会让进度行跟着批次走；`1ns` 在过去还是合法值时，一次 6400 行导出打出 1355 条进度、日志 175,557 字节）|
| `--commit-count` | `≥1` | `invalid --commit-count`，退出码 2 |
| `--skip-count` / `--row-count` | `≥0`（`row-count=0` 表示无限制） | 退出码 2 |
| `--log-level` | `debug`/`info`/`warn`/`error`（大小写与首尾空格容错） | `invalid --log-level`，退出码 2（此前非法值会被静默降级为 info） |
| `--log-format` / `IMEX_LOG_FORMAT` | `console`（默认）/ `text`；**两者同时给出时由 env 覆盖**（与其余 `IMEX_*` 开关一致：env 是覆盖，不是补默认值） | `invalid --log-format`，退出码 2（此前非法取值静默回落默认布局） |
| `--format` / `--mode` | 各自枚举白名单 | 退出码 2 |
| `--default-charset` | 字符集白名单；非 `utf8mb4` 不可用于 `parquet`；`gbk`/`gb18030` 不可用于 `sql`（按字节转义不安全） | `invalid --default-charset` / 格式冲突，退出码 2 |
| `--hex-blob` + `--no-hex-blob` | 二者互斥 | `mutually exclusive`，退出码 2 |
| `--char-del` `--col-del` `--row-del` `--escape` `--no-escape` `--timestamp-fmt` `--date-fmt` `--no-char-del` `--no-col-names` | 仅 `--format del` 生效（SQL/Parquet 写端不使用这些修饰） | `--xxx can only be used with --format del`，退出码 2 |
| DEL 分隔符两两同值（`--col-del`/`--row-del`/`--char-del`/`--escape`）| 五对组合必须互不相同：列=行、字符=列、字符=行、转义=列、转义=行；`--escape` 与 `--char-del` 同值**合法**（默认 `"` 即双写转义），`--no-char-del` 免除字符相关项、`--no-escape` 免除转义相关项；判定按固定顺序（char、col、row、escape）取第一条冲突 | `invalid --col-del ",": it is the same as --row-del, which makes the file layout ambiguous`，退出码 2。此前这类组合被接受：实测 200 行导出用 `--col-del , --row-del ,` 得到 14,501 B、**0 个行分隔符**、402 个逗号的文件，用同样参数回读时读出 401 行、**落库 0 行**（全部失败）却只报 `import aborted`|
| `--shard-key`（导出）指向的列 | 显式给出的分片键必须存在于目标表，但**校验只发生在并行路径**；自动检测失败仍回落单线程。`--threads 1` 或并行请求回落到单线程时，这个值既不被读取也不被校验，运行改记 `warn`（`single-threaded export ignores --shard-key`，带 `shard_key`/`threads`/`fallback`/`hint`），配置记录在写时就已确定用不上的场合（`--threads 1`）追加 `shard_key_ignored=true`；退出码仍为 0，产物与不带该选项逐字节一致 | `invalid --shard-key "nosuchcol" for table "t": <原因>`，退出码 2（并行路径；此前只记一条 `warn` 并按单线程跑完、报告成功）|
| `--hex-blob` / `--no-hex-blob` + `--format parquet` | Parquet 二进制列原生按 `binary` 存储，两个开关都不改变输出 | `--hex-blob cannot be used with --format parquet`，退出码 2 |
| `--parquet-compression` | `none`/`snappy`/`zstd` 三选一；仅 `--format parquet` 生效 | 非法值 `invalid --parquet-compression "gzip": must be one of none/snappy/zstd`、格式越界 `--parquet-compression can only be used with --format parquet`，均退出码 2 |
| `encrypt` 的位置参数 | 恰好 1 个（密码明文） | `imex encrypt accepts 1 argument(s), received N`，退出码 2 |

关联校验：`--statement-size`/`--extended-insert` 仅对 `--format sql` 生效（`--create`/`--no-data` 已随「SQL 导出只导数据」一并删除，不再登记，传入即为未知选项）（这一串是导出侧的 SQL 专属校验；导入侧的 `--statement-size` 与格式无关，DEL/SQL/Parquet 三种输入都生效），`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-escape`/`--timestamp-fmt`/`--date-fmt`/`--no-char-del`/`--no-col-names` 仅对 `--format del` 生效，显式传入且格式不符时一律报错而非静默忽略；`--hex-blob`/`--no-hex-blob` 对 SQL 与 DEL 有效，与 `--format parquet` 组合时同样报错（Parquet 二进制列已是原生 binary，开关无意义）；`--mode` 的三个合法值为 `insert`/`insert_update`/`replace`（历史上的 `create` 已整体移除，传入即 `invalid --mode "create"`，退出 2）；导出侧 `-q` 必填，`-q @文件` 从文件读取 SQL（文件不存在、内容为空或 `@` 后缺路径均为用法错误，退出码 2）。这三类判据、报错文案、`--help` 里追加的范围标记（`(only with --format del)` / `(only with --format sql)` / `(not with --format parquet)`）以及配置回显要打印的字段集合，全部由 `internal/config/formatscope.go` 这一张登记表产出，因此不会出现帮助文本说能用、运行时却报错，或反过来回显了一堆被忽略的字段的读感；新增一个格式专属选项只需在表里加一行。另有一类**按执行路径判定**的越界：`export --threads>1` 时显式给出 `--batch-size` 或 `--pipeline-buffer` 即为用法错误（退出码 2），因为它对分片流式路径不改变任何输出；该校验排在 `--threads`→`--batch-size`→`--pipeline-buffer` 的顺序里，因此非法的 `--threads` 或非法的选项值仍先于冲突报出。另有两类**跨选项 / 跨服务器**的判定：DEL 的四个文本修饰 `--char-del`/`--col-del`/`--row-del`/`--escape` 之间必须互不相同（例外只有 `--escape` 与 `--char-del` 同值，即默认的双写转义；`--no-char-del`、`--no-escape` 各自免除相关项），同值写出的文件无法回读，因此按 char→col→row→escape 的固定顺序取第一条冲突报用法错误（退出码 2），同一条组合在导出与导入两侧判定一致；`--threads` 与目标库 `@@max_connections`、并行导入重放缓冲总量的关系则不是范围能表达的，连上库之后各记一条 `warn`（见上表）。SSL/TLS 选项已移除，`--ssl-*` 现为未知选项（退出码 2）。OceanBase 租户选项 `--tenant` 已移除（见下节）；`--strip-lzeros` 与 `--dec-pt`（数值列始终输出 MySQL wire text，小数点恒为 `.`）亦已移除。早期的 `--codepage` 已移除——它只描述文件码页且对二进制列有损；连接/文件字符集改由 mysqldump 惯用法的 `--default-charset` 掌管，二进制列的字符集安全由 `--hex-blob`（非 utf8mb4 自动开启）保证（见上文"字符集与二进制编码"）。

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

导出/导入的运行日志同时打印**行数**与**文件字节数**，行数据全部来自服务端/驱动的真实反馈：导出侧行数按**实际写入文件的行数**统计；导入侧 `read` 按**进入写入管道的文件行数**统计（不含 `--skip-count` 跳过与被 `--row-count` 截断的行），并按每批的 `RowsAffected` 拆成互斥的 `inserted` / `updated` / `unchanged` 三个计数（`insert_update` 模式下一条新行贡献 1、一条被实际改动的已存在行贡献 2、一条写入相同值的行贡献 0，因此 `updated = max(0, affected - rows)`、`unchanged = max(0, rows - affected)`、`inserted = rows - updated - unchanged`，三者之和恒等于该批行数）；`insert`/`replace` 模式的 affected 恒等于行数，全部计入 `inserted`——`replace` 是先把目标表清空再插入，所以文件里每一行都是新插入。批次执行失败的行计入 `failed`。导入、导出均每隔 `--progress-interval`（默认 **5 秒**）打印一次进度：`rate` 是**自开始以来的总速率**（已处理 ÷ 已用时长，平滑，反映整体快慢），`interval_rate` 是**上一个进度点到这一个进度点之间的区间速率**（两次快照之差，反映此刻吞吐与抖动；两者无倍率关系，长跑时看 `interval_rate` 判断当前快不快、看 `rate` 判断整体多少），另打印已处理条数与 `elapsed`。所有耗时字段（进度里的 `elapsed` 与汇总里的 `duration`）固定以**秒**为单位、保留**三位小数**（`842ms` 写成 `0.842s`、90 秒写成 `90.000s`），不再随量级在 `ms`/`s`/`m` 之间切换，跨记录可以直接相减比较。结束日志打印总条数、`bytes`、`duration` 与总速率；运行中止时打印 `import aborted` 汇总（含 `failed`）。**汇总恒为本次运行的最后一条记录**：写汇总之前先停掉进度计时器，而停止会等待正在写的那条进度落笔，因此不会再出现 `completed` 之后跟一条 `progress` 的读感。反过来说，**运行时长不足一个间隔时一条进度也不会打印**：首个计时点要等满一个间隔才触发，停止只等在写的那条记录、不空等剩余间隔，所以一个 3 行的小表 6 ms 就结束，日志里只有横幅、配置与汇总（实测），把间隔调成 `200ms` 后同一类长跑（100 万行）则每 200 ms 一条、共 29 条进度，最后一条仍是 `export completed`。

**阶段计时**：汇总里的 `duration` 不再是一个笼统总数。单线程导出拆成 `connect`（拨号 + ping + 一致性快照，在 `Export` 之前发生，故不计入 `duration`）、`setup`、`first_row`（发起查询到第一行返回，含建文件与写表头——大表的 filesort、锁等待、冷缓冲池都在这里现形）、`write`（第一行到写最后一行）、`finish`（footer + close，parquet 的末段编码也在这一段）；这四段相加即为 `duration`（毫秒取整）。单线程导入拆成 `connect`、`setup`、`header`（打开文件到解析完表头）、`read`（读文件并解析，不含等队列）、`send`（执行 INSERT）、`wait_empty`（写侧等数据）、`wait_full`（读侧等空位）：`read+wait_full` 是读协程的视角、`send+wait_empty` 是写侧的视角，两组各自接近 `duration`，**不能四段相加**。并行导出的每个分片记 `rows`/`duration`/`rate`，并行导入的每个 worker 同样记 `rows`/`duration`/`rate`。实测 20 万行 DEL：导出 `duration=0.160s connect=0.001s setup=0.000s first_row=0.004s write=0.152s finish=0.003s`；导入 `duration=1.875s read=0.409s send=1.391s wait_full=1.364s wait_empty=0.002s`（读侧 `0.409+1.364≈1.77s`，写侧 `1.391+0.002≈1.39s`）。

三个阈值告警只在异常时出现，健康运行一行都不多：`export scan stalled`（扫描连续 **10 秒**取不到行，每 64 行读一次时钟；实测把 `SLEEP(12)` 夹在第 500 行得到 `gap=12.000s` 且这 12 秒记为 `write` 而不是 `first_row`）、`slow insert batch`（**单批**超过 **5 秒**，带 `rows`/`bytes`/`duration`；实测触发器每行 `SLEEP(0.002)`、`--batch-size 5000` 记 `duration=10.430s`，而同样的数据用默认 `--batch-size 1000` 分成 5 批各约 2 秒就不告警）、`slow commit`（单次提交超过 **5 秒**，带 `session`/`rows`/`duration`）。

版本横幅（`imex starting`，含 version/build_time/git_commit/go_version/command）始终作为运行的**首行**日志输出，随后是配置参数快照（`export/import config`，由 `NewExporter`/`NewImporter` 在**建立连接之前**打印，因此连不上服务端时也看得到参数；原先重复它的 `export started`/`import started` 已删除）。快照只打印**所选 `--format` 真正读取的字段**，判据与用法错误的拒绝逻辑、`--help` 里的范围标记同出自一张登记表 `internal/config/formatscope.go`：`-f sql` 不再打印 `char_del`/`col_del`/`row_del`/`escape`/`timestamp_fmt` 这些被忽略的 DEL 布局，`-f del` 不再打印 `statement_size`/`no_create`/`extended_insert`，`-f parquet` 两类都不打印（实测：`export` 的回显改动前对三种格式是**逐字节相同**的同一套 26 项，现在分别是 25/20/15；`import` 现在是 29/20/20）。字段按「连接 → 输入输出 → 并行与流水线 → 字符集 → 该格式的专属布局 → 日志」排列，且补齐了 DEL 写入器一直在读、快照却从未打印的 `date_fmt`、`no_escape`、`no_char_del` 三项；`hex_blob` 打印的是**生效值**（`--hex-blob`/`--no-hex-blob` 都没给时由字符集决定），与写入器用的是同一个判定函数；唯一例外是 `imex version` 子命令——它只往标准输出打印版本、Git commit、构建时间与 Go 版本四行，不产生任何日志记录（这些字段与横幅完全重复），因此可以安全地 `imex version | grep ...`。所有记录的消息统一为**小写、单行的 `<对象> <事件>`**：主要事件词是 `config`/`progress`/`completed`/`aborted`/`failed`（`started` 只出现在版本横幅 `imex starting` 里；如 `export config`、`shard export completed`、`insert batch failed`、`worker completed`），另有 `retrying`/`executing`/`applied`/`derived` 等状态词与交互 SQL 记录 `db interaction`（见「SQL 交互回显」），诊断告警则以 `slow` 开头（`slow insert batch`、`slow commit`）或以 `stalled` 结尾（`export scan stalled`），大小写敏感的一条 grep 就能覆盖全链路；配置、进度、完成、中止每次运行至多一条，慢批、扫描停顿与交互 SQL 按发生次数各一条。默认 `console` 布局与 `text` 布局都保证**一条记录 = 一个物理行**：值为空或含空白/控制字符/`"`/`\`/`=` 时按 slog 规则加引号转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`），因此打印 DEL 定界符的配置行不会被真实换行拆开，切换 `--log-format` 也不改变字段切分；`--log-format` 只换布局、不改 `--log-level`。进度记录随 `--progress-interval` 定时打印，没有关闭开关；带 `--messages` 时进度同样写进文件。

`--messages` / `-m` 把命令行日志**追加**到该文件（与 DB2 的 `MESSAGES` 一致），文件是这次运行日志的唯一目的地：一条记录一个物理行，本次运行的第一条记录一定是版本横幅（文件在根命令打印横幅之前打开，所以它紧接在已有内容之后，而不是文件的绝对首行），`export/import config`（建连接之前）、定时进度、WARN 告警、`completed` 统计与收尾的 `command failed` 都在里面，stdout 与 stderr 一行也不打印；`--log-level`、`--log-format` 一样作用于文件，退出码不受影响（0 成功、1 运行时错误、2 用法错误），脚本照样能判断成败。已有内容不会被删除，多次运行按各自的横幅分段；工具不做截断、轮转或清理，需要每次独立文件请换路径或先自行删除。实测同一条注定失败的导出：**新文件**下带 `-m` 时 stdout/stderr 均 0 字节、文件 5 条记录、退出 1，而不带 `-m` 那次的 stderr 与该文件去掉时间戳/PID 前缀后 `diff` 逐行相同；先写入 `previous run` 再跑同一命令，旧行仍在，新记录从横幅开始追加。两类例外：未知选项在文件打开前就失败，错误只上 stderr 且不建文件；`-m` 路径无法创建时 `failed to create message file <path>: ...` 也只上 stderr。`--help` 与 `-m` 同时给不产生文件。`list`、`show` 与 `exec` 在这一点上更严格：`--messages` 只装日志与错误，**结果不被它接管**——没有 `--output` 时结果仍去 stdout（屏幕依然全静，因为日志进了文件），有 `--output` 时结果去该文件；`--output` 与 `--messages` 同路径是用法错误退出 2，此时文件已由日志打开过，因此文件里只有记录与那句拒绝。早期版本按 DB2 LOAD/EXPORT 风格另写一份 `SQL3104N/3105N/3107N/3109N/3110N` 报告，那份重复的记录与 `MessageWriter` 已整体删除，告警与失败只保留一条日志记录。会出现 WARN 的场景：导出侧 `--no-hex-blob` 搭配非 utf8mb4 字符集、单行 INSERT 超过 `--statement-size`；导入侧读不到 `max_allowed_packet`（按保守值分批）、`--batch-size` 被 65535 占位符上限下调、无表头文件被当作表头丢弃首行、`insert_update` 命中无唯一键的表；另有三个诊断阈值告警：扫描 10 秒取不到行（`export scan stalled`）、单批 INSERT 超过 5 秒（`slow insert batch`）、单次 COMMIT 超过 5 秒（`slow commit`）。

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

### SQL 交互回显

```bash
# 默认就能看到导出到底发了哪些语句
imex export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del --threads 4 -q "SELECT * FROM users" 2>&1 \
  | grep 'db interaction'
```

工具与数据库之间的**交互语句**逐条记成 `db interaction`，从 v0.16.0 起走 **info 级别，即默认就能看到**，不需要加 `--log-level debug`——跑在生产上的导入/导出出错时，第一个要确认的就是"工具究竟发了什么"。绑定参数按既有脱敏规则以 `num`/`str(长度)`/`bytes(长度)`/`NULL` 呈现，真实值仍需 `--echo-sql-values`。

覆盖范围是"语句数量不随行数增长"的那一类：导出侧的 `INFORMATION_SCHEMA` 分片统计与列类型、`MIN`/`MAX`、`LIMIT 0` 列探测、采样探针、`version_comment`、`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ` 与 `START TRANSACTION WITH CONSISTENT SNAPSHOT`；导入侧的表/列/唯一键校验、表头判定、`SHOW VARIABLES max_allowed_packet`、`CREATE TABLE`、`DELETE`、`SET FOREIGN_KEY_CHECKS`；`list databases|tables|columns|indexes` 的那条元数据查询、以及 `show <kind>` 的那条 `SHOW CREATE …` 定义查询同样回显（自变更记录 127 起，导出不再读 `SHOW CREATE TABLE`，全工具只有 `show` 读它）。**逐行数据语句不打印**——攒批 INSERT/REPLACE 只在失败时经脱敏错误路径出现，因此默认日志不会因为导入百万行而膨胀。并行导出的每条记录都带 `worker=N` 与 `shard=N`：分片查询、以及每个分片连接自己开的快照都在其上；分片查询失败的文案也以 `(worker N, SQL: ...)` 开头。

体量实测（本地 MariaDB 10.6.23，20 万行表；下列字节数都是**整份命令日志**的大小，不是交互记录单独的字节）：串行导出 3 条记录、1,029 字节；`--threads 4` 自动 8 分片 32 条、5,928 字节（同一命令复跑在 5,884–5,929 字节间浮动，长度由分片边界的浮点写法决定）（每个分片连接各 2 条快照语句 + 1 条分片 SELECT）；导入 20 万行 SQL 文件 4 条、DEL 文件 5 条；`list tables` 1 条。开销与副作用实测：同一条 8 分片导出提到 `--log-level warn` 只剩版本横幅 1 条、151 字节，两级的产物 md5 逐字节相同（`2ac5b51d2374…`），三轮交替计时 99/104/103 ms 对 104/100/102 ms，看不出差别；导入 20 万行同样为 warn 1,079–1,242 ms 对 info 1,095–1,119 ms，默认级别日志里已知行值 `value-199999` 命中 **0**，落库 200,000 行与源表的 `BIT_XOR(CRC32(...))` 同为 `2180467309`。与逐条对比服务端 `general_log` 的结果：凡经 imex 代码发出的语句全部可见（改前 7 条里只见得到 2 条），唯一看不到的是**驱动自己注入**的 `SET NAMES`（由 `--default-charset` 决定，不经过本工具的日志），`--session-var` 的取值则在开局以 `session variables will be set on every connection vars=[...]` 记录。要一条不漏就查服务端 `general_log`。

两条与日志对账有关的写法差别：回显的是**语句本身**，服务端收到的是同一条语句再加 `/* imex:<commit7> */` 前缀（见下一节）；日志级别提到 `warn` 或 `error` 时交互记录一条都不打，这是把它关掉的唯一开关，不另设选项。`--messages` 文件里出现的内容与命令行一致，因此回显同样进那个文件；脱敏规则保证其中不含行数据值。

单条交互记录的语句文本上限是 **512 字节**（`MaxSQLInteractionPreview`），比失败回显的 2048 字节小：一次运行只打一条的错误需要看全，而交互记录每发一条语句就打一条，并行导出还会把用户整条 `--query` 塞进每个分片的 SELECT。超限处在空白边界截断并补 ` /* ... truncated */`，脱敏规则不变——两个上限共用同一个渲染器，不会走偏。**即便如此，长 SQL × 高分片仍会把日志推到几十~上百 KB**：实测一条 3,522 字节的 `SELECT ... WHERE id IN (1..899)` 配 `--threads 32`（64 个分片区间）产出 36,550 字节，上限前整份日志 **165,731 字节**（200 条交互记录占 153,585 字节、最长一条 2,138 字节），加上限后同一命令复跑在 **67,497–67,777 字节**之间（同样 200 条记录占 55,348–55,539 字节、最长一条 603–604 字节，浮动来自分片边界的浮点写法），其余是既有的固定行，最长的一条是 3,991 字节的 `export config` 快照。普通规模不受影响：同一轮 300 万行 `--threads 64` 导出仍是 268 行 / 35,788 字节且产物 md5 与加上限前逐字节相同，导入 20 万行 DEL 文件仍是 9 行 / 2,051 字节。日志体积敏感的作业请把级别提到 `--log-level warn` 关掉回显，或降 `--threads` 减少分片数（分片区间最多 64，随 `--threads` 增长）。

### 运行时 SQL 归因注释

默认情况下，工具在自己**主动执行**的语句前追加一段自描述注释 `/* imex:<commit7> */`（commit 为构建经 ldflags 注入的 git 短哈希前 7 位）。目的是在数据库 `general_log`、慢查询日志或审计流里，按该前缀即可过滤出"imex 执行的语句"，并据 commit 定位到具体二进制版本。

加注释的范围是**工具对数据/结构/元数据主动发起的语句**：

- 导出：主 `SELECT`、并行分片查询、列探测（`LIMIT 0`）、主键/唯一键/列类型/min-max 探测
- 结构定义：`show tab|vws|trg|evt|prc|fcn` 的 `SHOW CREATE …`（导出不在此列——SQL 产物只有数据）
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
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00');
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (2,'Bob','bob@example.com','2026-01-02 11:00:00');
```

产物**只有数据**：逐行 `INSERT`，**不含任何前导**（历史上曾写一行 `/*!40101 SET NAMES utf8mb4 */;`，为让同一份 SQL 文件跨 MySQL/PostgreSQL/达梦 通用回放而移除；`imex import` 仍会跳过旧文件里的这类前导语句），没有任何 DDL 与分节注释。建表语句由 `imex show tab users -D mydb -o users_ddl.sql` 单独取，要一个自带结构的文件就把两个产物拼起来（`cat users_ddl.sql users.sql > full.sql`）：这份拼接文件交给标准 `mysql`/`psql` 客户端可一次建表并灌数据，而 `imex import` 不重放 DDL，它跳过 `CREATE` 只导 `INSERT`，所以先用建表语句把表建好、再以 `--mode insert` 导入即可。

SQL 文件的默认布局是**每行一条 INSERT**（mysqldump `--skip-extended-insert` 的布局），`--extended-insert` 才把多行合并进一条语句。攒批交给导入侧是刻意的分工：导入器把语句拆成行后按 `--batch-size` 重新组批，文件是单行还是合并布局，服务端收到的语句一模一样（实测 20,000 行、`--batch-size 1000`：单行文件 20,000 条语句与合并文件 1 条语句都只产生 20 条服务端 INSERT）。合并布局在导出侧换到的只有体积——100 万行窄表实测：单行 2.5 s / 峰值内存 33 MB / 278,790,434 B（gzip 28,247,005 B），合并 2.7 s / 70 MB / 153,792,809 B（gzip 27,050,101 B，19 条语句）；代价是导入侧要一次性解析整条语句，同一份数据在 `--batch-size 1000` 下峰值内存 44 MB（单行）对 216 MB（合并）。

数值列按列类型写成裸字面量（不加引号），与 mysqldump 的 `VALUES (1,'Alice',…)` 一致：`INT`/`BIGINT`（含 `UNSIGNED`）、`DECIMAL`、`FLOAT`、`DOUBLE` 都直接输出驱动返回的文本；字符串、日期时间与 `YEAR` 仍加引号（`YEAR` 是独立列类型，`'0000'` 的写法保持可读且不依赖两位年份规则）。元组内部与元组之间的分隔符都是 `,`，元组之间的连接形态是 `),(`（无空格）；列名列表仍保留 `, ` 便于阅读。实测 3 行边界表：`BIGINT UNSIGNED` 最大值、`DECIMAL(65,30)` 的 65 位精度、`DOUBLE` 的 `5e-324` 都原样回导，旧（带引号）与新（裸字面量）两种文件加载后的 `CHECKSUM TABLE` 相同。

### 三类格式共有的值处理

- **`YEAR` 零值**：MySQL/MariaDB 文本协议把 `YEAR(0000)` 发回裸字符串 `0`，直接回导会被服务端两位 YEAR 规则解释成 2000。导出时识别 YEAR 列并统一写成 `0000`，DEL/SQL/Parquet 一致（实测 `--format del` 得 `"1","0000",`，`--format sql` 得 `'0000'`）。
- **NULL 与空字符串**：DEL 默认模式下 NULL 是不加定界符的空字段、空串是 `""`，可区分；`--no-char-del` 下两者形态相同，回读一律按 NULL。SQL 用 `NULL` 与 `''` 区分。Parquet 用列式 NULL 区分。
- **二进制列**：DEL/SQL 在 `--hex-blob` 生效时写 `0x…`，空值写 `0x`；Parquet 存原始 BYTE_ARRAY，不做 UTF-8 标注。
- **日期与时间戳**：`--date-fmt`/`--timestamp-fmt` 仅作用于 DATE 与 DATETIME/TIMESTAMP 列，其余列不受影响。

## 历史版本记录

> **全部 22 节逐版记录（v0.6.0 → v0.38.0）已外迁至 [`docs/CHANGELOG.md`](docs/CHANGELOG.md)**：原文一字未改，仅换载体；按「历史只追加」纪律保留，描述的是各版本**当时**的改动，**不代表当前行为**。旧版本里与当前实现不一致的口径属正常历史存档，不是待修错误；当前口径一律以本文上半部分为准，每轮完整理由见 `docs/requirements.md` 十四节编号变更记录。

最近三版摘要（完整记录见 [`docs/CHANGELOG.md`](docs/CHANGELOG.md)）：

| 版本 | 档位 | 一句话摘要 | 变更记录 |
|---|---|---|---|
| v0.38.0 | Y | 达梦 `export --threads 2–256` 由连库前退 2 翻为真并行（分片规划器加达梦目录方言、复合键按列序展开、分片语句按引擎语法重渲染），顺带修掉反引号回渲染与字面量反斜杠转义两处缺陷；`--help` 一字未改 | 170 / 171 |
| v0.37.0 | Y | `imex list` 子命令短名升为主名、全名降为等价别名，与 `show` 对齐；父命令缺对象提示同步改列短名 | 169 |
| v0.36.1 | Z | `list`／`show` 父命令遇连接选项简写时，`unknown shorthand flag` 报错追加一句「缺 object 子命令」并列举具体对象清单 | 168 |

> 更早 19 节（v0.6.0 → v0.35.0 的修复记录、v0.14.0 → v0.17.0 的功能记录）全部在 `docs/CHANGELOG.md`，查看某一版请在该文件内按版本号检索。

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
│   ├── list.go                # list 子命令树（8 个对象、输出选项与校验）
│   ├── show.go                # show 子命令树（6 个 kind、对象名解析与校验）
│   ├── output_channel.go      # list/show 共用的结果通道（缓冲投递、--no-overwrite 拒绝、同路径守卫）
│   ├── validate.go            # 共享参数边界校验（port/threads/statement-size/log-level/分隔符解析/格式适用性）
│   ├── option_scope.go        # 把格式适用范围写进 flag 自己的帮助文本（与拒绝逻辑同一张表）
│   ├── cli_matrix_test.go     # 参数组合矩阵与退出码契约测试
│   ├── list_test.go           # list 校验、命令树与帮助宽度测试
│   ├── show_test.go           # show 命令树、kind 唯一性、通道 flag 与校验测试
│   ├── output_channel_test.go # 通道矩阵、--no-overwrite、同路径守卫测试
│   ├── validate_fuzz_test.go  # 校验函数 fuzz 测试
│   └── encrypt.go             # encrypt 子命令（密码加密）
├── internal/
│   ├── backend/
│   │   ├── backend.go         # 方言接口（35 个方法）、`--engine` 取值解析与折叠（`Parse`/`Normalize`）与 `backends` 注册表
│   │   ├── dsn_mysql.go       # MySQL 连接串构造与 29 个驱动保留参数
│   │   ├── dberr_mysql.go     # MySQL 瞬态错误码表（7 个码）与取码方式
│   │   ├── dberr_postgres.go  # PostgreSQL 瞬态 SQLSTATE 表（9 个码）与 `*pgconn.PgError` 取码（134 轮）
│   │   ├── dberr_dameng.go    # 达梦瞬态码表（4 个驱动连接层码）与 `*dm.DmError` 取码（135 轮）
│   │   ├── charset_mysql.go   # MySQL 的 16 个 `--default-charset` 名字与 `KnownCharset`（134 轮自 `cmd` 下沉）
│   │   ├── show_mysql.go      # MySQL 的六条 `SHOW CREATE …` 与定义结果列名（133 轮自 `internal/show` 下沉）
│   │   ├── refusals.go        # 选项拒绝表的数据形状（`Command`/`RunOptions`/`Refusal`，不含引擎名）
│   │   ├── refusals_mysql.go  # MySQL 的拒绝表为空、字符集校验恒通过
│   │   ├── refusals_postgres.go # pg 的选项拒绝表（export 4 / import 3 / list 1 / show 1）与 `client_encoding` 校验
│   │   ├── refusals_dameng.go # dm 的选项拒绝表（export 4 / import 4 / list 1 / show 1）（135 轮）
│   │   ├── dsn_dameng.go      # 达梦连接串构造（恒带 `compatibleMode=oracle`）与驱动保留参数表（135 轮）
│   │   ├── types_dameng.go    # 达梦 Oracle 模式类型词表：一行同时回答分类、二进制、取值文本与绑定形态四类（135 轮，142 轮加 `bind`）
│   │   ├── bind_dameng.go     # 达梦导入侧的取值绑定规则：六个 bind 类别、五个实测上限常量与三条缺陷的修法（142 轮）
│   │   ├── types.go           # 类型词 → `common.ColumnKind` 的共享词表与二进制列判定（不含引擎名）
│   │   ├── mysql.go           # MySQL/MariaDB/OceanBase 方言
│   │   ├── postgres.go        # PostgreSQL 方言（`$n`、`client_encoding` 表、类型与二进制词表、单元格文本规则）
│   │   ├── dameng.go          # 达梦 DM8 Oracle 兼容模式方言（`?`、双引号引用、强制 0x hex、保守 4 MiB 批次预算、`BindFixups` 逐列问类型表）（135 轮，142 轮加）
│   │   ├── backend_test.go    # 三方言逐字节对照与注册表测试（含 `BindFixups` 的逐引擎逐位置断言，142 轮）
│   │   ├── types_test.go      # 类型词表分类断言
│   │   ├── dberr_test.go      # 码表归属与三方言瞬态判据断言
│   │   ├── dberr_postgres_test.go # pg 的 9 个瞬态 SQLSTATE、缺席码与 `*pgconn.PgError` 拆包
│   │   ├── dameng_test.go     # 达梦的时间文本、瞬态码表与 16 位截断防护、upsert 未实现，以及单一类型表「分类即改写」的五条防漂移断言（135 轮、137 轮）
│   │   ├── bind_dameng_test.go # 五个绑定适配器与四条 helper 的取值对照，含驱动十进制的 `#` 文本 oracle（142 轮）
│   │   ├── charset_mysql_test.go # MySQL 16 个字符集名字与 `KnownCharset` 的裁空白/折叠
│   │   ├── dsn_mysql_test.go  # 连接串 golden 与保留参数清单
│   │   ├── refusals_test.go   # 拒绝表的顺序、触发条件与文案取值
│   │   └── show_mysql_test.go # 六条定义语句与结果列名的逐字断言、pg 侧空回答（133 轮）
│   ├── config/
│   │   ├── config.go            # 配置定义与默认值（连接/超时/字符集/DelModifier/导出导入选项）
│   │   ├── list.go              # list 配置与输出格式枚举
│   │   ├── show.go              # show 配置（对象名、库限定、结果通道选项）
│   │   └── formatscope.go       # 格式适用范围登记表（校验拒绝文案、帮助文本标记、配置回显字段三处共用）
│   ├── common/
│   │   ├── util.go              # 工具函数（定界符解析、文件大小）；DSN 构建在 internal/backend/dsn_mysql.go
│   │   ├── colkind.go           # `ColumnKind` 输出词汇（writer/reader 共用，词表在 internal/backend/types.go）
│   │   ├── sessionvars.go       # 会话变量解析与 DSN 注入（--session-var；保留名由引擎以谓词交出）
│   │   ├── sqlpreview.go        # 失败 SQL 脱敏回显（--echo-sql-values）
│   │   ├── sqltag.go            # 运行时 SQL 归因注释 /* imex:<commit> */（--no-sql-tag）
│   │   ├── dberr.go             # 瞬态错误判定的中性链（网络错误/文本兜底）+ 判据注入口；MySQL 码表在 internal/backend/dberr_mysql.go
│   │   ├── retry.go             # 重试策略（次数、指数退避、抖动、ctx 取消）
│   │   ├── pool.go              # 连接池老化（ConnMaxIdleTime / ConnMaxLifetime）
│   │   ├── usage.go             # 用法错误分类（退出码 2）
│   │   ├── log.go               # 日志模块（console/text 两种格式、版本信息、启动日志）
│   │   ├── diag.go              # 诊断辅助（阈值告警 WarnIfSlow、扫描停顿 StallWatch）
│   │   ├── stats.go             # 吞吐统计、速率计算与定时进度计时器（停止时排空在写的记录）
│   │   ├── password.go          # 密码加密/解密（PBKDF2-HMAC-SHA256 派生 + AES-256-GCM）
│   │   └── detect.go            # 服务端 max_connections 读取（并行 worker 上限告警）
│   ├── export/
│   │   ├── exporter.go          # 导出调度器（单线程流水线、并行分片与合并、hex-blob 判定）
│   │   ├── configlog.go         # 配置回显（按 --format 过滤字段、hex-blob 取生效值）
│   │   ├── del_writer.go        # DEL 格式写入器（双写/反斜杠转义、字段缓存优化）
│   │   ├── sql_writer.go        # SQL 格式写入器（默认每行一条 INSERT、`--extended-insert` 才按字节攒批、快速路径优化）
│   │   ├── parquet_writer.go    # Parquet 格式写入器（列式存储）
│   │   ├── shard.go             # 分片逻辑（主键/唯一键检测）
│   │   ├── diag.go              # 导出侧诊断常量（扫描停顿 10 秒、分片速率换算）
│   │   └── sql_parser.go        # SQL 解析器（JOIN 检测）
│   ├── import/
│   │   ├── importer.go          # 导入调度器（单线程流水线、并行 worker、列映射、攒批）
│   │   ├── configlog.go         # 配置回显（按 --format 过滤 DEL 字段）
│   │   ├── batchcap.go          # 攒批上限（max_allowed_packet 字节预算 + --statement-size 压低 + 65535 占位符）
│   │   ├── diag.go              # 导入侧诊断常量（慢批/慢提交 5 秒、批字节估算、worker 速率）
│   │   ├── session.go           # 写入会话（事务、重连回放、空闲提交、commit-count）
│   │   ├── record_reader.go     # 无上限记录读取器（自定义 SplitFunc，零拷贝别名）
│   │   ├── del_reader.go        # DEL 格式读取器（文本修饰符反解、0x 解码、达梦偏移量加空格、值适配器在修饰符之后）
│   │   ├── sql_reader.go        # SQL 格式读取器
│   │   └── parquet_reader.go    # Parquet 格式读取器（按文件 schema 判定内容字节、只在目标引擎要时交出去、达梦偏移量加空格）
│   ├── list/
│       ├── list.go              # 执行查询、逐行读取并交给渲染器
│       ├── queries.go           # 对象名与别名顺序、`BuildQuery()` 的引擎分派表（引擎经 `backend.Normalize` 折叠后再选）
│       ├── queries_mysql.go     # information_schema 查询构建（对象、范围、pattern、type）
│       ├── queries_pg.go        # `--engine pg` 的同构语句（pg_catalog / information_schema，`$n` 顺序编号）
│       ├── queries_dameng.go    # `--engine dm` 的同构语句（`SYS.DBA_*` 目录，`?` 顺序占位）（135 轮）
│       ├── aliases.go           # 对象简写注册表（dbs、tab、trg…）
│       ├── render.go            # table 对齐输出
│       ├── queries_test.go      # 查询构建测试
│       ├── queries_dameng_test.go # 达梦目录查询的分派、列名对齐、`?` 与绑定参数一一对齐、范围与转义（135 轮）
│       ├── aliases_test.go      # 简写唯一性与注册表隔离测试
│       └── render_test.go       # 渲染与 NULL/转义测试
│   └── show/
│       ├── show.go              # kind 注册表、对象名解析、SHOW CREATE 执行与定义列选取
│       └── show_test.go         # 名字切分/引号转义、定义列按名选取与回退测试
├── test/
│   └── integration/             # 集成测试
│       ├── roundtrip_test.go    # 离线往返（导出写入器→导入读取器：转义/换行/0x/NULL 与空串/单行 INSERT/DEL 修饰符）
│       ├── live_mysql_test.go   # 真实 MySQL 端到端（默认跳过，需 IMEX_TEST_DSN）
│       ├── option_matrix_test.go # 真实 CLI 交叉选项矩阵（argv 驱动编译出的二进制，同上门控）
│       ├── list_test.go         # list 八类对象、table 格式、stdout/stderr 分离与用法错误
│       ├── show_test.go         # show 六个 kind 的定义文本、库限定名、别名与回放
│       ├── channel_test.go      # list/show 输出通道、同路径拒绝、--no-overwrite
│       ├── lock_contention_test.go # 重复主键 × 导入并发的锁冲突回归（真实二进制 + 真实库）
│       └── export_cli_test.go   # 导出 CLI 三项：schema 限定查询、产物无 DDL 且旧选项为未知选项、`-q @file`
├── docs/
│   ├── requirements.md          # 需求文档
│   ├── cli-oracle.txt           # 已发布二进制的 --help 契约快照（回归基线）
│   └── CHANGELOG.md             # 逐版修复／功能记录（22 节历史存档，2026-11-14 由 README 外迁）
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
| SQL 导出逐行在 `strings.Builder` 里拼值、再整行复制进批次、刷新时再复制进 `bufio`，300 万行单线程合并要 2.95 s，一次导出触发 215 次 GC | SQL 写入器改为字节追加：`row`/`batch` 改用复用的 `[]byte`，数值与字符串直接追加（`appendQuotedSQL` 就地转义），合并批次一次写出。同一张 300 万行表、5 次交错实测中位数：单行 2.73→2.33 s（-15%）、合并单线程 2.95→2.21 s（-25%）、合并 4 线程 2.16→1.70 s（-21%），SQL 产物 md5 全部不变；GC 次数 362→213、215→71；峰值 RSS 单线程 56→50 MB、4 线程 156→139 MB。DEL 写入器试改后仅 1–3%，已回退，`del_writer.go` 未改。 |
