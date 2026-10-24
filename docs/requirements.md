# gomydump 技术需求文档

## 一、项目概述

### 1.1 项目目标

基于 Go 语言开发一个**表粒度**的 MySQL 逻辑备份与恢复工具，**功能设计参考 IBM DB2 的 EXPORT/IMPORT**，同时支持 **MySQL** 和 **OceanBase MySQL 模式**。工具为单一二进制文件 **gomydump**，通过子命令区分操作：

```bash
gomydump export    # 导出数据
gomydump import    # 导入数据
gomydump version   # 显示版本信息
gomydump encrypt   # 加密密码
```

### 1.2 核心设计理念（参考 DB2，命令行风格为 Go 标准选项）

- **导出基于 SQL**：通过 `--query` 传入 SELECT 语句，用户完全控制导出什么数据
- **导入多种模式**：通过 `--mode` 指定 INSERT / INSERT_UPDATE / REPLACE / CREATE
- **定界文件格式**：默认使用 DEL（定界 ASCII）格式，支持自定义分隔符
- **Parquet 列式格式**：支持 Parquet 二进制列式存储，普通列存储为可空 UTF-8 字符串，二进制列存储为原始 BYTE_ARRAY
- **文件类型修饰符**：DB2 的 `MODIFIED BY` 拆分为独立命令行选项
- **日志文件**：`--messages` / `-m` 把本次运行的命令行日志逐行另存一份（内容与 stderr 相同，不再另写 DB2 风格报告）
- **并行导出/导入**：支持多线程并行处理，提升大表性能
- **列映射导入**：支持文件列到表列的自定义映射（DB2 风格）

### 1.3 技术栈

| 组件 | 选型 | 说明 |
|------|------|------|
| 开发语言 | Go 1.26（`go.mod` 声明 `go 1.26.9`）| 纯 Go 实现，无 CGO 依赖；发布构建强制 `CGO_ENABLED=0`，产物为静态链接二进制 |
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
gomydump export [连接选项] [导出选项] [修饰选项] --query "<SELECT SQL>"
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
  gomydump export ... --session-var "sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE" --session-var foreign_key_checks=0
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
| `--query` | `-q` | SQL SELECT 语句（支持 OceanBase / MySQL 优化器 hint，见 2.1.8） | 无 | 是 |
| `--messages` | `-m` | 把本次运行的全部日志记录同时写入该文件（内容与 stderr 逐行相同） | 空（只打印到 stderr） | 否 |
| `--no-replace` | | 拒绝覆盖已存在的输出文件（默认行为为覆盖，即 `os.Create` 截断） | `false` | 否 |
| `--batch-size` | | 读→写管道每批行数（仅作用于**单线程导出**与**导入**；并行导出为分片内同步 scan→write，不受影响） | `1000` | 否 |
| `--pipeline-buffer` | | 读→写管道队列深度（批数为单位，0 表示同步交接） | `10` | 否 |
| `--hex-blob` | | 将二进制列编码为 `0x` 十六进制字面量（非 utf8mb4 字符集时自动启用）；作用于 DEL/SQL 文本格式，`--format parquet` 下显式传入报退出码 2 | `false` | 否 |
| `--no-hex-blob` | | 强制原始二进制透传，即使在非 utf8mb4 字符集下（可能导致二进制数据损坏）；与 `--hex-blob` 互斥；`--format parquet` 下显式传入报退出码 2 | `false` | 否 |
| `--threads` | | 并行导出线程数（1=不并行，范围 1–256） | `1` | 否 |
| `--shard-key` | | 分片键列名（空=自动检测主键/唯一键） | 空 | 否 |
| `--log-level` | | 日志级别（debug/info/warn/error） | `info` | 否 |

#### 2.1.4 文件格式（`--format`）

| 值 | 说明 |
|----|------|
| `del` | 定界 ASCII 格式（默认），每行一条记录，列之间用分隔符分隔 |
| `sql` | SQL INSERT 语句格式，包含 CREATE TABLE + INSERT，可直接导入 MySQL |
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

**格式适用性：** 上表中的定界符/转义/时间格式/列名行选项只描述 DEL 文件的布局与编码，SQL 与 Parquet 写端不使用它们；显式传入且 `--format` 不是 `del` 时报用法错误并退出 2（与 `--no-create` 等 SQL 专属选项的行为对称），不会被静默忽略。

**分隔符格式说明：**

- **单字符**：`--col-del ","`、`--col-del "\t"`
- **多字节**：`--col-del "||"`、`--row-del "\r\n"`、`--col-del ":::"`
- **十六进制**：`--col-del 0x09`（制表符）、`--row-del 0x0D0A`（\r\n）、`--col-del 0x7C0x7C`（||）

#### 2.1.6 SQL 格式修饰选项

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--statement-size` | 单条 INSERT 语句最大字节数（范围 1–268435456） | `8388608`（8 MiB） |
| `--single-row-insert` | 每行一条 INSERT 语句（禁用 VALUES 攒批，等价 mysqldump `--skip-extended-insert`）；SQL 格式专属，此时 `--statement-size` 不再参与攒批，仅在单行语句超限时告警一次（一行无法再拆分） | `false` |
| `--no-create` | 不输出 CREATE TABLE 语句 | `false` |
| `--no-data` | 不输出 INSERT 语句（仅导出表结构） | `false` |

**`--single-row-insert` 说明：**

- 仅 `--format sql` 时可用；在非 SQL 格式下显式传入会报错。
- 开启后每条 INSERT 恰好占一个物理行（文件可按行切分），与 mysqldump `--skip-extended-insert` 布局等价。
- `--statement-size` 在单行模式下退化为告警阈值：当单行 INSERT 字节数超过该值时打印一次警告，但语句仍然完整写出（因为单行无法拆分）。

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

#### 2.1.7 使用示例

```bash
# 基本导出：导出整张表到 DEL 格式
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 带 WHERE 条件的导出
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/active_users.del \
  -q "SELECT id, name, email FROM users WHERE status = 'active'"

# 自定义分隔符
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --char-del '!' --col-del '@' --default-charset utf8mb4 \
  -q "SELECT * FROM users"

# 导出为 SQL INSERT 格式（可直接导入）
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.sql -f sql \
  -q "SELECT * FROM users"

# 导出为 Parquet 格式
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.parquet -f parquet \
  -q "SELECT * FROM users"

# 导出表结构 + 数据，控制 INSERT 大小
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.sql -f sql \
  --statement-size 500000 \
  -q "SELECT * FROM users"

# 仅导出表结构
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users_schema.sql -f sql \
  --no-data \
  -q "SELECT * FROM users LIMIT 0"

# 导出关联查询结果
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/user_orders.del \
  -q "SELECT u.name, o.order_id, o.amount FROM users u JOIN orders o ON u.id = o.user_id"

# 带日志文件
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -m /var/log/gomydump_export.log \
  -q "SELECT * FROM users"

# 指定时间戳格式
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/logs.del \
  --timestamp-fmt "yyyy/MM/dd HH:mm:ss" \
  -q "SELECT * FROM logs"

# 制表符分隔、无字符串定界符
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.tsv \
  --col-del "\t" --no-char-del \
  -q "SELECT * FROM users"

# 并行导出（4线程，自动检测主键分片）
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "SELECT * FROM users"

# 指定分片键并行导出
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/orders.del \
  --threads 8 \
  --shard-key order_id \
  -q "SELECT * FROM orders"
```

#### 2.1.8 优化器 Hint 支持（`--query`）

`--query` 完整保留 SQL 中的优化器 hint，可在导出时控制 OceanBase / MySQL 的执行计划：

```bash
# OceanBase 并行 hint（放在 SELECT 之前）
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "/*+ parallel(8) */ SELECT id, name FROM users"

# hint 紧跟关键字（写在 SELECT 之后）同样支持
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
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
gomydump import [连接选项] [导入选项] [修饰选项]
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
| `--messages` | `-m` | 把本次运行的全部日志记录同时写入该文件（内容与 stderr 逐行相同） | 空（只打印到 stderr） | 否 |
| `--batch-size` | | 读→写管道每批行数（范围 1–100000） | `1000` | 否 |
| `--pipeline-buffer` | | 读→写管道队列深度（批数为单位，0 表示同步交接，范围 0–1024） | `10` | 否 |
| `--threads` | | 并行导入线程数（1=不并行，范围 1–256） | `1` | 否 |
| `--log-level` | | 日志级别（debug/info/warn/error） | `info` | 否 |

**`--columns` 列映射说明：**

支持两种格式，通过检测 `=` 符号自动识别：

1. **位置映射**：`--columns "name,age,email"` - 文件列按位置映射到表列
2. **名称映射**：`--columns "user_name=name,user_age=age"` - 显式指定文件列到表列的映射

**位置映射的数量校验：** 位置映射按索引配对，因此 `--columns` 的条目数必须等于文件的列数（列名行给出的列数），条目本身不得为空。数量不符时工具在写入任何数据前报错，提示改用名称映射导入子集。使用 `--no-col-names`（文件无表头）时列数无从得知，只校验条目非空，列数是否匹配交由服务端报错。

**无表头文件与 `-c` 的配对（交叉测试暴露的歧义）：** DEL 读取器总是先调用 `ReadHeader`，只有同时给出 `--no-col-names` 才跳过这一步。于是"文件由 `--no-col-names` 导出、导入时只给 `-c`"的组合会把**首条数据行当成列名行丢掉**，且行数少一并不报错（实测 3 行文件只导入 2 行）；同一组合用于带表头的普通导出文件时行为又是对的。文件本身无法区分这两种情况，改名导入（表头列名与目标表列名不同）也走同一条位置映射路径，因此不做任何猜测、不改变导入结果，只把被丢弃的记录说出来：位置 `-c` + DEL + 未加 `--no-col-names` 时，若被当作表头的那一行字段与目标表列名**全不匹配**（目标表列名取自 `information_schema`），记一条 WARN 日志（`--messages` 打开时它同时进日志文件）——`the first record of the input file was treated as the column header and not imported (fields: …); none of them is a column of table "…", so if the file was exported with --no-col-names add --no-col-names to the import to load that record as data`。字段名能对上目标表列时（正常表头）不产生任何噪声，改名导入仍然成功、只是多一条提醒。结论：**`--no-col-names` 是成对选项，导出加了、导入也必须加**（`-c` 只补列名，不补"没有表头"这一事实）。

**目标列前置校验（DEL/Parquet 格式）：** 导入开始前，工具通过 `information_schema` 校验目标表存在以及每个目标列名在表中存在（大小写不敏感）。DEL/Parquet 文件的列头取自导出查询的结果集列名，若 `--query` 含未起别名的表达式列（如 `SELECT 1, COUNT(*)`），列头（`1`、`COUNT(*)`）在目标表中不存在，工具会在写入任何数据前立即报错并提示修复方式（为表达式列起别名，或使用 `--columns` 按位置指定目标列），替代原先晦涩的服务端错误 1054（`Unknown column ... in 'field list'`）。校验位于模式处理之前，replace 模式不再出现"先清空表、后报列错误"的数据丢失。SQL 格式不做该校验：create 模式在表建立前执行属正常流程，且其列名来自文件自身的 INSERT 语句。

**失败 SQL 回显（脱敏）：** 导入/导出语句在服务端执行失败时，错误信息与日志（含 `--messages` 文件）会附带失败 SQL 的结构回显（表名、列名、占位符完整保留）。数据值默认**不**内联：仅以 `/* args: str(14), num, NULL, bytes(256) */` 形式的类型/长度标签呈现（可定位"哪一列、什么类型、是否超长"，但不泄露密码、个人信息等真实数据）。回显在 2048 字节处截断且保持引号配平。需要内联真实值时显式开启 `--echo-sql-values` 或 `GOMYDUMP_ECHO_SQL_VALUES=1`（警告：真实数据将写入 stderr 与 `--messages` 文件）。`--query` SELECT 与 CREATE TABLE 语句不含行数据，失败时原文回显。

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

**格式适用性与行选择：** `--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-escape`/`--timestamp-fmt`/`--date-fmt`/`--no-char-del`/`--no-col-names` 只描述 DEL 文件的解析方式，`--format sql`/`parquet` 下显式传入会报用法错误并退出 2；`--skip-count`/`--row-count`/`--commit-count` 与格式无关，三种格式都生效。

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
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users

# 从 Parquet 文件导入
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.parquet -f parquet \
  -t users

# 自定义分隔符导入
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --char-del '!' --col-del '@' \
  -t users

# insert_update 模式：插入或更新
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode insert_update \
  -t users

# replace 模式：替换所有数据
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode replace \
  -t users

# create 模式：从 SQL 文件创建表并导入
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.sql -f sql \
  --mode create

# 指定目标列（位置映射）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "id,name,email"

# 指定目标列（名称映射）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "user_id=id,user_name=name,user_email=email"

# 跳过前 1 行数据（列名行由解析器自动读取，不占用 skip-count），导入 1000 行
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --skip-count 1 --row-count 1000 --commit-count 100 \
  -t users

# 并行导入（4线程）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --threads 4 \
  -t users

# 带日志文件
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -m /var/log/gomydump_import.log \
  -t users
```

---

### 2.3 日志文件（`--messages` / `-m`）

`--messages <file>` 让**命令行的日志输出同时写入文件**：文件里的记录与 stderr 上看到的逐行相同（同一个 sink 先写 stderr 再写文件，一条记录一个物理行），不再是由 `MessageWriter` 另写一遍的 DB2 `SQL3xxxN` 报告。实测（`-m` + 一条注定失败的导出）文件与 stderr `diff` 完全一致：

```
2026-10-24 03:39:27.535 [2976] INFO gomydump starting version=dev build_time=unknown git_commit=unknown go_version=go1.26.9 command=export
2026-10-24 03:39:27.536 [2976] INFO export config host=127.0.0.1 port=3306 user=gd database=mydb output=/tmp/fail.del format=del query="SELECT * FROM `no_such`" threads=1 ...
2026-10-24 03:39:27.537 [2976] ERROR query failed error="Error 1146 (42S02): Table 'mydb.no_such' doesn't exist" sql="SELECT * FROM `no_such`"
2026-10-24 03:39:27.537 [2976] ERROR command failed error="export failed: failed to execute query (SQL: SELECT * FROM `no_such`): Error 1146 (42S02): ..."
```

**行为约定：**

| 项目 | 行为 |
|------|------|
| 打开时机 | 根命令 `PersistentPreRunE` 在打印版本横幅**之前**打开文件（`os.Create`，已存在的同名文件被截断），因此横幅恒为文件首行；用法错误也照样落进文件 |
| 覆盖范围 | 全部日志记录：横幅、`export/import config`、定时进度、WARN 告警、`completed` 统计、收尾的 `command failed`；导出器与导入器内部不再有独立的消息写入路径（原 35 处 `MessageWriter` 调用删除：导出 15、导入 20） |
| 关闭时机 | `cmd.Execute()` 返回前关闭，因此最后那条 `command failed` 也在文件里；关闭自身失败的 ERROR 记录只进 stderr（此时文件已脱离 sink） |
| `--log-level` | 生效：`--log-level error` 下成功导出，文件只剩版本横幅一条（横幅在 `PersistentPreRunE` 打印，早于 `RunE` 应用 `--log-level`，这是既有行为） |
| `--log-format` | 生效：文件与 stderr 用同一布局（`java` / `text`） |
| `--quiet` | 生效：定时进度被抑制时文件里也没有。旧版"消息文件是显式产物、不受静默影响"的例外随 DB2 报告一起取消 |
| 写入串行化 | 日志 sink 带互斥锁，并行路径（进度监视协程与分片协程）不会把一条记录拆进两处 |
| 安全 | 密码不进日志，因而也不进该文件（见 8.4）；`--echo-sql-values` 打开后真实行数据同时进入 stderr 与该文件 |

会出现告警（WARN）的场景：导出侧 `--no-hex-blob` 与非 utf8mb4 字符集组合、单行 INSERT 超过 `--statement-size`；导入侧读不到 `max_allowed_packet`（按保守值分批）、`--batch-size` 被 65535 占位符上限下调、无表头文件被当作表头丢弃首行（见 7.4）、`insert_update` 命中无唯一键的表。每个场景只有一条 WARN 记录，不再另写一份 `SQL3107N`。

### 2.4 encrypt 子命令 — 密码加密

把明文密码加密成 `ENC:` 前缀密文，供 `-p/--password` 直接使用，避免明文出现在 `ps` 输出与 shell history 中。

| 项目 | 实现 |
|------|------|
| 语法 | `gomydump encrypt <password>`，位置参数恰好 1 个；多给或不给是用法错误，退出 2（见 3.3.4）|
| 密钥来源 | 环境变量 `GOMYDUMP_KEY`（`common.EnvKey`）；未设置或为空即失败，提示先 `export GOMYDUMP_KEY="..."` |
| 算法 | AES-256-GCM：密钥 = `sha256(GOMYDUMP_KEY)`，随机 nonce 前置进密文，整体 `base64.StdEncoding` 后加 `ENC:` 前缀（`internal/common/password.go`）|
| 使用侧 | export/import 在 `RunE` 起始处调用 `common.ResolvePassword`（`cmd/export.go:72`、`cmd/import.go:67`）：值以 `ENC:` 开头则解密，否则原样按明文处理。密钥缺失报 `failed to resolve password: password is encrypted but GOMYDUMP_KEY environment variable is not set`，密钥不符报 `failed to resolve password: failed to decrypt: wrong key or corrupted data`，两者都是**运行时错误，退出码 1**（不是用法错误的 2，因为失败发生在参数校验之后）|
| 密文非确定性 | nonce 每次随机，同一明文 + 同一密钥两次加密结果不同，不可把密文当等值键比较或做 diff |
| 输出 | 标准输出仅一行密文；stderr 首行仍是 java 布局的版本横幅（`command=encrypt`），便于日志归属 |
| 安全边界 | 密码不写入日志，因而也不写入 `--messages` 文件（见 8.4）。但 `GOMYDUMP_KEY` 本身就是凭据，明文放在环境变量里与明文密码同级风险，该机制只解决"命令行可见性"，不解决密钥托管 |

---

## 三、全局选项

### 3.1 错误堆栈开关（`--error-stack`）

命令失败时统一以 **ERROR 级日志记录**输出（`level=ERROR msg="command failed" error="..."`），与其它诊断信息格式一致，不再单独打印裸 `Error:` 行。默认只含错误链文本；开启错误堆栈后，通过 `github.com/pkg/errors` 的 `%+v` 格式在 `error=` 字段内追加完整调用堆栈，便于排障。

**开启方式：**

1. **命令行标志**（持久标志，所有子命令可用）：
   ```bash
   gomydump export --error-stack -h 127.0.0.1 -u root -p pass -D mydb \
     -o /backup/users.del -q "SELECT * FROM nope"
   ```

2. **环境变量**（适合脚本/容器）：
   ```bash
   GOMYDUMP_ERROR_STACK=1 gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
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
github.com/gomydump/internal/export.(*Exporter).exportSingle
        /path/exporter.go:185
...
export failed
github.com/gomydump/cmd.init.func2
        /path/export.go:111
..."
```

### 3.2 帮助短选项（`-?`）

export/import 子命令把 `-h` 让给了 `--host`，因此这两个子命令的帮助短选项改为 `-?`（根命令仍使用默认的 `--help`，帮助文本中也会显示 `-?, --help`）：

```bash
gomydump export -?       # 显示 export 子命令帮助（-h 为 --host）
gomydump import -?       # 显示 import 子命令帮助（-h 为 --host）
gomydump --help          # 根命令帮助
```

### 3.3 completion 命令禁用

cobra 框架默认生成的 `completion` 子命令已被禁用（`rootCmd.CompletionOptions.DisableDefaultCmd = true`），避免与工具本身的命令风格不一致。

### 3.3.1 静默模式（`--quiet`）

默认导出、导入均每隔 5 秒打印一次进度日志。`--quiet`（或 `GOMYDUMP_QUIET=1`，值支持 `1`/`true`/`yes`/`on`，不区分大小写）禁止定时进度日志，但**保留**版本横幅、配置参数、结束统计与 ERROR 记录。`--messages` 日志文件是 stderr 的同一份记录，因此同样被静默（旧版"消息文件仍记 `SQL3105N`"的例外随 DB2 报告一起取消）。

```bash
gomydump import --quiet -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
GOMYDUMP_QUIET=1 gomydump export -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

### 3.3.2 日志格式（`--log-format`）

`--log-format` 控制日志记录的布局格式（持久标志，所有子命令可用），也可通过环境变量 `GOMYDUMP_LOG_FORMAT` 设置。

| 值 | 说明 |
|----|------|
| `java`（默认） | 固定宽度行格式：`2026-01-02 15:04:05.000 [pid] LEVEL message key=value ...`，类似 JUL Formatter，便于 grep 和 tail |
| `text` | slog 原生 key=value 文本格式，适合机器解析 |

```bash
gomydump export --log-format text -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
GOMYDUMP_LOG_FORMAT=text gomydump import -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
```

日志格式在 `PersistentPreRunE` 中尽早应用，即使用法错误（退出码 2）也使用所选格式输出。

**单行保证：** 两种布局都遵循「一条记录 = 一个物理行」。值为空或含空白、控制字符、`"`、`\`、`=` 时，按 slog 原生规则加引号并转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`）。因此打印 DEL 定界符的配置行不会被真实换行拆开，`grep`/`tail` 每次都能取到完整记录；同一值在 `java` 与 `text` 两种布局下渲染成同一个 token，切换格式不影响字段切分。

**布局切换不改级别：** `ApplyLogFormat()` 只重设布局，`--log-level` 选择的阈值保持不变，版本横幅与随后所有 INFO 记录照常输出。

**消息命名：** 所有经全局 logger 输出的记录统一为小写、单行的 `<对象> <事件>` 短语，事件词固定为 `started` / `config` / `progress` / `completed` / `aborted` / `failed`，因此大小写敏感的 grep 一条 pattern 就能覆盖全链路。主要记录：

| 记录 | 触发时机 |
|------|----------|
| `gomydump starting` | 每次真实运行的首行（version/build_time/git_commit/go_version/command）；`version` 子命令本身不产生任何日志记录 |
| `export started` / `import started` | 子命令入口，含 output/input、format、连接目标 |
| `export config` / `import config` | 完整生效参数快照（含 DEL 定界符、字符集、批/管道参数） |
| `session variables will be set on every connection` | 传入了 `--session-var` |
| `export progress` / `import progress` | 每 5 秒定时进度（`--quiet` 抑制） |
| `parallel export detected` / `shard export completed` / `parallel export completed` | 并行导出：分片计划、每个分片、汇总 |
| `shard stream failed` / `incomplete output file removed` | 导出写出失败（如 `ENOSPC`、写入被拒）：前者 `error` 仅并行路径出现，带具体错误；后者 `warn` 带被删除的 `output=`，并行与单线程两条路径都会在失败时删除已写出的半截产物（单线程此前不会删除，实测留下 21 MB 残留文件），其余仍在队列上等待的分片此时以 `shard output consumer stopped` 结束 |
| `worker completed` / `parallel import completed` | 并行导入：每个工作线程、汇总 |
| `export completed` / `import completed` / `import aborted` | 结束统计（中止时仍给出已达进度与 `failed`） |
| `query failed` / `insert batch failed` / `create table failed` / `shard query failed` / `shard export failed` | 语句级失败，附 `sql` 预览（默认脱敏） |
| `command failed` | 最终失败汇总，`--error-stack` 附带调用栈 |

同一事件只输出一行：原先紧随 `export config`/`import config` 的 `Starting export`/`Starting import` 属于重复记录，已删除（字段全部被 config 记录覆盖，输入文件字节数改由 `import config` 的 `bytes` 承载）。该约定由 `main_test.go` 的 `TestLogMessageStyle` 静态守护：解析全部源文件，检查每个 `logger.Debug/Info/Warn/Error` 的字面量消息首字母小写且不含换行。

### 3.3.3 SQL 归因注释（`--no-sql-tag`）

默认情况下，工具执行的每条 SQL 语句前会添加归因注释 `/* gomydump:<commit7> */`（`<commit7>` 为构建时通过 ldflags 注入的 Git commit 前 7 位十六进制短哈希），便于在 `general_log`、`slow_query_log` 和审计流中识别工具来源。

`--no-sql-tag`（或 `GOMYDUMP_NO_SQL_TAG=1`）关闭该注释。以下情况注释自动为空：

- 构建未注入 commit（dev 构建报告 `"unknown"`）
- 注入值不是合法的十六进制哈希（防止 SQL 注入）

注意：归因注释仅添加到数据操作语句（INSERT、DELETE、SHOW CREATE TABLE 等），连接生命周期语句（SET NAMES、START TRANSACTION、COMMIT、SET FOREIGN_KEY_CHECKS）不添加。

```bash
gomydump import --no-sql-tag -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
GOMYDUMP_NO_SQL_TAG=1 gomydump export -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

### 3.3.4 退出码

| 退出码 | 含义 |
|--------|------|
| `0` | 成功 |
| `1` | 运行时错误（连接失败、SQL 执行失败、文件 I/O 错误等） |
| `2` | 用法错误（参数校验失败、未知选项） |

判定口径（`main.go:17-21`）：错误来自 `common.Usagef`（`common.IsUsageError`）或 cobra 的 `unknown flag` / `unknown command`，一律 2；其余为运行时错误 1。两条容易误解的边界：

- 环境变量导致的失败按发生位置分流：`GOMYDUMP_LOG_FORMAT` 非法在 root 的 `PersistentPreRunE` 校验阶段拦下，退出 2；`GOMYDUMP_KEY` 缺失/不符发生在参数校验之后的密码解析阶段，退出 1。
- 位置参数个数也属于用法错误。cobra 自带的 `ExactArgs` 返回的是普通 error（会被算成运行时错误的 1），因此 `encrypt` 改用本仓库的 `cmd/validate.go:exactArgs`，消息为 `gomydump encrypt accepts 1 argument(s), received N` 并退出 2；`export`/`import` 的 `cobra.NoArgs` 消息含 `unknown command`，由 `main.go` 兜底归为 2。由 `cmd/cli_matrix_test.go:TestCLIEncryptArgsCountIsUsageError` 锁定（同时断言密钥缺失仍是 1，两级错误不许混淆）。

### 3.4 失败 SQL 回显与脱敏（`--echo-sql-values`）

导入/导出过程中语句在服务端执行失败时（如列不存在、类型不符、约束冲突、数值超长），错误信息、结构化日志与 `--messages` 日志文件都会附带**失败 SQL 的结构回显**，便于定位问题：表名、列名、`?` 占位符完整保留。

出于数据安全，默认**不内联真实行数据值**。`?` 占位符只以类型/长度标签呈现：

```
INSERT INTO `u` (`id`, `pw`) VALUES (?,?),(?,?) /* args: str(1), str(14), str(1), str(12) */
```

标签可判断"哪一列、什么类型、字符串/字节长度、是否 NULL"，足以解决 `Data too long for column` 一类问题，同时避免密码、个人信息、业务数据落入长期留存的日志与日志文件。回显在 2048 字节处按空白边界截断并保持单引号配平。

**开启真实值内联（谨慎）：**

```bash
# 命令行标志（持久标志，所有子命令可用）
gomydump import --echo-sql-values ...

# 环境变量（适合脚本/容器）
GOMYDUMP_ECHO_SQL_VALUES=1 gomydump import ...
```

> 警告：开启后失败 SQL 回显将包含真实数据值，会写入 stderr、日志与 `--messages` 文件，仅应在受信任的排障环境临时使用。

`--query` 的 SELECT 语句与 SQL 文件里的 CREATE TABLE 语句本身不含行数据，失败时按原文回显，不受该开关影响。

---

## 四、DB2 命令到 gomydump 选项映射

| DB2 EXPORT/IMPORT 语法 | gomydump 选项 |
|------------------------|---------------|
| `TO <filename>` | `--output` / `-o` |
| `FROM <filename>` | `--input` / `-i` |
| `OF <filetype>` | `--format` / `-f` |
| `MESSAGES <file>` | `--messages` / `-m`（**语义不同**：DB2 写独立消息报告，本工具写本次运行日志的同一份副本，见 2.3） |
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
| `MODIFIED BY REPLACE` | 默认覆盖（`os.Create` 截断）；`--no-replace` 改为拒绝覆盖 |
| `MODIFIED BY STATEMENTSIZE=<n>` | `--statement-size <n>` |
| `MODIFIED BY NOCREATE` | `--no-create` |
| `MODIFIED BY NODATA` | `--no-data` |
| `MODIFIED BY SKIPCOUNT <n>` | `--skip-count <n>` |
| `MODIFIED BY ROWCOUNT <n>` | `--row-count <n>` |
| `MODIFIED BY COMMITCOUNT <n>` | `--commit-count <n>` |

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

**文件结构：**
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
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00'),(2,'Bob','bob@example.com','2026-01-02 11:00:00'),(3,'Charlie','charlie@example.com','2026-01-03 12:00:00');
UNLOCK TABLES;
```

**`--single-row-insert` 模式下的文件结构：**
```sql
/*!40101 SET NAMES utf8mb4 */;
LOCK TABLES `users` WRITE;
-- Table structure for users
CREATE TABLE `users` (...);

-- Data for users
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00');
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (2,'Bob','bob@example.com','2026-01-02 11:00:00');
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (3,'Charlie','charlie@example.com','2026-01-03 12:00:00');
UNLOCK TABLES;
```

**特点：**
- 包含 CREATE TABLE 语句（可通过 `--no-create` 跳过）
- 默认模式：多条 VALUES 合并为一条 INSERT 语句，由 `--statement-size`（默认 8 MiB）控制攒批上限
- 单行模式（`--single-row-insert`）：每行一条 INSERT，文件可按行切分
- 字符串值按转义规则处理（见 2.1.6），确保每条 INSERT 恰好占一个物理行
- 文件首行包含 `/*!40101 SET NAMES <charset> */;`
- MySQL 下数据段带 LOCK TABLES/UNLOCK TABLES 一对；OceanBase 下不带
- 可直接用 MySQL 客户端执行

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
│   gomydump export / import / version │
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
   - sql：生成 CREATE TABLE + INSERT 语句
   - parquet：列式写入 Parquet 文件（普通列为可空 UTF-8 字符串，二进制列为原始 BYTE_ARRAY）
5. 应用修饰选项（--char-del, --col-del, --default-charset 等）
6. 全程记录运行日志（stderr，以及 --messages 文件的同一份副本）
7. 输出统计信息
```

**并行导出流程（`--threads>1`）：**
```
1. 检测查询复杂度（JOIN 查询自动降级为单线程）
2. 检测分片键（主键 > 唯一键 > 用户指定 --shard-key）
3. 生成分片范围
4. 每个分片独立连接、独立查询，把序列化结果写入自己的字节队列（`shardPipe`）
   - SQL 格式下，分片片段不写 LOCK TABLES/UNLOCK TABLES（SetSkipTableLocks=true）
   - 分片片段不写 CREATE TABLE（writeCreate=false）
5. writeShardStream 流式写出到最终文件：
   - 先由 SQLWriter/DelWriter 写出 header（SET NAMES、CREATE TABLE、LOCK TABLES 等）
   - 再按分片序号依次消费各自的队列（分片仍在并行产出，只是文件位置由这一个 goroutine 独占）
   - 最后由 SQLWriter 写出 footer（UNLOCK TABLES 等）
6. 不产生任何临时文件
```

**并行导出不再写临时文件（`shard_stream.go`）：** 旧实现是每个分片写一份 `<output>.shard_<i>.tmp`，全部分片结束后再 `io.Copy` 拼接、删除临时文件。代价是**每个字节落盘两次**：实测 50 万行表（DEL 输出 124,903,023 B）单次并行导出的写入量为 487,920 × 512 B ≈ 238 MB，约输出体积的 2 倍，SQL 格式为 515,152 块 ≈ 251 MB 对 131,872,590 B（两轮重复测得同一组数字）；容量上意味着并行导出需要两倍的空闲空间，且中途被信号打断会在输出目录留下一堆 `<output>.shard_N.tmp`（实测 4 个共 33 MB）。现在分片直接把 `[]byte` 块交给按分片序号消费的那个 goroutine，写入量降到 243,952 / 257,568 块（等于输出体积按 512 B 向上取整，写放大归零），输出目录里只有最终文件本身。字节序列与旧实现**逐字节一致**（DEL/SQL 两种格式、同一库同一时刻分别与旧二进制比对），导出内容与单线程导出、以及导入回读的行集（count + 顺序无关 CRC 和）也都一致。

队列深度不是细节：单个输出 goroutine 一旦偶尔落后，分片就会填满队列并停下，而它停下的时候其他分片的数据库读取仍在推进，整体变成"读一批、写一批"的停顿。同一台 4 核机器上以 SQL 格式实测（每组 7 次交替、取中位数，消除温漂）：旧实现 1.09 s；每分片 2.5 MiB 前瞻为 1.31 s、8 MiB 为 1.24 s（慢 14~20%）；16 MiB 为 1.09 s，与旧实现持平——写入量减半不再牺牲时间。因此 `shardStreamQueueBlocks` 用一个固定的 64 MiB 总预算按分片数均分，上界 64 块（16 MiB）、下界 2 块，与 `--pipeline-buffer`（按行数计）无关。代价是这部分在途字节从磁盘移到堆上：4 分片并行导出的进程峰值 RSS 由约 33 MB 升到约 111 MB（DEL）、114 MB 升到 215 MB（SQL），8 分片 DEL 为 36→118 MB；`--threads 8` 的产物同样与旧实现逐字节一致、写入量同样是恰好一半。

**导出的失败与中断语义（两条路径一致）：** 并行导出的流式写法意味着失败时输出文件已经存在并写了部分内容。任一分片报错或写文件报错（如 `ENOSPC`）时，导出整体报错、把已写出的输出文件删除并记一条 `incomplete output file removed` 警告，其余仍在队列上等待的分片由 `shard output consumer stopped` 立即解除阻塞（不会挂死），实测 32 MB tmpfs 目标下 4 分片导出在第 1 个分片写满后整体失败且不留任何文件。旧实现在分片失败时是"不产生输出文件"，在合并阶段失败时同样会留下半截输出，因此新语义在两种情况下都只留一种结果：**要么完整、要么没有**。单线程路径过去并不遵守这一条：`os.Create` 之后的任何失败只报错、不清理，交叉测试实测 20 万行表在末行被 `--no-char-del` 拒绝后留下 21,233,664 字节的半截 DEL 文件（文件名为导出名，看起来像成功产物）。现在 `exportSingle` 在创建输出文件之后注册同一份 `removeIncompleteOutput`，任何后续失败都删除该文件，并行路径改为复用同一个方法；删除只在文件确已创建之后注册，因此创建前的失败（查询报错、`--no-replace` 命中已有文件）不会误删用户原有文件，实测 `--threads 1/4` 两种失败都只留日志不留文件。注意：程序未安装信号处理器，`SIGINT`/`SIGTERM` 仍是直接终止进程，此时会留下一个不完整的输出文件（旧实现留下的是等量的分片临时文件），需要用户自行删除。

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
   - sql：解析并执行 SQL 语句
   - parquet：读取 Parquet 列式数据
5. 应用修饰选项（--skip-count, --row-count, --commit-count 等）
6. 全程记录运行日志（stderr，以及 --messages 文件的同一份副本）
7. 输出统计信息
```

**并行导入流程（`--threads>1`）：**
```
1. 单个 reader goroutine 读取文件，按 --commit-count 与字节上限分批
2. 多个 worker goroutine 通过有界 channel 接收批次，各自独立连接/事务
3. 每个 worker 构建多行参数化 INSERT 并执行
4. 汇总统计信息
```

### 7.4 导入攒批字节上限（`batchcap.go`）

导入侧按 `--batch-size`（行数）攒批重建参数化 INSERT，现追加两道保护：

**1) 占位符上限：**

MySQL 预处理语句参数个数为 2 字节无符号整数，故 `rows × columns ≤ 65535`。超出时自动下调有效 `batch-size`（并打印一次告警），确保不触发协议层限制。

**2) 包上限（`max_allowed_packet`）：**

启动后首次攒批时读取服务端 `SHOW VARIABLES LIKE 'max_allowed_packet'`：
- 每批字节预算 = 该值的 **9/10**（下限 64 KiB）
- 读不到则按 **4 MiB** 保守值并告警
- 字节按值的实际宽度估算：字符串/字节按含转义后的宽度精确计算，标量按最宽文本形式界定

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

### 7.6 幂等性

| 导入模式 | 幂等性 | 说明 |
|----------|--------|------|
| `insert` | **不幂等** | 重复执行会追加重复数据 |
| `insert_update` | **幂等** | `INSERT ... ON DUPLICATE KEY UPDATE`，重复执行收敛 |
| `replace` | **幂等** | 先 DELETE 全表后 INSERT，重复执行收敛 |
| `create` | **不幂等** | 重复执行会因表已存在而报错 |

**导出覆盖语义：** 默认行为为覆盖输出文件（`os.Create` 截断）。`--no-replace` 开启后，若输出文件已存在则报错退出，不覆盖。

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
| 日志文件 | `--messages` / `-m`：INFO/WARN/ERROR 全部记录逐行写入文件，内容与 stderr 相同 |
| 失败处理 | 明确标识失败原因和行号 |
| 连接重连 | 连接级瞬态错误（`ErrBadConn`、2006/2013、网络超时等）默认最多 3 次尝试，指数退避 1s→2s（上限 4s、20% 抖动）；重连后先回放当前事务的未提交批次再重试失败批次；`COMMIT` 与语义错误不重试（§7.7） |
| 连接池老化 | 导入池 `ConnMaxIdleTime=1m`、`ConnMaxLifetime=5m`；导出池两者显式设为 0（快照连接会归还空闲池，老化会静默回滚快照）；并行导入 worker `MaxOpenConns=1` |
| 事务空闲提交 | 导入写入端 30s 未收到新批次时提交当前部分事务，缩短未提交窗口与断线重放集合 |

### 8.3 可观测性

| 项目 | 要求 |
|------|------|
| 进度输出 | 导出、导入均每隔 5 秒打印一次进度：已处理条数、总速率、区间速率、已用时长；`--quiet` / `GOMYDUMP_QUIET=1` 禁止定时进度日志（配置、结束统计、ERROR 仍输出；`--messages` 文件与 stderr 同一份记录，因而同样无进度行） |
| 日志顺序 | 版本横幅（version/build_time/git_commit/go_version/command）作为运行首行输出，其后为命令启动与配置参数；`version` 子命令例外——它只向标准输出打印版本四行，不写任何日志（`help` 子命令仍走横幅）|
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
gomydump/
├── main.go                    # 入口：执行命令，把用法错误映射为退出码 2，其余 1
├── main_test.go               # 日志消息风格静态守卫（go/ast 遍历全部 logger 调用点）
├── cmd/
│   ├── root.go                # 根命令（--error-stack、--echo-sql-values、--quiet、--log-format、--no-sql-tag、completion 禁用）
│   ├── export.go              # export 子命令（RunE：密码解析 → 校验 → 调度）
│   ├── export_flags.go        # export 子命令标志注册与默认值
│   ├── import.go              # import 子命令（RunE：同上）
│   ├── import_flags.go        # import 子命令标志注册与默认值
│   ├── validate.go            # 参数校验：数值边界、字符集冲突、格式适用性（SQL 专属/DEL 专属/parquet 专属选项）
│   ├── cli_matrix_test.go     # 参数组合矩阵与退出码契约测试
│   ├── validate_fuzz_test.go  # 校验函数的 fuzz 测试
│   └── encrypt.go             # encrypt 子命令（密码加密）
├── internal/
│   ├── config/
│   │   └── config.go          # 配置定义与默认值常量
│   ├── common/
│   │   ├── util.go            # DSN 构建（超时/字符集/会话变量注入）与通用工具
│   │   ├── usage.go           # 用法错误类型（Usagef / IsUsageError），退出码 2 的依据
│   │   ├── stats.go           # 吞吐统计与速率计算（rows/sec、字节数、耗时）
│   │   ├── colkind.go         # 列类型分类（修饰选项/Parquet 二进制判定共用）
│   │   ├── sessionvars.go     # 会话变量解析与 DSN 注入（--session-var）
│   │   ├── sqlpreview.go      # 失败 SQL 脱敏回显（--echo-sql-values）
│   │   ├── sqltag.go          # SQL 归因注释（/* gomydump:<commit> */）
│   │   ├── dberr.go           # 瞬态错误判定（连接级错误码/网络错误/文本兜底）
│   │   ├── retry.go           # 重试策略（次数、指数退避、抖动、ctx 取消）
│   │   ├── pool.go            # 连接池老化（ConnMaxIdleTime / ConnMaxLifetime）
│   │   ├── log.go             # 日志模块（java/text 双布局、版本横幅、一条记录一行）
│   │   ├── password.go        # 密码加密/解密（AES-256-GCM）
│   │   └── detect.go          # 服务器类型检测
│   ├── export/
│   │   ├── exporter.go        # 导出调度器（Pipeline 模式、并行分片、writeShardStream 流式写出）
│   │   ├── del_writer.go      # DEL 格式写入器
│   │   ├── sql_writer.go      # SQL 格式写入器（INSERT 合并/转义/单行模式/LOCK 抑制）
│   │   ├── parquet_writer.go  # Parquet 格式写入器（列式存储）
│   │   ├── shard.go           # 分片逻辑（主键/唯一键检测）
│   │   ├── shard_stream.go    # 分片到输出文件的字节队列（并行导出流式写出，免临时文件）
│   │   └── sql_parser.go      # SQL 解析器（JOIN 检测、hint 保留）
│   └── import/
│       ├── importer.go        # 导入调度器（Pipeline 模式、列映射、并行导入）
│       ├── batchcap.go        # 导入攒批字节上限（max_allowed_packet 与 65535 占位符）
│       ├── session.go         # 写入会话（事务、重连回放、空闲提交、commit-count）
│       ├── record_reader.go   # 无上限记录读取器（替代 bufio.Scanner）
│       ├── del_reader.go      # DEL 格式读取器
│       ├── sql_reader.go      # SQL 格式读取器
│       └── parquet_reader.go  # Parquet 格式读取器
├── test/
│   └── integration/
│       ├── roundtrip_test.go  # 离线往返（写入器→读取器，无需数据库）
│       ├── live_mysql_test.go # 真实 MySQL/OceanBase 库级端到端，需 GOMYDUMP_TEST_DSN，未设置则跳过
│       └── option_matrix_test.go # 真实 CLI 交叉选项矩阵（argv 驱动编译出的二进制），同上门控
├── docs/
│   ├── requirements.md        # 本文件
│   └── cli-oracle.txt         # 已发布二进制 --help 契约快照（回归基线）
├── go.mod                     # go 1.26.9
├── go.sum
├── Makefile
├── Dockerfile
└── README.md
```

另有 22 个包内 `*_test.go`（`internal/common` 6、`internal/export` 7、`internal/import` 9），加上 `cmd` 3、仓库根 1、集成 3，全仓库共 29 个测试文件，清单见十一节。

---

## 十、使用示例汇总

### 10.1 导出场景

```bash
# 场景 1：导出整张表到 CSV 风格文件
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 场景 2：导出符合条件的数据
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/active_users.del \
  -q "SELECT id, name, email FROM users WHERE status = 'active'"

# 场景 3：导出为可直接导入的 SQL 文件
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.sql -f sql \
  -q "SELECT * FROM users"

# 场景 4：导出为 Parquet 格式
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.parquet -f parquet \
  -q "SELECT * FROM users"

# 场景 5：自定义分隔符（制表符分隔，无字符串定界符）
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.tsv \
  --col-del "\t" --no-char-del \
  -q "SELECT * FROM users"

# 场景 6：导出关联查询结果
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/user_orders.del \
  -q "SELECT u.name, o.order_id, o.amount FROM users u JOIN orders o ON u.id = o.user_id"

# 场景 7：仅导出表结构
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users_schema.sql -f sql \
  --no-data \
  -q "SELECT * FROM users LIMIT 0"

# 场景 8：自定义时间戳格式 + 日志文件
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/logs.del \
  --timestamp-fmt "yyyy/MM/dd HH:mm:ss" \
  -m /var/log/gomydump_export.log \
  -q "SELECT * FROM logs"

# 场景 9：并行导出（4线程）
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "SELECT * FROM users"

# 场景 10：带错误堆栈的导出
gomydump export --error-stack -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 场景 11：导出前设置会话变量（每条连接生效，含并行分片）
gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
  -o /backup/users.del \
  --threads 4 \
  --session-var "sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE" \
  --session-var foreign_key_checks=0 \
  -q "SELECT * FROM users"
```

### 10.2 导入场景

```bash
# 场景 1：追加数据到现有表
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users

# 场景 2：从 Parquet 文件导入
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.parquet -f parquet \
  -t users

# 场景 3：同步数据（插入或更新）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode insert_update \
  -t users

# 场景 4：完全替换表数据
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --mode replace \
  -t users

# 场景 5：从 SQL 文件创建表并导入
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.sql -f sql \
  --mode create

# 场景 6：导入指定列（位置映射）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "id,name,email"

# 场景 7：导入指定列（名称映射）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  -t users -c "user_id=id,user_name=name,user_email=email"

# 场景 8：跳过列名行，限制导入行数
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --skip-count 1 --row-count 1000 \
  -t users

# 场景 9：自定义分隔符 + 日志文件
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
  -i /backup/users.del \
  --char-del '!' --col-del '@' \
  -m /var/log/gomydump_import.log \
  -t users

# 场景 10：并行导入（4线程）
gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
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
| 日志文件 | 验证 `--messages` / `-m` 文件与 stderr 逐行相同、首行为版本横幅、`--log-level` 与 `--log-format` 生效、不再出现 `SQL3xxxN` |
| 错误堆栈 | 验证 `--error-stack` 和 `GOMYDUMP_ERROR_STACK=1` 输出完整堆栈 |
| OceanBase | 在 OceanBase 上执行上述场景 |
| `--single-row-insert` | 导出为 SQL + `--single-row-insert` → 验证每行一条 INSERT → 导入 → 数据校验 |
| 导入字节上限 | 宽表/BLOB 大行导入 → 验证不触发 error 1153（`Packet too large`） |
| 巨型 INSERT 导入 | 导入含超长 INSERT 语句的 SQL 文件 → 验证 `recordReader` 无上限读取 |
| 导出/导入转义往返 | 含 `\n`/`\r`/`\t`/`'`/`\`/`\0`/`\x1a` 的数据 → 导出 SQL → 导入 → 数据一致 |
| 占位符上限 | 宽表（columns > 100）导入 → 验证 `batch-size` 自动下调且不报错 |
| 告警记录 | 触发 `--no-hex-blob`/超长语句/`max_allowed_packet` 读取失败/无表头首行丢弃/`insert_update` 无唯一键 → 验证对应 WARN 记录同时出现在 stderr 与 `--messages` 文件 |
| `--no-escape` 冲突数据 | `--no-escape` 导出含定界符的数据 → 验证写入前报错且不产生半行残留 |
| SQL 格式多表查询 | `--format sql` + JOIN/派生表/CTE → 验证解析阶段报错并提示改用 del/parquet |
| 位置映射列数 | `--columns` 条目数与文件列数不符 → 验证写入前报错并提示名称映射 |

### 11.1 测试资产清单

上表按"场景"组织，这里按"文件"给出对应的落地位置与规模（当前共 30 个 `_test.go`、161 个 `Test*`（另有 1 个 `TestMain` 负责一次性构建被 CLI 测试调用的二进制）、5 个 `Fuzz*`、8 个 `Benchmark*`）。

| 文件 | 用例数 | 覆盖点 | 运行方式 |
|------|--------|--------|----------|
| `cmd/cli_matrix_test.go` | 6 | 参数组合矩阵与退出码契约：DEL/SQL/parquet 专属选项互斥、`--log-format` 标志与环境变量、必填项缺失、`encrypt` 位置参数个数（越界为用法错误 2、密钥缺失仍为运行时 1）| `make test` |
| `cmd/validate_fuzz_test.go` | 4 fuzz | 边界校验函数（batch-size、pipeline-buffer、statement-size、threads）对畸形输入不 panic | `make test` |
| `cmd/version_log_test.go` | 2 | `version` 子命令不写日志记录（stdout/stderr 捕获后断言），`run` 横幅仍照常记录 | `make test` |
| `internal/common/*_test.go`（6 个文件）| 27 | 瞬态错误判定、日志单行与切布局保持级别、日志文件 sink（与 stderr 逐字节相同、关闭后脱钩、并发不拆行、替换与失败路径）、重试退避与取消、会话变量解析与 DSN 注入、失败 SQL 脱敏回显、DSN 构建 | `make test` |
| `internal/export/*_test.go`（7 个文件）| 62 | DEL/SQL/Parquet 写入器转义与攒批、分片边界精确运算与 hint 保留、hex-blob 判定、并行导出流式写出的 header/LOCK 配对与分块顺序（`exporter_stream_test.go`）、分片字节队列的语义（`shard_stream.go`：任意写入切分后重组逐字节一致、块不超上限、队列满时背压、消费端停止后生产端被 `shard output consumer stopped` 解除阻塞、ctx 取消同样解除、Close 幂等且刷出尾部、队列深度按分片数均分且内存有界）| `make test` |
| `internal/import/*_test.go`（9 个文件）| 36 + 1 fuzz + 8 bench | 攒批字节上限与 65535 占位符、DEL/SQL 读取器回读、无上限记录读取、会话重连回放与空闲提交、位置映射列数校验、`inserted`/`updated`/`unchanged` 拆分（含 affected 读不到与超限钳制）；DEL 读取器重写以差分方式守住语义——保留改动前的 `parseLine`/`scanRows` 为参考实现，用 28 个手写用例 + 4000 行随机语料 × 6 套定界符配置逐字段比对，配 `FuzzDelReaderParse` 持续模糊，另有空 `--col-del` 的超时守卫用例（`del_reader_equiv_test.go`、`del_reader_test.go`）；`del_reader_bench_test.go` 用 2 万行、贴近导出器真实产物（全字段带引号、空串与 NULL 可区分、二进制列裸 `0x`、引号内含 `||`/`;`）的确定性语料覆盖整行读取、纯分帧、纯解析三段 | `make test`（bench 用 `go test -run '^$' -bench . ./internal/import`，fuzz 用 `-fuzz FuzzDelReaderParse`）|
| `main_test.go` | 1 | go/ast 静态守卫：全部 logger 调用消息首字母小写且单行（防日志词汇回归）| `make test` |
| `test/integration/roundtrip_test.go` | 10 | 离线往返（写入器→读取器）：转义文本、每条语句一个物理行、`--statement-size` 攒批、`0x` 二进制字面量、MySQL 文本值与 HEX 一致、DEL 引号/反斜杠两种模式、看似十六进制的文本不被误解码、单行 INSERT | `make test` |
| `test/integration/live_mysql_test.go` | 5 | 真实 MySQL 端到端：三种格式往返、并行导出与并行导入、四种导入模式、列映射、导入行数拆分对真实服务端 `RowsAffected` 的断言；`--messages` 日志文件随用例断言（记录齐全、无 `SQL3xxxN`、每行都是日志） | `GOMYDUMP_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/db' make test-live`，未设置该变量时自动跳过 |
| `test/integration/option_matrix_test.go` | 12 | 编译出真实二进制后用 argv 跑交叉矩阵：DEL 布局修饰符的导出/导入成对性、`--no-col-names` 无表头配对歧义、逐字写入模式的拒绝与残留文件清理、SQL 攒批/单行 INSERT 与 `--statement-size` 上界、并行 DEL 逐字节等于串行而并行 SQL 只多分片边界的语句头（按头数与字节差精确断言）、日期时间格式的对称性与精度截断、`--no-data`/`--no-create` × `--mode create`、`--skip-count`/`--row-count` × 线程/批量/提交、`--messages`/`-m` 在三格式与并行路径下等于命令行日志（含 `-m` 短选项、`--log-format text`、`--log-level error`、失败记录）、`--no-replace` 哨兵文件存活、`--default-charset` 与二进制列 0x 自动切换 | 同上（`TestMain` 先 `go build` 一次，全部用例复用该二进制） |

`make test` 与 `make test-live` 固定 `CGO_ENABLED=1 go test -race`，这是 `-race` 的要求，与发布构建的 `CGO_ENABLED=0` 无关。

### 11.2 当前环境尚未自动化覆盖的场景

| 场景 | 现状 |
|------|------|
| OceanBase 端到端 | 沙箱内无 OceanBase 实例。仅覆盖到 SQL 层的 OceanBase 分支单测（如 OceanBase 下不写表锁的合并用例），完整链路未验证 |
| GB 级大文件 | 由 8 个 benchmark 覆盖单点（记录读取器、DEL 整行读取/分帧/解析、攒批），属微基准；8.1 的大文件与吞吐是目标值，没有端到端自动化断言，需在真实环境按压测计划验证 |
| 长时间稳定性 / 断连恢复 | `internal/import/session_test.go` 用模拟重连覆盖回放逻辑，真实网络中断（LB/NAT 回收）未做长时间实验 |

---

## 十二、交付物

| 交付物 | 说明 |
|--------|------|
| 源代码 | 完整 Go 项目源码（含 29 个 `_test.go`、`docs/requirements.md`、`docs/cli-oracle.txt`），发布形态为 `gomydump-<version>-<commit>[-dirty]-src.tar.gz`，排除 `.git/`、`bin/`、`dist/`、`vendor/` |
| 二进制文件 | Linux amd64、Windows amd64 两个平台；均 `CGO_ENABLED=0` 静态链接、`-s -w` 去符号，由 `make build-all` 产出，版本信息经 ldflags 注入。Linux arm64 与 macOS 不在发布矩阵内，按 README 的单平台命令自行编译 |
| 校验清单 | 每个发布批次附 `*-bin-SHA256SUMS.txt`，逐产物一行 SHA256 |
| Dockerfile | 容器构建（`make docker`）|
| README.md | 使用说明；本文件为需求与实现说明 |

自 2026-10-23 起**不提供 macOS 预编译产物**（见变更记录 33），同日进一步**只保留 amd64**：Linux arm64 也不再随包发布（见变更记录 37）。工具是纯 Go 无 CGO，这些平台拉源码后自行编译即可——macOS 执行 `make build`，Linux arm64 用 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o gomydump .`。

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

### 2026-10-22 本轮改动

| # | 改动项 | 说明 |
|---|--------|------|
| 1 | `--statement-size` 默认值调整 | 从 16 MiB（16777216 字节）降为 **8 MiB（8388608 字节）**，更贴近典型 `max_allowed_packet` 设置，降低导入侧重建语句时的内存峰值 |
| 2 | 导入攒批新增字节上限 | 新增 `batchcap.go`：导入侧按 `max_allowed_packet` 的 9/10 设定每批字节预算（下限 64 KiB），同时按 `rows × columns ≤ 65535` 下调有效 `batch-size`。补上了宽表/BLOB 大行在导入侧可能触发 error 1153 的缺口 |
| 3 | 补齐导出/导入转义与换行往返测试 | SQL 格式转义规则完整覆盖 `\n`/`\r`/`\t`/`\\`/`\0`/`\x1a`/`''`，确保每条 INSERT 恰好占一个物理行；导入侧 `recordReader` 替代 `bufio.Scanner`，无 token 长度上限 |
| 4 | 新增 `--single-row-insert` | SQL 格式专属选项，开启后每行一条 INSERT 语句（等价 mysqldump `--skip-extended-insert`），`--statement-size` 退化为告警阈值 |
| 5 | 并行导出 header/footer 统一 | `mergeShardFiles` 由 SQLWriter 统一写出 SET NAMES、CREATE TABLE、LOCK/UNLOCK TABLES；各分片片段设置 `SetSkipTableLocks(true)` 且 `writeCreate=false`，不再各自写 LOCK/UNLOCK |
| 6 | DEL 导入解码 `0x` 二进制字面量 | 此前 DEL 导出在 `--hex-blob`（或非 utf8mb4 自动开启）下写出 `0x…`，但 DEL 读取端无列类型信息，按原文返回字符串，往返不还原字节。现由 `columnInfoFor` 在同一次 `information_schema` 查询中同时取回列类型与二进制标记，`DelReader.SetBinaryColumns` 据此解码；判定要求"无引号 + 目标列二进制 + 合法偶数长度十六进制"，因此文本列中的 `0xdead` 保持原样 |
| 7 | 补齐连接加固三件套 | 新增 `common/dberr.go`（瞬态错误判定）、`common/retry.go`（3 次尝试、1s→2s 退避、上限 4s、20% 抖动）、`common/pool.go`（导入池 1m 空闲 / 5m 生存；导出池不做老化以免快照被回收）、`import/session.go`（事务、重连回放、30s 空闲提交）。修复单线程导入 `rowCount % commitCount` 在批大小不能整除时永不提交的缺陷，改为按事务内累计行数判断；重连后回放整个未提交事务而非仅失败批次，避免丢行；`COMMIT` 永不重试 |
| 8 | 分片边界改为精确运算 | `shard.go` 不再经过 float64：用 `math/big.Rat` 计算边界，整数键保持整数文本（继续命中索引），小数键按 30 位精度输出；相邻分片共享同一界值字符串，`BIGINT`/`DECIMAL` 超过 2^53 时不再静默丢行 |
| 9 | DEL 多字节分隔符/转义修复 | 写端按完整分隔符前缀匹配转义；自定义转义符为多字节时整体双写；读端消费完整转义序列（分隔符/转义符/单字节），多字节 `--char-del` 配合 `--escape` 可正确往返 |
| 10 | 回放缓冲增加字节上限 | `import/session.go` 除 `--commit-count` 外再按 64 MiB 字节预算提前提交，宽 BLOB/TEXT 行不会把重放缓冲撑大，同时也缩短断线重放窗口 |
| 11 | 真实 MySQL 集成测试 | 新增 `test/integration/live_mysql_test.go`（`GOMYDUMP_TEST_DSN` 门控，未设置时跳过）：DEL/SQL/Parquet 往返、insert/insert_update/replace/create、列映射、并行导出/导入、消息文件；`make test-live` 一键执行 |
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
| 23 | 参数错误统一退出码 2 | 补齐三类漏网：未知标志的取值错误（`--threads=abc`）、多余位置参数、未知子命令。经 `SetFlagErrorFunc` + `cobra.NoArgs` + `main.go` 归类，与用法错误共用退出码 2；运行期错误仍为 0/1，`gomydump` 无参数仍返回 0 |
| 24 | `--session-var` 取值非法按用法处理 | `--session-var foo`（无 `=`）此前是运行期错误退出 1，现包装为用法错误退出 2 |
| 25 | `insert_update` 无唯一键时告警 | 无主键/唯一索引时 `ON DUPLICATE KEY UPDATE` 退化为普通 INSERT，重复导入会静默翻倍。导入开始前查 `information_schema.STATISTICS`，命中则写 WARN 日志并在消息文件追加 `SQL3107N` |
| 26 | `--no-char-del` 写出前校验 | 读端只在**引号内**处理转义，因此 `--no-char-del`（或空字符定界符）时每个字段都是裸文本，含列分隔符/行分隔符的数据会产出无法解析的文件（实测 `sql: expected N arguments` 或整行错位）。现与 `--no-escape` 共用写出前校验，报 `--no-char-del: column N value contains the ... delimiter` 并中止 |
| 27 | Parquet 导出限制行组大小 | parquet-go 的行组上限默认是 `math.MaxInt64`，写端会把**整个结果集**缓存在内存：1M 行 10 列表实测峰值 RSS 271 MB 且随行数线性增长。现固定每行组 100 000 行，同一导出 RSS 降到 ~73 MB 且与行数无关，文件体积仅增 0.02% |
| 28 | `make test` / `make test-live` 可用 | 两个目标都带 `-race`，而 `-race` 需要 cgo；此前沿用发布构建的 `CGO_ENABLED=0`，导致 `make test` 在任何环境都直接失败（`-race requires cgo`）。测试目标固定 `CGO_ENABLED=1`，发布构建不变 |
| 29 | DEL 专属选项改为显式报错 | 此前 `-f sql --col-del '@' --no-char-del --date-fmt ...` 与 `-f parquet --col-del '\|' --hex-blob` 都退出 0 并静默忽略这些标志，而 SQL 专属选项（`--no-create`/`--statement-size` 等）早已有强制校验，两类格式专属标志的不对称会误导用户以为文件按指定布局写出。新增 `validateDELOnlyOptions`：`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-escape`/`--timestamp-fmt`/`--date-fmt`/`--no-char-del`/`--no-col-names` 在导出与导入的 `--format sql`/`parquet` 下一律报 `can only be used with --format del` 并退出 2（默认值不算显式传入，故不影响既有命令）；同时新增 `validateParquetBinaryOptions`，`--hex-blob`/`--no-hex-blob` 与 `--format parquet` 组合退出 2——Parquet 二进制列已是原生 `binary`，开关无意义，而 SQL/DEL 仍正常使用它们 |
| 30 | 日志一条记录一行、切布局不改级别 | 两处不一致。① `java` 布局只给含空格/制表/换行的值加引号，且不转义换行本身：配置行打印 DEL 定界符时 `row_del=` 后面是真实换行，一条记录被拆成两行，`char_del="` 与 `escape="` 又是裸双引号，后续 token 无法切分。现统一按 slog 规则转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`），一条记录恒为一个物理行，`java`/`text` 对同一值渲染出同一 token。② `currentLevel()` 只探测 debug/warn/error，`ApplyLogFormat()` 在 `PersistentPreRunE` 里重设布局时把 info 误判为更高级别，抬高阈值后文档承诺的版本横幅 `gomydump starting` 以及任何早于子命令 `InitLogger` 的 INFO 记录被静默丢弃（实测 export/import/version 三种运行均无首行横幅）。补 `LevelInfo` 探测，横幅恢复为运行首行；新增 `internal/common/log_test.go` 固化单行、转义、级别保持、横幅四项行为 |
| 31 | `--log-format` 非法取值改为用法错误 | `internal/common.ValidLogFormat` 与 `cmd/validate.go` 的 `validateLogFormat` 此前无调用方，`--log-format bogus` 与 `GOMYDUMP_LOG_FORMAT=javaness` 都退出 0 并静默回落 java 布局，与其它参数不一致（`log.go` 的注释还声称会退出 2）。现把它接进 root 的 `PersistentPreRunE`（在应用布局之前校验），非法取值报 `invalid --log-format %q: must be one of java/text` 并退出 2；`cmd/cli_matrix_test.go` 补标志与环境变量两条用例，并修正测试夹具：`logFormat` 是包级标志变量，pflag 只在显式传入时写入，必须逐例复位，否则一条非法取值会污染后续所有用例 |
| 32 | 日志消息统一为小写 `<对象> <事件>` | 原先同一份日志混着三种写法：`Export completed`/`Query failed` 大写开头、`export configuration`/`gomydump starting` 小写、`Starting export`/`Executing CREATE TABLE statement` 用动名词或 SQL 关键字，大小写敏感的 grep 会漏掉一半记录；而且 `Starting export`/`Starting import` 与紧随其后的配置行字段完全重复。现将全部 56 处 logger 调用（50 个不同消息）统一为小写单行短语，事件词固定为 started/config/progress/completed/aborted/failed（`export command started`→`export started`，`export configuration`→`export config`，`Thread completed`→`worker completed`，`Executing CREATE TABLE statement`→`create table executing`，余同），删除重复的 `Starting export`/`Starting import`（导入的 `bytes` 字段并入 `import config`）；`gomydump starting` 与 `command failed` 保持原样以免破坏既有文档与告警脚本。新增 `main_test.go` 用 go/ast 静态检查所有 logger 调用的消息首字母小写且单行，防回归 |

| 33 | 发布产物去掉 macOS 架构 | 按要求，打包矩阵不再包含 darwin。`Makefile` 删除 `darwin` 目标、`build-all` 改为 `linux windows`、`.PHONY` 与两处 upx 通配去掉 `-darwin-*`；`bin/gomydump-darwin-*` 一并清理。工具本身仍是纯 Go、`CGO_ENABLED=0`，在 macOS 上 `make build` 照常可用，只是不再随包发布预编译产物。README 构建说明与本文 §8.5「跨平台」行同步为"预编译产物覆盖 Linux / Windows" |

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
| 34 | 需求文档按实现全量核对后修订 | 以 `--help` 输出、`cmd/validate.go`、`internal/*` 常量与 `main.go` 的退出码分派为准逐项核对：全部 49 个选项在本文与 README 均有落点（无遗漏、无废弃选项），数值边界（port 1–65535、threads 1–256、batch-size 1–100000、pipeline-buffer 0–1024、statement-size ≤268435456、commit-count ≥1、skip/row-count ≥0）与 `docs/cli-oracle.txt` 快照一致。据此修订：1.3 技术栈补齐真实版本（`go.mod` 的 `go 1.26.9`、mysql v1.9.3、cobra v1.10.2、vitess v0.24.5、parquet-go v0.26.4、pkg/errors v0.9.1）并说明超时经驱动字段注入；二节开头补子命令别名 `exp`/`imp`、`-?` 帮助短选项与 completion 禁用；新增 2.4 `encrypt`（`ENC:` + AES-256-GCM + `GOMYDUMP_KEY`，含解密调用点与失败退出码实测）；3.3.4 退出码补判定口径（含环境变量失败的分流与位置参数个数的归类）；九节项目结构补 `main.go`、`main_test.go`、`usage.go`、`stats.go`、`test/`、`docs/`、`go.sum`；十一节新增 11.1 测试资产清单（24 文件 / 127 `Test*` / 4 `Fuzz*` / 5 `Benchmark*`）与 11.2 未自动化覆盖场景；十二节交付物改为三平台二进制 + 源码包 + SHA256 清单，并显式声明不再提供 macOS 预编译产物。README 另修两处遗留目录名 `mysql-backup-tool` → `gomydump` |
| 35 | `encrypt` 位置参数个数错误改为退出 2 | `gomydump encrypt`（缺参数）与 `encrypt a b`（多参数）此前由 cobra `ExactArgs(1)` 拦截，返回普通 error，实测退出 1，违反"用法错误 → 2"的契约（同类的未知选项、未知子命令、缺必填项都是 2）。`cmd/validate.go` 新增 `exactArgs`，用 `common.Usagef` 报 `gomydump encrypt accepts 1 argument(s), received N`，`cmd/encrypt.go` 换用它；新增 `cmd/cli_matrix_test.go:TestCLIEncryptArgsCountIsUsageError`，除两种越界外还断言正确参数下密钥缺失仍是运行时错误 1，避免把两类错误合并。实测：无参/两参 → 2，正常加密 → 0，空 `GOMYDUMP_KEY` → 1 |
| 36 | 补齐值处理与并行降级两处长年缺失的说明 | 新增 5.4「三种格式共有的值处理」：`YEAR(0000)` 归一化为 `0000`（文本协议回裸 `0`，回导会被两位 YEAR 规则解释为 2000）、NULL 与空字符串在 DEL/SQL/Parquet 下的表示、`--no-char-del` 时二者不可区分且回读一律为 NULL（均已在 MariaDB 上实测确认）。7.2 补「并行导出的降级触发点」表（4 个 warn 触发点 + parquet 的 info 分支，附实际日志文本与代码行号）与复合键/非数值首列语义。同时修正一处过期引用：十四节边界表里 parquet 降级的消息仍写作大写 `Parallel export is not supported for parquet format`，实际已是小写且级别为 `info`。README 对应补：无包裹符模式的 NULL/空串注意、并行导出的降级触发点与复合键说明、格式示例后的「三类格式共有的值处理」小节，并把遗留的 `见 3.6` 引用改为需求文档 3.3.4 |
| 37 | 发布矩阵收窄为 amd64 | 按要求，预编译产物只保留 Linux amd64 与 Windows amd64。`Makefile` 的 `linux` 目标去掉 `GOARCH=arm64` 那行（`build-all` 由 3 个二进制降为 2 个），注释同步；README 构建说明与本文 §8.5「跨平台」、§12 交付物改为两平台，并给出 Linux arm64 的单平台编译命令。本批次的 `*-bin-SHA256SUMS.txt` 相应只列 2 个二进制 |
| 38 | 导入行数拆分新增 `unchanged` 计数 | 实测发现 `insert_update` 重复导入同一文件时（服务端 `RowsAffected=0`）旧推导报 `inserted=4 updated=0`，而真实是 0 新增 0 改动 4 行相同；根因是 `updated = affected - rows` 再钳 0 的推导隐含"每行至少贡献 1 个 affected"。`internal/import/importer.go:addBatchResult` 改为互斥三分：`updated = max(0, affected-rows)`、`unchanged = max(0, rows-affected)`、`inserted = rows-updated-unchanged`（另对 affected 超过 2×rows 的异常值把 `updated` 钳到 `rows`），三数之和恒等于批行数；`internal/common.Stats` 随之增 `AddUnchanged`/`Unchanged`，五处计数日志（`import progress`、结构-only 完成、`import completed`、`import aborted`、`parallel import completed`）统一加 `unchanged` 字段。同时以 go-sql-driver 实测确认 `insert`/`create`/`replace` 三种模式 affected 恒等于行数（`replace` 先清空目标表再插，故每行都是新增），推导对它们是恒等变形。新增 `internal/import/counters_test.go`（4 个测试：11 例拆分表、affected 不可读与负值、跨批累加、`Stats()` 未初始化零值）与 `test/integration/live_mysql_test.go:TestLive_ImportCounters`（真实服务端 5 个场景，配套 `importFileStats` 辅助与 `Importer.Stats()` 只读访问器）。实测：相同内容重导 `inserted=0 updated=0 unchanged=4`，全部改动 `updated=4`，删 2 行重导 `inserted=2 unchanged=2`，replace `inserted=4`。8.3「导入行数」与 README 的行数统计说明、修复记录同步 |
| 39 | `version` 子命令不再写日志 | 此前 `gomydump version` 先在 stderr 打一条 java 布局的 `gomydump starting ... command=version`，再打四行版本信息，内容完全重复，且 `--log-format text` 也会改变首行形状，导致 `gomydump version \| grep` 之类的取版本管道要先过滤日志。`cmd/root.go` 的 `PersistentPreRunE` 把 `common.LogVersion` 改为对 `cmd.Name() == "version"` 跳过，其余子命令（`export`/`import`/`encrypt`/`help`）首行横幅不变。新增 `cmd/version_log_test.go` 两个用例：一个断言 version 的 stderr 为空且四行齐全、输出中不含日志记录，一个反向断言 `encrypt` 运行时横幅仍是 stderr 首行（防止把例外扩成全量禁用）。README 版本说明与本文的日志事件表、8.3「日志顺序」同步 |

### 2026-10-23 DEL 读取路径性能改写

| # | 改动项 | 说明 |
|---|--------|------|
| 40 | DEL 导入解析改为全程 `[]byte`、去掉无效字节↔字符串转换 | 性能分析（pprof CPU + heap，4 核 / 本地 MariaDB，`bench` 表 50 万行 DEL 125 MB）显示导入 CPU 的约 60% 落在 DEL 分帧与字段解析上，堆分配量 1392 MB 是输入体积的 11 倍，其中 `parseLine` 479 MB + `recordReader.Text()` 121 MB。改写 `internal/import/del_reader.go`：① 四个定界符在 `NewDelReader` 里预计算成 `delSpec`（`[]byte` + 长度 + 首字节），`scanRows`/`parseLine` 逐位置比较改为定长比较；② `ReadHeader`/`ReadRow` 用 `recordReader.Bytes()` 取代 `Text()`，取消整行 `string()` 物化；③ 无转义字段直接一次 `string(line[start:end])`，含转义字段用复用的 `scratch` 拼装，`fields`/`quoted`/`scratch` 在 reader 内跨行复用；④ 门控空 `--col-del` 分支（`hasCol`）——CLI 已拒绝空定界符，但库内直接构造 `DelReader` 时旧实现会因空前缀恒命中而不返回，新实现把整行作为单个字段返回。值类型仍保持 `string`——装箱到 `driver.Value` 对 `[]byte` 同样分配且更宽（`convTslice` 24 B vs `convTstring` 16 B），换类型不会减少分配。改写过程由差分测试暴露并修复两个引号边界缺陷：闭合定界符被留在值末尾（`TestDelReader_LargeRecord` 长度 +1）、双写转义后未把干净片段并入 `scratch` 导致 `Alice ""The Great""` 解析为 `Alice ""`。新增 `del_reader_equiv_test.go`（保留改写前的 `parseLine`/`scanRows` 作参考实现，28 个手写用例 + 4000 行随机语料 × 6 套定界符配置逐字段比对 + `FuzzDelReaderParse` 差分模糊，25 s 246,417 次执行无发现）与 `del_reader_bench_test.go`（2 万行贴近导出器真实产物的确定性语料，`ReadRow`/分帧/纯解析三段基准）。实测：微基准 89.4→31.4 ms（2.8×）、31.6→9.3 ms（3.4×）、45.8→18.5 ms（2.5×），`ReadRow` 分配 757,137→440,256 次/操作、驻留 26.8→10.5 MB/操作（同一语料在 HEAD 工作树与新树上背靠背各测两轮，取其一）；端到端 `--threads 1` 6.81→5.70 s、客户端 CPU 8.02→3.76 s，t4 5.61→3.64 s，t1→t4 加速比 1.21×→1.87×。6 组定界符/转义/十六进制标志组合的真实往返逐行一致（其中一组暴露上表的 `--escape "''"` 既有缺陷）。`make test`（`-race`）、`make test-live`、`go vet ./internal/...`、`gofmt -l` 全通过。新增测试资产使全仓库规模升至 28 文件 / 139 `Test*` / 5 `Fuzz*` / 8 `Benchmark*`，11.1、11.2、九节、十二节的计数同步更新，7.5 增补「DEL 解析路径」小节 |
| 41 | 并行导出去掉分片临时文件，改为流式写出 | 承接 40 之后的性能分析：并行导出每个分片先写 `<output>.shard_<i>.tmp`，全部分片结束后再 `io.Copy` 拼接，**每个字节落盘两次**（实测 DEL 输出 124,903,023 B 对应写入量 487,920×512 B ≈ 238 MB，SQL 131,872,590 B 对应 515,152 块 ≈ 251 MB），并行导出因此需要两倍输出的空闲空间，且被信号打断时输出目录残留整批临时文件（实测 4 个共 33 MB）。新增 `internal/export/shard_stream.go`：每个分片序列化到自己的 `shardPipe`（256 KiB 块，块深由 64 MiB 总预算按分片数均分、上界 64 块下界 2 块），`exporter.go` 的 `mergeShardFiles` 改为 `writeShardStream`，在分片仍在产出时按分片序号依次消费并写进最终文件，不再创建任何临时文件；分片失败或写文件失败（如 `ENOSPC`）时删除已写出的不完整输出并警告 `incomplete output file removed`，其余阻塞在队列上的分片由 `shard output consumer stopped` 解除（不会挂死，ctx 取消同路径）。队列深度实测是关键：SQL 格式每分片 2.5 MiB 时墙钟 1.31 s、8 MiB 时 1.24 s，对旧实现 1.09 s（慢 14~20%，写合并 goroutine 反复等已经停下的生产端），16 MiB 回到 1.09 s 持平，故取预算值而非常数；代价是在途字节从磁盘移到堆上，4 分片峰值 RSS 约 33→111 MB（DEL）、114→215 MB（SQL）。验收：与旧二进制在同一库同一时刻并行导出的产物**逐字节一致**（DEL/SQL），写入量减半（DEL 487,920→243,952 块、SQL 515,152→257,568 块），单线程导出产物不变，导入回读的行数与顺序无关 CRC 和与原表一致，32 MB tmpfs 上的 ENOSPC 用例失败后不留任何文件，7 次交替 A/B 取中位数。测试侧 `exporter_merge_test.go` 改为 `exporter_stream_test.go`（同一组 header/LOCK/前导断言改走队列）并新增分块顺序用例，`shard_stream_test.go` 以 9 个用例覆盖队列与流式语义；全仓库测试资产升至 29 文件 / 149 `Test*` / 5 `Fuzz*` / 8 `Benchmark*`，九节项目结构、11.1 计数同步，7.2 并行导出流程改写并新增失败与中断语义段落，README「并行导出」补充说明 |
| 42 | 交叉选项 / 交叉命令行为矩阵，并修复它暴露的两个数据完整性缺陷 | 此前 CLI 侧只有 `cmd/cli_matrix_test.go` 钉住"哪些组合被接受或被拒"，库侧只有 `live_mysql_test.go` 走 API；**"选项被接受之后是否真的生效、导出与导入两侧是否对称"没有覆盖**。新增 `test/integration/option_matrix_test.go`（12 个测试入口、`TestMain` 一次性 `go build` 出真实二进制后用 argv 驱动，故 flag 解析、校验、config 装配、写入器、读取器全程在链路上）：DEL 布局修饰符成对往返（`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-char-del`/`--no-escape`/`--no-col-names`，含 `\t`、`\r\n` 转义写法）、逐字写入模式对不可表示数据的拒绝、SQL 攒批与单行 INSERT 的语句数/字节上界、`--threads` 的输出确定性、`--date-fmt`/`--timestamp-fmt`、`--no-data`/`--no-create` × `--mode create`、`--skip-count`/`--row-count` × 线程/`--batch-size`/`--commit-count`、`--messages` 在三格式与并行路径下的 SQL3104N/SQL3110N、`--no-replace` 哨兵存活、`--default-charset` × 二进制列 0x 自动切换。矩阵首轮跑出 3 个真缺陷：① **单线程导出失败留下半截产物**（20 万行表被 `--no-char-del` 拒于末行时残留 21,233,664 B 文件，并行路径早已删除），修复为 `exportSingle` 在 `os.Create` 之后注册共用的 `removeIncompleteOutput`（`exportParallel` 改为复用同一方法），注册点在处理之后保证创建前失败不会误删既有文件；② **无表头文件配位置 `-c` 静默丢首行**（`--no-col-names` 导出 + 只给 `-c` 导入时 3 行只进 2 行且退出 0，同一命令用于带表头文件又是对的），因文件自身无法区分两者、改名导入也走同一路径，故不改结果只补齐可见性：被当表头丢弃的那行若与目标表列名全不匹配，记 warn 并写入消息文件，说明加 `--no-col-names` 才不漏行；③ `--columns` 缺失时的报错只提示 `-c`，未说明无表头需成对使用，已在 7.4 与 README 的标志表补"成对使用"。同时确认三项**属既有行为而非缺陷**并写进文档：并行 SQL 输出在分片边界多出 `threads-1` 个 INSERT 头（数据一致，DEL 则逐字节一致，已按头数与字节差精确钉住）、`--timestamp-fmt` 省略 `ss` 会把秒截为 00（用户选定的精度，现固定其余字段不得变动）、非 utf8mb4 连接字符集对不可映射字符由服务端替换为 `?`（文本有损、二进制因 0x 自动切换无损）。另记录两项待用户定级的可用性问题：认证/字符集/库名这类永久错误被 ping 重试 3 次并 6 s 才失败，且错误前缀 `failed to ping database after 3 attempts` 掩盖真实原因；`--no-create` 文件配 `--mode create` 直接撞服务端 1146 而非事先提示文件不含建表语句。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产升至 30 文件 / 161 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`，九节项目结构、11.1 清单、7.2 失败与中断语义、7.4 位置映射、5.3 日志表、README 并行导出与 `--no-col-names` 说明同步 |
| 43 | `--messages` 改为命令行日志文件，删除 DB2 风格消息报告 | 用户指出 `--messages` 提供的应当是**命令行的日志打印**，而不是 `SQL3xxxN` 样例日志，且带上该选项后所有日志都要进文件。实现落在日志层而不是命令层：`internal/common/log.go` 新增进程级 sink（带互斥锁；`os.Stderr` 在 `Write` 时解析而非构造时捕获，因此既有的 stderr 重定向测试与 `--echo-sql-values` 行为不变），`java` 与 `text` 两种布局都改经该 sink，`OpenLogFile(path)` 把文件挂到 sink 上（同一条记录写完 stderr 再写文件）、`CloseLogFile()` 摘下并关闭；根命令 `PersistentPreRunE` 在应用 `--log-format`、打印版本横幅**之前**打开文件，故横幅恒为文件首行、用法错误也进文件，`cmd.Execute()` 在记录完 `command failed` 之后关闭，故收尾错误同样在文件里。删除 `internal/common/message.go`（`MessageWriter` 及其 `SQL3104N/3105N/3107N/3109N/3110N` 与 `# Completed/# Duration/# Rate` 页脚）与 `message_test.go`，导出器 15 处、导入器 20 处（含 `batchcap.go` 2 处）消息调用全部移除：每条告警与失败本来就有对应的 `common.Logger` 记录，失败还会由 `Execute` 统一记成 `command failed`，因此不丢信息，只去掉重复的第二份；`SQLWriter.SetWarningFunc` 与 `Exporter.warningFunc`/`messageTableName` 一并删除。选项语义变更后 `-m` 短选项沿用（`export`、`import` 两侧均已注册），帮助文本改为「Write every log record to this file as well as to stderr」；`--quiet` 现在同样作用于文件（不再有「消息文件是显式产物」的例外）；`config.ExportConfig.Messages`/`ImportConfig.Messages` 随实现移除，日志文件是进程级 CLI 关注点而非库配置项。实测：`-m` 导出失败时文件与 stderr `diff` 逐字节一致（4 行含版本横幅、`export config`、`query failed` 真实原因与 `command failed` 包装）；`--log-level error` 下成功导出文件只剩横幅；`--log-format text` 布局随 stderr 一起切换。测试：`live_mysql_test.go` 的 `--messages` 断言改为经 `common.OpenLogFile` 走同一 sink，并断言每条告警与失败在日志里都有对应记录、文件里不再出现 `SQL31` 码；`option_matrix_test.go` 的 `--messages` 用例重写为 5 种格式/并行组合下「文件等于命令行日志」（首行为横幅、含 `export config`/`export completed`/`rows=10`、每一行都出现在 stderr 上），另加 `-m` 短选项、`--log-format text`、`--log-level error`、失败记录四个子用例。文档：1.2、导出/导入标志表、2.3 整节改写为「日志文件（`--messages` / `-m`）」、3.3.1 静默、四节 DB2 映射（显式标注语义不同）、导出导入流程步骤、5.3 功能表、九节结构与测试文件计数、11.1/11.2 清单同步，README 同步；`docs/cli-oracle.txt` 保持冻结基线并在头注登记这一处有意差异。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 29 文件 / 161 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`（`internal/common` 少一个文件、少 5 个 `MessageWriter` 用例，新增 5 个 sink 用例：文件与 stderr 逐字节相等、闭包后脱钩且二次关闭无错、早建好的 text 布局也跟随 sink、200 条并发记录不拆行、替换路径只写最新文件、创建失败时报出路径且不影响 stderr） |
