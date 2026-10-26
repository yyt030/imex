# imex 技术需求文档

## 一、项目概述

### 1.1 项目目标

工具名 **imex**（写作 **ImEx**，取自 **Im**port + **Ex**port）。
基于 Go 语言开发一个**表粒度**的 MySQL 逻辑备份与恢复工具，**功能设计参考 IBM DB2 的 EXPORT/IMPORT**，同时支持 **MySQL** 和 **OceanBase MySQL 模式**。工具为单一二进制文件 **imex**，通过子命令区分操作：

```bash
imex export    # 导出数据
imex import    # 导入数据
imex version   # 显示版本信息
imex encrypt   # 加密密码
```

### 1.2 核心设计理念（参考 DB2，命令行风格为 Go 标准选项）

- **导出基于 SQL**：通过 `--query` 传入 SELECT 语句，用户完全控制导出什么数据
- **导入多种模式**：通过 `--mode` 指定 INSERT / INSERT_UPDATE / REPLACE / CREATE
- **定界文件格式**：默认使用 DEL（定界 ASCII）格式，支持自定义分隔符
- **单行 SQL 布局**：`--format sql` 默认每行一条 INSERT（文件逐行可切分），攒批统一由导入侧按 `--batch-size` 完成；`--extended-insert` 保留 mysqldump 的合并布局，只用于换取更小的文件体积
- **Parquet 列式格式**：支持 Parquet 二进制列式存储，普通列存储为可空 UTF-8 字符串，二进制列存储为原始 BYTE_ARRAY
- **文件类型修饰符**：DB2 的 `MODIFIED BY` 拆分为独立命令行选项
- **日志文件**：`--messages` / `-m` 把本次运行的命令行日志整体交给该文件（终端因此不再打印，文件内容是标准日志记录而非 DB2 风格报告）
- **可调进度节奏**：`--progress-interval` 控制定时进度日志的间隔（默认 5 秒），`--quiet` 则完全不打印进度记录
- **并行导出/导入**：支持多线程并行处理，提升大表性能
- **列映射导入**：支持文件列到表列的自定义映射（DB2 风格）

### 1.3 技术栈

| 组件 | 选型 | 说明 |
|------|------|------|
| 开发语言 | Go 1.26（`go.mod` 声明 `go 1.26.10`）| 纯 Go 实现，无 CGO 依赖；发布构建强制 `CGO_ENABLED=0`，产物为静态链接二进制 |
| MySQL 驱动 | github.com/go-sql-driver/mysql v1.9.3 | 纯 Go MySQL 驱动；三个超时参数经驱动的 `Config.Timeout/ReadTimeout/WriteTimeout` 字段注入（见 2.1.2），而不是拼进 DSN 参数串，`FormatDSN/ParseDSN` 往返后不丢失 |
| 命令行 | github.com/spf13/cobra v1.10.2（pflag v1.0.10）| 子命令框架；`completion` 已禁用，帮助短选项为 `-?`（`-h` 让给 `--host`） |
| SQL 解析 | vitess.io/vitess v0.24.5（sqlparser）| 解析/重写 SELECT，用于并行分片条件注入与优化器 hint 保留 |
| Parquet | github.com/parquet-go/parquet-go v0.26.4 | Parquet 格式读写 |
| 错误处理 | github.com/pkg/errors v0.9.1 | 错误包装与调用栈，配合 `--error-stack` 打印堆栈 |
| 日志 | 标准库 log/slog | 结构化日志，双布局（`java`/`text`），见 3.3.2 |

间接依赖中 `grpc`/`protobuf`/`genproto` 由 vitess 引入，仅用于其 parser 内部类型；本工具不监听也不发起 gRPC 连接。

---

## 二、功能需求

顶层子命令四个：`export`、`import`、`version`、`encrypt`，另有 cobra 自带的 `help`。`export` 与 `import` 各有一个短别名——`exp`、`imp`（`cmd/export.go:17`、`cmd/import.go:17`），二者与全名共享同一套标志与校验，帮助文本也完全一致；文档与示例统一使用全名，脚本里可用别名。`completion` 子命令已禁用（见 3.3），`--help` 的短选项是 `-?`，因为 `-h` 已让给 `--host`。

### 2.1 export 子命令 — 数据导出

#### 2.1.1 命令语法

```bash
imex export [连接选项] [导出选项] [修饰选项] --query "<SELECT SQL>"
```

#### 2.1.2 连接选项

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--host` | `-h` | 数据库主机地址 | `127.0.0.1` |
| `--port` | `-P` | TCP/IP 端口（范围 1–65535） | `3306` |
| `--user` | `-u` | 数据库用户名 | `root` |
| `--password` | `-p` | 数据库密码 | 空 |
| `--socket` | `-S` | UNIX 域套接字路径 | 空 |
| `--database` | `-D` | 数据库名 | 无 |
| `--connect-timeout` | | TCP 连接超时（对应 JDBC connectTimeout） | `5s` |
| `--socket-read-timeout` | | 每次读套接字超时（对应 JDBC socketTimeout） | `5m` |
| `--socket-write-timeout` | | 每次写套接字超时（Go 独有，无 JDBC 等价） | `5m` |
| `--default-charset` | | 连接与输出文件字符集（utf8mb4 默认；还支持 utf8、latin1、ascii、gbk、gb18030 及其别名） | `utf8mb4` |
| `--session-var` | | 在每条数据库连接上设置的会话变量，`name=value` 格式，可重复传入多个 | 空 |

**会话变量选项（`--session-var`）说明：**

- 用于在导出/导入开始前为数据库连接设置会话变量（如 `sql_mode`、`foreign_key_checks`、OceanBase 的 `query_timeout` 等），export 与 import 子命令均支持。
- 可重复传入，每个参数一组 `name=value`，例如：
  ```bash
  imex export ... --session-var "sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE" --session-var foreign_key_checks=0
  ```
- 变量以 DSN 附加参数方式注入，驱动会在**每一条新建物理连接**上自动执行对应 `SET` 语句。因此主连接、并行导出的每个分片连接、并行导入的每个工作线程连接都会一致生效，不受连接池复用影响。
- 值会自动按 SQL 字面量处理：纯数字与 `ON/OFF/TRUE/FALSE/DEFAULT/NULL` 关键字原样传入，其余值加单引号（内部单引号双写）。值中不允许出现反引号、注释序列（`/*`、`--`）和反斜杠，防止破坏或注入 SET 语句。
- 变量名必须匹配 `[A-Za-z][A-Za-z0-9_]*`；与工具或驱动内部 DSN 参数同名的变量（如 `charset`、`parseTime`、`tls`、`tenant`、`timeout` 等）会被拒绝，避免破坏连接行为。
- 设置的变量对未知变量名会由服务端报错（如 `Unknown system variable`），命令在连接阶段即失败，便于尽早发现拼写错误。

#### 2.1.3 导出选项

| 选项 | 短选项 | 说明 | 默认值 | 必填 |
|------|--------|------|--------|------|
| `--output` | `-o` | 输出文件路径 | 无 | 是 |
| `--format` | `-f` | 输出文件格式（`del` / `sql` / `parquet`） | `del` | 否 |
| `--query` | `-q` | SQL SELECT 语句（支持 OceanBase / MySQL 优化器 hint，见 2.1.8），与 `--query-file` 互斥 | 无 | 是（与 `--query-file` 恰好给一个） |
| `--query-file` | | 从该文件读取 SQL SELECT 语句，读入时去掉首尾空白与结尾分号；与 `-q` 互斥，是 `-q` 的替代输入源，与格式无关（三种格式都可用） | 空 | 否 |
| `--messages` | `-m` | 把本次运行的全部日志记录写入该文件，终端不再打印（文件即命令行日志本身） | 空（日志只打印到 stderr） | 否 |
| `--progress-interval` | | 定时进度日志的间隔（必须 > 0，否则用法错误退出码 2；`--quiet` 是不打印进度，二者语义不同） | `5s` | 否 |
| `--no-overwrite` | | 拒绝覆盖已存在的输出文件（默认行为为覆盖，即 `os.Create` 截断；原名 `--no-replace`，2026-10-26 改名，不留别名） | `false` | 否 |
| `--batch-size` | | 读→写管道每批行数（仅作用于**单线程导出**与**导入**；并行导出为分片内同步 scan→write，不受影响，改动过它时并行路径记一条 `parallel export ignores pipeline options`） | `1000` | 否 |
| `--pipeline-buffer` | | 读→写管道队列深度（批数为单位，0 表示同步交接；仅单线程导出，`--threads>1` 时不生效并记日志） | `10` | 否 |
| `--hex-blob` | | 将二进制列编码为 `0x` 十六进制字面量（非 utf8mb4 字符集时自动启用）；作用于 DEL/SQL 文本格式，`--format parquet` 下显式传入报退出码 2 | `false` | 否 |
| `--no-hex-blob` | | 强制原始二进制透传，即使在非 utf8mb4 字符集下（可能导致二进制数据损坏）；与 `--hex-blob` 互斥；`--format parquet` 下显式传入报退出码 2 | `false` | 否 |
| `--threads` | | 并行导出线程数（1=不并行，范围 1–256） | `1` | 否 |
| `--shard-key` | | 分片键列名（空=自动检测主键/唯一键） | 空 | 否 |
| `--log-level` | | 日志级别（debug/info/warn/error） | `info` | 否 |

#### 2.1.4 文件格式（`--format`）

| 值 | 说明 |
|----|------|
| `del` | 定界 ASCII 格式（默认），每行一条记录，列之间用分隔符分隔 |
| `sql` | SQL 语句格式，默认只写指向单表的 `INSERT` 数据语句；带 `--create` 时才在文件头部加 `CREATE TABLE`，可直接导入 MySQL（`--mode create` 需要该结构） |
| `parquet` | Parquet 列式二进制格式，普通列存储为可空 UTF-8 字符串，二进制列（BLOB/BINARY 等）存储为原始 BYTE_ARRAY，NULL 保留为 Parquet NULL |

**Parquet 格式特点：**
- 普通列存储为 `optional binary (STRING)`（可空 UTF-8 字符串），二进制列存储为不带 UTF-8 标注的 `optional binary`
- NULL 值保留为 Parquet NULL（区别于空字符串）
- 行序与 `--query` 查询结果一致
- 不支持并行导出（分片文件无法拼接），`--threads>1` 时自动降级为单线程
- 二进制格式，无法直接文本预览，需使用 parquet-tools 或 DuckDB 等工具查看

#### 2.1.5 DEL 格式修饰选项

| 选项 | 说明 | 默认值 | 支持格式 |
|------|------|--------|----------|
| `--char-del` | 字符串定界符（包裹字符串值） | `"` | 单字符、多字节、十六进制（0x） |
| `--col-del` | 列分隔符（分隔同一行中的列） | `,` | 单字符、多字节、十六进制（0x） |
| `--row-del` | 行分隔符（标记一行结束） | `\n` | 单字符、多字节、十六进制（0x） |
| `--escape` | 转义符（用于转义定界符） | `"`（双写转义） | 单字符、多字节、十六进制（0x） |
| `--no-escape` | 不使用转义（写出前校验，数据含冲突定界符时报错） | `false` | - |
| `--timestamp-fmt` | 时间戳格式（作用于 DATETIME/TIMESTAMP 列） | `yyyy-MM-dd HH:mm:ss` | - |
| `--date-fmt` | 日期格式（作用于 DATE 列） | `yyyy-MM-dd` | - |
| `--no-char-del` | 不使用字符串定界符（裸字段不转义：数据含列/行分隔符时导出报错） | `false` | - |
| `--no-col-names` | 不输出列名到第一行 | `false` | - |

**格式适用性：** 上表中的定界符/转义/时间格式/列名行选项只描述 DEL 文件的布局与编码，SQL 与 Parquet 写端不使用它们；显式传入且 `--format` 不是 `del` 时报用法错误并退出 2（与 `--create`/`--no-data` 等 SQL 专属选项的行为对称），不会被静默忽略。这一"哪个选项对哪种格式生效"的判据只有一处定义——`internal/config/formatscope.go` 的登记表，用法错误的拒绝顺序与文案、`--help` 每个选项末尾的范围标记（`(only with --format del)` / `(only with --format sql)` / `(not with --format parquet)`）、以及 `export/import config` 回显要打印的字段集合都由它产出，新增一个格式专属选项只需加一行登记。

**分隔符格式说明：**

- **单字符**：`--col-del ","`、`--col-del "\t"`
- **多字节**：`--col-del "||"`、`--row-del "\r\n"`、`--col-del ":::"`
- **十六进制**：`--col-del 0x09`（制表符）、`--row-del 0x0D0A`（\r\n）、`--col-del 0x7C0x7C`（||）

#### 2.1.6 SQL 格式修饰选项

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--statement-size` | 单条 INSERT 语句最大字节数（范围 1–268435456）。仅在使用 `--extended-insert` 时参与分组；默认单行布局下一行无法再拆分，它退化为告警阈值 | `8388608`（8 MiB） |
| `--extended-insert` | 把多行合并进一条 INSERT（等价 mysqldump 的默认布局），按 `--statement-size` 的字节预算分组 | `false` |
| `--single-row-insert` | **已废弃**：每行一条 INSERT 现在是默认布局，该选项保留以兼容既有脚本，传入后不改变任何行为（与 `--extended-insert` 同时给出为用法错误） | `false` |
| `--create` | 在 SQL 文件头部输出 `CREATE TABLE` 语句（SQL 导出默认只导数据，需自带建表语句时才加；仅对 `--format sql` 生效，其他格式传入报用法错误退出 2；`SHOW CREATE TABLE` 按 `--query` 点名的 schema 查找，写出的语句恒为裸表名） | `false` |
| `--no-data` | 不输出 INSERT 语句（仅导出表结构，须与 `--create` 同时给出才有内容） | `false` |

**SQL 布局默认值说明：**

- SQL 导出默认**只写数据**（`INSERT`），不再默认附带 `CREATE TABLE`；要自带建表语句须显式加 `--create`。这一改动的动机是数据文件更通用（导入到已存在的目标表即可），而把建表当默认会让"导出→换个库导入"这类最常见路径带上可能冲突的 DDL。`--no-create` 已随默认值反转一并删除：结构既然要 `--create` 才写，反向开关唯一剩下的作用就是否决 `--create`，于是它不再注册，传入即 `unknown flag: --no-create` 退出 2。
- 默认布局是每行一条 INSERT（mysqldump `--skip-extended-insert` 的形态），每条语句恰好占一个物理行，文件可按行切分。
- 攒批改由导入侧承担：导入器把语句拆成行后按 `--batch-size` 重新组批，与文件布局无关。实测 20,000 行、`--batch-size 1000`，单行文件（20,000 条语句）与合并文件（1 条语句）在目标库 general log 里都是 20 条服务端 INSERT。
- `--extended-insert` 换到的是体积而非速度：100 万行窄表实测单行 2.5 s / 峰值内存 33 MB / 278,790,434 B（gzip 28,247,005 B），合并 2.7 s / 70 MB / 153,792,809 B（gzip 27,050,101 B，19 条语句）。导入侧要一次性解析整条语句，同一份数据在 `--batch-size 1000` 下单线程峰值内存为 44 MB（单行文件）对 216 MB（合并文件）。
- 三个选项均仅 `--format sql` 可用；在非 SQL 格式下显式传入会报错。`--single-row-insert` 与 `--extended-insert` 互斥，同时给出报 `--single-row-insert and --extended-insert cannot be used together`（退出码 2）。

**SQL 格式转义规则：**

导出 SQL 格式时，字符串值按以下规则转义，确保每条 INSERT 恰好占一个物理行：

| 原始字节 | 转义结果 |
|----------|----------|
| `\`（反斜杠） | `\\`（双反斜杠） |
| `'`（单引号） | `''`（SQL 标准双写） |
| `\n`（换行） | `\\n` |
| `\r`（回车） | `\\r` |
| `\t`（制表符） | `\\t` |
| `\0`（NUL） | `\\0` |
| `\x1a`（Ctrl-Z） | `\\Z` |

**SQL 文件结构：**

- 文件首行：`/*!40101 SET NAMES <charset> */;`（charset 取自 `--default-charset`）
- MySQL 下数据段带 `LOCK TABLES <table> WRITE;` / `UNLOCK TABLES;` 一对
- OceanBase 下不输出 LOCK/UNLOCK（OceanBase 对 LOCK TABLES 支持有限）
- 并行导出时，LOCK/UNLOCK 由流式写出步骤（`writeShardStream`）统一写出，各分片片段自身不写 LOCK/UNLOCK

**SQL 格式的单表限制：** SQL 输出文件描述的是**一张真实表**（`CREATE TABLE <table>` + `LOCK TABLES <table> WRITE` + 指向该表的 `INSERT`）。因此 `--format sql` 要求 `--query` 为单表查询，解析阶段即拒绝 JOIN、隐式多表（`FROM a, b`）、派生表（`FROM (SELECT …) t`）与 `WITH` CTE，报错并提示改用 `--format del` / `--format parquet`（二者只写出结果集，不依赖表结构）。校验在输出文件创建之前完成，不会留下半成品文件。不带 `FROM` 的 `SELECT 1` 按 MySQL 惯例解析为隐式 `DUAL` 表，工具同样按单表处理（服务端因无 `DUAL` 表结构而报错属预期行为）。

**表名里的 schema 一律剥离：** `--query`（或 `--query-file`）里的表名可带 schema 限定（`` `db`.`tbl` ``）。工具解析出的**裸表名** `tbl` 才是写进 `CREATE TABLE`/`INSERT`/`LOCK TABLES` 以及分片键探测（`information_schema` 的 `TABLE_NAME = ?` 且 `TABLE_SCHEMA = DATABASE()`）用的值；schema 限定符单独保留，仅用于 `SHOW CREATE TABLE` 的定位——查询点了 schema 就在该 schema 查，没点就在连接的 `--database` 查。修复的缺陷：旧实现把带反引号的 `` `db`.`tbl` `` 原样塞进 `` SHOW CREATE TABLE `%s` `` 与 `` INSERT INTO `%s` ``，等于再套一层库名，服务端报 `Error 1146: Table 'db.db.tbl' doesn't exist`（单线程取建表语句即中止），并行路径也会因 `TABLE_NAME` 对不上而退化；剥离后导出的文件恒引用裸表，可在任意目标库回放。

#### 2.1.7 使用示例

```bash
# 基本导出：导出整张表到 DEL 格式
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 带 WHERE 条件的导出
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/active_users.del \
  -q "SELECT id, name, email FROM users WHERE status = 'active'"

# 自定义分隔符
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --char-del '!' --col-del '@' --default-charset utf8mb4 \
  -q "SELECT * FROM users"

# 导出为 SQL INSERT 格式（可直接导入）
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.sql -f sql \
  -q "SELECT * FROM users"

# 导出为 Parquet 格式
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.parquet -f parquet \
  -q "SELECT * FROM users"

# 导出表结构 + 数据，控制 INSERT 大小
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.sql -f sql \
  --statement-size 500000 \
  -q "SELECT * FROM users"

# 仅导出表结构
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users_schema.sql -f sql \
  --no-data \
  -q "SELECT * FROM users LIMIT 0"

# 导出关联查询结果
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/user_orders.del \
  -q "SELECT u.name, o.order_id, o.amount FROM users u JOIN orders o ON u.id = o.user_id"

# 带日志文件
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -m /var/log/imex_export.log \
  -q "SELECT * FROM users"

# 指定时间戳格式
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/logs.del \
  --timestamp-fmt "yyyy/MM/dd HH:mm:ss" \
  -q "SELECT * FROM logs"

# 制表符分隔、无字符串定界符
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.tsv \
  --col-del "\t" --no-char-del \
  -q "SELECT * FROM users"

# 并行导出（4线程，自动检测主键分片）
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "SELECT * FROM users"

# 指定分片键并行导出
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/orders.del \
  --threads 8 \
  --shard-key order_id \
  -q "SELECT * FROM orders"
```

#### 2.1.8 优化器 Hint 支持（`--query`）

`--query` 完整保留 SQL 中的优化器 hint，可在导出时控制 OceanBase / MySQL 的执行计划：

```bash
# OceanBase 并行 hint（放在 SELECT 之前）
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "/*+ parallel(8) */ SELECT id, name FROM users"

# hint 紧跟关键字（写在 SELECT 之后）同样支持
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/orders.del \
  -q "SELECT /*+ INDEX(orders idx_status) */ * FROM orders WHERE status='active'"
```

**处理策略：**

- **单线程（`--threads=1`）**：原样执行 `--query`，hint 天然保留。
- **并行导出（`--threads>1`）**：为注入分片 WHERE 条件，工具会解析并重写 SQL。重写前会剥离位于语句最前面的 `/*+ ... */` hint，完成分片条件注入后再原样拼回，确保下发给数据库的分片查询仍携带 hint（含分片键探测语句）。写在 `SELECT` 之后的 hint 由解析器直接保留。

---

### 2.2 import 子命令 — 数据导入

#### 2.2.1 命令语法

```bash
imex import [连接选项] [导入选项] [修饰选项]
```

#### 2.2.2 连接选项

与 export 子命令相同。

#### 2.2.3 导入选项

| 选项 | 短选项 | 说明 | 默认值 | 必填 |
|------|--------|------|--------|------|
| `--input` | `-i` | 输入文件路径 | 无 | 是 |
| `--format` | `-f` | 输入文件格式（`del` / `sql` / `parquet`） | `del` | 否 |
| `--mode` | `-M` | 导入模式（见下表） | `insert` | 否 |
| `--table` | `-t` | 目标表名（insert/replace/insert_update 必填；create 模式可省略，自动取 SQL 文件中 CREATE 语句的表名，指定则作为新表名重写） | 无 | 是（SQL 格式 CREATE 模式除外） |
| `--columns` | `-c` | 列映射（见下方说明） | 空（使用文件中全部列） | 否 |
| `--messages` | `-m` | 把本次运行的全部日志记录写入该文件，终端不再打印（文件即命令行日志本身） | 空（日志只打印到 stderr） | 否 |
| `--progress-interval` | | 定时进度日志的间隔（必须 > 0，否则用法错误退出码 2；`--quiet` 是不打印进度，二者语义不同） | `5s` | 否 |
| `--batch-size` | | 读→写管道每批行数（范围 1–100000） | `1000` | 否 |
| `--pipeline-buffer` | | 读→写管道队列深度（批数为单位，0 表示同步交接，范围 0–1024） | `10` | 否 |
| `--statement-size` | | 单条生成 INSERT 的字节上限，取它与 `max_allowed_packet` 派生预算的较小值（**只压低、不抬高**；范围 0–268435456，`0`=不额外压低，见 7.4） | `0` | 否 |
| `--threads` | | 并行导入线程数（1=不并行，范围 1–256） | `1` | 否 |
| `--log-level` | | 日志级别（debug/info/warn/error） | `info` | 否 |

**`--columns` 列映射说明：**

支持两种格式，通过检测 `=` 符号自动识别：

1. **位置映射**：`--columns "name,age,email"` - 文件列按位置映射到表列
2. **名称映射**：`--columns "user_name=name,user_age=age"` - 显式指定文件列到表列的映射

**位置映射的数量校验：** 位置映射按索引配对，因此 `--columns` 的条目数必须等于文件的列数（列名行给出的列数），条目本身不得为空。数量不符时工具在写入任何数据前报错，提示改用名称映射导入子集。使用 `--no-col-names`（文件无表头）时列数无从得知，只校验条目非空，列数是否匹配交由服务端报错。

**无表头文件与 `-c` 的配对（交叉测试暴露的歧义）：** DEL 读取器总是先调用 `ReadHeader`，只有同时给出 `--no-col-names` 才跳过这一步。于是"文件由 `--no-col-names` 导出、导入时只给 `-c`"的组合会把**首条数据行当成列名行丢掉**，且行数少一并不报错（实测 3 行文件只导入 2 行）；同一组合用于带表头的普通导出文件时行为又是对的。文件本身无法区分这两种情况，改名导入（表头列名与目标表列名不同）也走同一条位置映射路径，因此不做任何猜测、不改变导入结果，只把被丢弃的记录说出来：位置 `-c` + DEL + 未加 `--no-col-names` 时，若被当作表头的那一行字段与目标表列名**全不匹配**（目标表列名取自 `information_schema`），记一条 WARN 日志（`--messages` 打开时这条记录只进日志文件）——`the first record of the input file was treated as the column header and not imported (fields: …); none of them is a column of table "…", so if the file was exported with --no-col-names add --no-col-names to the import to load that record as data`。字段名能对上目标表列时（正常表头）不产生任何噪声，改名导入仍然成功、只是多一条提醒。结论：**`--no-col-names` 是成对选项，导出加了、导入也必须加**（`-c` 只补列名，不补"没有表头"这一事实）。

**目标列前置校验（DEL/Parquet 格式）：** 导入开始前，工具通过 `information_schema` 校验目标表存在以及每个目标列名在表中存在（大小写不敏感）。DEL/Parquet 文件的列头取自导出查询的结果集列名，若 `--query` 含未起别名的表达式列（如 `SELECT 1, COUNT(*)`），列头（`1`、`COUNT(*)`）在目标表中不存在，工具会在写入任何数据前立即报错并提示修复方式（为表达式列起别名，或使用 `--columns` 按位置指定目标列），替代原先晦涩的服务端错误 1054（`Unknown column ... in 'field list'`）。校验位于模式处理之前，replace 模式不再出现"先清空表、后报列错误"的数据丢失。SQL 格式不做该校验：create 模式在表建立前执行属正常流程，且其列名来自文件自身的 INSERT 语句。

**失败 SQL 回显（脱敏）：** 导入/导出语句在服务端执行失败时，错误信息与日志会附带失败 SQL 的结构回显（给了 `--messages` 时这条日志在文件里）（表名、列名、占位符完整保留）。数据值默认**不**内联：仅以 `/* args: str(14), num, NULL, bytes(256) */` 形式的类型/长度标签呈现（可定位"哪一列、什么类型、是否超长"，但不泄露密码、个人信息等真实数据）。回显在 2048 字节处截断且保持引号配平。需要内联真实值时显式开启 `--echo-sql-values` 或 `IMEX_ECHO_SQL_VALUES=1`（警告：真实数据将写入 stderr，或写入 `--messages` 文件）。`--query` SELECT 与 CREATE TABLE 语句不含行数据，失败时原文回显。

#### 2.2.4 导入模式（`--mode`）

| 值 | 说明 | 适用场景 |
|----|------|----------|
| `insert` | 追加数据到表，不修改现有数据 | 表已存在，追加新数据 |
| `insert_update` | 插入新行，或更新主键匹配的行 | 表已存在且有主键，同步数据 |
| `replace` | 删除表的所有数据，然后插入导入的数据 | 表已存在，完全替换数据 |
| `create` | 创建表并导入数据（仅 SQL 格式） | 表不存在，从 SQL 文件创建 |

**CREATE 模式的表名重写：** 当使用 `--mode create` 导入 SQL 文件时，工具会将 SQL 文件中 CREATE TABLE 语句的表名重写为 `--table` 参数指定的目标表名；`--table` 省略时自动沿用 SQL 文件中的原表名。

#### 2.2.5 DEL 格式修饰选项

| 选项 | 说明 | 默认值 | 支持格式 |
|------|------|--------|----------|
| `--char-del` | 字符串定界符（包裹字符串值） | `"` | 单字符、多字节、十六进制（0x） |
| `--col-del` | 列分隔符（分隔同一行中的列） | `,` | 单字符、多字节、十六进制（0x） |
| `--row-del` | 行分隔符（标记一行结束） | `\n` | 单字符、多字节、十六进制（0x） |
| `--escape` | 转义符（用于转义定界符） | `"`（双写转义） | 单字符、多字节、十六进制（0x） |
| `--no-escape` | 不使用转义（读取时不解析转义序列；定界符出现在数据中会破坏记录边界） | `false` | - |
| `--timestamp-fmt` | 时间戳格式（按目标列类型回转换 DATETIME/TIMESTAMP 列） | `yyyy-MM-dd HH:mm:ss` | - |
| `--date-fmt` | 日期格式（按目标列类型回转换 DATE 列） | `yyyy-MM-dd` | - |
| `--skip-count` | 跳过前 n 行**数据**（≥0）；列名行由解析器单独读取，不计入 n。与 `--no-col-names` 同用时首行即为数据行，计入 n | `0` | - |
| `--row-count` | 最多导入 n 行（≥0） | `0`（无限制） | - |
| `--commit-count` | 每 n 行提交一次事务（≥1） | `1000` | - |
| `--no-char-del` | 不使用字符串定界符（裸字段不转义：数据含列/行分隔符时导出报错） | `false` | - |
| `--no-col-names` | 输入文件第一行不是列名。与导出侧同名选项**成对使用**：只给 `-c` 而不给本选项时第一行仍按列名行丢弃，字段与目标表列名全不匹配会给出 warn（见上文"无表头文件与 `-c` 的配对"） | `false` | - |

**格式适用性与行选择：** `--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-escape`/`--timestamp-fmt`/`--date-fmt`/`--no-char-del`/`--no-col-names` 只描述 DEL 文件的解析方式，`--format sql`/`parquet` 下显式传入会报用法错误并退出 2；`--skip-count`/`--row-count`/`--commit-count` 与格式无关，三种格式都生效（它们作用于读取器产出的行流，`--statement-size` 同理作用于本命令生成的语句，故这两组字段在三种格式的配置回显里都打印）。判据同样取自 `internal/config/formatscope.go`，与导出侧的 DEL 组必须是同一批选项（由 `TestDelModifierFlagsAgreeAcrossCommands` 守护）。

**分隔符格式说明：**

- **单字符**：`--col-del ","`、`--col-del "\t"`
- **多字节**：`--col-del "||"`、`--row-del "\r\n"`、`--col-del ":::"`
- **十六进制**：`--col-del 0x09`（制表符）、`--row-del 0x0D0A`（\r\n）、`--col-del 0x7C0x7C`（||）

**`--skip-count` 语义：** 计数的对象是**数据行**。带表头的输入文件，列名行由解析器单独读取并用于列映射，不占用 `--skip-count` 额度；`--no-col-names`（文件无表头）时首行即数据行，才计入跳过额度。这一点与 DB2 `MODIFIED BY SKIPCOUNT <n>`（作用于文件物理首行，含列名行）不同：从 DB2 迁移脚本时，`SKIPCOUNT 1` 对应本工具的默认 `--skip-count 0`。

#### 2.2.6 SQL 格式导入说明

SQL 格式导入无额外修饰选项。`--commit-count`（每 n 行提交一次事务）已在 DEL 修饰选项中列出，对所有格式通用。

**SQL 导入的 `0x` 十六进制字面量解码规则：**

- `0x` 后跟偶数个十六进制数字：解码为对应的字节串。
- 孤立 `0x`（无后续十六进制数字）：解码为**空字节串** `[]byte{}`（与 DEL 格式一致）。
- 奇数长度的 `0x` 字面量：视为畸形，保留原始字节。
- 只有**不带引号**的令牌才参与解码；引号内的 `'0xdead'` 是普通字符串（SQL 导出器对所有字符串值都加引号，因此不存在歧义）。

**DEL 导入的 `0x` 十六进制字面量解码规则：**

DEL 没有引号就无法区分"文本"与"编码"，`--no-char-del` 时更是全部裸字段，因此解码需要**同时满足**三个条件：

1. 该字段**未被字符串定界符包裹**；
2. 该字段对应的**目标列是二进制类型**（TINYBLOB/BLOB/MEDIUMBLOB/LONGBLOB/BINARY/VARBINARY/GEOMETRY/BIT，导入前由 `information_schema.COLUMNS.DATA_TYPE` 查得）；
3. 令牌是**合法的偶数长度** `0x…`（孤立 `0x` 解码为空字节串）。

任一条件不满足即按原文处理：写入 varchar 列的 `"0xdead"` 仍是 6 个字符；无 schema 信息时（列类型查询失败）不启用解码，退化为原行为。

#### 2.2.7 使用示例

```bash
# 基本导入：从 DEL 文件追加数据
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users

# 从 Parquet 文件导入
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.parquet -f parquet \
  -t users

# 自定义分隔符导入
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --char-del '!' --col-del '@' \
  -t users

# insert_update 模式：插入或更新
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode insert_update \
  -t users

# replace 模式：替换所有数据
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode replace \
  -t users

# create 模式：从 SQL 文件创建表并导入
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.sql -f sql \
  --mode create

# 指定目标列（位置映射）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "id,name,email"

# 指定目标列（名称映射）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "user_id=id,user_name=name,user_email=email"

# 跳过前 1 行数据（列名行由解析器自动读取，不占用 skip-count），导入 1000 行
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --skip-count 1 --row-count 1000 --commit-count 100 \
  -t users

# 并行导入（4线程）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --threads 4 \
  -t users

# 带日志文件
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -m /var/log/imex_import.log \
  -t users
```

---

### 2.3 日志文件（`--messages` / `-m`）

`--messages <file>` 把**本次运行的命令行日志交给这个文件**：记录原本的目的地是 stderr，挂载文件后同一个 sink 改写文件，因此终端不再打印任何一行，文件是这次运行的唯一记录；内容仍是标准日志记录（一条记录一个物理行），不再是由 `MessageWriter` 另写一遍的 DB2 `SQL3xxxN` 报告。

`imex export -u gd -p *** -D imex_test -h 127.0.0.1 -P 3306 -q "SELECT * FROM no_such_table_x" -o <tmp>/smoke5.del -m <tmp>/smoke5.log` 写出的文件全部内容（4 行，`export config` 的中间字段以 `...` 省略）：

```
2026-10-24 12:08:59.384 [2201] INFO imex starting version=dev build_time=unknown git_commit=unknown go_version=go1.26.9 command=export
2026-10-24 12:08:59.384 [2201] INFO export config host=127.0.0.1 port=3306 user=gd database=imex_test output=<tmp>/smoke5.del format=del query="SELECT * FROM no_such_table_x" threads=1 ... log_level=info batch_size=1000 pipeline_buffer=10 progress_interval=5s hex_blob=false
2026-10-24 12:08:59.390 [2201] ERROR query failed error="Error 1146 (42S02): Table 'imex_test.no_such_table_x' doesn't exist" sql="SELECT * FROM no_such_table_x"
2026-10-24 12:08:59.390 [2201] ERROR command failed error="export failed: failed to execute query (SQL: SELECT * FROM no_such_table_x): Error 1146 (42S02): ..."
```

那一次运行的 stdout 与 stderr 各 0 字节，退出码 1；把 `-m` 去掉重跑同一条命令，stderr 上就是上面这 4 行（只差时间戳与 PID 前缀）。

配置回显由 `NewExporter`/`NewImporter` 在**建立连接之前**打印，因此连不上服务端时参数照样可见（上一条 `export started` 与它字段重复，已删除）。实测 `-p ***` 打错密码那次：文件 6 行、退出 1、输出文件根本没有创建。上面两处样例是 2026-10-24 的快照，当时的回显对三种格式是同一套 26 个字段；现版本按 `--format` 过滤（判据见 5.3 与 `internal/config/formatscope.go`），实测同一命令 `-f del/sql/parquet` 为 25/20/15 个字段，排列也改为「连接 → 输入输出 → 并行与流水线 → 字符集 → 该格式专属布局 → 日志」，`-m` 文件的"一行一条记录"与记录条数不受影响（实测两次成功导出各 3 行）。

```
... INFO imex starting version=dev ... command=export
... INFO export config host=127.0.0.1 port=3306 user=gd database=imex_test ...
... WARN database ping failed, retrying attempt=1 error="Error 1045 (28000): Access denied for user 'gd'@'127.0.0.1' (using password: YES)"
... WARN database ping failed, retrying attempt=2 error="Error 1045 (28000): ..."
... WARN database ping failed, retrying attempt=3 error="Error 1045 (28000): ..."
... ERROR command failed error="failed to create exporter: failed to ping database after 3 attempts: Error 1045 (28000): ..."
```

**行为约定：**

| 项目 | 行为 |
|------|------|
| 打开时机 | 根命令 `PersistentPreRunE` 在打印版本横幅**之前**打开文件（`os.Create`，已存在的同名文件被截断），因此横幅恒为文件首行；参数校验阶段的用法错误也照样落进文件 |
| 终端 | **静默**：文件挂载期间 stderr 不再打印任何记录，stdout 本来就不承载日志。成败只由退出码判断（0 成功、1 运行时错误、2 用法错误，见 3.2）|
| 覆盖范围 | 全部日志记录：横幅、`export/import config`（在建连接之前）、定时进度、WARN 告警、`completed` 统计、收尾的 `command failed`；导出器与导入器内部不再有独立的消息写入路径（原 35 处 `MessageWriter` 调用删除：导出 15、导入 20） |
| 关闭时机 | `cmd.Execute()` 返回前关闭，因此最后那条 `command failed` 也在文件里；关闭自身失败的 ERROR 记录只进 stderr（此时文件已脱离 sink，这条唯一不落文件的记录因此不会丢）|
| 例外 | 选项解析阶段的错误（未知选项、缺少参数值）发生在 `PersistentPreRunE` **之前**，文件尚未打开，因此该错误只出现在 stderr 且**不创建文件**；`--messages` 路径本身无法创建时同样只报 stderr（`failed to create message file <path>: ...`，退出 1）；`--help` 与 `-m` 同时给不产生文件 |
| `--log-level` | 生效：`--log-level error` 下成功导出，文件只剩版本横幅一条（横幅在 `PersistentPreRunE` 打印，早于 `RunE` 应用 `--log-level`，这是既有行为，且这条 INFO 只进文件、不回到终端）|
| `--log-format` | 生效：文件采用所选布局（`java` / `text`），终端不重复输出 |
| `--quiet` | 生效：定时进度被抑制时文件里也没有。旧版"消息文件是显式产物、不受静默影响"的例外随 DB2 报告一起取消 |
| 写入串行化 | 日志 sink 带互斥锁，并行路径（进度监视协程与分片协程）不会把两条记录交叉写进文件 |
| 安全 | 密码不进日志，因而也不进该文件（见 8.4）；`--echo-sql-values` 打开后真实行数据写入 stderr 或该文件（二选一，取决于是否给了 `-m`）|

会出现告警（WARN）的场景：导出侧 `--no-hex-blob` 与非 utf8mb4 字符集组合、单行 INSERT 超过 `--statement-size`；导入侧读不到 `max_allowed_packet`（按保守值分批）、`--batch-size` 被 65535 占位符上限下调、无表头文件被当作表头丢弃首行（见 7.4）、`insert_update` 命中无唯一键的表。每个场景只有一条 WARN 记录，不再另写一份 `SQL3107N`。

### 2.4 encrypt 子命令 — 密码加密

把明文密码加密成 `ENC:` 前缀密文，供 `-p/--password` 直接使用，避免明文出现在 `ps` 输出与 shell history 中。

| 项目 | 实现 |
|------|------|
| 语法 | `imex encrypt <password>`，位置参数恰好 1 个；多给或不给是用法错误，退出 2（见 3.3.4）|
| 密钥来源 | 环境变量 `IMEX_KEY`（`common.EnvKey`）；未设置或为空即失败，提示先 `export IMEX_KEY="..."` |
| 算法 | AES-256-GCM：密钥 = `sha256(IMEX_KEY)`，随机 nonce 前置进密文，整体 `base64.StdEncoding` 后加 `ENC:` 前缀（`internal/common/password.go`）|
| 使用侧 | export/import 在 `RunE` 起始处调用 `common.ResolvePassword`（`cmd/export.go:72`、`cmd/import.go:67`）：值以 `ENC:` 开头则解密，否则原样按明文处理。密钥缺失报 `failed to resolve password: password is encrypted but IMEX_KEY environment variable is not set`，密钥不符报 `failed to resolve password: failed to decrypt: wrong key or corrupted data`，两者都是**运行时错误，退出码 1**（不是用法错误的 2，因为失败发生在参数校验之后）|
| 密文非确定性 | nonce 每次随机，同一明文 + 同一密钥两次加密结果不同，不可把密文当等值键比较或做 diff |
| 输出 | 标准输出仅一行密文；stderr 首行仍是 java 布局的版本横幅（`command=encrypt`），便于日志归属 |
| 安全边界 | 密码不写入日志，因而也不写入 `--messages` 文件（见 8.4）。但 `IMEX_KEY` 本身就是凭据，明文放在环境变量里与明文密码同级风险，该机制只解决"命令行可见性"，不解决密钥托管 |

---

## 三、全局选项

### 3.1 错误堆栈开关（`--error-stack`）

命令失败时统一以 **ERROR 级日志记录**输出（`level=ERROR msg="command failed" error="..."`），与其它诊断信息格式一致，不再单独打印裸 `Error:` 行。默认只含错误链文本；开启错误堆栈后，通过 `github.com/pkg/errors` 的 `%+v` 格式在 `error=` 字段内追加完整调用堆栈，便于排障。

**开启方式：**

1. **命令行标志**（持久标志，所有子命令可用）：
   ```bash
   imex export --error-stack -h 127.0.0.1 -u root -p pass -D mydb \
     -o /backup/users.del -q "SELECT * FROM nope"
   ```

2. **环境变量**（适合脚本/容器）：
   ```bash
   IMEX_ERROR_STACK=1 imex import -h 127.0.0.1 -u root -p pass -D mydb \
     -i /backup/users.del -t users
   ```
   环境变量支持的值：`1`、`true`、`yes`、`on`（不区分大小写）

**输出示例（默认）：**
```
time=... level=ERROR msg="command failed" error="failed to create exporter: Error 1146 (42S02): Table 'mydb.nope' doesn't exist"
```

**开启 `--error-stack` 后（`error=` 字段内追加各层 wrap 调用栈）：**
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

### 3.2 帮助短选项（`-?`）

export/import 子命令把 `-h` 让给了 `--host`，因此这两个子命令的帮助短选项改为 `-?`（根命令仍使用默认的 `--help`，帮助文本中也会显示 `-?, --help`）：

```bash
imex export -?       # 显示 export 子命令帮助（-h 为 --host）
imex import -?       # 显示 import 子命令帮助（-h 为 --host）
imex --help          # 根命令帮助
```

### 3.3 completion 命令禁用

cobra 框架默认生成的 `completion` 子命令已被禁用（`rootCmd.CompletionOptions.DisableDefaultCmd = true`），避免与工具本身的命令风格不一致。

### 3.3.1 静默模式（`--quiet`）

默认导出、导入均每隔 5 秒打印一次进度日志，间隔由 `--progress-interval` 调整（必须 > 0，否则用法错误退出 2）。`--quiet`（或 `IMEX_QUIET=1`，值支持 `1`/`true`/`yes`/`on`，不区分大小写）禁止定时进度日志，但**保留**版本横幅、配置参数、结束统计与 ERROR 记录。该记录本就不产生，因此 `--messages` 文件里同样没有进度行（旧版"消息文件仍记 `SQL3105N`"的例外随 DB2 报告一起取消）。

```bash
imex import --quiet -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
IMEX_QUIET=1 imex export -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

### 3.3.2 日志格式（`--log-format`）

`--log-format` 控制日志记录的布局格式（持久标志，所有子命令可用），也可通过环境变量 `IMEX_LOG_FORMAT` 设置。

| 值 | 说明 |
|----|------|
| `java`（默认） | 固定宽度行格式：`2026-01-02 15:04:05.000 [pid] LEVEL message key=value ...`，类似 JUL Formatter，便于 grep 和 tail |
| `text` | slog 原生 key=value 文本格式，适合机器解析 |

```bash
imex export --log-format text -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
IMEX_LOG_FORMAT=text imex import -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
```

日志格式在 `PersistentPreRunE` 中尽早应用，即使用法错误（退出码 2）也使用所选格式输出。

**单行保证：** 两种布局都遵循「一条记录 = 一个物理行」。值为空或含空白、控制字符、`"`、`\`、`=` 时，按 slog 原生规则加引号并转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`）。因此打印 DEL 定界符的配置行不会被真实换行拆开，`grep`/`tail` 每次都能取到完整记录；同一值在 `java` 与 `text` 两种布局下渲染成同一个 token，切换格式不影响字段切分。

**布局切换不改级别：** `ApplyLogFormat()` 只重设布局，`--log-level` 选择的阈值保持不变，版本横幅与随后所有 INFO 记录照常输出。

**消息命名：** 所有经全局 logger 输出的记录统一为小写、单行的 `<对象> <事件>` 短语，事件词固定为 `config` / `progress` / `completed` / `aborted` / `failed`（`started` 只出现在版本横幅 `imex starting` 里），因此大小写敏感的 grep 一条 pattern 就能覆盖全链路。主要记录：

| 记录 | 触发时机 |
|------|----------|
| `imex starting` | 每次真实运行的首行（version/build_time/git_commit/go_version/command）；`version` 子命令本身不产生任何日志记录 |
| `export config` / `import config` | 生效参数快照，**只打印所选 `--format` 真正读取的字段**：判据取自 `internal/config/formatscope.go` 这张登记表，与用法错误的拒绝逻辑、`--help` 里的范围标记同源（`-f del` 不打 `statement_size`/`no_create`/`extended_insert`，`-f sql` 不打 DEL 定界符与文本修饰，`-f parquet` 两类都不打；实测 export 为 25/20/15 个字段、import 为 29/20/20）。其余字段按「连接 → 输入输出 → 行流控制 → 并行与流水线 → 字符集 → 该格式专属布局 → 日志」排列，含 DEL 定界符与文本修饰（`date_fmt`/`no_escape`/`no_char_del` 三项此前漏打，现已补齐）、`hex_blob`（导出侧打印**生效值**而非 `--hex-blob` 标志位，与写入器共用 `effectiveHexBlob()`）、`progress_interval`；导入侧的 `statement_size` 与格式无关（它约束本命令生成的语句，不约束输入文件），故三种输入格式都打印。由 `NewExporter`/`NewImporter` 在**建连接之前**打印，因此连接失败时也看得到参数；紧随横幅之后（原先重复它的 `export started`/`import started` 已删除） |
| `session variables will be set on every connection` | 传入了 `--session-var` |
| `export progress` / `import progress` | 每 `--progress-interval`（默认 5 秒）一条定时进度（`--quiet` 抑制；运行短于间隔时一条也不打印） |
| `parallel export detected` / `parallel export ignores pipeline options` / `shard export completed` / `parallel export completed` | 并行导出：分片计划、被忽略的管道选项（仅当改动过 `--batch-size`/`--pipeline-buffer` 任一值才出现，级别 `info`，带 `batch_size`/`pipeline_buffer`/`look_ahead_per_shard`/`applies_to`）、每个分片、汇总 |
| `shard stream failed` / `incomplete output file removed` | 导出写出失败（如 `ENOSPC`、写入被拒）：前者 `error` 仅并行路径出现，带具体错误；后者 `warn` 带被删除的 `output=`，并行与单线程两条路径都会在失败时删除已写出的半截产物（单线程此前不会删除，实测留下 21 MB 残留文件），其余仍在队列上等待的分片此时以 `shard output consumer stopped` 结束 |
| `import batch byte cap resolved` / `could not read max_allowed_packet, assuming conservative batch limit` | 导入首次攒批前解析每批字节预算（每次运行一条，`info`）：带 `max_allowed_packet`（读到的服务端值）、`batch_bytes`（生效预算）、`statement_size`（导入侧的可选压低值，`0`=不额外压低）、`limiting_factor`（`max_allowed_packet` 或 `statement-size`，即谁定的预算）；后者 `warn` 在读不到该变量时出现，按 4 MiB 保守值继续 |
| `worker completed` / `parallel import completed` | 并行导入：每个工作线程、汇总 |
| `export completed` / `import completed` / `import aborted` | 结束统计（中止时仍给出已达进度与 `failed`） |
| `query failed` / `insert batch failed` / `create table failed` / `shard query failed` / `shard export failed` | 语句级失败，附 `sql` 预览（默认脱敏） |
| `command failed` | 最终失败汇总，`--error-stack` 附带调用栈 |

同一事件只输出一行：原先紧随 `export config`/`import config` 的 `Starting export`/`Starting import` 属于重复记录，已删除（字段全部被 config 记录覆盖，输入文件字节数改由 `import config` 的 `bytes` 承载）。该约定由 `main_test.go` 的 `TestLogMessageStyle` 静态守护：解析全部源文件，检查每个 `logger.Debug/Info/Warn/Error` 的字面量消息首字母小写且不含换行。

### 3.3.3 SQL 归因注释（`--no-sql-tag`）

默认情况下，工具执行的每条 SQL 语句前会添加归因注释 `/* imex:<commit7> */`（`<commit7>` 为构建时通过 ldflags 注入的 Git commit 前 7 位十六进制短哈希），便于在 `general_log`、`slow_query_log` 和审计流中识别工具来源。

`--no-sql-tag`（或 `IMEX_NO_SQL_TAG=1`）关闭该注释。以下情况注释自动为空：

- 构建未注入 commit（dev 构建报告 `"unknown"`）
- 注入值不是合法的十六进制哈希（防止 SQL 注入）

注意：归因注释仅添加到数据操作语句（INSERT、DELETE、SHOW CREATE TABLE 等），连接生命周期语句（SET NAMES、START TRANSACTION、COMMIT、SET FOREIGN_KEY_CHECKS）不添加。

```bash
imex import --no-sql-tag -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
IMEX_NO_SQL_TAG=1 imex export -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

### 3.3.4 退出码

| 退出码 | 含义 |
|--------|------|
| `0` | 成功 |
| `1` | 运行时错误（连接失败、SQL 执行失败、文件 I/O 错误等） |
| `2` | 用法错误（参数校验失败、未知选项） |

判定口径（`main.go:17-21`）：错误来自 `common.Usagef`（`common.IsUsageError`）或 cobra 的 `unknown flag` / `unknown command`，一律 2；其余为运行时错误 1。两条容易误解的边界：

- 环境变量导致的失败按发生位置分流：`IMEX_LOG_FORMAT` 非法在 root 的 `PersistentPreRunE` 校验阶段拦下，退出 2；`IMEX_KEY` 缺失/不符发生在参数校验之后的密码解析阶段，退出 1。
- 位置参数个数也属于用法错误。cobra 自带的 `ExactArgs` 返回的是普通 error（会被算成运行时错误的 1），因此 `encrypt` 改用本仓库的 `cmd/validate.go:exactArgs`，消息为 `imex encrypt accepts 1 argument(s), received N` 并退出 2；`export`/`import` 的 `cobra.NoArgs` 消息含 `unknown command`，由 `main.go` 兜底归为 2。由 `cmd/cli_matrix_test.go:TestCLIEncryptArgsCountIsUsageError` 锁定（同时断言密钥缺失仍是 1，两级错误不许混淆）。

### 3.4 失败 SQL 回显与脱敏（`--echo-sql-values`）

导入/导出过程中语句在服务端执行失败时（如列不存在、类型不符、约束冲突、数值超长），错误信息与结构化日志都会附带**失败 SQL 的结构回显**（给了 `--messages` 时这条日志在文件里），便于定位问题：表名、列名、`?` 占位符完整保留。

出于数据安全，默认**不内联真实行数据值**。`?` 占位符只以类型/长度标签呈现：

```
INSERT INTO `u` (`id`, `pw`) VALUES (?,?),(?,?) /* args: str(1), str(14), str(1), str(12) */
```

标签可判断"哪一列、什么类型、字符串/字节长度、是否 NULL"，足以解决 `Data too long for column` 一类问题，同时避免密码、个人信息、业务数据落入长期留存的日志与日志文件。回显在 2048 字节处按空白边界截断并保持单引号配平。

**开启真实值内联（谨慎）：**

```bash
# 命令行标志（持久标志，所有子命令可用）
imex import --echo-sql-values ...

# 环境变量（适合脚本/容器）
IMEX_ECHO_SQL_VALUES=1 imex import ...
```

> 警告：开启后失败 SQL 回显将包含真实数据值，会写入日志——stderr，或 `--messages` 文件，仅应在受信任的排障环境临时使用。

`--query` 的 SELECT 语句与 SQL 文件里的 CREATE TABLE 语句本身不含行数据，失败时按原文回显，不受该开关影响。

---

## 四、DB2 命令到 imex 选项映射

| DB2 EXPORT/IMPORT 语法 | imex 选项 |
|------------------------|---------------|
| `TO <filename>` | `--output` / `-o` |
| `FROM <filename>` | `--input` / `-i` |
| `OF <filetype>` | `--format` / `-f` |
| `MESSAGES <file>` | `--messages` / `-m`（**语义不同**：DB2 写独立消息报告，本工具把本次运行的命令行日志整体交给该文件、终端静默，见 2.3） |
| `<select-statement>` | `--query` / `-q` |
| `<import-mode>` | `--mode` / `-M` |
| `INTO <table>` | `--table` / `-t` |
| `(<column-list>)` | `--columns` / `-c` |
| `MODIFIED BY CHARDEL<x>` | `--char-del <x>` |
| `MODIFIED BY COLDEL<x>` | `--col-del <x>` |
| `MODIFIED BY ROWDEL<x>` | `--row-del <x>`（扩展，DB2 无直接等价） |
| `MODIFIED BY ESCAPECHAR<x>` | `--escape <x>` |
| `MODIFIED BY NOESCAPE` | `--no-escape` |
| `MODIFIED BY TIMESTAMPFORMAT="<x>"` | `--timestamp-fmt "<x>"` |
| `MODIFIED BY DATEFORMAT="<x>"` | `--date-fmt "<x>"` |
| `MODIFIED BY NOCHARDEL` | `--no-char-del` |
| `MODIFIED BY NOCOLNAMES` | `--no-col-names` |
| `MODIFIED BY REPLACE` | 默认覆盖（`os.Create` 截断）；`--no-overwrite` 改为拒绝覆盖（该 DB2 子句仍叫 `REPLACE`，imex 侧选项 2026-10-26 由 `--no-replace` 改名为 `--no-overwrite`） |
| `MODIFIED BY STATEMENTSIZE=<n>` | `--statement-size <n>`（默认单行布局下退化为告警阈值，只有 `--extended-insert` 时参与语句合并） |
| `MODIFIED BY NOCREATE` | 默认行为（SQL 导出本就不写 `CREATE TABLE`，`--create` 才附带；`--no-create` 已删除） |
| `MODIFIED BY NODATA` | `--no-data` |
| `MODIFIED BY SKIPCOUNT <n>` | `--skip-count <n>` |
| `MODIFIED BY ROWCOUNT <n>` | `--row-count <n>` |
| `MODIFIED BY COMMITCOUNT <n>` | `--commit-count <n>` |
| —（DB2 无对应） | `--progress-interval <dur>`：定时进度日志的间隔，默认 5 秒（本工具自有选项） |
| —（DB2 无对应） | `--extended-insert` 与已废弃的 `--single-row-insert`：SQL 文件的语句布局开关（本工具自有选项，mysqldump 用 `--extended-insert`/`--skip-extended-insert` 表达同一对语义） |

**已移除的 DB2 映射选项（代码中不再存在）：**

| 已移除 DB2 语法 | 说明 |
|----------------|------|
| `MODIFIED BY DECPT<x>` | 小数点字符——已移除，数值始终使用 MySQL 线格式（`.`） |
| `MODIFIED BY CODEPAGE=<x>` | 字符集——已替换为 `--default-charset` |
| `MODIFIED BY STRIPLZEROS` | 去除前导零——已移除 |

---

## 五、文件格式详细说明

### 5.1 DEL 格式（定界 ASCII）

**文件结构：**
```
"col1","col2","col3"          # 第一行：列名（可选）
"value1","value2","value3"    # 数据行
"value4","value5","value6"
```

**示例：**
```
"id","name","email","created_at"
1,"Alice","alice@example.com","2026-01-01 10:00:00"
2,"Bob","bob@example.com","2026-01-02 11:00:00"
3,"Charlie","charlie@example.com","2026-01-03 12:00:00"
```

**特点：**
- 每行一条记录
- 列之间用列分隔符分隔（默认逗号）
- 字符串用字符串定界符括起来（默认双引号）
- NULL 值表示为空（两个连续分隔符之间无内容）
- 支持自定义分隔符

### 5.2 SQL 格式

**文件结构（默认，每行一条 INSERT）：**
```sql
/*!40101 SET NAMES utf8mb4 */;
LOCK TABLES `users` WRITE;
-- Table structure for users
CREATE TABLE `users` (
  `id` int NOT NULL AUTO_INCREMENT,
  `name` varchar(100) NOT NULL,
  `email` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Data for users
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00');
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (2,'Bob','bob@example.com','2026-01-02 11:00:00');
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (3,'Charlie','charlie@example.com','2026-01-03 12:00:00');
UNLOCK TABLES;
```

**`--extended-insert` 模式下的文件结构：**
```sql
/*!40101 SET NAMES utf8mb4 */;
LOCK TABLES `users` WRITE;
-- Table structure for users
CREATE TABLE `users` (...);

-- Data for users
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00'),(2,'Bob','bob@example.com','2026-01-02 11:00:00'),(3,'Charlie','charlie@example.com','2026-01-03 12:00:00');
UNLOCK TABLES;
```

**特点：**
- 示例含 CREATE TABLE 语句，对应 `--create`；SQL 导出默认**不带** CREATE TABLE，只写下面的 `INSERT` 数据（详见 2.1.6）
- 默认模式：每行一条 INSERT，文件可按行切分（mysqldump `--skip-extended-insert` 的布局）
- `--extended-insert`：多条 VALUES 合并为一条 INSERT，合并上限由 `--statement-size`（默认 8 MiB）控制。合并只改变文件体积，不改变导入侧打给服务端的语句——导入器把语句拆成行后按 `--batch-size` 重新攒批（见 2.1.6 实测）
- 字符串值按转义规则处理（见 2.1.6），确保每条 INSERT 恰好占一个物理行
- 文件首行包含 `/*!40101 SET NAMES <charset> */;`
- MySQL 下数据段带 LOCK TABLES/UNLOCK TABLES 一对；OceanBase 下不带
- imex 自身的导入路径跳过 `SET NAMES`/`LOCK TABLES`/`UNLOCK TABLES` 这些前导语句，不受布局影响。**但用标准 `mysql` 客户端回放空库要注意**：`LOCK TABLES` 写在 `CREATE TABLE` 之前，实测对不存在的表执行该文件在第 2 行就 `ERROR 1146 (42S02) Table doesn't exist` 中止，需先建表或剥掉这对锁语句（该顺序问题已在变更记录 47 行登记，待用户定级）

### 5.3 Parquet 格式

**文件结构：**
Parquet 为二进制列式存储格式，无法直接文本预览。

**Schema 示例：**
```
message schema {
  optional binary id (STRING);
  optional binary name (STRING);
  optional binary email (STRING);
  optional binary created_at (STRING);
}
```

**特点：**
- 普通列存储为可空 UTF-8 字符串（`optional binary (STRING)`）；二进制列（BLOB/BINARY 等）存储为不带 UTF-8 标注的原始 BYTE_ARRAY，非 UTF-8 内容可完整往返
- NULL 值保留为 Parquet NULL（与空字符串区分）
- 行序与 `--query` 查询结果一致
- 列式存储，适合分析型查询和大数据工具（DuckDB、Spark 等）
- 不支持并行导出（分片文件无法拼接），`--threads>1` 时自动降级为单线程
- 查看 schema 需要 parquet-tools 或 DuckDB 等工具：
  ```bash
  parquet-tools schema /backup/users.parquet
  ```

### 5.4 三种格式共有的值处理

| 项 | 行为 | 依据 |
|----|------|------|
| `YEAR` 零值 | 文本协议把 `YEAR(0000)` 发回裸字符串 `0`，直接回导会命中服务端两位 YEAR 规则解释为 2000（静默改值）。导出时按 `DatabaseTypeName()==YEAR` 识别列，把单字符 `0` 重写为 `0000`，DEL/SQL/Parquet 一致；非零 YEAR 原样透传 | `internal/export/exporter.go:242-246`、`370-372`；实测 `--format del` 得 `"2","1999",""`、`"1","0000",` |
| NULL 与空字符串 | DEL（默认带字符串定界符）：NULL 写成两个列分隔符之间的空内容且不加定界符，空字符串写成 `""`，因此可区分。DEL + `--no-char-del`：两者都是空字段，**不可区分**，回读时一律按 NULL 处理。SQL：NULL 写 `NULL` 关键字、空串写 `''`。Parquet：NULL 保留为 Parquet NULL，与空字符串天然区分 | `internal/export/del_writer.go:105-121`；实测 `--no-char-del` 下 `2,1999,` 与 `1,0000,` 形态一致 |
| 二进制列 | DEL/SQL 在 `--hex-blob` 生效时写 `0x…` 字面量、空值写 `0x`（与 NULL 区分）；Parquet 存原始 BYTE_ARRAY，不带 UTF-8 标注 | 见 2.1.6、5.3 |
| 日期/时间戳 | `--timestamp-fmt`/`--date-fmt` 只按列类型（`common.ColumnKind`）修饰文本形态，数值/字符串列不受影响 | 见 2.1.5、2.2.5 |

---

## 六、OceanBase MySQL 模式兼容性

### 6.1 自动检测

```sql
SHOW VARIABLES LIKE 'version_comment';
-- 返回包含 'OceanBase' → 识别为 OceanBase
```

### 6.2 兼容性处理

| 特性 | MySQL | OceanBase | 处理策略 |
|------|-------|-----------|----------|
| 一致性快照 | 支持 | 支持 | 统一使用 |
| `LOCK TABLES` | 支持 | 部分支持 | OceanBase 默认跳过 LOCK/UNLOCK |
| 外键 | 支持 | 部分支持 | 导入时 `SET FOREIGN_KEY_CHECKS=0` |
| 字符集 | utf8mb4 | utf8mb4 | 统一使用 `--default-charset` |
| 租户连接 | 不需要 | 可能需要 | 通过用户名 `user@tenant#cluster` 格式传递 |

---

## 七、核心架构设计

### 7.1 整体架构

```
┌──────────────────────────────────────┐
│          CLI (cobra)                 │
│   imex export / import / version │
├──────────────────────────────────────┤
│           Config Manager             │
├─────────────┬────────────────────────┤
│  Exporter   │      Importer          │
│  ┌───────┐  │  ┌────────┐           │
│  │  DEL  │  │  │  DEL   │           │
│  │Writer │  │  │ Reader │           │
│  ├───────┤  │  ├────────┤           │
│  │  SQL  │  │  │  SQL   │           │
│  │Writer │  │  │ Reader │           │
│  ├───────┤  │  ├────────┤           │
│  │Parquet│  │  │Parquet │           │
│  │Writer │  │  │ Reader │           │
│  └───┬───┘  │  └───┬────┘           │
│      │      │      │                │
├──────┴──────┴──────┴────────────────┤
│         Connection Pool             │
├─────────────────────────────────────┤
│      Server Detector                │
│   (MySQL / OceanBase)               │
├─────────────────────────────────────┤
│      go-sql-driver/mysql            │
└─────────────────────────────────────┘
```

### 7.2 export 流程

```
1. 解析命令行选项
2. 建立数据库连接
3. 执行 --query 中的 SELECT 语句获取结果集
4. 根据 --format 选择输出格式：
   - del：流式写入定界 ASCII 文件
   - sql：默认只生成指向裸表名的 `INSERT` 语句（`--create` 才在头部加 `CREATE TABLE`；默认每行一条 INSERT，`--extended-insert` 才按 `--statement-size` 把多行合并进一条语句；`--query` 里的 schema 限定被剥离，见 2.1.6）
   - parquet：列式写入 Parquet 文件（普通列为可空 UTF-8 字符串，二进制列为原始 BYTE_ARRAY）
5. 应用修饰选项（--char-del, --col-del, --default-charset 等）
6. 输出配置参数快照（在建立连接之前，见 NewExporter/NewImporter）
7. 每隔 --progress-interval（默认 5 秒）记录一次进度（--quiet 时不记；运行短于一个间隔则一条也没有），全程写入 stderr 或 --messages 文件
8. 停止进度计时器，再输出统计信息（汇总恒为最后一条记录）
```

**并行导出流程（`--threads>1`）：**
```
1. 检测查询复杂度（JOIN 查询自动降级为单线程）
2. 检测分片键（主键 > 唯一键 > 用户指定 --shard-key）
3. 生成分片范围
4. 每个分片独立连接、独立查询，把序列化结果写入自己的字节队列（`shardPipe`）
   - SQL 格式下，分片片段不写 LOCK TABLES/UNLOCK TABLES（SetSkipTableLocks=true）
   - 分片片段不写 CREATE TABLE（writeCreate=false）
   - 分片读完自己的全部行之后**立刻**关闭 `rows` 与该分片连接（结束一致性快照），再写页脚、关队列
5. writeShardStream 流式写出到最终文件：
   - 先由 SQLWriter/DelWriter 写出 header（SET NAMES、LOCK TABLES，`--create` 时还有 CREATE TABLE）
   - 再按分片序号依次消费各自的队列（分片仍在并行产出，只是文件位置由这一个 goroutine 独占）
   - 最后由 SQLWriter 写出 footer（UNLOCK TABLES 等）
6. 不产生任何临时文件
7. 停止进度计时器（间隔由 --progress-interval 决定），再写出 `parallel export completed` 汇总（汇总恒为最后一条记录）
```

**并行导出不再写临时文件（`shard_stream.go`）：** 旧实现是每个分片写一份 `<output>.shard_<i>.tmp`，全部分片结束后再 `io.Copy` 拼接、删除临时文件。代价是**每个字节落盘两次**：实测 50 万行表（DEL 输出 124,903,023 B）单次并行导出的写入量为 487,920 × 512 B ≈ 238 MB，约输出体积的 2 倍，SQL 格式为 515,152 块 ≈ 251 MB 对 131,872,590 B（两轮重复测得同一组数字）；容量上意味着并行导出需要两倍的空闲空间，且中途被信号打断会在输出目录留下一堆 `<output>.shard_N.tmp`（实测 4 个共 33 MB）。现在分片直接把 `[]byte` 块交给按分片序号消费的那个 goroutine，写入量降到 243,952 / 257,568 块（等于输出体积按 512 B 向上取整，写放大归零），输出目录里只有最终文件本身。字节序列与旧实现**逐字节一致**（DEL/SQL 两种格式、同一库同一时刻分别与旧二进制比对），导出内容与单线程导出、以及导入回读的行集（count + 顺序无关 CRC 和）也都一致。

队列深度不是细节：单个输出 goroutine 一旦偶尔落后，分片就会填满队列并停下，而它停下的时候其他分片的数据库读取仍在推进，整体变成"读一批、写一批"的停顿。同一台 4 核机器上以 SQL 格式实测（每组 7 次交替、取中位数，消除温漂）：旧实现 1.09 s；每分片 2.5 MiB 前瞻为 1.31 s、8 MiB 为 1.24 s（慢 14~20%）；16 MiB 为 1.09 s，与旧实现持平——写入量减半不再牺牲时间。因此 `shardStreamQueueBlocks` 用一个固定的 64 MiB 总预算按分片数均分，上界 64 块（16 MiB）、下界 2 块，与 `--pipeline-buffer`（按行数计）无关。代价是这部分在途字节从磁盘移到堆上：4 分片并行导出的进程峰值 RSS 由约 33 MB 升到约 111 MB（DEL）、114 MB 升到 215 MB（SQL），8 分片 DEL 为 36→118 MB；`--threads 8` 的产物同样与旧实现逐字节一致、写入量同样是恰好一半。也正因为每分片前瞻由这个预算决定，`--batch-size`/`--pipeline-buffer` 在并行路径上没有任何作用（这两个字段只有 `exportSingle` 读取）；改动过其中任一个时 `exportParallel` 记一条 `info` 级 `parallel export ignores pipeline options`，带上生效值与实际前瞻字节，把"参数被忽略"变成可见的事实，而不是静按默认跑完——都保持默认值的常见运行完全静默。

**分片读完即放连接，不等队列排空：** 上面那种"停下"发生在两种地方，代价完全不同。读到一半停下是背压，是设计要的；而**行已经全部读进内存、只剩把尾部字节交给流式写出 goroutine**时停下，是什么都换不到的空等——这段时间里那条分片连接连同它的 `START TRANSACTION WITH CONSISTENT SNAPSHOT` 一直被持有。旧实现正是这个顺序：`exportShard` 里 `defer rows.Close()` / `defer db.Close()` 排在函数返回之后，而返回之前要先写页脚（SQL 格式还要刷出最多 `--statement-size`，默认 8 MiB 的攒批 INSERT）、再 `pipe.Close()`，这两步都可能阻塞到写出 goroutine 轮到本分片。云环境下这个空等窗口是要命的：连接经过 LB/NAT 时空闲 socket 会被回收，实测本地以 400 万行倾斜表（分片 0 占 360 万行、分片 4 只有 40 万行）`--threads 5` 导出，`SHOW PROCESSLIST` 里一个已完成读取的分片停在 `Writing to net` 约 5 秒、`INNODB_TRX` 快照年龄一路涨到 9 秒——那 5 秒它一行数据都不再产生，只是被前面的分片堵着，而连接和快照都还占着。现在 `exportShard` 在行迭代结束（`rows.Err()` 判过）之后显式调用 `releaseDB()` 关掉 `rows` 与 `db`，页脚与 `pipe.Close()` 挪到释放之后；`releaseDB` 幂等，`defer` 只做兜底，注册顺序保证查询失败、页脚失败等路径下数据库仍是最先被放弃的资源。实测这条**是不变量而不是吞吐**：同一份倾斜数据新旧二进制的产物逐字节一致（DEL 587,806,929 B、md5 `2c9834af64e6` 相同，SQL `--extended-insert` 642,102,879 B、md5 `503ae9219565` 相同），耗时也一致（DEL 8.334/8.167 s 对 8.140/8.050 s，SQL 12.284/12.674 s 对 12.321/13.384 s）——本地写普通文件时"尾部"只有 1~2 个 256 KiB 块（写入器用的就是 `bufio.NewWriterSize(w, 256*1024)`），几乎堵不住；差别在观察到的连接状态上：旧二进制那次跑到中途抓到 `[3 2:270=Sleep:0 268=Query:6 267=Sleep:6]`，即一个读完的分片仍计在打开连接里，新二进制的所有采样里再也没有"已完成但仍打开"的连接，空分片更是毫秒级就释放。回归由 `internal/export/exporter_shard_test.go` 钉住：它用进程内的假 `driver.Connector` 起一个**故意没有消费者**的 `shardPipe`，断言分片的连接已关闭而 `exportShard` 尚未返回（把改动前的 `defer` 顺序还原回去，该用例必然在 5 s 后失败并报 `exportShard still held its connection while waiting on the shard pipe`），另一用例断言空分片既不挂住也不留连接。

**导出的失败与中断语义（两条路径一致）：** 并行导出的流式写法意味着失败时输出文件已经存在并写了部分内容。任一分片报错或写文件报错（如 `ENOSPC`）时，导出整体报错、把已写出的输出文件删除并记一条 `incomplete output file removed` 警告，其余仍在队列上等待的分片由 `shard output consumer stopped` 立即解除阻塞（不会挂死），实测 32 MB tmpfs 目标下 4 分片导出在第 1 个分片写满后整体失败且不留任何文件。旧实现在分片失败时是"不产生输出文件"，在合并阶段失败时同样会留下半截输出，因此新语义在两种情况下都只留一种结果：**要么完整、要么没有**。单线程路径过去并不遵守这一条：`os.Create` 之后的任何失败只报错、不清理，交叉测试实测 20 万行表在末行被 `--no-char-del` 拒绝后留下 21,233,664 字节的半截 DEL 文件（文件名为导出名，看起来像成功产物）。现在 `exportSingle` 在创建输出文件之后注册同一份 `removeIncompleteOutput`，任何后续失败都删除该文件，并行路径改为复用同一个方法；删除只在文件确已创建之后注册，因此创建前的失败（查询报错、`--no-overwrite` 命中已有文件）不会误删用户原有文件，实测 `--threads 1/4` 两种失败都只留日志不留文件。注意：程序未安装信号处理器，`SIGINT`/`SIGTERM` 仍是直接终止进程，此时会留下一个不完整的输出文件（旧实现留下的是等量的分片临时文件），需要用户自行删除。

**并行导出的结果语义：**

- **完整性**：分片边界由精确十进制/整数运算生成（`BIGINT`/`DECIMAL` 超出 float64 精度时不丢行），相邻分片共享同一个边界字符串（前一分为 `<=`、后一分为 `>`），因此每行恰好落入一个分片，不重不漏。
- **行序**：最终文件按分片序号顺序拼接，**不保证**与 `--query` 的全局结果顺序一致；需要稳定全局顺序时请使用单线程导出。
- **一致性**：每个分片使用独立连接并各自执行 `START TRANSACTION WITH CONSISTENT SNAPSHOT`，各分片内部是一致的，但分片之间的快照建立时刻略有差异；导出期间源表仍有写入时，跨分片不构成同一个时间点快照。

**注意：** `--batch-size` / `--pipeline-buffer` 仅作用于单线程导出与导入。并行导出为分片内同步 scan→write，不受管道参数影响；分片到输出文件之间的字节队列深度是内部常量（64 MiB 总预算按分片数均分，见 7.2），不通过命令行暴露。

**并行导出的降级触发点：** 以下任一条件命中即回落单线程，并记一条 `warn` 级日志（消息文本即下表右列，`--quiet` 不抑制 warn）：

| 触发条件 | 日志消息 |
|----------|----------|
| `--query` 含 JOIN/派生表/集合运算等复杂特性 | `query has complex features, falling back to single-threaded export`（带 `feature=<识别到的特性>`）|
| 无法从语句提取表名 | `failed to extract table name, falling back to single-threaded export`（带 `error=`）|
| 检测不到可用分片键（无主键、无唯一键、`--shard-key` 列不存在）| `failed to detect shard key, falling back to single-threaded export`（带 `error=`）|
| 分片范围为空或只能切出一段（`MIN()==MAX()`、首列非数值如 UUID/字符串主键、区间无法精确运算）| `only 1 shard range, falling back to single-threaded export` |

前三项与第四项都在 `exporter.go:461-486`，级别 `warn`。`--format parquet` 走的是更早的独立分支，级别是 **`info`** 而非 warn，消息为 `parallel export is not supported for parquet format, using single-threaded export`（`exporter.go:456`，详见 5.3）。

选中键时会先写一条 `debug`：`using primary key for sharding` 或 `using unique key for sharding`，附 `columns=`，便于确认复合键实际用了哪几列。

**复合键与非数值键：** 主键/唯一键可以是复合的（`shard.go` 的 `ShardInfo.Columns` 为切片）。切分只作用于**首列**，其余列保持其全范围，因此每个元组仍被恰好覆盖一次；首列非数值（字符串、UUID、日期之外的类型）时无法计算数值边界，按上表第 4 行降级为单分片。数值边界一律用 `math/big.Rat` 精确运算，整数键保持整数文本形态以命中索引，`DECIMAL` 按 30 位精度输出。

### 7.3 import 流程

```
1. 解析命令行选项
2. 建立数据库连接
3. 根据 --mode 执行操作：
   - insert：直接 INSERT
   - insert_update：INSERT ... ON DUPLICATE KEY UPDATE
   - replace：DELETE + INSERT
   - create：执行 CREATE TABLE（表名重写为 --table）+ INSERT（仅 SQL 格式）
4. 根据 --format 选择输入格式：
   - del：解析定界 ASCII 文件
   - sql：拆出语句里的行值，按 `--batch-size` 重新组批后写入（文件的语句布局只影响体积，不影响下发的语句）
   - parquet：读取 Parquet 列式数据
5. 应用修饰选项（--skip-count, --row-count, --commit-count 等）
6. 输出配置参数快照（在建立连接之前，见 NewExporter/NewImporter）
7. 每隔 --progress-interval（默认 5 秒）记录一次进度（--quiet 时不记；运行短于一个间隔则一条也没有），全程写入 stderr 或 --messages 文件
8. 停止进度计时器，再输出统计信息（汇总恒为最后一条记录）
```

**并行导入流程（`--threads>1`）：**
```
1. 单个 reader goroutine 读取文件，按 --commit-count 与字节上限分批
2. 多个 worker goroutine 通过有界 channel 接收批次，各自独立连接/事务
3. 每个 worker 构建多行参数化 INSERT 并执行
4. 全部 worker 汇合后停止进度计时器（间隔由 --progress-interval 决定），再写出汇总（汇总恒为最后一条记录）
```

### 7.4 导入攒批字节上限（`batchcap.go`）

导入侧按 `--batch-size`（行数）攒批重建参数化 INSERT，攒批规模与文件的语句布局无关（导出默认为每行一条 INSERT，见 2.1.6），在此之上追加下列约束：

**1) 占位符上限：**

MySQL 预处理语句参数个数为 2 字节无符号整数，故 `rows × columns ≤ 65535`。超出时自动下调有效 `batch-size`（并打印一次告警），确保不触发协议层限制。

**2) 包上限（`max_allowed_packet`）：**

启动后首次攒批时读取服务端 `SHOW VARIABLES LIKE 'max_allowed_packet'`：
- 每批字节预算 = 该值的 **9/10**（下限 64 KiB）
- 读不到则按 **4 MiB** 保守值并告警
- 字节按值的实际宽度估算：字符串/字节按含转义后的宽度精确计算，标量按最宽文本形式界定

**3) 语句文本一次成型：** 每批下发的 `INSERT INTO ... VALUES (?,?,?) ...` 由一块按精确长度预分配的缓冲区一次写出。归属注释作为最前面的字节进去，不再用 `common.TagSQL` 在成品文本前面拼一遍（那会把整批内容白复制一次）；占位符组 `(?,?,?)` 按列数先拼好、每行只写一次，不再逐列逐字节写。1000 行 × 10 列的一批由 131 µs / 138,160 B / 20 次分配降到 21 µs / 24,792 B / 3 次分配（`BenchmarkBuildBatchInsertSQL`，打开归属注释），产出的文本与改写前逐字节相同，由 `TestBuildBatchInsertSQLTagged` 钉住（含 `insert_update` 的 `ON DUPLICATE KEY UPDATE` 分支）。

**4) 可选压低（`--statement-size`）：** 上面的包预算由服务端决定，用户此前无法主动把批次切小。`import --statement-size <bytes>` 与该预算取**较小值**，因此只能压低、不能抬高（抬高即让一批在服务端中途触发 error 1153）；默认 `0` 表示不额外压低，给在 64 KiB 下限以下的显式数字照原值执行。压低点在 `batchBudget()`（`applyStatementCap()`）上，单线程与并行导入、DEL/SQL/Parquet 三种输入共用，每次运行只解析一次。与导出侧同名选项的语义刻意不同：导出侧是合并语句的预算（默认 8 MiB，`0` 为用法错误），导入侧是可选上限（默认 `0`，`<0` 或 `>268435456` 为用法错误），校验函数分列为 `validateStatementSize` 与 `validateImportStatementSize`。实测（本地 MariaDB，`max_allowed_packet=134217728` → 预算 120,795,955 B；2048 行 × 约 200 B；`--batch-size 20480` 使行数不触顶；以目标库 `Com_insert` 差值计条数）：默认 **1** 条，`65536` → **7** 条（`--threads 4` 并行导入同样 7 条；SQL、Parquet 文件同样 1→7 条），`1024` → **342** 条，`268435456`（高于预算）→ 仍 **1** 条并记 `limiting_factor=max_allowed_packet`；各次落库行数与 `CRC32` 校验和全部一致（2048 / 4397778945034）。回归见 `internal/import/batchcap_test.go` 的 `TestApplyStatementCap` / `TestBatchBudgetAppliesStatementCapWithoutConnection`、`cmd/cli_matrix_test.go` 的 `TestCLIImportStatementSize`、`test/integration/live_mysql_test.go` 的 `TestLive_ImportStatementSizeCapsStatements`。

**刷新条件：** 行数或字节任一达到上限即提交一批。单行自身超过预算时独立成批（不丢弃、不拆分）。

**设计动机：** 此前只有导出侧的 `--statement-size` 有字节保护，导入侧只有行数保护。宽表/BLOB 大行在导入侧可能超出 `max_allowed_packet`（error 1153 "Packet too large"），该机制补上了这一缺口。

### 7.5 记录读取器（`record_reader.go`）

DEL 与 SQL 导入不再使用 `bufio.Scanner`（有 token 长度上限 10 MB），改为自定义 `recordReader`：

- **无 token 长度上限**：自定义 `SplitFunc`，`[start, end)` 游标设计
- **token 别名复用缓冲**：`token` 字段是 `buf` 的别名而非拷贝，减少内存分配
- **智能压缩**：仅当尾部空余 < 4 KiB 时才将未消费数据压缩到缓冲头部
- **几何倍率扩容**：缓冲不够时翻倍扩容，避免 O(n²) memmove

因此**单条巨型 INSERT / 单条巨型 DEL 记录都能读**，代价是该语句一次性解析。进程内存由 `GOMEMLIMIT` 环境变量控制。

**DEL 解析路径（`del_reader.go`）：** 记录分帧与字段解析是 DEL 导入的主要 CPU 开销（改写前占导入 CPU 的约 60%），按以下方式约束在"每个值一次拷贝"：

- **定界符预计算**：`--row-del`/`--char-del`/`--col-del`/`--escape` 在构造时各转一次 `[]byte`（`delSpec`，含首字节），`scanRows` 与 `parseLine` 逐字节位置比较时用 `bytes.Equal` 或直接比首字节，不再在每个位置把字符串定界符转成 `[]byte`
- **不额外物化整行**：`ReadHeader`/`ReadRow` 取 `recordReader.Bytes()` 的切片，不调用会 `string()` 拷贝的 `Text()`
- **字段值只在需要时拷贝**：未被转义的字段一次 `string(line[start:end])` 物化；含双写引号或反斜杠转义的字段用 reader 自带的 `scratch` 缓冲拼装，`fields`/`quoted`/`scratch` 三个切片在同一 reader 内复用
- **值的类型仍是 `string`**：改为 `[]byte` 不能减少分配——`driver.Value` 的装箱对非指针类型必然分配（`convTslice` 24 B > `convTstring` 16 B），还会改变预处理语句的字符集语义，因此只去掉多余的*拷贝次数*，不换类型

`--escape` 与 `--char-del` 相同（默认双写转义）或不同（反斜杠转义）两条分支都覆盖；`--no-char-del`、`--no-escape` 与多字节定界符（如 `||`、`\r\n`）走同一套 `delSpec` 比较。另消除一处库内隐患：空 `--col-del` 在 CLI 层已被 `validateDelimiters` 拒绝（`delimiter cannot be empty`），但直接构造 `DelReader` 时旧实现会在 `parseLine` 里对空前缀逐位置命中、`i += 0` 无限追加空字段（实测不返回），新实现用 `hasCol` 门控该分支，此时整行作为单个字段返回（`del_reader_test.go` 有 2 s 超时守卫用例盯住它）。语义等价性由 `del_reader_equiv_test.go` 的差分测试守住（见 11.1）。

实测收益（4 核 / 本地 MariaDB，`bench` 表 50 万行 × 11 列、DEL 文件 125 MB）：微基准 `ReadRow` 89.4→31.4 ms（2.8×）、分帧 31.6→9.3 ms（3.4×）、纯解析 45.8→18.5 ms（2.5×），`ReadRow` 每次操作分配 757,137→440,256 次、驻留字节 26.8→10.5 MB；端到端导入 `--threads 1` 6.81→5.70 s，客户端 CPU 时间 8.02→3.76 s（-53%），`--threads` 的扩展性因此被释放——t4 由 5.61 s 降到 3.64 s，t1→t4 加速比从 1.21× 提高到 1.87×（继续加线程时数据库写入侧成为主要限制）。同一份基准语料在改动前的代码（HEAD 工作树）与新代码上背靠背各测两轮，上表取其中一次。

**SQL 语句解析路径（`sql_reader.go`）：** SQL 导入同样把语句级留在 `[]byte` 上，去掉三处字符串物化与一处平方级扫描：

- **不物化整条语句**：`nextStatement` 逐行扫描时携带引号状态（`stmtState`），单行语句直接把记录缓冲里的切片交给上层，只有跨行语句才复制进 reader 自带的 `stmt` 复用缓冲；原先每条语句先经 `recordReader.Text()` 拷一份字符串、再经 `strings.ToUpper` 拷一份大写副本来判关键字
- **关键字比较在字节上做**：`hasPrefixFold`/`indexFold`/`equalFoldNull` 只折叠 ASCII 区间，多字节字符的字节宽度因此不变，派生出的偏移始终对应当前语句（`strings.ToUpper` 对某些码点会改变字节长度，偏移可能错位）
- **不再为每个元组建缓冲**：`extractRows` 定出元组在语句中的边界后，把窗口直接交给 `parseValueList`，只有真正留存的值才拷贝一次；原先每个元组都要经 `strings.Builder` 逐字节重打一遍
- **完成判定不再重复扫描**：引号状态跨行携带，一条语句只走一遍；原先每追加一行都从头重扫已积累的文本，代价随语句长度平方增长
- **值的类型仍是 `string`**：与 DEL 侧同理，装箱到 `driver.Value` 必然分配，只减少拷贝次数

实测（2 万行 × 8 列，同一份数据三种布局，`-benchmem`，HEAD 工作树与新树背靠背）：默认单行布局 90.0→42.3 ms（2.1×）、36.6→9.3 MB/op、728,986→340,009 次分配；`--extended-insert` 多行 69.1→32.8 ms（2.1×）、32.1→10.2 MB、629,507→320,368 次；跨物理行折行 1839.3→32.1 ms（57×）、40.8→10.4 MB、650,226→320,387 次。端到端（本地 MariaDB，`--batch-size 1000` 单线程，`-trimpath` 且注入归属注释的构建，`/usr/bin/time -v` 峰值 RSS）：20 万行的默认单行文件 2.61→2.29 s / 41.6→34.6 MB，同一数据的 `--extended-insert` 文件 2.85→2.44 s / 187.8→137.6 MB，148 万字节、10 条语句的折行文件 5.98→0.184 s（32×）/ 37.4→35.4 MB。12 次导入（新旧二进制 × 三种布局 × 注释开关）目标表 `CHECKSUM TABLE` 逐组相同；服务端 general log 复核折行文件仍只产生 10 条约 138,556 字节的 INSERT（每批 1000 行），且语句以 `/* imex:<commit> */` 开头——下发内容与改写前一致。

### 7.6 幂等性

| 导入模式 | 幂等性 | 说明 |
|----------|--------|------|
| `insert` | **不幂等** | 重复执行会追加重复数据 |
| `insert_update` | **幂等** | `INSERT ... ON DUPLICATE KEY UPDATE`，重复执行收敛 |
| `replace` | **幂等** | 先 DELETE 全表后 INSERT，重复执行收敛 |
| `create` | **不幂等** | 重复执行会因表已存在而报错 |

**导出覆盖语义：** 默认行为为覆盖输出文件（`os.Create` 截断）。`--no-overwrite` 开启后，若输出文件已存在则报错退出，不覆盖。

### 7.7 连接加固与瞬断重试

云环境的 LB/NAT 网关会静默回收长时间无往返的 TCP 连接，工具随后在读写时得到 `broken pipe` / `connection reset` / `invalid connection` / `server has gone away`。从三个层面加固，默认值内置、无需配置：

**1) 连接池老化（`common/pool.go`）**

| 参数 | 默认值 | 适用范围 |
|------|--------|----------|
| `SetConnMaxIdleTime` | 1 分钟 | 导入池；导出池显式设为 0 |
| `SetConnMaxLifetime` | 5 分钟 | 导入池；导出池显式设为 0 |
| `SetMaxOpenConns(1)` | — | 导出主连接、每个并行导入 worker |

导出池不做老化，是因为一致性快照用裸 `START TRANSACTION ... WITH CONSISTENT SNAPSHOT` 建立，连接在语句之间会归还空闲池；被当作空闲连接关闭会让快照被静默回滚，后续读取失去一致性。导入池没有跨语句事务依赖（事务由 `sql.Tx` 持有连接），可以安全老化。

**2) 事务空闲提交（`import/session.go`）**

导入写入端（单线程管线与每个并行 worker）在 **30s** 内没有收到新批次，且当前事务已有行时，主动 `COMMIT` 并开启下一个事务。`--commit-count` 到期时同样提交（按事务内累计行数判断，而非总行数取模，避免批大小不能整除时永不提交）；回放缓冲累计达到 **64 MiB** 字节预算时也提前提交，使内存与重放集合不受宽 BLOB/TEXT 行影响。

**3) 瞬态错误重试与回放（`common/dberr.go`、`common/retry.go`）**

- **判定为瞬态**：`driver.ErrBadConn`、`mysql.ErrInvalidConn`、`io.EOF` / `io.ErrUnexpectedEOF`、`ECONNRESET` / `ECONNABORTED` / `EPIPE`、网络超时、MySQL `1040/1053/1152/1927/2006/2013/2055`，以及 `invalid connection` / `broken pipe` / `connection reset` / `server has gone away` / `lost connection` 等文本兜底（穿透代理时不会得到类型化错误）。
- **策略**：默认最多 3 次尝试（首次 + 2 次重试），退避 1s → 2s，上限 4s，带 20% 向下抖动以避免多个 worker 同步重连；`ctx` 取消会中断退避并立即返回。
- **回放规则**：连接级错误意味着 MySQL 已回滚整个打开的事务。重连后先按顺序回放该事务内所有已执行但未提交的批次，再重试失败批次；仅重试单批会静默丢失同事务先前的行。回放缓冲只保留当前提交窗口——`--commit-count`（默认 1000 行）或 64 MiB 字节预算，任一触顶即提交。
- **不重试**：`COMMIT`（响应丢失时无法判断是否生效，重放可能重复写入）；`context.Canceled` / `DeadlineExceeded`；语法/约束类语义错误；`1213` 死锁与 `1205` 锁等待超时（`innodb_rollback_on_timeout=OFF` 下只回滚语句，盲目回放整个事务可能重复行）；`1153` 包过大（应缩小批次而非重试）。

---

## 八、非功能性需求

### 8.1 性能

| 指标 | 目标 |
|------|------|
| 流式处理 | 不在内存中缓存全量数据 |
| 大文件支持 | 支持 GB 级文件导入导出 |
| 事务控制 | `--commit-count` 控制事务提交频率 |
| 并行导出 | 基于主键/唯一键的多线程分片导出 |
| 并行导入 | 多线程并行导入 |
| Pipeline 模式 | goroutine → channel → goroutine 流水线架构 |

### 8.2 可靠性

| 项目 | 要求 |
|------|------|
| 日志文件 | `--messages` / `-m`：INFO/WARN/ERROR 全部记录逐行写入文件，终端不再打印 |
| 失败处理 | 明确标识失败原因和行号 |
| 连接重连 | 连接级瞬态错误（`ErrBadConn`、2006/2013、网络超时等）默认最多 3 次尝试，指数退避 1s→2s（上限 4s、20% 抖动）；重连后先回放当前事务的未提交批次再重试失败批次；`COMMIT` 与语义错误不重试（§7.7） |
| 连接池老化 | 导入池 `ConnMaxIdleTime=1m`、`ConnMaxLifetime=5m`；导出池两者显式设为 0（快照连接会归还空闲池，老化会静默回滚快照）；并行导入 worker `MaxOpenConns=1` |
| 事务空闲提交 | 导入写入端 30s 未收到新批次时提交当前部分事务，缩短未提交窗口与断线重放集合 |
| 并行导出分片释放 | 分片读完自己的全部行即结束一致性快照并关闭该分片连接，之后才把剩余字节交给按序写出的 goroutine；已读完的分片不持有空闲连接，也不延长快照寿命（§7.2），由 `exporter_shard_test.go` 在无数据库条件下钉住 |

### 8.3 可观测性

| 项目 | 要求 |
|------|------|
| 进度输出 | 导出、导入均每隔 `--progress-interval`（默认 5 秒）打印一次进度：`rate` 是**自开始以来的总速率**（已处理 ÷ 已用时长），`interval_rate` 是**上一个进度点到这一个进度点之间的区间速率**（两次快照之差），前者平滑、后者反映当前吞吐与抖动；另打印已用时长。`--quiet` / `IMEX_QUIET=1` 禁止定时进度日志（配置、结束统计、ERROR 仍输出；`--messages` 文件同样无进度行）。运行时长不足一个间隔时**一条进度也不打印**：首个 tick 要等满一个间隔才触发，停止计时器只等在写的那条记录落笔、不等到间隔走完，所以短任务直接以汇总结束。结束汇总（`completed`/`aborted`）是该次运行的**最后一条记录**：写汇总之前先停计时器，停止会等待正在写的那条进度落笔，因此汇总之后不会再出现进度行 |
| 日志顺序 | 版本横幅（version/build_time/git_commit/go_version/command）作为运行首行输出，其后为配置参数快照（`export/import config`，在 `NewExporter`/`NewImporter` 建连接之前打印，连接失败也能看到参数）；`version` 子命令例外——它只向标准输出打印版本四行，不写任何日志（`help` 子命令仍走横幅）|
| 错误输出 | 命令失败统一以 `level=ERROR msg="command failed"` 日志记录输出，与其它诊断格式一致，不再单独打印裸 `Error:` 行 |
| 导出行数 | 按实际写入文件的行数统计（含单线程与并行分片，统一取共享计数快照） |
| 导入行数 | `read` 按进入写入管道的文件行数统计（不含 `--skip-count` 跳过与被 `--row-count` 截断的行）；每批按服务端 `RowsAffected` 拆成 `inserted`/`updated`/`unchanged` 三个互斥计数（`insert_update` 下新行记 1、改动已存在的行记 2、写入相同值的行记 0，故 `updated=max(0, affected-rows)`、`unchanged=max(0, rows-affected)`、`inserted=rows-updated-unchanged`，三者之和恒等于该批行数；`insert`/`create`/`replace` 模式 affected 恒等于行数，全部记 `inserted`，其中 `replace` 是先清空目标表再插入，因此文件里的每一行都是新插入）；批次执行失败的行计入 `failed` |
| 统计摘要 | 完成后输出总条数、文件字节数、耗时、总速率；运行中止时输出中止汇总（含 `failed`） |
| 日志文件 | 同导出：`--messages` / `-m` 记录本次运行的全部日志 |
| 错误堆栈 | `--error-stack` 打印完整调用堆栈 |

### 8.4 安全

| 项目 | 要求 |
|------|------|
| 密码安全 | 不记录到日志，因而也不记录到 `--messages` 文件 |
| 权限最小化 | export 只需 SELECT；import 需要 INSERT/UPDATE/DELETE/CREATE |
| 传输安全 | 当前版本不提供 TLS 配置：`--ssl-*` 等选项已移除，连接只使用明文 TCP 或 Unix socket，`--session-var tls` 也会被拒绝。需要加密链路时请在工具之外终结（如本机 TLS 代理/隧道），或仅在可信网络内使用 |
| 密码加密 | AES-256-GCM 加密密码，避免在 ps 命令中泄露 |

### 8.5 部署

| 项目 | 要求 |
|------|------|
| 编译产物 | 单一静态二进制 |
| 跨平台 | 预编译产物覆盖 Linux amd64 / Windows amd64；macOS 与 Linux arm64 代码可编译，按 README 的单平台命令自行产出 |
| 容器化 | 提供 Dockerfile |

---

## 九、项目结构

```
imex/
├── main.go                    # 入口：执行命令，把用法错误映射为退出码 2，其余 1
├── main_test.go               # 日志消息风格静态守卫（go/ast 遍历全部 logger 调用点）
├── cmd/
│   ├── root.go                # 根命令（--error-stack、--echo-sql-values、--quiet、--log-format、--no-sql-tag、completion 禁用）
│   ├── export.go              # export 子命令（RunE：密码解析 → 校验 → 调度）
│   ├── export_flags.go        # export 子命令标志注册与默认值
│   ├── import.go              # import 子命令（RunE：同上）
│   ├── import_flags.go        # import 子命令标志注册与默认值
│   ├── validate.go            # 参数校验：数值边界、字符集冲突、格式适用性（SQL 专属/DEL 专属/parquet 专属选项）
│   ├── option_scope.go        # 把登记表里的格式适用范围追加进各 flag 的帮助文本
│   ├── cli_matrix_test.go     # 参数组合矩阵与退出码契约测试
│   ├── validate_fuzz_test.go  # 校验函数的 fuzz 测试
│   └── encrypt.go             # encrypt 子命令（密码加密）
├── internal/
│   ├── config/
│   │   ├── config.go          # 配置定义与默认值常量
│   │   └── formatscope.go     # 格式适用范围登记表（拒绝文案 / 帮助标记 / 配置回显字段三处共用）
│   ├── common/
│   │   ├── util.go            # DSN 构建（超时/字符集/会话变量注入）与通用工具
│   │   ├── usage.go           # 用法错误类型（Usagef / IsUsageError），退出码 2 的依据
│   │   ├── stats.go           # 吞吐统计、速率计算与定时进度计时器（停止时排空在写的记录）
│   │   ├── colkind.go         # 列类型分类（修饰选项/Parquet 二进制判定共用）
│   │   ├── sessionvars.go     # 会话变量解析与 DSN 注入（--session-var）
│   │   ├── sqlpreview.go      # 失败 SQL 脱敏回显（--echo-sql-values）
│   │   ├── sqltag.go          # SQL 归因注释（/* imex:<commit> */）
│   │   ├── dberr.go           # 瞬态错误判定（连接级错误码/网络错误/文本兜底）
│   │   ├── retry.go           # 重试策略（次数、指数退避、抖动、ctx 取消）
│   │   ├── pool.go            # 连接池老化（ConnMaxIdleTime / ConnMaxLifetime）
│   │   ├── log.go             # 日志模块（java/text 双布局、版本横幅、一条记录一行）
│   │   ├── password.go        # 密码加密/解密（AES-256-GCM）
│   │   └── detect.go          # 服务器类型检测
│   ├── export/
│   │   ├── exporter.go        # 导出调度器（Pipeline 模式、并行分片、writeShardStream 流式写出、分片读完即释放连接）
│   │   ├── configlog.go       # `export config` 回显（按登记表过滤字段、`effectiveHexBlob()` 判定生效值）
│   │   ├── del_writer.go      # DEL 格式写入器
│   │   ├── sql_writer.go      # SQL 格式写入器（默认每行一条 INSERT、`--extended-insert` 才按字节合并/转义/LOCK 抑制）
│   │   ├── parquet_writer.go  # Parquet 格式写入器（列式存储）
│   │   ├── shard.go           # 分片逻辑（主键/唯一键检测）
│   │   ├── shard_stream.go    # 分片到输出文件的字节队列（并行导出流式写出，免临时文件）
│   │   └── sql_parser.go      # SQL 解析器（JOIN 检测、hint 保留）
│   └── import/
│       ├── importer.go        # 导入调度器（Pipeline 模式、列映射、并行导入）
│       ├── configlog.go       # `import config` 回显（按登记表过滤 DEL 布局字段）
│       ├── batchcap.go        # 导入攒批字节上限（max_allowed_packet、--statement-size 压低、65535 占位符）
│       ├── session.go         # 写入会话（事务、重连回放、空闲提交、commit-count）
│       ├── record_reader.go   # 无上限记录读取器（替代 bufio.Scanner）
│       ├── del_reader.go      # DEL 格式读取器
│       ├── sql_reader.go      # SQL 格式读取器（语句级全程 []byte，元组按窗口解析）
│       └── parquet_reader.go  # Parquet 格式读取器
├── test/
│   └── integration/
│       ├── roundtrip_test.go  # 离线往返（写入器→读取器，无需数据库）
│       ├── live_mysql_test.go # 真实 MySQL/OceanBase 库级端到端，需 IMEX_TEST_DSN，未设置则跳过
│       ├── option_matrix_test.go # 真实 CLI 交叉选项矩阵（argv 驱动编译出的二进制），同上门控
│       ├── lock_contention_test.go # 导入并发 × 重复主键的锁冲突与顺序对照，同上门控
│       └── export_cli_test.go      # 导出 CLI 侧回归：schema 限定查询、--create 默认、--query-file 互斥，同上门控
├── docs/
│   ├── requirements.md        # 本文件
│   └── cli-oracle.txt         # 已发布二进制 --help 契约快照（回归基线）
├── go.mod                     # go 1.26.10
├── go.sum
├── Makefile
├── Dockerfile
└── README.md
```

另有 29 个包内 `*_test.go`（`internal/common` 7、`internal/export` 9、`internal/import` 11、`internal/config` 2），加上 `cmd` 4、仓库根 1、集成 5，全仓库共 39 个测试文件，清单见十一节。

---

## 十、使用示例汇总

### 10.1 导出场景

```bash
# 场景 1：导出整张表到 CSV 风格文件
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 场景 2：导出符合条件的数据
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/active_users.del \
  -q "SELECT id, name, email FROM users WHERE status = 'active'"

# 场景 3：导出为可直接导入的 SQL 文件
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.sql -f sql \
  -q "SELECT * FROM users"

# 场景 4：导出为 Parquet 格式
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.parquet -f parquet \
  -q "SELECT * FROM users"

# 场景 5：自定义分隔符（制表符分隔，无字符串定界符）
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.tsv \
  --col-del "\t" --no-char-del \
  -q "SELECT * FROM users"

# 场景 6：导出关联查询结果
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/user_orders.del \
  -q "SELECT u.name, o.order_id, o.amount FROM users u JOIN orders o ON u.id = o.user_id"

# 场景 7：仅导出表结构
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users_schema.sql -f sql \
  --no-data \
  -q "SELECT * FROM users LIMIT 0"

# 场景 8：自定义时间戳格式 + 日志文件
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/logs.del \
  --timestamp-fmt "yyyy/MM/dd HH:mm:ss" \
  -m /var/log/imex_export.log \
  -q "SELECT * FROM logs"

# 场景 9：并行导出（4线程）
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "SELECT * FROM users"

# 场景 10：带错误堆栈的导出
imex export --error-stack -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 场景 11：导出前设置会话变量（每条连接生效，含并行分片）
imex export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  --session-var "sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE" \
  --session-var foreign_key_checks=0 \
  -q "SELECT * FROM users"
```

### 10.2 导入场景

```bash
# 场景 1：追加数据到现有表
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users

# 场景 2：从 Parquet 文件导入
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.parquet -f parquet \
  -t users

# 场景 3：同步数据（插入或更新）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode insert_update \
  -t users

# 场景 4：完全替换表数据
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode replace \
  -t users

# 场景 5：从 SQL 文件创建表并导入
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.sql -f sql \
  --mode create

# 场景 6：导入指定列（位置映射）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "id,name,email"

# 场景 7：导入指定列（名称映射）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "user_id=id,user_name=name,user_email=email"

# 场景 8：跳过列名行，限制导入行数
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --skip-count 1 --row-count 1000 \
  -t users

# 场景 9：自定义分隔符 + 日志文件
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --char-del '!' --col-del '@' \
  -m /var/log/imex_import.log \
  -t users

# 场景 10：并行导入（4线程）
imex import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --threads 4 \
  -t users
```

---

## 十一、测试需求

| 场景 | 说明 |
|------|------|
| DEL 导出导入 | 导出为 DEL → 导入 → 数据校验 |
| SQL 导出导入 | 导出为 SQL → 导入 → 数据校验 |
| Parquet 导出导入 | 导出为 Parquet → 导入 → 数据校验（NULL、空字符串区分） |
| 自定义分隔符 | 使用 `--char-del` / `--col-del` 导出 → 导入 → 校验 |
| INSERT 模式 | 追加导入 → 验证数据累加 |
| INSERT_UPDATE 模式 | 有主键冲突时更新 → 验证数据正确 |
| REPLACE 模式 | 替换导入 → 验证旧数据被清除 |
| CREATE 模式 | 从 SQL 创建表 → 验证表结构和数据（表名重写） |
| 列映射导入 | 位置映射和名称映射 → 验证数据正确导入到目标列 |
| 并行导出 | 多线程导出 → 验证数据完整性（行不重不漏）、分片序号拼接、分片合并；不校验全局行序 |
| 并行导入 | 多线程导入 → 验证数据完整性、事务一致性 |
| JOIN 查询降级 | JOIN 查询 + `--threads>1` → 验证自动降级为单线程 |
| Parquet 并行降级 | Parquet + `--threads>1` → 验证自动降级为单线程 |
| 大文件 | GB 级文件导出导入 → 验证性能和稳定性 |
| 日志文件 | 验证 `--messages` / `-m` 下 stdout 与 stderr 均为空、文件即命令行日志（与不带该选项的 stderr 去掉时间戳/PID 后逐行相同）、首行为版本横幅、`--log-level` 与 `--log-format` 生效、不再出现 `SQL3xxxN` |
| 错误堆栈 | 验证 `--error-stack` 和 `IMEX_ERROR_STACK=1` 输出完整堆栈 |
| OceanBase | 在 OceanBase 上执行上述场景 |
| SQL 语句布局 | 默认导出为 SQL → 验证每行一条 INSERT；`--extended-insert` → 验证语句数严格减少且合并后的语句不超 `--statement-size`；`--single-row-insert` → 验证它仍是默认布局；三种布局各自导入 → 数据校验一致，且服务端收到的语句数相同 |
| 导入字节上限 | 宽表/BLOB 大行导入 → 验证不触发 error 1153（`Packet too large`） |
| 巨型 INSERT 导入 | 导入含超长 INSERT 语句的 SQL 文件 → 验证 `recordReader` 无上限读取 |
| 导出/导入转义往返 | 含 `\n`/`\r`/`\t`/`'`/`\`/`\0`/`\x1a` 的数据 → 导出 SQL → 导入 → 数据一致 |
| 占位符上限 | 宽表（columns > 100）导入 → 验证 `batch-size` 自动下调且不报错 |
| 告警记录 | 触发 `--no-hex-blob`/超长语句/`max_allowed_packet` 读取失败/无表头首行丢弃/`insert_update` 无唯一键 → 验证对应 WARN 记录写进 `--messages` 文件（未给该选项时在 stderr） |
| `--no-escape` 冲突数据 | `--no-escape` 导出含定界符的数据 → 验证写入前报错且不产生半行残留 |
| SQL 格式多表查询 | `--format sql` + JOIN/派生表/CTE → 验证解析阶段报错并提示改用 del/parquet |
| 位置映射列数 | `--columns` 条目数与文件列数不符 → 验证写入前报错并提示名称映射 |

### 11.1 测试资产清单

上表按"场景"组织，这里按"文件"给出对应的落地位置与规模（当前共 39 个 `_test.go`、201 个 `Test*`（另有 1 个 `TestMain` 负责一次性构建被 CLI 测试调用的二进制）、6 个 `Fuzz*`、12 个 `Benchmark*`）。

| 文件 | 用例数 | 覆盖点 | 运行方式 |
|------|--------|--------|----------|
| `cmd/cli_matrix_test.go` | 9 | 参数组合矩阵与退出码契约：DEL/SQL/parquet 专属选项互斥、`--log-format` 标志与环境变量、必填项缺失、`encrypt` 位置参数个数（越界为用法错误 2、密钥缺失仍为运行时 1）、`--progress-interval` 的默认值与取值边界（0 与负值均为用法错误 2）、SQL 语句布局标志（缺省 `ExtendedInsert=false`、`--extended-insert` 落到 config、`--single-row-insert` 与 `--extended-insert` 同时给出为用法错误 2、`--extended-insert` 配 `--format del` 被拒）、同名 `--statement-size` 在两侧相反的合法性（导入接受 `0` 并原样落到 config、导出给 `0` 仍是用法错误 2、两侧共享 268435456 上界）| `make test` |
| `cmd/validate_fuzz_test.go` | 5 fuzz | 边界校验函数（batch-size、pipeline-buffer、statement-size 的导出与导入两版、threads）对畸形输入不 panic，且两侧的 `0` 判定相反这一事实被分别钉住 | `make test` |
| `cmd/version_log_test.go` | 2 | `version` 子命令不写日志记录（stdout/stderr 捕获后断言），`run` 横幅仍照常记录 | `make test` |
| `cmd/option_scope_test.go` | 3 | 格式适用范围三处同源的命令行侧：登记表里每个 flag 都存在于对应子命令且其帮助文本以范围标记结尾、渲染后的 `--help` 里确实看得到该标记；九个 DEL 布局选项不得漏登记（漏一个就会被静默忽略）；`annotateOptionScopes` 对同一命令重复调用不会追加第二个标记（命令树是包级的，init 与测试会走同一条注册路径）| `make test` |
| `internal/common/*_test.go`（7 个文件）| 29 | 瞬态错误判定、日志单行与切布局保持级别、日志文件 sink（挂载后接管 stderr、关闭后归还、并发写入不交叉、替换与失败路径）、进度计时器 stop 排空在飞的记录且二次 stop 不死锁、间隔未到不产出任何进度记录且 stop 不空等一个间隔、重试退避与取消、会话变量解析与 DSN 注入、失败 SQL 脱敏回显、DSN 构建 | `make test` |
| `internal/config/config_test.go` | 2 | `--progress-interval` 的解析结果：导出与导入两侧默认恒为 5 秒，显式取值原样生效，零值与负值回落默认值（`time.NewTicker` 对非正间隔会 panic，库调用方不许把它传进来）| `make test` |
| `internal/config/formatscope_test.go` | 4 | 格式适用范围登记表本身：`AppliesTo` 的格式×选项矩阵（含未登记选项恒等于"与格式无关"、导入侧 `statement-size` 刻意不登记）；`Violation` 的文案与首条命中顺序必须与既有的用法错误逐字相同（把拒绝逻辑合并到一个函数不能改变用户读到的内容）；登记表自洽性（无重复、无"列出全部格式"的伪范围、每个未覆盖格式都拿得到点名该选项的文案）；导出与导入两侧的 DEL 组必须是同一批九个选项，且含此前回显漏掉的 `date-fmt`/`no-escape`/`no-char-del` | `make test` |
| `internal/export/*_test.go`（9 个文件）| 70 | 配置回显按 `--format` 过滤（`configlog_test.go`：三种格式的字段集合、DEL 布局 9 项只在 `-f del` 出现、SQL 布局 4 项只在 `-f sql` 出现、`hex_blob` 三种格式都不越界出现在 parquet，且参数恒为成对 key/value；此前漏打的 `date_fmt`/`no_escape`/`no_char_del` 必须出现；`hex_blob` 取生效值——`gbk` 自动开、`--no-hex-blob` 强制关，`effectiveHexBlob()` 六种组合逐一钉住）、DEL/SQL/Parquet 写入器转义与攒批、分片边界精确运算与 hint 保留、hex-blob 判定、并行导出流式写出的 header/LOCK 配对与分块顺序（`exporter_stream_test.go`）、分片字节队列的语义（`shard_stream.go`：任意写入切分后重组逐字节一致、块不超上限、队列满时背压、消费端停止后生产端被 `shard output consumer stopped` 解除阻塞、ctx 取消同样解除、Close 幂等且刷出尾部、队列深度按分片数均分且内存有界）、并行路径对被忽略选项的判定（`shard_stream_test.go` 的 `TestParallelExportPipelineOptionNotice`：默认值不报警、任一值被改动即报警，含 `--pipeline-buffer 0` 这个合法值）；分片资源释放顺序（`exporter_shard_test.go`：进程内假 `driver.Connector` 记录连接关闭，一个用例起一个**故意没有消费者**的 `shardPipe`，断言分片的连接与快照在读完时就结束、而 `exportShard` 仍阻塞在交尾字节上——改动前的 `defer` 顺序下该用例必然超时失败，另一个用例断言空分片既不挂住写出的队列也不留连接）、`--query` 的表名解析（`sql_parser_test.go`：`ExtractTableName`/`SimpleTableName` 对 `` `db`.`tbl` `` 只返回**裸表名**——把 schema 二次拼接的缺陷钉住；新抽出的 `ExtractTableRef` 单独把 schema 与表名拆开，仅供 `SHOW CREATE TABLE` 使用；非 SELECT 语句一律拒绝）| `make test` |
| `internal/import/*_test.go`（11 个文件）| 44 + 1 fuzz + 12 bench | 配置回显按 `--format` 过滤（`configlog_test.go`：DEL 布局 9 项只在 `-f del` 出现，行流控制 `skip_count`/`row_count`/`commit_count` 与 `statement_size` 三种格式都必须在场——后者约束本命令生成的语句而非输入文件）、攒批字节上限与 65535 占位符（`TestApplyStatementCap` 钉住导入侧 `--statement-size` 只压低不抬高、等于预算与低于 64 KiB 下限时各自的取值，`TestBatchBudgetAppliesStatementCapWithoutConnection` 确认没有连接（`imp.db == nil`，走 4 MiB 保守值）时也在同一处压低预算）、DEL/SQL 读取器回读、无上限记录读取、会话重连回放与空闲提交、位置映射列数校验、`inserted`/`updated`/`unchanged` 拆分（含 affected 读不到与超限钳制）；DEL 读取器重写以差分方式守住语义——保留改动前的 `parseLine`/`scanRows` 为参考实现，用 28 个手写用例 + 4000 行随机语料 × 6 套定界符配置逐字段比对，配 `FuzzDelReaderParse` 持续模糊，另有空 `--col-del` 的超时守卫用例（`del_reader_equiv_test.go`、`del_reader_test.go`）；`del_reader_bench_test.go` 用 2 万行、贴近导出器真实产物（全字段带引号、空串与 NULL 可区分、二进制列裸 `0x`、引号内含 `||`/`;`）的确定性语料覆盖整行读取、纯分帧、纯解析三段；SQL 语句级重写另有 `sql_reader_bench_test.go`（同一份 2 万行数据按默认单行、`--extended-insert` 多行、跨物理行折行三种布局各一段，每段都断言行数与列数，防止用跳过工作来换速度），`sql_reader_test.go` 补多行语句重组（值内真换行、值内 `;`、双写引号、`;` 单独占一行）与小写关键字 + 非 ASCII 值两个用例，`importer_test.go` 的 `TestBuildBatchInsertSQLTagged` 钉住攒批语句文本与 `common.TagSQL` 逐字节相等（含 `insert_update` 的 `ON DUPLICATE KEY UPDATE` 后缀），`BenchmarkBuildBatchInsertSQL` 在打开归属注释的前提下测每条批次的语句文本构造 | `make test`（bench 用 `go test -run '^$' -bench . ./internal/import`，fuzz 用 `-fuzz FuzzDelReaderParse`）|
| `main_test.go` | 1 | go/ast 静态守卫：全部 logger 调用消息首字母小写且单行（防日志词汇回归）| `make test` |
| `test/integration/roundtrip_test.go` | 10 | 离线往返（写入器→读取器）：转义文本、每条语句一个物理行、`--statement-size` 攒批、`0x` 二进制字面量、MySQL 文本值与 HEX 一致、DEL 引号/反斜杠两种模式、看似十六进制的文本不被误解码、单行 INSERT | `make test` |
| `test/integration/live_mysql_test.go` | 8 | 真实 MySQL 端到端：三种格式往返、并行导出与并行导入、四种导入模式、列映射、导入行数拆分对真实服务端 `RowsAffected` 的断言；`--messages` 日志文件随用例断言（记录齐全、无 `SQL3xxxN`、每行都是日志）；进度记录不得晚于结束汇总（单线程与并行 × 导出与导入，计时器压到 1 ms；末段走真实二进制，断言 `--progress-interval 1ms` 生效、配置回显带 `progress_interval`、不再有 `started` 记录）；并行导出对 `--batch-size` 的忽略告警三段式（改动过它在并行分片下必须出现、两者保持默认值必须静默、单线程下用了就不得报警）；导入 `--statement-size` 的服务端实测（同一份 2048 行文件、同一 `--batch-size`，只用目标库 `Com_insert` 差值断言默认 1 条语句、`--statement-size 65536` 为 7 条，并复核两表落库行数与 `CRC32` 校验和一致、日志里 `limiting_factor` 由 `max_allowed_packet` 变为 `statement-size`） | `IMEX_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/db' make test-live`，未设置该变量时自动跳过 |
| `test/integration/lock_contention_test.go` | 3 | 导入并发与重复主键的锁冲突（真实二进制 + 真实库）。fixture 让每个主键恰好出现两次，且第二次在**相邻批次里反序**（`--batch-size` 与块大小对齐，块序号取模决定升/降序），DEL 与 SQL 两份文件由同一张表按 `ORDER BY seq` 导出。① 单线程：重复键只能是 `1062`/`Duplicate entry`，断言输出里**不出现** `1213`/`Deadlock`（单会话不可能自锁），`--mode insert_update` 则全部键落地；② `--threads 4` + 反序重叠键（sql、del 各一次）：不断言"必须死锁"——是否成环取决于服务端调度，实测同一 fixture 下 del 抓到 1 次、sql 那次没成环——断言的是任何退出码下都不落重复行（`COUNT(*) == COUNT(DISTINCT id)`）、落库行数不超过文件里的键数、失败时只能报 `1213`/`1205` 这类服务端锁错误（`1213/1205` 不在可重试集合内这条由 `internal/common/dberr_test.go` 钉住，因此不会先重放再报成功），服务端死锁计数增量作为观测值打印（变量名 MySQL 为 `Innodb_row_lock_deadlocks`、MariaDB 为 `Innodb_deadlocks`，两者都取不到时降级为只看报错文本）；③ 对照组：同一键集、同样 4 线程、批次一律升序，必须零死锁且全部键落地——把"按键有序"这条规避手段变成被测性质 | 同上 |
| `test/integration/export_cli_test.go` | 4 | 导出 CLI 侧的三项改动固化为真实二进制 + 真实库回归。① `TestLive_ExportSchemaQualifiedQuery`：`--query` 用 `` `db`.`tbl` `` 这种带 schema 的表名，默认（数据）与 `--create` 两种都必须成功、输出不得出现 `1146`/`doesn't exist`，产物只按裸表名写 `INSERT`（断言文件不含 `` `db` `` 且含 `INSERT INTO \`tbl\``），再导入一张 `LIKE` 出来的新表逐行核对——把"schema 二次拼接"这条缺陷钉死；② `TestLive_ExportCreateDefault`：默认 SQL 导出无 `CREATE TABLE`、`--create` 才有、`--no-create` 已是未知选项（退出 2 且文案点名该 flag）、`--create` 配 `-f del` 报范围用法错误退出 2；③ `TestLive_ExportQueryFile`：`--query-file` 读文件（含首尾空白与结尾分号的语料）导出后导入核对，配置回显打印 `query_file=<路径>`，并钉四条用法错误各退出 2——`-q` 与 `--query-file` 互斥、两者都不给、空/仅分号文件、文件不存在；④ `TestLive_ExportQueryFileParquet`：`--query-file` 与格式无关，parquet 也能用它导出且产物非空。fixture 复用 `makePair` 的标准多列表，`snapshotTable`/`assertSameRows` 可直接比对 | 同上 |
| `test/integration/option_matrix_test.go` | 12 | 编译出真实二进制后用 argv 跑交叉矩阵：DEL 布局修饰符的导出/导入成对性、`--no-col-names` 无表头配对歧义、逐字写入模式的拒绝与残留文件清理、SQL 语句布局（默认 10 行 10 条语句、`--single-row-insert` 仍是默认、`--extended-insert` 严格减少语句数、`--extended-insert --statement-size 512` 的合并语句不超上限+4 字节、单给 `--statement-size` 不改布局，每种布局都逐行往返校验）、并行导出：DEL 与默认 SQL 逐字节等于串行，`--extended-insert` 时只多分片边界的语句头（按头数与字节差精确断言）、日期时间格式的对称性与精度截断、`--create`/`--no-data` × `--mode create`（SQL 导出默认仅数据，四个子用例分别覆盖默认仅数据可直接导入、`--create` 带回建表且保留数据、`--create --no-data` 仅结构建表、默认仅数据配 `--mode create` 必须明确失败；用例名由 `TestOptionMatrixNoDataNoCreate` 改为 `TestOptionMatrixNoDataCreate`）、`--skip-count`/`--row-count` × 线程/批量/提交、`--messages`/`-m` 在三格式与并行路径下等于命令行日志且终端静默（含 `-m` 短选项、`--log-format text`、`--log-level error`、失败记录）、`--no-overwrite` 哨兵文件存活、`--default-charset` 与二进制列 0x 自动切换 | 同上（`TestMain` 先 `go build` 一次，全部用例复用该二进制） |

`make test` 与 `make test-live` 固定 `CGO_ENABLED=1 go test -race`，这是 `-race` 的要求，与发布构建的 `CGO_ENABLED=0` 无关。

### 11.2 当前环境尚未自动化覆盖的场景

| 场景 | 现状 |
|------|------|
| OceanBase 端到端 | 沙箱内无 OceanBase 实例。仅覆盖到 SQL 层的 OceanBase 分支单测（如 OceanBase 下不写表锁的合并用例），完整链路未验证 |
| GB 级大文件 | 由 12 个 benchmark 覆盖单点（记录读取器、DEL 整行读取/分帧/解析、SQL 三种语句布局的读取、攒批字节估算、批次语句文本构造），属微基准；8.1 的大文件与吞吐是目标值，没有端到端自动化断言，需在真实环境按压测计划验证 |
| 长时间稳定性 / 断连恢复 | `internal/import/session_test.go` 用模拟重连覆盖回放逻辑，真实网络中断（LB/NAT 回收）未做长时间实验；导出侧只把"已完成读取的分片不再持有连接与快照"这一空等窗口缩到零（`exporter_shard_test.go` 用进程内假连接器钉住释放顺序），真正的 LB/NAT 回收仍需在有云的环境上实测，本仓库只覆盖到"空闲窗口归零"这一层（§8.2 并行导出分片释放）|

---

## 十二、交付物

| 交付物 | 说明 |
|--------|------|
| 源代码 | 完整 Go 项目源码（含 39 个 `_test.go`、`docs/requirements.md`、`docs/cli-oracle.txt`），发布形态为 `imex-<version>-<commit>[-dirty]-src.tar.gz`，排除 `.git/`、`bin/`、`dist/`、`vendor/` |
| 二进制文件 | Linux amd64、Windows amd64 两个平台；均 `CGO_ENABLED=0` 静态链接、`-s -w` 去符号，由 `make build-all` 产出，版本信息经 ldflags 注入。Linux arm64 与 macOS 不在发布矩阵内，按 README 的单平台命令自行编译 |
| 校验清单 | 每个发布批次附 `*-bin-SHA256SUMS.txt`，逐产物一行 SHA256 |
| Dockerfile | 容器构建（`make docker`）|
| README.md | 使用说明；本文件为需求与实现说明 |

自 2026-10-23 起**不提供 macOS 预编译产物**（见变更记录 33），同日进一步**只保留 amd64**：Linux arm64 也不再随包发布（见变更记录 37）。工具是纯 Go 无 CGO，这些平台拉源码后自行编译即可——macOS 执行 `make build`，Linux arm64 用 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o imex .`。

---

## 十三、里程碑

| 阶段 | 内容 | 优先级 |
|------|------|--------|
| P0 | 基础框架 + DEL 格式导出导入 | 最高 |
| P1 | SQL 格式导出导入 | 最高 |
| P2 | Parquet 格式导出导入 | 最高 |
| P3 | 修饰选项支持（分隔符、字符集、时间格式等） | 高 |
| P4 | insert_update / replace / create 模式 | 高 |
| P5 | 列映射导入 | 高 |
| P6 | 并行导出（分片、JOIN 检测） | 高 |
| P7 | 并行导入 | 高 |
| P8 | 日志文件 + 错误处理（`--error-stack`） | 中 |
| P9 | OceanBase 兼容 + Docker | 低 |

---

## 十四、变更记录

> 本节的沙箱绝对路径与测试库凭据已脱敏：`<workspace>/` 指本地工作区根目录，`<tmp>/` 指临时目录，`<user>:<password>` 指本地测试库账号；历史记录原文保留在提交历史中。

### 2026-10-22 本轮改动

| # | 改动项 | 说明 |
|---|--------|------|
| 1 | `--statement-size` 默认值调整 | 从 16 MiB（16777216 字节）降为 **8 MiB（8388608 字节）**，更贴近典型 `max_allowed_packet` 设置，降低导入侧重建语句时的内存峰值 |
| 2 | 导入攒批新增字节上限 | 新增 `batchcap.go`：导入侧按 `max_allowed_packet` 的 9/10 设定每批字节预算（下限 64 KiB），同时按 `rows × columns ≤ 65535` 下调有效 `batch-size`。补上了宽表/BLOB 大行在导入侧可能触发 error 1153 的缺口 |
| 3 | 补齐导出/导入转义与换行往返测试 | SQL 格式转义规则完整覆盖 `\n`/`\r`/`\t`/`\\`/`\0`/`\x1a`/`''`，确保每条 INSERT 恰好占一个物理行；导入侧 `recordReader` 替代 `bufio.Scanner`，无 token 长度上限 |
| 4 | 新增 `--single-row-insert` | SQL 格式专属选项，开启后每行一条 INSERT 语句（等价 mysqldump `--skip-extended-insert`），`--statement-size` 退化为告警阈值。**注：第 47 行把这一布局改为 `--format sql` 的默认值，该选项降级为兼容别名，合并改由 `--extended-insert` 显式开启。** |
| 5 | 并行导出 header/footer 统一 | `mergeShardFiles` 由 SQLWriter 统一写出 SET NAMES、CREATE TABLE、LOCK/UNLOCK TABLES；各分片片段设置 `SetSkipTableLocks(true)` 且 `writeCreate=false`，不再各自写 LOCK/UNLOCK |
| 6 | DEL 导入解码 `0x` 二进制字面量 | 此前 DEL 导出在 `--hex-blob`（或非 utf8mb4 自动开启）下写出 `0x…`，但 DEL 读取端无列类型信息，按原文返回字符串，往返不还原字节。现由 `columnInfoFor` 在同一次 `information_schema` 查询中同时取回列类型与二进制标记，`DelReader.SetBinaryColumns` 据此解码；判定要求"无引号 + 目标列二进制 + 合法偶数长度十六进制"，因此文本列中的 `0xdead` 保持原样 |
| 7 | 补齐连接加固三件套 | 新增 `common/dberr.go`（瞬态错误判定）、`common/retry.go`（3 次尝试、1s→2s 退避、上限 4s、20% 抖动）、`common/pool.go`（导入池 1m 空闲 / 5m 生存；导出池不做老化以免快照被回收）、`import/session.go`（事务、重连回放、30s 空闲提交）。修复单线程导入 `rowCount % commitCount` 在批大小不能整除时永不提交的缺陷，改为按事务内累计行数判断；重连后回放整个未提交事务而非仅失败批次，避免丢行；`COMMIT` 永不重试 |
| 8 | 分片边界改为精确运算 | `shard.go` 不再经过 float64：用 `math/big.Rat` 计算边界，整数键保持整数文本（继续命中索引），小数键按 30 位精度输出；相邻分片共享同一界值字符串，`BIGINT`/`DECIMAL` 超过 2^53 时不再静默丢行 |
| 9 | DEL 多字节分隔符/转义修复 | 写端按完整分隔符前缀匹配转义；自定义转义符为多字节时整体双写；读端消费完整转义序列（分隔符/转义符/单字节），多字节 `--char-del` 配合 `--escape` 可正确往返 |
| 10 | 回放缓冲增加字节上限 | `import/session.go` 除 `--commit-count` 外再按 64 MiB 字节预算提前提交，宽 BLOB/TEXT 行不会把重放缓冲撑大，同时也缩短断线重放窗口 |
| 11 | 真实 MySQL 集成测试 | 新增 `test/integration/live_mysql_test.go`（`IMEX_TEST_DSN` 门控，未设置时跳过）：DEL/SQL/Parquet 往返、insert/insert_update/replace/create、列映射、并行导出/导入、消息文件；`make test-live` 一键执行 |
| 12 | 文档收敛 | 明确并行导出按分片序号拼接、不保证全局行序，各分片快照时刻独立；修正 TLS 说明（当前版本不提供 TLS 配置，需在工具外终结加密链路），并同步 Dockerfile 注释 |
| 13 | 消息文件补齐开始/告警记录 | 导出写 `SQL3104N`（含表名）、导入写 `SQL3109N`（含目标表名）；`MessageWriter` 新增 `WriteBegin`/`WriteWarning`，运行时告警（`--no-hex-blob` 与非 utf8mb4 组合、单行 INSERT 超 `--statement-size`、读不到 `max_allowed_packet`、`--batch-size` 被占位符上限下调）逐条写 `SQL3107N`，结束时在 `SQL3110N` 前追加一条告警汇总 |
| 14 | `--no-escape` 写出前校验 | 此前 `--no-escape` 直接把原值写出，数据含定界符时产出无法回读的歧义文件（文档声明"报错"未实现）。现于写入前校验：使用字符定界符时值中不得出现该定界符，`--no-char-del`/空字符定界符时不得出现列分隔符与行分隔符；`--hex-blob` 二进制列为十六进制字面量不受影响。命中时报 `--no-escape: column N value contains the ... delimiter` 并中止，校验在任何字节写出之前完成，不留半行 |
| 15 | `--skip-count` 文档对齐 | 实现一直是对跳过**数据行**计数（列名行由解析器单独读取，`--no-col-names` 时首行才计入）。修正需求文档、README 的标志说明与示例注释中"跳过前 1 行（列名行）"的错误表述，并补充与 DB2 `SKIPCOUNT` 的差异说明 |
| 16 | SQL 格式拒绝多表查询 | 新增 `SQLParser.SimpleTableName`：JOIN、隐式多表、派生表、`WITH` CTE 一律在解析阶段报错，提示改用 `--format del`/`--format parquet`。此前这类查询会以 FROM 首表生成 `CREATE TABLE a` + 列数不匹配的 `INSERT INTO a`，产出一个无法执行的 SQL 文件 |
| 17 | 位置 `--columns` 数量校验 | 位置映射按索引配对，此前条目数与文件列数不符时一路带到服务端报列数不匹配（且 replace 模式已先清表）。现在写入前校验条目非空且数量与文件列数一致（`--no-col-names` 无表头时只校验非空），并提示改用 `file_col=table_col` 名称映射导入子集 |
| 18 | DEL 表头不再被十六进制化 | `--hex-blob`（或非 utf8mb4 自动开启）时，写端此前对**表头行**也按列序号套用二进制/十六进制转换，导出文件首列名变成 `0x7662` 之类的字面量，导致该文件无法回读。现在表头一律按标识符原样写出，并补充回归用例 |
| 19 | 并行导出补齐 NULL 键行 | 分片以 `MIN()/MAX()` 取值、以 `col >= b0 AND col <= bN` 圈定区间，两者都**不含 NULL**：分片键可空时 NULL 行此前被静默丢弃。现按 `information_schema.IS_NULLABLE` 追加一个 `IS NULL` 分片；复合分片键无法用单个 `IS NULL` 表达，改为不分片（回退单线程全表扫描），并在用例中固化两种行为 |
| 20 | 损坏的 Parquet 文件返回错误而非 panic | parquet-go 在魔数/页头非法时 `panic`，整个进程带着未落盘的输出崩溃。读端 `NewParquetReader`/`ReadRow` 增加 `recover()`，转成 `invalid parquet file: …` 错误，退出码 1，错误信息保留原始 panic 文本便于定位 |
| 21 | 支持仅结构 SQL 文件导入 | `--no-data` 导出的 SQL 文件只有 `CREATE TABLE`：读端此前把"读不到 INSERT"当 EOF 报错，create 模式无法用来建表。现在 EOF 与"已捕获建表语句"区分，create 模式执行完 DDL 后以 0 行正常结束（日志注明 structure-only，消息文件写 `SQL3110N` 收尾）；既无 CREATE 也无 INSERT 的文件仍判为错误，但改为可读提示而不是裸 EOF |
| 22 | SQL 格式导入也纳入目标列预检 | 此前只有 DEL/Parquet 走 `validateTargetColumns`，SQL 格式在 insert/update/replace 模式下表不存在或列名不符时，直到服务端报错（replace 模式甚至已先清表）才失败。现在除 create 模式外一律前置校验 |
| 23 | 参数错误统一退出码 2 | 补齐三类漏网：未知标志的取值错误（`--threads=abc`）、多余位置参数、未知子命令。经 `SetFlagErrorFunc` + `cobra.NoArgs` + `main.go` 归类，与用法错误共用退出码 2；运行期错误仍为 0/1，`imex` 无参数仍返回 0 |
| 24 | `--session-var` 取值非法按用法处理 | `--session-var foo`（无 `=`）此前是运行期错误退出 1，现包装为用法错误退出 2 |
| 25 | `insert_update` 无唯一键时告警 | 无主键/唯一索引时 `ON DUPLICATE KEY UPDATE` 退化为普通 INSERT，重复导入会静默翻倍。导入开始前查 `information_schema.STATISTICS`，命中则写 WARN 日志并在消息文件追加 `SQL3107N` |
| 26 | `--no-char-del` 写出前校验 | 读端只在**引号内**处理转义，因此 `--no-char-del`（或空字符定界符）时每个字段都是裸文本，含列分隔符/行分隔符的数据会产出无法解析的文件（实测 `sql: expected N arguments` 或整行错位）。现与 `--no-escape` 共用写出前校验，报 `--no-char-del: column N value contains the ... delimiter` 并中止 |
| 27 | Parquet 导出限制行组大小 | parquet-go 的行组上限默认是 `math.MaxInt64`，写端会把**整个结果集**缓存在内存：1M 行 10 列表实测峰值 RSS 271 MB 且随行数线性增长。现固定每行组 100 000 行，同一导出 RSS 降到 ~73 MB 且与行数无关，文件体积仅增 0.02% |
| 28 | `make test` / `make test-live` 可用 | 两个目标都带 `-race`，而 `-race` 需要 cgo；此前沿用发布构建的 `CGO_ENABLED=0`，导致 `make test` 在任何环境都直接失败（`-race requires cgo`）。测试目标固定 `CGO_ENABLED=1`，发布构建不变 |
| 29 | DEL 专属选项改为显式报错 | 此前 `-f sql --col-del '@' --no-char-del --date-fmt ...` 与 `-f parquet --col-del '\|' --hex-blob` 都退出 0 并静默忽略这些标志，而 SQL 专属选项（`--no-create`/`--statement-size` 等）早已有强制校验，两类格式专属标志的不对称会误导用户以为文件按指定布局写出。新增 `validateDELOnlyOptions`：`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-escape`/`--timestamp-fmt`/`--date-fmt`/`--no-char-del`/`--no-col-names` 在导出与导入的 `--format sql`/`parquet` 下一律报 `can only be used with --format del` 并退出 2（默认值不算显式传入，故不影响既有命令）；同时新增 `validateParquetBinaryOptions`，`--hex-blob`/`--no-hex-blob` 与 `--format parquet` 组合退出 2——Parquet 二进制列已是原生 `binary`，开关无意义，而 SQL/DEL 仍正常使用它们 |
| 30 | 日志一条记录一行、切布局不改级别 | 两处不一致。① `java` 布局只给含空格/制表/换行的值加引号，且不转义换行本身：配置行打印 DEL 定界符时 `row_del=` 后面是真实换行，一条记录被拆成两行，`char_del="` 与 `escape="` 又是裸双引号，后续 token 无法切分。现统一按 slog 规则转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`），一条记录恒为一个物理行，`java`/`text` 对同一值渲染出同一 token。② `currentLevel()` 只探测 debug/warn/error，`ApplyLogFormat()` 在 `PersistentPreRunE` 里重设布局时把 info 误判为更高级别，抬高阈值后文档承诺的版本横幅 `imex starting` 以及任何早于子命令 `InitLogger` 的 INFO 记录被静默丢弃（实测 export/import/version 三种运行均无首行横幅）。补 `LevelInfo` 探测，横幅恢复为运行首行；新增 `internal/common/log_test.go` 固化单行、转义、级别保持、横幅四项行为 |
| 31 | `--log-format` 非法取值改为用法错误 | `internal/common.ValidLogFormat` 与 `cmd/validate.go` 的 `validateLogFormat` 此前无调用方，`--log-format bogus` 与 `IMEX_LOG_FORMAT=javaness` 都退出 0 并静默回落 java 布局，与其它参数不一致（`log.go` 的注释还声称会退出 2）。现把它接进 root 的 `PersistentPreRunE`（在应用布局之前校验），非法取值报 `invalid --log-format %q: must be one of java/text` 并退出 2；`cmd/cli_matrix_test.go` 补标志与环境变量两条用例，并修正测试夹具：`logFormat` 是包级标志变量，pflag 只在显式传入时写入，必须逐例复位，否则一条非法取值会污染后续所有用例 |
| 32 | 日志消息统一为小写 `<对象> <事件>` | 原先同一份日志混着三种写法：`Export completed`/`Query failed` 大写开头、`export configuration`/`imex starting` 小写、`Starting export`/`Executing CREATE TABLE statement` 用动名词或 SQL 关键字，大小写敏感的 grep 会漏掉一半记录；而且 `Starting export`/`Starting import` 与紧随其后的配置行字段完全重复。现将全部 56 处 logger 调用（50 个不同消息）统一为小写单行短语，事件词固定为 started/config/progress/completed/aborted/failed（`export command started`→`export started`，`export configuration`→`export config`，`Thread completed`→`worker completed`，`Executing CREATE TABLE statement`→`create table executing`，余同），删除重复的 `Starting export`/`Starting import`（导入的 `bytes` 字段并入 `import config`）；`imex starting` 与 `command failed` 保持原样以免破坏既有文档与告警脚本。新增 `main_test.go` 用 go/ast 静态检查所有 logger 调用的消息首字母小写且单行，防回归 |

| 33 | 发布产物去掉 macOS 架构 | 按要求，打包矩阵不再包含 darwin。`Makefile` 删除 `darwin` 目标、`build-all` 改为 `linux windows`、`.PHONY` 与两处 upx 通配去掉 `-darwin-*`；`bin/imex-darwin-*` 一并清理。工具本身仍是纯 Go、`CGO_ENABLED=0`，在 macOS 上 `make build` 照常可用，只是不再随包发布预编译产物。README 构建说明与本文 §8.5「跨平台」行同步为"预编译产物覆盖 Linux / Windows" |

### 本轮测试确认的既有行为（未改动）

| 现象 | 结论 |
|------|------|
| `--default-charset` 取 `gbk`/`gb18030` 时 SQL 格式拒绝导出 | 有意约束：SQL 文本需按行回读，多字节字符集下无可靠转义。Parquet 仅接受 utf8mb4，DEL 全部可用 |
| 非 utf8mb4 字符集导出含非 ASCII 文本的列 | 由驱动协商的 collation 决定，latin1 下实测出现 `????` 替换且不产生服务端 Warning。属外部行为，工具无法在不改变协议语义的前提下代为报错，建议非 ASCII 数据固定 utf8mb4，二进制列用 `--hex-blob` |
| Parquet + `--threads>1` | 记录 `parallel export is not supported for parquet format, using single-threaded export`（`info` 级）后降级单线程；实测吞吐不随行数变化，属文档化限制而非缺陷 |
| `--commit-count 0` | 视为参数错误退出 2（最小值 1），"只在结束时提交"需要足够大的 `--commit-count` |
| `-q "SELECT 1"`（无 FROM）+ SQL 格式 | 隐式 `DUAL` 表没有 `SHOW CREATE TABLE`，导出退出 1；改用 `--format del`/`parquet` |
| `--char-del "'" --escape "''"`（转义串是定界符的重复）DEL 往返失败 | 既有缺陷，非本轮引入（旧二进制同样失败，已隔离最小复现）：导出端把内嵌 `'` 写成 `'''`，读取端先判字符定界符再判转义符，引号状态错位，导入报 `sql: expected 27 arguments, got 57`。改用 `--escape "'"` 或默认 `"` 均正常。若要修复需统一读写两端的"转义符等于定界符"判定顺序，属独立改动 |

### 2026-10-23 文档与实现对齐

| # | 改动项 | 说明 |
|---|--------|------|
| 34 | 需求文档按实现全量核对后修订 | 以 `--help` 输出、`cmd/validate.go`、`internal/*` 常量与 `main.go` 的退出码分派为准逐项核对：全部 49 个选项在本文与 README 均有落点（无遗漏、无废弃选项）。**注：此后新增 `--progress-interval`（第 46 行）与 `--extended-insert`（第 47 行），按同一口径实测当前选项并集为 51 个（export 43、import 39、全局 6）**，数值边界（port 1–65535、threads 1–256、batch-size 1–100000、pipeline-buffer 0–1024、statement-size ≤268435456、commit-count ≥1、skip/row-count ≥0）与 `docs/cli-oracle.txt` 快照一致。据此修订：1.3 技术栈补齐真实版本（`go.mod` 的 `go 1.26.9`、mysql v1.9.3、cobra v1.10.2、vitess v0.24.5、parquet-go v0.26.4、pkg/errors v0.9.1）并说明超时经驱动字段注入；二节开头补子命令别名 `exp`/`imp`、`-?` 帮助短选项与 completion 禁用；新增 2.4 `encrypt`（`ENC:` + AES-256-GCM + `IMEX_KEY`，含解密调用点与失败退出码实测）；3.3.4 退出码补判定口径（含环境变量失败的分流与位置参数个数的归类）；九节项目结构补 `main.go`、`main_test.go`、`usage.go`、`stats.go`、`test/`、`docs/`、`go.sum`；十一节新增 11.1 测试资产清单（24 文件 / 127 `Test*` / 4 `Fuzz*` / 5 `Benchmark*`）与 11.2 未自动化覆盖场景；十二节交付物改为三平台二进制 + 源码包 + SHA256 清单，并显式声明不再提供 macOS 预编译产物。README 另修两处遗留目录名 `mysql-backup-tool` → `imex` |
| 35 | `encrypt` 位置参数个数错误改为退出 2 | `imex encrypt`（缺参数）与 `encrypt a b`（多参数）此前由 cobra `ExactArgs(1)` 拦截，返回普通 error，实测退出 1，违反"用法错误 → 2"的契约（同类的未知选项、未知子命令、缺必填项都是 2）。`cmd/validate.go` 新增 `exactArgs`，用 `common.Usagef` 报 `imex encrypt accepts 1 argument(s), received N`，`cmd/encrypt.go` 换用它；新增 `cmd/cli_matrix_test.go:TestCLIEncryptArgsCountIsUsageError`，除两种越界外还断言正确参数下密钥缺失仍是运行时错误 1，避免把两类错误合并。实测：无参/两参 → 2，正常加密 → 0，空 `IMEX_KEY` → 1 |
| 36 | 补齐值处理与并行降级两处长年缺失的说明 | 新增 5.4「三种格式共有的值处理」：`YEAR(0000)` 归一化为 `0000`（文本协议回裸 `0`，回导会被两位 YEAR 规则解释为 2000）、NULL 与空字符串在 DEL/SQL/Parquet 下的表示、`--no-char-del` 时二者不可区分且回读一律为 NULL（均已在 MariaDB 上实测确认）。7.2 补「并行导出的降级触发点」表（4 个 warn 触发点 + parquet 的 info 分支，附实际日志文本与代码行号）与复合键/非数值首列语义。同时修正一处过期引用：十四节边界表里 parquet 降级的消息仍写作大写 `Parallel export is not supported for parquet format`，实际已是小写且级别为 `info`。README 对应补：无包裹符模式的 NULL/空串注意、并行导出的降级触发点与复合键说明、格式示例后的「三类格式共有的值处理」小节，并把遗留的 `见 3.6` 引用改为需求文档 3.3.4 |
| 37 | 发布矩阵收窄为 amd64 | 按要求，预编译产物只保留 Linux amd64 与 Windows amd64。`Makefile` 的 `linux` 目标去掉 `GOARCH=arm64` 那行（`build-all` 由 3 个二进制降为 2 个），注释同步；README 构建说明与本文 §8.5「跨平台」、§12 交付物改为两平台，并给出 Linux arm64 的单平台编译命令。本批次的 `*-bin-SHA256SUMS.txt` 相应只列 2 个二进制 |
| 38 | 导入行数拆分新增 `unchanged` 计数 | 实测发现 `insert_update` 重复导入同一文件时（服务端 `RowsAffected=0`）旧推导报 `inserted=4 updated=0`，而真实是 0 新增 0 改动 4 行相同；根因是 `updated = affected - rows` 再钳 0 的推导隐含"每行至少贡献 1 个 affected"。`internal/import/importer.go:addBatchResult` 改为互斥三分：`updated = max(0, affected-rows)`、`unchanged = max(0, rows-affected)`、`inserted = rows-updated-unchanged`（另对 affected 超过 2×rows 的异常值把 `updated` 钳到 `rows`），三数之和恒等于批行数；`internal/common.Stats` 随之增 `AddUnchanged`/`Unchanged`，五处计数日志（`import progress`、结构-only 完成、`import completed`、`import aborted`、`parallel import completed`）统一加 `unchanged` 字段。同时以 go-sql-driver 实测确认 `insert`/`create`/`replace` 三种模式 affected 恒等于行数（`replace` 先清空目标表再插，故每行都是新增），推导对它们是恒等变形。新增 `internal/import/counters_test.go`（4 个测试：11 例拆分表、affected 不可读与负值、跨批累加、`Stats()` 未初始化零值）与 `test/integration/live_mysql_test.go:TestLive_ImportCounters`（真实服务端 5 个场景，配套 `importFileStats` 辅助与 `Importer.Stats()` 只读访问器）。实测：相同内容重导 `inserted=0 updated=0 unchanged=4`，全部改动 `updated=4`，删 2 行重导 `inserted=2 unchanged=2`，replace `inserted=4`。8.3「导入行数」与 README 的行数统计说明、修复记录同步 |
| 39 | `version` 子命令不再写日志 | 此前 `imex version` 先在 stderr 打一条 java 布局的 `imex starting ... command=version`，再打四行版本信息，内容完全重复，且 `--log-format text` 也会改变首行形状，导致 `imex version \| grep` 之类的取版本管道要先过滤日志。`cmd/root.go` 的 `PersistentPreRunE` 把 `common.LogVersion` 改为对 `cmd.Name() == "version"` 跳过，其余子命令（`export`/`import`/`encrypt`/`help`）首行横幅不变。新增 `cmd/version_log_test.go` 两个用例：一个断言 version 的 stderr 为空且四行齐全、输出中不含日志记录，一个反向断言 `encrypt` 运行时横幅仍是 stderr 首行（防止把例外扩成全量禁用）。README 版本说明与本文的日志事件表、8.3「日志顺序」同步 |

### 2026-10-23 DEL 读取路径性能改写

| # | 改动项 | 说明 |
|---|--------|------|
| 40 | DEL 导入解析改为全程 `[]byte`、去掉无效字节↔字符串转换 | 性能分析（pprof CPU + heap，4 核 / 本地 MariaDB，`bench` 表 50 万行 DEL 125 MB）显示导入 CPU 的约 60% 落在 DEL 分帧与字段解析上，堆分配量 1392 MB 是输入体积的 11 倍，其中 `parseLine` 479 MB + `recordReader.Text()` 121 MB。改写 `internal/import/del_reader.go`：① 四个定界符在 `NewDelReader` 里预计算成 `delSpec`（`[]byte` + 长度 + 首字节），`scanRows`/`parseLine` 逐位置比较改为定长比较；② `ReadHeader`/`ReadRow` 用 `recordReader.Bytes()` 取代 `Text()`，取消整行 `string()` 物化；③ 无转义字段直接一次 `string(line[start:end])`，含转义字段用复用的 `scratch` 拼装，`fields`/`quoted`/`scratch` 在 reader 内跨行复用；④ 门控空 `--col-del` 分支（`hasCol`）——CLI 已拒绝空定界符，但库内直接构造 `DelReader` 时旧实现会因空前缀恒命中而不返回，新实现把整行作为单个字段返回。值类型仍保持 `string`——装箱到 `driver.Value` 对 `[]byte` 同样分配且更宽（`convTslice` 24 B vs `convTstring` 16 B），换类型不会减少分配。改写过程由差分测试暴露并修复两个引号边界缺陷：闭合定界符被留在值末尾（`TestDelReader_LargeRecord` 长度 +1）、双写转义后未把干净片段并入 `scratch` 导致 `Alice ""The Great""` 解析为 `Alice ""`。新增 `del_reader_equiv_test.go`（保留改写前的 `parseLine`/`scanRows` 作参考实现，28 个手写用例 + 4000 行随机语料 × 6 套定界符配置逐字段比对 + `FuzzDelReaderParse` 差分模糊，25 s 246,417 次执行无发现）与 `del_reader_bench_test.go`（2 万行贴近导出器真实产物的确定性语料，`ReadRow`/分帧/纯解析三段基准）。实测：微基准 89.4→31.4 ms（2.8×）、31.6→9.3 ms（3.4×）、45.8→18.5 ms（2.5×），`ReadRow` 分配 757,137→440,256 次/操作、驻留 26.8→10.5 MB/操作（同一语料在 HEAD 工作树与新树上背靠背各测两轮，取其一）；端到端 `--threads 1` 6.81→5.70 s、客户端 CPU 8.02→3.76 s，t4 5.61→3.64 s，t1→t4 加速比 1.21×→1.87×。6 组定界符/转义/十六进制标志组合的真实往返逐行一致（其中一组暴露上表的 `--escape "''"` 既有缺陷）。`make test`（`-race`）、`make test-live`、`go vet ./internal/...`、`gofmt -l` 全通过。新增测试资产使全仓库规模升至 28 文件 / 139 `Test*` / 5 `Fuzz*` / 8 `Benchmark*`，11.1、11.2、九节、十二节的计数同步更新，7.5 增补「DEL 解析路径」小节 |
| 41 | 并行导出去掉分片临时文件，改为流式写出 | 承接 40 之后的性能分析：并行导出每个分片先写 `<output>.shard_<i>.tmp`，全部分片结束后再 `io.Copy` 拼接，**每个字节落盘两次**（实测 DEL 输出 124,903,023 B 对应写入量 487,920×512 B ≈ 238 MB，SQL 131,872,590 B 对应 515,152 块 ≈ 251 MB），并行导出因此需要两倍输出的空闲空间，且被信号打断时输出目录残留整批临时文件（实测 4 个共 33 MB）。新增 `internal/export/shard_stream.go`：每个分片序列化到自己的 `shardPipe`（256 KiB 块，块深由 64 MiB 总预算按分片数均分、上界 64 块下界 2 块），`exporter.go` 的 `mergeShardFiles` 改为 `writeShardStream`，在分片仍在产出时按分片序号依次消费并写进最终文件，不再创建任何临时文件；分片失败或写文件失败（如 `ENOSPC`）时删除已写出的不完整输出并警告 `incomplete output file removed`，其余阻塞在队列上的分片由 `shard output consumer stopped` 解除（不会挂死，ctx 取消同路径）。队列深度实测是关键：SQL 格式每分片 2.5 MiB 时墙钟 1.31 s、8 MiB 时 1.24 s，对旧实现 1.09 s（慢 14~20%，写合并 goroutine 反复等已经停下的生产端），16 MiB 回到 1.09 s 持平，故取预算值而非常数；代价是在途字节从磁盘移到堆上，4 分片峰值 RSS 约 33→111 MB（DEL）、114→215 MB（SQL）。验收：与旧二进制在同一库同一时刻并行导出的产物**逐字节一致**（DEL/SQL），写入量减半（DEL 487,920→243,952 块、SQL 515,152→257,568 块），单线程导出产物不变，导入回读的行数与顺序无关 CRC 和与原表一致，32 MB tmpfs 上的 ENOSPC 用例失败后不留任何文件，7 次交替 A/B 取中位数。测试侧 `exporter_merge_test.go` 改为 `exporter_stream_test.go`（同一组 header/LOCK/前导断言改走队列）并新增分块顺序用例，`shard_stream_test.go` 以 9 个用例覆盖队列与流式语义；全仓库测试资产升至 29 文件 / 149 `Test*` / 5 `Fuzz*` / 8 `Benchmark*`，九节项目结构、11.1 计数同步，7.2 并行导出流程改写并新增失败与中断语义段落，README「并行导出」补充说明 |
| 42 | 交叉选项 / 交叉命令行为矩阵，并修复它暴露的两个数据完整性缺陷 | 此前 CLI 侧只有 `cmd/cli_matrix_test.go` 钉住"哪些组合被接受或被拒"，库侧只有 `live_mysql_test.go` 走 API；**"选项被接受之后是否真的生效、导出与导入两侧是否对称"没有覆盖**。新增 `test/integration/option_matrix_test.go`（12 个测试入口、`TestMain` 一次性 `go build` 出真实二进制后用 argv 驱动，故 flag 解析、校验、config 装配、写入器、读取器全程在链路上）：DEL 布局修饰符成对往返（`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-char-del`/`--no-escape`/`--no-col-names`，含 `\t`、`\r\n` 转义写法）、逐字写入模式对不可表示数据的拒绝、SQL 攒批与单行 INSERT 的语句数/字节上界、`--threads` 的输出确定性、`--date-fmt`/`--timestamp-fmt`、`--no-data`/`--no-create` × `--mode create`、`--skip-count`/`--row-count` × 线程/`--batch-size`/`--commit-count`、`--messages` 在三格式与并行路径下的 SQL3104N/SQL3110N、`--no-replace` 哨兵存活、`--default-charset` × 二进制列 0x 自动切换。矩阵首轮跑出 3 个真缺陷：① **单线程导出失败留下半截产物**（20 万行表被 `--no-char-del` 拒于末行时残留 21,233,664 B 文件，并行路径早已删除），修复为 `exportSingle` 在 `os.Create` 之后注册共用的 `removeIncompleteOutput`（`exportParallel` 改为复用同一方法），注册点在处理之后保证创建前失败不会误删既有文件；② **无表头文件配位置 `-c` 静默丢首行**（`--no-col-names` 导出 + 只给 `-c` 导入时 3 行只进 2 行且退出 0，同一命令用于带表头文件又是对的），因文件自身无法区分两者、改名导入也走同一路径，故不改结果只补齐可见性：被当表头丢弃的那行若与目标表列名全不匹配，记 warn 并写入消息文件，说明加 `--no-col-names` 才不漏行；③ `--columns` 缺失时的报错只提示 `-c`，未说明无表头需成对使用，已在 7.4 与 README 的标志表补"成对使用"。同时确认三项**属既有行为而非缺陷**并写进文档：并行 SQL 输出在分片边界多出 `threads-1` 个 INSERT 头（数据一致，DEL 则逐字节一致，已按头数与字节差精确钉住）、`--timestamp-fmt` 省略 `ss` 会把秒截为 00（用户选定的精度，现固定其余字段不得变动）、非 utf8mb4 连接字符集对不可映射字符由服务端替换为 `?`（文本有损、二进制因 0x 自动切换无损）。另记录两项待用户定级的可用性问题：认证/字符集/库名这类永久错误被 ping 重试 3 次并 6 s 才失败，且错误前缀 `failed to ping database after 3 attempts` 掩盖真实原因；`--no-create` 文件配 `--mode create` 直接撞服务端 1146 而非事先提示文件不含建表语句。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产升至 30 文件 / 161 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`，九节项目结构、11.1 清单、7.2 失败与中断语义、7.4 位置映射、5.3 日志表、README 并行导出与 `--no-col-names` 说明同步 |
| 43 | `--messages` 改为命令行日志文件，删除 DB2 风格消息报告 | 用户指出 `--messages` 提供的应当是**命令行的日志打印**，而不是 `SQL3xxxN` 样例日志，且带上该选项后所有日志都要进文件。实现落在日志层而不是命令层：`internal/common/log.go` 新增进程级 sink（带互斥锁；`os.Stderr` 在 `Write` 时解析而非构造时捕获，因此既有的 stderr 重定向测试与 `--echo-sql-values` 行为不变），`java` 与 `text` 两种布局都改经该 sink，`OpenLogFile(path)` 把文件挂到 sink 上（同一条记录写完 stderr 再写文件）、`CloseLogFile()` 摘下并关闭；根命令 `PersistentPreRunE` 在应用 `--log-format`、打印版本横幅**之前**打开文件，故横幅恒为文件首行、用法错误也进文件，`cmd.Execute()` 在记录完 `command failed` 之后关闭，故收尾错误同样在文件里。删除 `internal/common/message.go`（`MessageWriter` 及其 `SQL3104N/3105N/3107N/3109N/3110N` 与 `# Completed/# Duration/# Rate` 页脚）与 `message_test.go`，导出器 15 处、导入器 20 处（含 `batchcap.go` 2 处）消息调用全部移除：每条告警与失败本来就有对应的 `common.Logger` 记录，失败还会由 `Execute` 统一记成 `command failed`，因此不丢信息，只去掉重复的第二份；`SQLWriter.SetWarningFunc` 与 `Exporter.warningFunc`/`messageTableName` 一并删除。选项语义变更后 `-m` 短选项沿用（`export`、`import` 两侧均已注册），帮助文本改为「Write every log record to this file as well as to stderr」；`--quiet` 现在同样作用于文件（不再有「消息文件是显式产物」的例外）；`config.ExportConfig.Messages`/`ImportConfig.Messages` 随实现移除，日志文件是进程级 CLI 关注点而非库配置项。实测：`-m` 导出失败时文件与 stderr `diff` 逐字节一致（4 行含版本横幅、`export config`、`query failed` 真实原因与 `command failed` 包装）；`--log-level error` 下成功导出文件只剩横幅；`--log-format text` 布局随 stderr 一起切换。测试：`live_mysql_test.go` 的 `--messages` 断言改为经 `common.OpenLogFile` 走同一 sink，并断言每条告警与失败在日志里都有对应记录、文件里不再出现 `SQL31` 码；`option_matrix_test.go` 的 `--messages` 用例重写为 5 种格式/并行组合下「文件等于命令行日志」（首行为横幅、含 `export config`/`export completed`/`rows=10`、每一行都出现在 stderr 上），另加 `-m` 短选项、`--log-format text`、`--log-level error`、失败记录四个子用例。文档：1.2、导出/导入标志表、2.3 整节改写为「日志文件（`--messages` / `-m`）」、3.3.1 静默、四节 DB2 映射（显式标注语义不同）、导出导入流程步骤、5.3 功能表、九节结构与测试文件计数、11.1/11.2 清单同步，README 同步；`docs/cli-oracle.txt` 保持冻结基线并在头注登记这一处有意差异。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 29 文件 / 161 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`（`internal/common` 少一个文件、少 5 个 `MessageWriter` 用例，新增 5 个 sink 用例：文件与 stderr 逐字节相等、闭包后脱钩且二次关闭无错、早建好的 text 布局也跟随 sink、200 条并发记录不拆行、替换路径只写最新文件、创建失败时报出路径且不影响 stderr）。**本行是中间态**：该实现把记录同时写 stderr 与文件，下一行（44）改为只写文件 |
| 44 | `--messages` 改为独占：带 `-m` 时终端不再打印 | 第 43 行留下的选择题有了答案——用户要求"带 `-m` 就只进文件、终端不再打印"。改动只在 `internal/common/log.go` 的 `sink.Write`：文件挂载时**只写文件**、不再写 stderr，未挂载时照旧写 stderr（`os.Stderr` 仍在写入时解析，logger 的重定向测试不受影响），互斥锁保留（并行路径的两个协程仍会同时写同一个文件，交叉写入会毁掉记录）。`CloseLogFile` 先脱钩再关闭，因此"关闭失败"这条 ERROR 是唯一回到 stderr、不落进文件的记录，不会丢。帮助文本由「Write every log record to this file as well as to stderr」改为「Redirect the command log to this file; with it, nothing is printed to stderr」，`--echo-sql-values` 的告警措辞改为"写入 stderr 或 `--messages` 文件"，导出/导入两处进度注释同步。实测（真实二进制 + 本地 MariaDB）：带 `-m` 的成功导出 stdout 与 stderr 均 0 字节、文件 4 条记录、退出 0；带 `-m` 的失败导出同样 0 字节、文件 5 条含 `query failed` 与 `command failed`、退出码仍为 1；同一条失败命令去掉 `-m` 的 stderr 与该文件去掉时间戳/PID 前缀后 `diff` 逐行相同；`--log-level error` 下文件只剩横幅且横幅不回终端；`--log-format text` 同样只进文件。两类例外如实记录并在文档中说明：未知选项在 `PersistentPreRunE` **之前**失败，文件尚未打开，错误只上 stderr 且不建文件；`-m` 路径无法创建时报 `failed to create message file <path>: ...`（stderr，退出 1）。测试：`TestLogFileMirrorsStderr` 改名 `TestLogFileTakesOverFromStderr`，断言 stderr 为空、文件恰有 3 条、关闭后记录回到 stderr 且文件不再增长；text 布局用例改为"处理器早于挂载也只得写文件"；矩阵 `--messages` 用例把"每一行都出现在 stderr"换成"stdout+stderr 为空"（5 种格式/并行组合 + `-m` 短选项 + 失败记录 + 级别过滤四处各加一条静默断言）；`--no-col-names` 对称用例的 WARN 断言从输出移到文件，并加终端必须为空的断言。测试资产数量不变（29 文件 / 161 `Test*` / 1 `TestMain` / 5 `Fuzz*` / 8 `Benchmark*`）。文档：1.2 特性、导出/导入标志表、2.3 引言与行为约定（新增"终端"与"例外"两行）、3.3.1 静默、4 节 DB2 映射、6/7 节流程步骤、5.3 两处功能表、11.1/11.2 清单，README 同步 |
| 45 | 进度记录不再晚于结束汇总出现，并澄清 `rate` 与 `interval_rate` | 用户实测发现 `completed` 之后还会打印 `progress`，即日志以一个"还没跑完"的记录收尾。根因在计时器的停止方式：`common.StartMonitor` 返回的是 `context.CancelFunc`，`defer stopMon()` 只发出取消信号，goroutine 里**已经开始的那次 report 收不回来**；而汇总在 `Export`/`Import` 返回之前写好，于是"取消之后才落笔"的进度行必然排在 `completed`/`aborted` 之后。两处一起修：① `internal/common/stats.go` 的 stop 改为**排空**——`once.Do(cancel)` 后等待 monitor goroutine 退出（`done` channel），并二次确认 `monCtx.Err()`，避免 select 同时就绪时随机挑到 `ticker.C` 再写一条；stop 幂等，可"汇总前停一次 + defer 兜底一次"。② 五个终点汇总（`export completed`、`parallel export completed`、结构-only `import completed`、`import completed`、`parallel import completed`）与 `import aborted` 之前显式 `stopMonitor()`，停止句柄从 `Export`/`Import` 的局部变量提升为 `Exporter.stopMonitor`/`Importer.stopMonitor` 字段（汇总写在子方法里）。③ 间隔由硬编码 `5*time.Second` 改为 `common.MonitorInterval` 变量（默认值不变，测试压到 1 ms 才能反复触发计时器）。**注：该变量在第 46 行升级为命令行选项 `--progress-interval`，配置字段取代了包级变量。**同一提交顺带澄清两个速率的口径并写进 5.3：`rate` 是自开始以来的**总速率**（已处理 ÷ 已用时长，平滑），`interval_rate` 是**相邻两个进度点之间的区间速率**（两次快照之差，反映当前吞吐与抖动），二者无倍率关系，长跑时以 `interval_rate` 判断"此刻快不快"、以 `rate` 判断"整体多少"。测试：新增 `internal/common/stats_test.go`（2 用例）——把一次 report 钉在"正在写"的状态再 stop，断言汇总之后没有任何记录追加、最后一条是汇总、二次 stop 不死锁；该用例在旧实现下**必然失败**（实测输出 `[completed progress progress]`），是这一缺陷的回归钉子。`test/integration/live_mysql_test.go` 新增 `TestLive_ProgressNeverOutlivesCompletion`：8192 行表（`growTable` 服务端翻倍造数）在 1 ms 间隔下跑单线程导出、并行导出、单线程导入、并行导入四段，断言日志里确有进度记录且最后一条是对应汇总。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 30 文件 / 164 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`（`internal/common` 7 文件 29 个、live 6 个），九节结构与计数、5.3 进度行、6/7 节流程步骤同步，README 补进度段落与修复记录 |
| 46 | 删除与配置回显重复的 `export started`/`import started`，进度间隔改为 `--progress-interval` | 用户确认了这一去重方案并要求间隔可由命令行调整。① 两条 `started` 记录的字段（output/input、format、host、port、database）是紧随其后的 `export config`/`import config` 的子集，重复源于早期"命令启动"与"参数快照"分开的历史。现删除这两条记录，并把配置回显从 `Export`/`Import` 上移到 `NewExporter`/`NewImporter` 的第一句，即**建立连接之前**——原先它打在连接之后，连接失败时反而一个参数都看不到。实测：3 行小表导出共 3 条记录（横幅、`export config`、`export completed`，已无 started）、耗时 6 ms；`-q "SELECT * FROM no_such_table_x"` 的失败导出文件 4 行（横幅、config、`query failed`、`command failed`）、退出 1；密码写错那次 6 行（横幅、config、3 条 `database ping failed, retrying`、`command failed`）、退出 1 且输出文件根本没有创建。② 间隔改为命令行选项：`config.Config.ProgressInterval`（导出与导入共用）+ `config.DefaultProgressInterval = 5s` + `EffectiveProgressInterval()`（零值与负值回落默认值，因为库调用方可直接构造结构体，而 `time.NewTicker` 对非正间隔会 panic）；`export`、`import` 两侧各注册 `--progress-interval`，`cmd/validate.go` 复用既有的 `validatePositiveDuration`，`--progress-interval 0` 报 `invalid --progress-interval 0s: must be > 0` 并退出 2（0 不解释成"关闭进度"，那是 `--quiet` 的语义，一个概念只留一个开关）；第 45 行引入的包级变量 `common.MonitorInterval` 删除，导出器与导入器改用 `cfg.EffectiveProgressInterval()`，配置回显增加 `progress_interval` 字段。口径同时写进 5.3：首个 tick 要等满一个间隔，所以运行短于间隔时**一条进度也不打印**，`stop()` 只等在写的记录落笔、不空等剩余间隔（实测 3 行表 6 ms 结束、日志无 progress 行）；`--progress-interval 200ms` 跑 100 万行导出得 29 条进度记录，最后一条仍是 `export completed`。测试：新增 `internal/config/config_test.go`（2 用例：默认值与取值解析，导出/导入两侧各测）、`cmd/cli_matrix_test.go:TestCLIProgressInterval`（缺省为 5s、1ms/30s 原样生效、0 与 -1s 均为用法错误退出 2）、`internal/common/stats_test.go:TestMonitorSilentBeforeFirstTick`（取代原默认值用例：1 小时间隔下回调 0 次、stop 立即返回）；`TestLive_ProgressNeverOutlivesCompletion` 的四段改由配置的 `progressInterval` 字段驱动（不再改包级变量），并追加第五段用编译出的真实二进制跑 `--log-level info --progress-interval 1ms --messages`，断言汇总仍是最后一条、日志中不含 ` started ` 记录、config 带 `progress_interval=1ms`。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 31 文件 / 167 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`。文档：1.2 特性、导出与导入标志表、四节 DB2 映射（标注无对应）、2.3 实测文件改为 4 行并补连接失败一例、记录来源表（删 `started` 行、config 行注明建连接之前打印）、3.3.1 静默、5.3 进度输出与日志顺序、6/7 节流程步骤、九节与 11.1 计数，README 同步 |
| 47 | `--format sql` 默认改为每行一条 INSERT，攒批交给导入侧 | 用户选定方案：攒批在导入侧实现，导出默认只出单条单行 SQL。① `config.Config` 的 `SingleRowInsert` 字段换成 `ExtendedInsert`（默认 `false`），`NewExporter` 的两条路径改为 `SetSingleRowInsert(!cfg.ExtendedInsert)`；写入器自身语义不变（`NewSQLWriter` 仍默认合并，供库调用方使用），只是导出器反过来在给了 `--extended-insert` 时才关掉单行。`--statement-size` 在默认布局下退化为告警阈值：一行无法再拆分，超过它只记一次 WARN 并完整写出。② CLI 新增 `--extended-insert`（help：`Merge rows into multi-row INSERT statements bounded by --statement-size`），`--single-row-insert` 保留为已废弃兼容项（help：`Deprecated: one INSERT statement per row is now the default`，传入后不改行为），二者同时给出由新增的 `validateInsertLayout` 报 `--single-row-insert and --extended-insert cannot be used together` 并退出 2；`--extended-insert` 并入 `validateSQLOnlyOptions`，非 SQL 格式照旧拒绝。③ 配置回显在 `statement_size` 之后新增 `extended_insert` 字段。**实测**（本地 MariaDB，`bench_narrow` 100 万行，热缓存，`/usr/bin/time -v` 取峰值 RSS）：导出默认单行 2.43–2.57 s / 35.5–38.5 MB / 278,790,434 B（gzip 28,247,005 B）/ 1,000,000 条语句；`--extended-insert` 2.65–2.69 s / 71.3–74.1 MB / 153,792,809 B（gzip 27,050,101 B）/ 19 条语句，且产物与旧默认**逐字节一致**（md5 `61e8b0714b262972dac8b66dbedb2f90`）；DEL 参考 1.76 s / 25.6 MB / 141,218,462 B（gzip 26,795,207 B）。导入同一份数据：`--batch-size 1000` 单线程下 20,000 条语句的单行文件 12.49–12.87 s / 42–44 MB，1 条语句的合并文件 12.96–13.19 s / 215–224 MB；`--threads 4 --batch-size 20000` 单行 10.00–10.22 s / 67–69 MB，合并 8.90–8.91 s / 273–276 MB；DEL 参考 11.01 s / 35 MB（t1）、7.42 s / 166 MB（t4）。目标库 general log 复核（20,000 行、`--batch-size 1000`）：两种布局都只产生 20 条服务端 INSERT，即文件布局不改变下发的语句。结论：**内存差是结构性的**（导出峰值减半、导入峰值降到 1/5～1/2，因为合并布局要把整条语句物化），**速度不是**（±10% 内，t1 单行略快、t4 大批量合并略快），代价是文件体积 +81%（gzip 后 +4.4%）。测试：`cmd/cli_matrix_test.go` 新增 `TestCLIInsertLayout`（缺省 `ExtendedInsert=false`、`--extended-insert` 落到 config、废弃项解析但不启用合并）与 3 条矩阵用例（`--extended-insert` 合法、配 `--format del` 被拒、与 `--single-row-insert` 互斥）；`test/integration/option_matrix_test.go:TestOptionMatrixSQLBatching` 以“10 行 10 条语句”为新默认基线重写，覆盖旧选项、合并、`--extended-insert --statement-size 512` 的字节上界、单给 `--statement-size` 不改布局四种布局并逐一往返校验（上限放宽 4 字节，新增 `statementCapSlack` 常量说明写入器是先判上限后加 `, ` 与 `;\n`，原 128 字节用例小于任何单行语句因而合并必然为 0，改为 512）；`TestOptionMatrixParallelOutput` 增加“默认 SQL 布局的并行产物逐字节等于串行”，把分片边界头数与字节差的检查移到 `--extended-insert` 路径。文档：1.2 特性、2.1.6 标志表与布局说明、4 节 DB2 映射（`MODIFIED BY STATEMENTSIZE` 语义收窄并新增“无对应”一行）、5.2 文件结构（默认与 `--extended-insert` 两例互换，顺带记录 `LOCK TABLES` 早于 `CREATE TABLE` 使 `mysql < file` 进空库在第 2 行 `ERROR 1146 (42S02)` 中止的既有缺陷，**待用户定级**）、5.3 记录来源表、6/7 节流程步骤、7.4 攒批前提、10 节测试场景行、11.1 计数（31 文件 / 168 `Test*`，此前该行的 30/161 已与逐文件实测不符，一并更正），README 特性、SQL 标志表、导入攒批章节、参数校验表、SQL 格式示例说明、项目结构注释与修复记录同步。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 48 | SQL 导入的语句级改为全程 `[]byte`，去掉字符串物化与平方级扫描 | 用户要求"语句级避免 string 和字节的转换"。改写 `internal/import/sql_reader.go`：① `nextStatement` 返回记录缓冲里的切片，单行语句不再经 `recordReader.Text()` 物化整条语句，只有跨行语句才复制进 reader 自带的 `stmt` 复用缓冲；② 引号状态提为 `stmtState` 跨行携带，追加一行不再从头重扫已积累的文本——正是那次重扫让折行文件的代价随语句长度平方增长；③ 关键字与 `NULL` 的比较改在字节上做（新增 `hasPrefixFold`/`indexFold`/`equalFoldNull`，只折叠 ASCII 区间，多字节字符不再因大写化改变字节长度而使偏移错位），删除整条语句的 `strings.ToUpper` 副本；④ `extractRows` 定出元组边界后把窗口直接交给 `parseValueList([]byte)`，取消每个元组一个 `strings.Builder`，只有留存的值拷贝一次；⑤ `0x` 字面量改走 `hex.Decode` 到预分配切片，不再 `hex.DecodeString(string(...))`，`decodeHexLiteralBytes` 的"畸形则原样返回"分支显式复制一份字面量。`internal/import/importer.go` 的 `buildBatchInsertSQL` 配套改为精确长度 `Grow` 的一次成型：归属注释作为最前面的字节写入（原 `common.TagSQL` 在成品文本前再拼一遍，整批内容白复制），占位符组 `(?,?,?)` 先按列数拼好、每行写一次（原逐列逐字节写）。值的类型仍是 `string`——装箱到 `driver.Value` 必然分配，只减拷贝次数。实测（新增 `sql_reader_bench_test.go`，同一份 2 万行 × 8 列数据三种布局，HEAD 工作树与新树背靠背，`-benchmem`）：默认单行 90.0→42.3 ms（2.1×）/ 36.6→9.3 MB/op / 728,986→340,009 次分配；`--extended-insert` 69.1→32.8 ms（2.1×）/ 32.1→10.2 MB / 629,507→320,368；跨物理行折行 1839.3→32.1 ms（57×）/ 40.8→10.4 MB / 650,226→320,387。`BenchmarkBuildBatchInsertSQL`（打开归属注释，1000 行 × 10 列的一批）131→21 µs、138,160→24,792 B、20→3 次分配。端到端（本地 MariaDB，`--batch-size 1000` 单线程，注入 commit 的 `-trimpath` 构建，`/usr/bin/time -v` 峰值 RSS）：20 万行单行布局 2.61→2.29 s / 41.6→34.6 MB，同数据合并布局 2.85→2.44 s / 187.8→137.6 MB，148 万字节 10 条语句的折行文件 5.98→0.184 s / 37.4→35.4 MB；12 次导入（新旧二进制 × 三种布局 × 注释开关）目标表 `CHECKSUM TABLE` 逐组相同，general log 复核折行文件仍只产生 10 条约 138,556 字节的 INSERT 且以 `/* imex:<commit> */` 开头，说明下发内容不变。**改写过程由测试抓出两处自伤**：占位符组的拼装首次写成 `"(?" + Repeat("?,", n-1) + ")"` 产出 `(??,)`（多一个 `?`），以及纯字面量快路径漏掉 `unescapeSQLString` 使 `\n` 不再还原——前者由 `TestBuildBatchInsertSQLTagged` 的字节相等断言暴露，后者由既有 `TestSQLRoundTrip_PreservesEscapedText` 暴露。测试资产升至 32 文件 / 172 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 12 `Benchmark*`，新增 `sql_reader_bench_test.go` 与 `TestSQLReader_MultiLineStatement`（值内真换行、值内 `;`、双写引号、`;` 独占一行）、`TestSQLReader_KeywordCaseAndNonASCII`（小写关键字、U+0131 与中文值、裸 `NULL` 与带引号 `"null"` 的分别）、`TestBuildBatchInsertSQLTagged`；7.4 增"语句文本一次成型"、7.5 增"SQL 语句解析路径"小节，11.1/11.2 与九节注释同步。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，`gofmt -l`、`go vet ./...` 干净 |
| 49 | 并行导出的分片在读完后立刻释放连接与快照，不再等尾部字节交给流式写出 | 用户提出云环境下的场景问题：5 线程并行导入/导出，先完成的那几个线程会不会持有空闲连接。实测结论分两侧——**导入侧本来就没有这个问题**：并行导入是单个读取 goroutine + 共享 `batchCh`（容量 `--pipeline-buffer`）的动态派工，没有静态分区，worker 拿到关闭的通道后走 `workerLoop` 退出 → `session.commit()` → `worker completed` → `defer db.Close()`，`--threads 5` 全程 `SHOW PROCESSLIST` 恰好 6 条连接（5 worker + 主池）且结束时全部消失，唯一空闲的是主池（0→32 s `Sleep`，属设计内的常备连接）；**导出侧确实持有**：`exportParallel` 用静态等宽分片，每个分片一条自己的连接与 `START TRANSACTION WITH CONSISTENT SNAPSHOT` 快照，行读完后 `exportShard` 还要写页脚（SQL 格式先刷出最多 `--statement-size`，默认 8 MiB 攒批 INSERT）再 `pipe.Close()`，而 `writeShardStream` 按分片序号消费队列，于是"已经读完的分片"会堵在交尾字节上，连接与快照一起被占着。400 万行倾斜表（分片 0 占 360 万行、分片 4 占 40 万行、`--threads 5 --shard-key bucket`）实测：`duration=9.28s rows=4000000 bytes=587806929`，`SHOW PROCESSLIST` 里一个已完成读取的分片停在 `Writing to net` 约 5 s，`INNODB_TRX` 快照年龄从 1 s 涨到 9 s，另有空分片毫秒级释放（这条本来就对）。改动：`exportShard` 引入幂等的 `releaseDB()`（`rows.Close()` + `db.Close()`），在行迭代结束且 `rows.Err()` 判过之后**立刻**调用，页脚与 `pipe.Close()` 移到其后；`defer releaseDB()` 注册在输出 `Close` 的 defer **之前**，因此查询失败、迭代失败、页脚失败等路径下数据库仍是最先被放弃的资源，`db.Close()` 一次都不会漏。为让这条不变量能在无数据库条件下回归，`Exporter` 新增未导出的 `openShard func() (*sql.DB, error)` 钩子（默认走 `createShardConnection`，测试注入进程内假 `driver.Connector`）。实测这条**是不变量而不是吞吐**：本地写普通文件时尾部只有 1~2 个 256 KiB 块（DEL/SQL 写入器都用 `bufio.NewWriterSize(w, 256*1024)`），几乎堵不出时间，A/B（同一倾斜表、同一 0.4 s 采样观察）为 DEL 8.334/8.167 s 对 8.140/8.050 s、SQL `--extended-insert` 12.284/12.674 s 对 12.321/13.384 s，产物逐字节一致（DEL 587,806,929 B md5 `2c9834af64e6`，SQL 642,102,879 B md5 `503ae9219565`）；差别在连接状态：旧二进制一次采样抓到 `[3 2:270=Sleep:0 268=Query:6 267=Sleep:6]`（含一个读完但仍打开的分片），新二进制全部采样里再没有"已完成但仍打开"的连接。输出设备变慢（网络挂载、写回风暴）时这个等待会被拉长到秒级，而云 LB/NAT 的空闲 socket 回收正打在这个窗口上，因此价值在云链路而不是本地跑分。测试：新增 `internal/export/exporter_shard_test.go`（2 用例、8 文件）——`TestExportShard_ReleasesConnectionBeforePipeDrain` 起一个**故意没有消费者**的 `shardPipe`，断言假连接已关闭而 `exportShard` 仍未返回（100 ms 内不得返回），随后开始消费并断言 `rows=3`、`err=nil`、输出恰 3 行；把改动前的两个 `defer` 顺序还原回去，该用例必然在 5.00 s 超时失败并报 `exportShard still held its connection while waiting on the shard pipe`（已实测验证，非推断）；`TestExportShard_EmptyShardReleasesConnectionAndClosesPipe` 用 0 行分片断言导出返回时连接已释放、队列已关闭且不含任何块。失败路径统一 `p.terminate()`，不留挂死的 goroutine。文档：7.2 流程步骤 4 补一行并新增"分片读完即放连接，不等队列排空"整段、8.2 可靠性表新增"并行导出分片释放"一行、十一节测试资产计数（33 个 `_test.go` / 174 `Test*`，`internal/export` 8 文件 64 用例）、九节结构与交付物计数同步，README 修复记录同步。测试资产升至 33 文件 / 174 `Test*`（含 1 个 `TestMain`）/ 5 `Fuzz*` / 12 `Benchmark*`。`gofmt -l`、`go vet ./...` 干净，`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 50 | 参数审计落地：三处帮助文本改述实际语义，并行导出对忽略的管道选项出声，删除死常量 | 用户要求对全部命令行参数做一次"有用/重复/多余"的排查。结论是没有重复项，需要动的是三类：① **纯兼容项**只有 `--single-row-insert`（`legacySingleRowInsert` 由 cobra 写入后全仓库无人读取，`validateInsertLayout` 与 `validateSQLOnlyOptions` 用的都是 `Changed()` 判存在性），属有意保留，不改行为；② **帮助文本与实际语义不符**——`--statement-size` 在默认单行布局下已经不是攒批上限、只退化成告警阈值，但 help 仍写 "Max bytes per INSERT statement"，`--batch-size`/`--pipeline-buffer` 虽标了 "(single-threaded export)" 却没说明与 `--threads>1` 同时给出时会被忽略。现分别改为 "Max bytes per INSERT statement (only bounds merged statements: without --extended-insert a longer row just warns)"、"Rows per reader->writer hand-off batch (single-threaded export; --threads>1 ignores it and says so)"、"Hand-off queue depth, in batches (single-threaded export; --threads>1 ignores it and says so)"，并登记进 `docs/cli-oracle.txt` 头注（该文件是冻结基线，导出侧的有意差异由六条增至九条）；③ **参数校验管不到的"运行时静默无效"**：格式类越界全部由 `validateSQLOnlyOptions`/`validateDELOnlyOptions`/`validateParquetBinaryOptions` 以 `Changed()` 前置拒绝并退出 2，但 `exportParallel` 根本不读这两个字段（只有 `exportSingle` 读，exporter.go:313/317），前瞻另由 64 MiB ÷ 分片数决定（shard_stream.go:70-83），于是 `--threads 8 --batch-size 50000` 静默等价于默认值。现在 `exportParallel` 在确认按分片并行执行之后（`GenerateShardRanges` 的分片数判定之后，避免降级为单线程时误报）经 `pipelineOptionsIgnoredByParallel()` 判定"用户确实改动过任一个值"，记一条 `info`：`parallel export ignores pipeline options batch_size=5000 pipeline_buffer=10 look_ahead_per_shard="16.0 MiB" applies_to="single-threaded export only"`。判定基准是"与出厂默认值不等"而非"是否传了该 flag"，因此库调用方与常见运行（不动这两个值）完全静默。实测（`bench_narrow` 100 万行，`--threads 4`）：`--batch-size 5000` 那次日志出现该记录且 `look_ahead_per_shard=16.0 MiB`（4 分片 × 64 块 × 256 KiB），全程 `duration=2.396s rows=1000000 bytes=141218462`；不带 `--batch-size` 的并行导出与该记录出现次数为 0；`--threads 1 --batch-size 5000`（真正生效的路径）同样为 0。另删除死常量 `config.DefaultMaxAllowedPacketCap`（全仓库仅定义处一处，`--statement-size` 的 268435456 上界在 `cmd/validate.go:47` 独立成立，导入侧的包上限另有 `internal/import/batchcap.go` 的 `defaultMaxAllowedPacket`=4 MiB 与 `packetSafetyMargin`，与此常量无关）。测试：新增 `internal/export/shard_stream_test.go:TestParallelExportPipelineOptionNotice`（默认值不报警、`--batch-size` 改动报警、`--pipeline-buffer 0` 这一合法取值也算改动）与 `test/integration/live_mysql_test.go:TestLive_ParallelExportReportsIgnoredPipelineOptions`（三段式：并行+改动必须出现、并行+默认必须静默、单线程+改动不得报警，走 `--messages` 文件断言）；`live_env.exportOptions` 增加 `batchSize` 字段用于驱动该场景。测试资产 33 文件 / 176 `Test*`（含 1 个 `TestMain`）/ 5 `Fuzz*` / 12 `Benchmark*`（`internal/export` 8 文件 65、live 7）。文档：README 导出标志表两行与 `--statement-size` 行、流水线生效范围段，requirements.md 5.3 记录来源表、7.2 队列深度段、2.1.3 两行、11.1 计数同步，`docs/cli-oracle.txt` 头注登记三处 reword。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 51 | 导入侧新增 `--statement-size`，把攒批字节预算变成用户可以主动压低的上限 | 用户要求"把该参数实现为 imp sql 类的功能"——`--statement-size` 此前只在导出侧有语义，导入侧每批的字节预算完全由目标库 `max_allowed_packet` 决定，用户无法主动把批次切小。实现：`config.ImportConfig` 新增 `StatementSize`（`internal/config/config.go`），`import` 注册同名选项（默认 `0`），`internal/import/batchcap.go` 新增 `applyStatementCap(packetBudget, statementSize)` 并接在 `batchBudget()` 上，与 `max_allowed_packet × 9/10` 预算取**较小值**——只压低、不抬高（抬高等于让一批在服务端中途触发 error 1153，而那正是该预算要防的事）；默认 `0` 不改任何现有行为；给定值低于 `batchByteLimit` 的 64 KiB 下限时照给定值执行（显式的数字是有意选择）；单行大于上限仍独立成批（`shouldFlush` 在追加之后判定，一行无法再拆）。压低点上选在 `batchBudget()` 而不是某条路径，因此单线程与并行导入、DEL/SQL/Parquet 三种输入一并生效。参数校验刻意分列：新增 `validateImportStatementSize`（接受 `0`，`<0` 或 `>268435456` 为用法错误退出 2），导出侧 `validateStatementSize` 保持拒绝 `0`——同一个名字在两侧含义不同，不能共用一个校验。日志：`import batch byte cap resolved` 增 `statement_size` 与 `limiting_factor`（`max_allowed_packet` / `statement-size`）两个字段以说明预算由谁定的，`import config` 回显增 `statement_size`。实测（本地 MariaDB，`max_allowed_packet=134217728` → 生效预算 120,795,955 B；2048 行 × 约 200 B 载荷；`--batch-size 20480`/`--commit-count 20480` 使行数与提交都不触顶；条数取目标库 `SHOW GLOBAL STATUS LIKE 'Com_insert'` 的差值而非日志自述）：不加选项 **1** 条语句，`--statement-size 65536` **7** 条，`--threads 4` 并行同样 **7** 条，SQL 与 Parquet 文件同样 **1→7** 条，`--statement-size 1024`（低于 64 KiB 下限）**342** 条，`--statement-size 268435456`（高于预算）**1** 条且记 `limiting_factor=max_allowed_packet`；各次落库行数与 `CRC32` 校验和逐组一致（2048 / 4397778945034）。测试：`internal/import/batchcap_test.go` 新增 `TestApplyStatementCap`（0/负值/等于预算/高于预算/真压低/低于下限六种取值）与 `TestBatchBudgetAppliesStatementCapWithoutConnection`（`imp.db == nil` 的保守分支同样压低）；`cmd/cli_matrix_test.go:TestCLIImportStatementSize` 钉住两侧相反的合法性（导入接受 `0`/`1`/`65536`/`268435456` 且原样落到 config，`-1` 与 `268435457` 为用法错误 2，导出 `--statement-size 0` 仍是用法错误 2）；`cmd/validate_fuzz_test.go` 新增 `FuzzValidateImportStatementSize`；`test/integration/live_mysql_test.go:TestLive_ImportStatementSizeCapsStatements` 用 `Com_insert` 断言默认 1 条、加上限后约 7 条，并复核两表落库行与 `limiting_factor` 的切换；`importOptions` 增加 `statementSize` 字段驱动该场景。测试资产 33 文件 / 180 `Test*`（含 1 个 `TestMain`）/ 6 `Fuzz*` / 12 `Benchmark*`（`internal/import` 41、`cmd` 11、live 8）。文档：README 导入标志表新增一行，"导入攒批的两道硬上限"一节改名"导入攒批的硬上限与可选压低"并在末段补上第三道可选上限与上述实测，参数边界表拆成导出/导入两行，关联校验段澄清那串 SQL 专属限制只约束导出侧，项目结构注释更新；requirements.md 2.2.3 导入选项表、7.4 新增第 4) 项、5.3 记录来源表补上此前未登记的 `import batch byte cap resolved` 与 config 回显字段、11.1 总数与四个文件行、九节结构注释同步；`docs/cli-oracle.txt` 头注登记导入侧新增该选项（有意差异 4→5 条）。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 52 | 配置回显与 `--help` 按 `--format` 过滤格式专属选项，判据收敛为一张登记表 | 用户要求"按照命令和不同的文本格式将选项的打印数据更让人使用"。缺陷实测：把 HEAD 二进制 `-f del`/`-f sql`/`-f parquet` 三条 `export config` 去掉 `format=` 后逐字节比对，三者完全相同——同一套 26 个字段里 `-f del` 带着根本不读的 `statement_size`/`no_create`/`no_data`/`extended_insert`、`-f sql` 带着根本不读的 `char_del`/`col_del`/`row_del`/`escape`/`timestamp_fmt`/`no_col_names`、`-f parquet` 两组都带（而这三类组合本来就会被用法错误拒绝），同时 DEL 写入器一直在读的 `date_fmt`/`no_escape`/`no_char_del` 三项**从来没打印过**，`hex_blob` 打的还是 `--hex-blob` 原始标志位而非生效值（`--default-charset gbk` 自动开启时回显仍是 `false`，与紧随其后的 `hex-blob auto-enabled by charset` 自相矛盾）；`--help` 也不说某个选项只对某种格式有效。实现：新增 `internal/config/formatscope.go` 作为唯一的格式适用范围登记表（`ExportOptionScopes`/`ImportOptionScopes`，每项带 `Hint` 与 `Reject`，暴露 `AppliesTo`/`Violation`），三个消费者同源：① `cmd/validate.go` 删去手写的 `validateSQLOnlyOptions`/`validateDELOnlyOptions`+`delOnlyFlags`/`validateParquetBinaryOptions`，合并为 `validateOptionScopes`，登记顺序保持原三函数的检查顺序，**拒绝文案与首条命中逐字不变**；② 新增 `cmd/option_scope.go:annotateOptionScopes`，在 `registerExportFlags`/`registerImportFlags` 末尾把 `Hint` 追加进各 flag 的帮助文本（幂等——命令树是包级的，init 与测试走同一注册路径）；③ 回显从 `NewExporter`/`NewImporter` 的内联调用抽到新增的 `internal/export/configlog.go`/`internal/import/configlog.go`，按 `AppliesTo` 过滤并按「连接 → 输入输出 →（导入另有行流控制）→ 并行与流水线 → 字符集 → 该格式专属布局 → 日志」排列，导出侧 `hex_blob` 改用新抽出的 `effectiveHexBlob()`（原单线程与并行分片各写一遍的重复表达式一并收敛）。回显仍在建连接之前、仍是一条命令一条记录，`--messages` 的"一行一条记录"布局与 5.3 的"同一事件只输出一行"不受影响。实测（本地 MariaDB，`SELECT * FROM allnull` 成功导出）：`-f del` 与 `-f sql` 的回显分别 25/20 个字段，`comm` 差集恰为只在 del 出现的 `char_del col_del date_fmt escape no_char_del no_col_names no_escape row_del timestamp_fmt` 九项与只在 sql 出现的 `extended_insert no_create no_data statement_size` 四项；`export -f del/sql/parquet` 为 25/20/15、`import` 为 29/20/20；`--default-charset gbk` 时回显 `hex_blob=true`；`--format sql --char-del '!'` 仍报 `--char-del can only be used with --format del` 退出 2；`export --help` 16 行、`import --help` 9 行带范围标记；两次成功运行的 `-m` 文件各 3 行且每行都是日志记录。测试：新增 `internal/config/formatscope_test.go`（4 用例）、`cmd/option_scope_test.go`（3 用例）、`internal/export/configlog_test.go`（4 用例）、`internal/import/configlog_test.go`（3 用例），测试资产 33→37 文件、180→194 `Test*`（`internal/config` 2→6、`cmd` 11→14、`internal/export` 65→69、`internal/import` 41→44），`Fuzz*` 6、`Benchmark*` 12 不变；既有钉拒绝文案的 `TestCLIDELFlagFormatValidation`/`TestCLIParquetHexBlobRejected`/`TestCLIDELFlagRejectedExitCode` 未改一行即全通过，作为"文案与顺序不变"的证据。文档：README 日志段改写为按格式过滤并给出实测字段数、"关联校验"段补三处同源口径、项目结构增 4 个文件、修复记录新增一行；requirements.md 2.2.2/2.2.3 两段格式适用性补登记表口径、5.3 记录来源表的 config 行重写、九节结构与测试文件计数、11.1 总数与四个文件行、十二节交付物计数同步；`docs/cli-oracle.txt` 头注登记范围标记带来的帮助文本差异（有意差异 export 9→21 行、import 5→14 行），并把头注里已过时的"Five intentional deltas"改为不带计数的表述。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 53 | `go.mod` 声明升至 `go 1.26.10` 并重新打包 | 用户要求"gomod 版本升级至 1.26.10，重新打包"。**源码零改动**：只有 `go.mod` 的 `go` 指令由 `1.26.9` 改为 `1.26.10`，本文 1.3 技术栈行与九节项目结构的 `go.mod` 注释同步。沙箱 GOROOT 仍是 go1.26.9，靠 `GOTOOLCHAIN=auto` 自动取到 go1.26.10 后编译（仓库内 `go version` 实测输出 `go version go1.26.10 linux/amd64`），所以这一行记录的是"工具链小版本升级后全量验证通过"，不是行为变更。`docs/cli-oracle.txt` 头注登记一条**环境性有意差异**：`## version` 基线冻结的是 `Go version: go1.26.9`，此后每个构建都会输出 `go1.26.10`，比对 `version` 输出时该行预期不同；5.3 节带日期的样例日志（`go_version=go1.26.9`）与本表第 34 行按历史快照保留、不改写。附带口径提示（本次未改）：`Dockerfile` 用的是浮动次版本 `golang:1.26-alpine`，镜像内 Go 若早于 1.26.10 会触发 `GOTOOLCHAIN` 联网下载，需要固定版本时再改。验证（全部在 go1.26.10 下重跑）：`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过、真实库 `go test -race -count=1 ./test/integration` 86 用例全通过且无 SKIP/FAIL；`make build-all`（`CGO_ENABLED=0`）产出 linux/amd64 与 windows/amd64 静态二进制，`version` 子命令实测输出 `Go version: go1.26.10`；解包发布用的 linux 产物对 3000 行（含 `VARBINARY`）表做三格式往返，`del`/`sql`/`parquet` 目标表的 `SUM(CRC32(...))` 与源表同为 6426483039391，`--threads 4` 并行导出同样通过。因工作区未提交，批次按约定命名 `imex-0.1.0-a716deb-dirty`（src + linux-amd64 + windows-amd64 + `bin-SHA256SUMS.txt`，三个文件 `sha256sum -c` 全 OK） |
| 54 | 把"重复主键 × 导入并发"的锁冲突固化为回归用例 | 用户要求"测试用例可以固化下"，前因是两条实测结论：① 单线程导入重复主键恒为 `1062`/`Duplicate entry`，一个会话不会自锁；② 并行导入没有按键分派——`internal/import/importer.go` 的并行路径是单读取 goroutine 把批次写入带缓冲 channel、N 个 worker 各自 `sql.Open` + `SetMaxOpenConns(1)` 先到先得地领取，谁先拿到哪个批次取决于调度，于是"相邻批次含同一批键且顺序相反"会让两个会话以相反次序申请同一组行锁，构成锁序反转，服务端可能报 `1213`（死锁）或 `1205`（锁等待超时）。而 `internal/common/dberr.go` 刻意不把 1213/1205 归入可重试错误（避免盲目重放把重复数据写进去），所以死锁直接令该批次失败并终止导入——这条"不会重放后谎报成功"依赖用例守卫。实现：新增 `test/integration/lock_contention_test.go`（3 个用例，真实库 + `TestMain` 编译出的真实二进制，沿用 `IMEX_TEST_DSN` 门控与 `option_matrix_test.go` 的 `cliEnv` 复用方式）。fixture 建 `(seq BIGINT PRIMARY KEY, id BIGINT, name VARCHAR(64))`，用 `digits(0..9)` 四次交叉拼出足量 `seq`，令 `id = ((seq-1) DIV batchRows DIV 2) * batchRows + 1 + offset`——每个键在文件里恰好出现两次，`offset` 按块序号奇偶取 `s.seq-1 MOD B` 或其镜像，`invert=true` 即"奇数块降序"，正好制造相邻批次键集重叠且顺序相反；两份数据文件由同一条 `-q "SELECT id, name FROM src ORDER BY seq"` 分别以 `-f sql` 与 `-f del` 导出，保证两种格式走同一份键序。① `TestLive_DuplicateKeysSingleThread`：单线程 `--mode insert` 断言退出码非 0、输出含 `1062` 或 `Duplicate entry`、且**不得**出现 `1213`/`Deadlock`，落库 `COUNT(*) == COUNT(DISTINCT id)`；`--mode insert_update` 断言退出码为 0 且全部键落地（120 行 / 120 键）。② `TestLive_ParallelImportKeyOrderInversion`（sql、del 两个子用例，`--mode insert_update --threads 4 --batch-size 40 --pipeline-buffer 0`）：**不断言必然死锁**——是否成环由服务端调度决定，断言的是一组与退出码无关的不变量：任何情况下不落重复行（`COUNT(*) == COUNT(DISTINCT id)`）、落库行数不超过文件内键数；成功时全部键必须在场，失败时输出只能出现 `1213`/`Deadlock` 或 `1205`/`Lock wait timeout` 这类服务端锁错误；服务端死锁计数增量作为观测值打印（变量名 MySQL 为 `Innodb_row_lock_deadlocks`、MariaDB 为 `Innodb_deadlocks`，两者都取不到时自动降级为只看报错文本）。③ `TestLive_ParallelImportConsistentKeyOrder`：同一键集、同样 4 线程、批次一律升序（`invert=false`），断言零死锁且 400 个键全部落地——把"按键有序 dispatch 可规避死锁"从口头结论升级为被测性质。实测（沙箱 MariaDB 10.6.23，4 核）：上一轮同一 fixture 下 sql 那次没成环（死锁增量 0、400 键全落），del 抓到 1 次死锁并 `import aborted`；本轮复跑 `-run` 分别取 `TestLive_Duplicate` 与 `TestLive_Parallel` 两个前缀 时 sql 与 del 两个子用例各抓到 1 次死锁、均以锁错误终止且落库 0 行（断言的是不变量而非具体调度结果，两种走向都通过）；三个用例耗时 0.07/0.08/0.07 s。文档：九节项目结构增该文件行、测试文件计数 集成 3→4 与全仓库 37→38，11.1 计数说明改 38 个 `_test.go`/196 个 `Test*` 并新增该文件行，十二节交付物计数 37→38。验证：`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 55 | 导出侧三项：`-q` 里的 schema 不再二次拼接、SQL 导出默认仅数据（`--create` 显式带回建表、`--no-create` 随之删除）、新增与 `-q` 互斥的 `--query-file` | 用户一次提了三点，第一点是缺陷，后两点是行为与易用性变更。① **缺陷：`--query` 带 schema 时建表语句被二次拼接**。复现（HEAD 9389a88 构建的二进制，本地 MariaDB，表 `imex_test.zzq_evi` 三行）：`export -q 'SELECT * FROM \`imex_test\`.\`zzq_evi\`' -f sql` 退出 1，报 `export failed: failed to get CREATE TABLE statement: Error 1146 (42S02): Table 'imex_test.imex_test.zzq_evi' doesn't exist`。根因在 `internal/export/sql_parser.go`：`ExtractTableName`/`SimpleTableName` 返回的是 `sqlparser.String(TableName)`，即**已带反引号的 `` `db`.`tbl` ``**，而 `getCreateTableStatement` 把它当裸名再包一层反引号、并前置连接自己的 `--database`，服务端于是把整串当作默认库里的单个标识符。修复口径是"产物永远只引用裸表名，schema 只在建表查询这一处用"：两处改为断言 `sqlparser.TableName` 后取 `tn.Name.String()`（裸名，不带反引号），新增 `ExtractTableRef(query) (schema, name string, err error)` 返回 `tn.Qualifier.String()` 与裸名，`getCreateTableStatement` 改签名为 `(schema, tableName string)`，只在 `schema != ""` 时加 `` `schema`. `` 前缀；单线程预取与并行分片预取两条路径都换用它。裸名同时供 SQL 写出的 `CREATE`/`INSERT`/`LOCK TABLES` 头与分片键探测使用（`shard.go` 的 `findPrimaryKey`/`findUniqueKey` 本就按 `TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?` 过滤，带前缀反而匹配不上主键）。② **SQL 导出默认不再写 `CREATE TABLE`**（默认仅数据，属破坏性变更，README 与本文同步标注）：`config.ExportConfig` 新增 `Create bool` 与 `WriteCreate() bool`（`Create && !NoCreate`），注册 `--create`（默认 `false`，帮助文本 "Output a CREATE TABLE statement (SQL export is data-only by default)"），`--no-create` 一并删除（注册、`ExportConfig.NoCreate` 字段、格式登记项、回显键四处同时消失；`WriteCreate()` 在失去反向开关后就只剩 `return c.Create`，属无意义间接层，故也删掉，写出侧与回显直接读 `Create`）；`--create` 登记进 `internal/config/formatscope.go` 的 `sqlOnly` 组（SQL 专属组仍是五项——`create`/`no-data`/`statement-size`/`single-row-insert`/`extended-insert`，一增一删，帮助文本里的范围标记行数与既有格式越界口径不变），因此 `--create -f del` 与既有的格式越界同口径拒绝；配置回显由打 `no_create` 改为打**生效值** `create`。导入侧 `--mode create` 依赖建表语句，故其文档示例前补了一条 `export ... --create`。③ **新增 `--query-file`，与 `-q` 互斥**：`ExportConfig` 新增 `QueryFile`，`cmd/export.go:resolveExportQuery` 以 `Flags().Changed()` 判三态——两者都给为用法错误 `--query and --query-file are mutually exclusive`；只给文件则读文件、`TrimSpace` 后去掉行尾分号再 `TrimSpace`，空内容报 `--query-file "..." contains no SQL statement`，读不到报 `cannot read --query-file "..."`；两者都不给报 `--query or --query-file is required`（均为退出 2，与必填项同口径）。回显新增 `query_file` 字段，仅在真正走了文件时出现，避免默认运行多一个无意义字段。实测（同一台沙箱 MariaDB，HEAD 与工作区两个二进制跑同一条命令）：HEAD 退出 1 / 1146，工作区默认退出 0 且产物 243 B、`CREATE TABLE` 行数 0、`INSERT INTO` 行数 3；`--create` 退出 0 产物 472 B，首块为 `CREATE TABLE \`zzq_evi\``，全文件 `imex_test` 出现 **0** 次；删掉源表后用 `import -f sql --mode create` 回灌该文件，退出 0、`inserted=3`、`SELECT COUNT(*)` = 3，证明"带 schema 的查询 + `--create`"这条最容易二次拼接的路径已闭合；`--no-create` 无论单独给还是跟着 `--create` 给都是 `unknown flag: --no-create` 且退出 2；`--create -f del` 退出 2 且文案点名 `--format sql`；`--query-file` 用含首尾空行与结尾分号的语料导出成功，INFO 回显同时出现 `query_file=<tmp>/r3_q.sql` 与 `create=true`，四条负例（与 `-q` 同时给、两者都不给、空文件、文件不存在）全部退出 2；不读文件的默认运行回显里 `query_file` 出现 0 次。测试：`internal/export/sql_parser_test.go` 把 `TestSQLParser_SimpleTableName` 里限定名用例的期望从 `db.users` 改为 `users`（改的就是缺陷本身，注释写明原因），新增 `TestSQLParser_ExtractTableRef`（裸名 / 限定名 / `AS x` 别名三种取值，以及 `UPDATE`、`DELETE`、`INSERT` 三种非 SELECT 一律拒绝）；`internal/export/configlog_test.go` 的 SQL 布局键由 `no_create` 换成 `create`；`internal/config/formatscope_test.go` 的 `AppliesTo` 矩阵把两处 `no-create` 换成 `create`（并补一条 `--create` 对 `del` 为 false）、"首条命中"用例改用 `changedOf("col-del", "create")` 断言 `--create can only be used with --format sql`、导入侧"未登记即恒适用"一例换成 `--mode`；`internal/export/exporter_stream_test.go` 的 `streamTestExporter` 参数由 `noCreate` 反转为 `withCreate`（全部用例走数据-only 分支），`TestWriteShardStream_SQLNoCreateStillLocks` 更名 `TestWriteShardStream_DataOnlyStillLocks`——它守的仍是"没有建表语句也必须成对写 LOCK/UNLOCK"，`internal/export/sql_writer_test.go` 两个用例名里的 `NoCreate` 也改为 `WithoutCreateOrData`/`WithoutCreateSkipsCreateTable`（写出器的布尔参数没动，只是不再用一个已不存在的 flag 给用例命名）；`test/integration/live_mysql_test.go` 的 `exportOptions` 增加 `create` 字段，结构往返用例改为显式 `--create`；`test/integration/option_matrix_test.go` 的 `TestOptionMatrixNoDataNoCreate` 改名 `TestOptionMatrixNoDataCreate` 并重写为四个子用例（默认仅数据可直接导入、`--create` 同时保留结构与数据、`--create --no-data` + `--mode create` 仅结构建表、默认仅数据 + `--mode create` 必须明确失败）；新增 `test/integration/export_cli_test.go`（4 个用例）把三项钉成真实二进制 + 真实库回归，其中 schema 限定用例的两个子用例各自 `CREATE TABLE ... LIKE` 出一张新目标表再逐行比对，避免共用目标表导致主键冲突。测试资产 38→39 个 `_test.go`、196→201 个 `Test*`（`internal/export` 用例 69→70，集成目录 4→5 个文件），`Fuzz*` 6、`Benchmark*` 12 不变。文档：README 导出标志表 `--query` 改述并新增 `--query-file` 与 `--create` 两行、**删掉 `--no-create` 行**并在 SQL 输出说明里写明它已移除（传入为未知选项退出 2），SQL 输出说明段重写为"默认仅数据 + schema 剥离 + `--mode create` 需配 `--create`"，"关联校验"段把 `--create` 并入 SQL 专属清单（不再列 `--no-create`）并写 `-q`/`--query-file` 互斥；requirements.md 2.2.2 导出选项与 SQL 修饰符表（删 `--no-create` 行）、2.2.1 格式适用性段的点名示例、DB2 映射表 `MODIFIED BY NOCREATE` 行改为"默认行为 + `--create` 才附带"、`sql` 格式说明、单表限制一节新增"表名里的 schema 一律剥离"段、导出流程两行、九节结构与计数、11.1 计数与 `internal/export`/`export_cli_test.go`/`option_matrix_test.go` 三行、十二节交付物计数 38→39；`docs/cli-oracle.txt` 的 `## export` 段**整体重新基线**为当前工作区的帮助文本（实测与 `export --help` 逐字节相同，导出侧不再有待对差异），头注记下重新基线日期并说明 `## import`/`## root`/`## version` 三段仍是 4934dd2 快照、SQL 专属组仍是五项（`--create` 顶掉 `--no-create`，范围标记行数不变）。验证：`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过，真实库 `go test -race -count=1 ./test/integration` 全通过（新增与改写的用例逐条 PASS，含四个中文命名的子用例） |
| 56 | 项目改名 `gomydump` → `imex`（ImEx = Import + Export），模块名、二进制名、环境变量、帮助与日志回显、文档全线统一 | 用户要求把工程名统一改为 imex，覆盖模块名、备注、帮助回显、文档。做法是先做标识符普查，再快照源码树（`<tmp>/imex_prefix_backup.tgz`，304,754 B，排除 `.git`、`bin`、`dist`），然后对全部 `.go`、`.md`、`.txt`、`Makefile`、`Dockerfile`、`go.mod` 施加两条规则 `s/gomydump/imex/g` 与 `s/GOMYDUMP/IMEX/g`，最后逐层复核。改动落点：`go.mod` 模块路径 `github.com/gomydump` → `github.com/imex`（因此 ldflags 注入路径 `cmd.Version`/`cmd.BuildTime`/`cmd.GitCommit` 与全部 import 语句同步）；`cmd/root.go` 的 `Use`、`Long`、版本行与五个环境变量名；`internal/common/password.go` 的 `EnvKey` 由 `GOMYDUMP_KEY` 改为 `IMEX_KEY`；`internal/common/log.go` 启动横幅 `imex starting`；`internal/common/sqltag.go:55` 归因注释 `/* imex:<commit> */`；`Makefile` 的 `BINARY := imex` 与 `make test-live` 提示语；`Dockerfile` 的 ldflags、`-o imex`、`COPY` 与 `ENTRYPOINT ["/app/imex"]`；集成测试的 DSN 环境变量 `IMEX_TEST_DSN` 与测试库 `imex_test`。**改名不是纯文本替换，两处必须实跑才能确认**：① 集成测试表前缀实际写作 `gmydump_it_`（少一个 o），两条规则都不命中，是真实套件失败输出暴露的，已改为 `imex_it_`；② `test/integration/option_matrix_test.go` 在本轮被环境回退成旧变体（`statementCapSlack` 标记计数从 3 归 0，并带回上一轮已删除的 `--no-create` 子用例），`git checkout --` 复原后重放了上一轮的 `TestOptionMatrixNoDataCreate` 四子用例改写与本轮改名。兼容性口径：归因注释是**只写不读**（`sqltag.go` 生成后拼进发给服务端的语句，导入侧解析器不反向识别它），所以旧产物里以 `/* gomydump:<commit> */` 开头的行仍按普通 SQL 注释忽略，**旧导出文件照常可导入**；本轮没有 flag、退出码、文件格式或 schema 变更，因此不留任何旧名兼容层。实测：`imex --help` 首行为 `imex is a table-level MySQL logical backup and restore tool.`、`Usage: imex [command]`，`Available Commands` 与 Global Flags 全部为 `imex`/`IMEX_*`；`imex version` 输出 `imex version 0.1.0`、`Git commit: 9389a88`、`Go version: go1.26.10`；`--log-level debug` 首条记录为 `INFO imex starting version=0.1.0 ... go_version=go1.26.10 command=export`；MariaDB general log 抓到服务端实际收到的 `/* imex:9389a88 */ SHOW CREATE TABLE ...` 与 `/* imex:9389a88 */ SELECT ...` 两条，证明归因注释按新名下发；代码与构建脚本（全部 `.go`、`Makefile`、`Dockerfile`、`go.mod`、`.yml`）对 `gomydump`、`GOMYDUMP`、`gmydump` 三者命中 **0**，文档里剩下的 4 处提及全部是本轮改名记录本身对旧名的引述（README 修复记录 1 行、requirements.md 本行、`cli-oracle.txt` 头注 2 行）。文档：README 的 H1 改为 `# imex` 并补一句名称由来；requirements.md 标题、1.1 名称说明、四节 DB2 映射标题、九节项目结构根目录名、十二节产物前缀 `imex-<version>-<commit>[-dirty]-…` 与 arm64 自行编译命令 `go build -trimpath -o imex .` 同步；**历史变更行与带日期的样例日志里的工具名一并改写**，即第 53 行的批次名等历史记录现在显示 `imex-…`，而该批次实际交付物当时仍是 `gomydump-…` 前缀——记录按当前名称统一，不代表当时的名字；`docs/cli-oracle.txt` 四段体内的命令词与 `IMEX_*` 环境变量随改名更新（体内其余内容零改动），`## export` 段改名后重新与 `imex export --help` 逐字节比对通过（`diff` 为空），并补登记此前头注未列出的 `## root` 两行差异（Features 条目现为 `Command log file (--messages)`、Global Flags 的 `--echo-sql-values` 文案与 import 段一致）。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七个包全通过（包路径均为 `github.com/imex/...`），真实库 `IMEX_TEST_DSN='...tcp(127.0.0.1:3306)/imex_test...' go test -count=1 -v ./test/integration` 全通过（104 条 PASS 含子用例、0 SKIP），测试资产数量不变（39 个 `_test.go`、201 个 `Test*` 含 1 个 `TestMain`、6 个 `Fuzz*`、12 个 `Benchmark*`）——本轮只改名，不增删用例。**本轮范围只到仓库内**：仓库外的约定文件（`<workspace>/AGENTS.md`、`<workspace>/prune-packages.sh`）、工作副本目录名 `<workspace>/gomydump` 与旧名产物 `bin/gomydump*`、`dist/gomydump-*.zip` 在同日的下一行（57）处理。 |
| 57 | 改名收尾：工作副本目录、仓库外约定文件与旧名产物一并统一 | 上一行只做到仓库内，本行把剩下的三层全部对齐。① **工作副本目录** `<workspace>/gomydump` → `<workspace>/imex`（同一文件系统内 `mv`，`.git` 随目录走；改动前后 `git status --porcelain` 都列出 **61** 行，HEAD 仍是 9389a88，工作树内容不变）。代价是平台挂载账本 `<workspace>/.qoder/managed-resources/session-resources.json` 里 **18** 条 `mount_path` 仍指 `<workspace>/gomydump/dist/*`（实测改名前 12:24 就按这些路径重新落回过 20 个旧名包文件，`dist/` 里那些 `gomydump-*` 就是这么来的），旧路径因此可能被再次创建；AGENTS.md 顶部已写明届时以 `<workspace>/imex` 为唯一工作副本、旧目录下只会有重新落回的产物而没有源码，那些包也不在保留脚本的 `<workspace>/work` 额度内、要手工删。当前 `<workspace>/gomydump` 已不存在（`ls -d` 报 No such file directory）。② **仓库内旧名产物删除**：`bin/gomydump`、`bin/gomydump-linux-amd64`、`bin/gomydump-windows-amd64.exe`（约 65 MB）与 `dist/gomydump-*`（20 个文件、约 111 MB，全是旧前缀，`dist/` 现为空目录）；`bin/` 只剩 `make build` 从新路径重出的 `imex`（ldflags 注入 `github.com/imex/cmd.Version` 等，`./bin/imex version` 输出 `imex version 0.1.0` / `Git commit: 9389a88`）。两目录都在 `.gitignore`（`bin/`、`dist/`）内，删除不触碰仓库内容。③ **`<workspace>/AGENTS.md`**：标题改为「工作区约定（imex）」，顶部写明改名范围与「若平台把旧路径 `<workspace>/gomydump` 落回来以 `<workspace>/imex` 为准」的处置；第 3 条源码包命令改为 `tar -czf imex-<version>-<short-hash>-src.tar.gz --exclude=.git --exclude=bin --exclude=dist --exclude=vendor -C /data imex`，`--transform` 由"必需"降级为"在旧路径上打包时的补偿手段"并保留写法；第 5 条 arm64 临时构建输出名改为 `bin/imex-linux-arm64`；第 6 条安全边界改为「以 `imex-` 开头**外加改名前遗留的 `gomydump-` 前缀」并点名脚本里的 `PREFIXES`。④ **`<workspace>/prune-packages.sh`**：头注示例文件名改 `imex-` 并新增前缀段；新增 `PREFIXES=${PREFIXES:-"imex- gomydump-"}`；`key_of()` 从硬编码 `${base#gomydump-}` 改为遍历前缀剥离，新增 `matches_prefix()`，两处目录扫描由 `for f in gomydump-*` 改为 `for f in *` + 前缀过滤——顺带修掉一个原有边界：状态文件 `last-src-package-date` 此前靠"不匹配前缀 glob"侥幸排除，现在由显式前缀判定排除；`REPO` 默认值随目录改为 `<workspace>/imex`。验证：`bash -n` 通过；`--dry-run` 在 37 项上归出 10 个批次（保留 3、删 7，与改名前行为一致）；实跑释放 **86,451,810 B**（84,425 KiB），目录由 37 项 / 129 MB 收敛到 15 项 / 47 MB；另用合成集验证新前缀归组（`imex-<HEAD>-src.tar.gz` 与 `imex-<HEAD>-bin-SHA256SUMS.txt` 归为一批且标注「当前 HEAD（占额度）」）。之所以保留旧前缀：只认 `imex-` 的话，改名前那 34 个文件（约 129 MB）永远清不掉。⑤ **仍留 12 个 `gomydump-*` 文件**，即保留额度内的 3 个改名前批次（`0.1.0-9389a88`、`0.1.0-a716deb`、`0.1.0-a716deb-dirty`）——**故意既不改名也不删**：这些包里的二进制是改名前构建的，`--help` 打印的仍是 `gomydump`，把文件名换成 `imex-` 等于给旧内容挂新名字；按 `KEEP_N`/`KEEP_DAYS` 规则它们会自然退役。⑥ 期间 `test/integration/option_matrix_test.go` 被环境**第二次**回退，这次退回更早的祖先版本（与 HEAD 差 43 增 / 196 删，`statementCapSlack` 标记 3→0，带回已删除的 `--no-create` 子用例以及 `GOMYDUMP_TEST_DSN`、`github.com/gomydump` 导入路径），处置仍是 `git checkout --` 复原 → 重放改名 sed → 重放 `TestOptionMatrixNoDataCreate` 四子用例改写；因此约定死的检查：任何暂存或打包之前 `grep -c statementCapSlack test/integration/option_matrix_test.go` 必须打印 **3**。⑦ 目录改名后的全量验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过；真实库 `IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' CGO_ENABLED=1 go test -race -count=1 ./test/integration` 通过（5.014 s），另跑一轮 `-v` 得 **104** 条 PASS（含子用例）、**0** SKIP，输出里 `gomydump_it_`/`gmydump_it_` 命中 **0**（MariaDB 中途再次掉线，按 `mysqld --defaults-file=<tmp>/mysqld/my.cnf` 拉起后 `ping` 与 `ss -ltn` 确认 3306 在听）；源码包顶层目录实测 **99** 个成员全在 `imex/` 下、`gomydump/` 下 **0** 个；`/data` 全树扫描旧名，只剩三处文档引述（README 修复记录 1 行、requirements.md 第 56/57 行、`cli-oracle.txt` 头注 2 行）。⑧ **另外两份仓库外文档一并统一**：早先交付的参数审计报告 `<workspace>/audit_report.md`（8 行旧名 → 0，标题改为「imex 命令行参数只读审计报告」，环境变量同步为 `IMEX_ERROR_STACK`/`IMEX_ECHO_SQL_VALUES`/`IMEX_QUIET`/`IMEX_NO_SQL_TAG`/`IMEX_LOG_FORMAT`）与用户挂载进来的早期需求文档快照 `<workspace>/mysql-backup-tool/docs/requirements.md`（675 行，43 行旧名 → 0，标题改为「imex 技术需求文档」；它是仓库内 1499 行版本的祖先快照，内容早已落后，只统一名称、不回填正文）。两份改前都留了备份：`<tmp>/audit_report.md.bak`、`<tmp>/mbt_req.md.bak`。**两类刻意不动**：`<workspace>/file_*`（32 个文件、105 MB）与 `<workspace>/.qoder/managed-resources/session-resources.json` 是平台侧的交付与挂载账本，记的就是当初以旧名投递出去的包，改名等于篡改投递记录；`<workspace>/work` 里保留额度内的 12 个 `gomydump-*` 文件（3 个改名前批次）内含的二进制确实会打印 `gomydump`，同理不改名。README 的改名条目同步指向本行。 |
| 58 | 导出选项 `--no-replace` 改名为 `--no-overwrite` | 用户建议。① **改名理由**：该选项唯一的语义是"输出文件已存在就拒绝覆盖"，与 REPLACE 没有任何关系，而原名同时撞上了 `--format sql` 生成的 REPLACE 语句与 `import --mode replace`，三个 replace 互相干扰。落点共十处：`cmd/export_flags.go` 注册名、`internal/config/config.go` 字段 `NoReplace` → `NoOverwrite`、`internal/export/exporter.go` 判定与报错文案、`internal/export/configlog.go` 回显项（命令行名 `no-overwrite`、日志键 `no_replace` → `no_overwrite`）、`internal/export/configlog_test.go` 的 `commonKeys` 清单、`test/integration/option_matrix_test.go`（用例名 `TestOptionMatrixNoReplace` → `TestOptionMatrixNoOverwrite`、表前缀参数 `noreplace` → `nooverwrite`、argv 与断言文案、文件头注释的覆盖清单）、README 导出标志表、requirements.md 本表之外的 2.2.2 选项表 / DB2 `MODIFIED BY REPLACE` 映射行 / 7.2 失败与中断语义 / 7.6 导出覆盖语义 / 11.1 测试清单、`docs/cli-oracle.txt`。② **不留别名**（与 `--no-create` 删除时同口径，与仍保留的 `--single-row-insert` 不同）：实测 `imex export ... --no-replace` 报 `unknown flag: --no-replace` 且退出 **2**；DB2 侧 `MODIFIED BY REPLACE` 子句名不变，映射行同时标注两侧现名与改名日期。③ **行为逐条实测**（本地 MariaDB，`imex_test.zz_ow` 两行，工作区二进制）：带 `--no-overwrite` 命中已存在文件 → 退出 **1**、哨兵文件内容原样存活、报错 `output file <tmp>/ow.del already exists, remove --no-overwrite to overwrite`；去掉该选项同命令 → 退出 **0**、文件被两行数据覆盖；配置回显两种情况分别打印 `no_overwrite=true` 与 `no_overwrite=false`；另证实"创建前失败不误删"这条既有语义：查询报错（表不存在）在 `os.Create` 之前，既有文件同样存活。④ **oracle**：`## export` 段按改名后的 `imex export --help` 重新基线，`diff` 为空；`--no-overwrite` 与 `--no-replace` 同为 `n` 开头且排在 `--no-hex-blob` 与 `-o, --output` 之间，列宽未变（最长名仍是 `--socket-*-timeout`），帮助文案一字未改；头注新增 Re-baselined note 2 登记改名与"旧名成为未知选项"的后果。⑤ **旧名清零复核**：代码与构建脚本（全部 `.go`、`Makefile`、`Dockerfile`、`go.mod`）对 `no-replace`、`NoReplace`、`no_replace` 命中 **0**；文档里仍出现旧名的只有三类——历史记录本身（README 修复记录第 799 行、requirements.md 第 42 行这两条带日期的条目按"记录不改写历史"的既有口径保留原名）、本行的改名说明、以及 2.2.2 / DB2 映射 / README 标志表里的"原名 `--no-replace`"标注。⑥ **验证**：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七个包全通过（`github.com/imex/cmd` 37.061 s）；真实库 `IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' CGO_ENABLED=1 go test -race -count=1 -v ./test/integration` 全通过，**104** 条 PASS（37 顶层 + 67 子用例）、**0** SKIP、**0** FAIL，其中 `TestOptionMatrixNoOverwrite/threads=1` 与 `/threads=4` 逐条 PASS；套件运行前按死规矩复核 `grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**（本轮未再被回退）。测试资产规模不变：39 个 `_test.go`、`^func Test` 命中 202（含 1 个 `TestMain`，即 11.1 口径的 201 个用例）、6 个 `Fuzz*`、12 个 `Benchmark*`——本轮只改名，不增删用例。⑦ 本轮改动未提交、未打包。 |
| 59 | 文档与测试注释脱敏：沙箱绝对路径与本地测试库凭据不再入库 | 仓库转为公开后的收尾。① **凭据**：真实测试库账号写死的 DSN（用户名与口令两个明文词）共 4 处（`docs/requirements.md` 变更记录 57、58 两行，`test/integration/option_matrix_test.go` 与 `test/integration/lock_contention_test.go` 的文件头运行说明注释）统一改为 `<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true`；测试文件只动注释、未动任何代码路径。`README.md:59` 与 `live_mysql_test.go:6` 原本就写的是 `root:secret`/`root:pass` 占位串，保持不动。② **路径**：`docs/requirements.md` 内 20 处工作区绝对路径与 9 处临时目录绝对路径，分别改为 `<workspace>/` 与 `<tmp>/` 前缀，并在「十四、变更记录」标题下加一行脱敏说明，声明占位符含义与「历史记录原文保留在提交历史中」。③ **口径**：只脱敏位置与凭据，不改写记录内容——被引用的日志样例里的 `user=gd`、`Access denied for user 'gd'@'127.0.0.1'`、退出码、字节数、时长、用例计数全部逐字保留；单元测试里当作参数占位的 `<tmp>/out.sql`、`<tmp>/a.del` 等 Go 字符串字面量（`cmd/cli_matrix_test.go`、`internal/common/log_test.go`，均为不落盘的假路径）不属于泄露，同样不动。④ **顺带修掉一个既有 Markdown 缺陷**：变更记录某行里 `-run` 的双用例正则选择符写成了裸竖线（`TestLive_Duplicate` 与 `TestLive_Parallel` 的交替，该行未转义竖线因此为 5 个），渲染时把表格单元切断、多出一列，改为 `-run` 分别取 `TestLive_Duplicate` 与 `TestLive_Parallel` 两个前缀；复核脚本由「数所有竖线」收紧为「只数未转义竖线」，因此 1451 行 `--col-del '\|'` 与 1478 行 `imex version \| grep` 两处**已转义**的写法不再误报。实测：58 个变更行的未转义竖线全部恰为 4（三格），README 四格标志表恰为 5。⑤ **清零复核**：真实账号口令串、工作区绝对路径前缀、临时目录绝对路径前缀三者在文档侧命中均为 **0**；token 串 `github_pat_` 在工作树与全部历史命中 **0**（token 只在 `~/.config/gh/hosts.yml`，模式 600）。注意**历史提交仍含脱敏前原文**，本行只改当前版本，不改写历史；如需彻底抹除，要在用户明确同意后重写历史并强推。⑥ 验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`go test -race -count=1 ./...` 七包全通过；真实库 `IMEX_TEST_DSN` 套件 104 条 PASS（37 顶层 + 67 子用例）、0 SKIP、0 FAIL（沙箱 mysqld 中途掉线一次，按 `<tmp>/mysqld/my.cnf` 拉起后重跑通过）；`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**（该文件本轮只改一行注释）。 |
