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
| 日志 | 标准库 log/slog | 结构化日志，双布局（`console`/`text`），见 3.3.2 |

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
| `--progress-interval` | | 定时进度日志的间隔（下限 **100ms**：0 与负值报 `must be > 0`、低于下限报 `must be >= 100ms`，均为用法错误退出码 2；`--quiet` 是不打印进度，二者语义不同） | `5s` | 否 |
| `--no-overwrite` | | 拒绝覆盖已存在的输出文件（默认行为为覆盖，即 `os.Create` 截断；原名 `--no-replace`，2026-10-26 改名，不留别名） | `false` | 否 |
| `--batch-size` | | 读→写管道每批行数（仅作用于**单线程导出**与**导入**；并行导出为分片内同步 scan→write，不受影响，因此 `export` 里与 `--threads>1` 同时**显式**给出即为用法错误、退出码 2，见 7.2；直接驱动库的调用方仍只收到一条 `parallel export ignores pipeline options` 的 info） | `1000` | 否 |
| `--pipeline-buffer` | | 读→写管道队列深度（批数为单位，0 表示同步交接；仅单线程导出，`export` 里与 `--threads>1` 同时显式给出即为用法错误、退出码 2；`import` 侧不受此限制） | `10` | 否 |
| `--hex-blob` | | 将二进制列编码为 `0x` 十六进制字面量（非 utf8mb4 字符集时自动启用）；作用于 DEL/SQL 文本格式，`--format parquet` 下显式传入报退出码 2 | `false` | 否 |
| `--no-hex-blob` | | 强制原始二进制透传，即使在非 utf8mb4 字符集下（可能导致二进制数据损坏）；与 `--hex-blob` 互斥；`--format parquet` 下显式传入报退出码 2 | `false` | 否 |
| `--parquet-compression` | | Parquet 页压缩算法（`none` / `snappy` / `zstd`，见 5.3）；仅 `--format parquet`，其它格式下显式传入报退出码 2。**导入无需对应选项**：编解码器写在每列块的 footer 元数据里，读端按 footer 解码 | `none` | 否 |
| `--threads` | | 并行导出线程数（1=不并行，范围 1–256） | `1` | 否 |
| `--shard-key` | | 分片键列名（空=自动检测主键/唯一键；**只有并行导出读取并校验它**：显式给出的列在表里不存在时为用法错误、退出码 2，自动检测失败仍回落单线程；`--threads 1` 或回落到单线程时该值不起作用，运行记 `warn` 并在配置记录标 `shard_key_ignored=true`） | 空 | 否 |
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
| `--extended-insert` | 把多行合并进一条 INSERT（等价 mysqldump 的默认布局），按 `--statement-size` 的字节预算分组；若该预算低于单行语句宽度则一条也合不成，产物退回每行一条 INSERT 并记一次告警 | `false` |
| `--create` | 在 SQL 文件头部输出 `CREATE TABLE` 语句（SQL 导出默认只导数据，需自带建表语句时才加；仅对 `--format sql` 生效，其他格式传入报用法错误退出 2；`SHOW CREATE TABLE` 按 `--query` 点名的 schema 查找，写出的语句恒为裸表名） | `false` |
| `--no-data` | 不输出 INSERT 语句（仅导出表结构，须与 `--create` 同时给出才有内容） | `false` |

**SQL 布局默认值说明：**

- SQL 导出默认**只写数据**（`INSERT`），不再默认附带 `CREATE TABLE`；要自带建表语句须显式加 `--create`。这一改动的动机是数据文件更通用（导入到已存在的目标表即可），而把建表当默认会让"导出→换个库导入"这类最常见路径带上可能冲突的 DDL。`--no-create` 已随默认值反转一并删除：结构既然要 `--create` 才写，反向开关唯一剩下的作用就是否决 `--create`，于是它不再注册，传入即 `unknown flag: --no-create` 退出 2。
- 默认布局是每行一条 INSERT（mysqldump `--skip-extended-insert` 的形态），每条语句恰好占一个物理行，文件可按行切分。
- 攒批改由导入侧承担：导入器把语句拆成行后按 `--batch-size` 重新组批，与文件布局无关。实测 20,000 行、`--batch-size 1000`，单行文件（20,000 条语句）与合并文件（1 条语句）在目标库 general log 里都是 20 条服务端 INSERT。
- `--extended-insert` 换到的是体积而非速度：100 万行窄表实测单行 2.5 s / 峰值内存 33 MB / 278,790,434 B（gzip 28,247,005 B），合并 2.7 s / 70 MB / 153,792,809 B（gzip 27,050,101 B，19 条语句）。导入侧要一次性解析整条语句，同一份数据在 `--batch-size 1000` 下单线程峰值内存为 44 MB（单行文件）对 216 MB（合并文件）。
- `--extended-insert` 的合并有一个退化点：写入器的判定是"当前批非空且加入下一元组会超上限才 flush"，所以 `--statement-size` 一旦低于单行语句自身的宽度，每条语句就只能装 1 个元组，产物与默认单行布局逐字节相同。实测（本地 MariaDB，`bench_lay` 20 万行 3 列，`-f sql`）：`--statement-size 64` → 17,224,453 B / 200,000 条语句（与默认单行布局逐字节相同，md5 `01cfcd0ba049a7194be925f1a490e389`），`400` → 8,690,869 B / 22,217 条，`1000` → 8,015,845 B / 8,154 条，默认 `8388608` → 7,624,501 B / 1 条（体积 -55.8%，gzip 后 1,859,386 B 对 1,677,322 B）。这种"要合并却一条也没合并"的情况现在由写入器在收尾时记一次 WARN（累计 ≥2 行且从未合并才报，单行文件不误报），与默认布局那条"单行超限只告警一次"对称。
- 两个选项均仅 `--format sql` 可用；在非 SQL 格式下显式传入会报错。曾与 `--extended-insert` 互斥的 `--single-row-insert` 已删除：自默认布局改为每行一条 INSERT 起它就不改变任何行为，传入现在是 `unknown flag: --single-row-insert` 并退出 2。

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
- 只有 `--create` 时才接着写 `-- Table structure for <table>` 注释与 `CREATE TABLE` 语句，然后才是 `-- Data for <table>` 与 `INSERT`
- **不写 `LOCK TABLES` / `UNLOCK TABLES`**（变更记录 72）：旧实现在 MySQL 下输出这一对，且锁语句排在 `CREATE TABLE` 之前，于是产物回放到空库时第 2 行就 `ERROR 1146 (42S02) Table … doesn't exist` 中止——锁住的表还不存在。imex 自己的导入跳过前导语句故不受影响，但任何标准 `mysql` 客户端都读不了这份产物。现在无论单线程还是并行、无论有没有 `--create`、无论对面是 MySQL 还是 OceanBase，都不再生成这一对；文件首行之后直接是结构或数据

**SQL 格式的单表限制：** SQL 输出文件描述的是**一张真实表**（`CREATE TABLE <table>` + 指向该表的 `INSERT`）。因此 `--format sql` 要求 `--query` 为单表查询，解析阶段即拒绝 JOIN、隐式多表（`FROM a, b`）、派生表（`FROM (SELECT …) t`）与 `WITH` CTE，报错并提示改用 `--format del` / `--format parquet`（二者只写出结果集，不依赖表结构）。校验在输出文件创建之前完成，不会留下半成品文件。不带 `FROM` 的 `SELECT 1` 按 MySQL 惯例解析为隐式 `DUAL` 表，工具同样按单表处理（服务端因无 `DUAL` 表结构而报错属预期行为）。

**表名里的 schema 一律剥离：** `--query`（或 `--query-file`）里的表名可带 schema 限定（`` `db`.`tbl` ``）。工具解析出的**裸表名** `tbl` 才是写进 `CREATE TABLE`/`INSERT` 以及分片键探测（`information_schema` 的 `TABLE_NAME = ?` 且 `TABLE_SCHEMA = DATABASE()`）用的值；schema 限定符单独保留，仅用于 `SHOW CREATE TABLE` 的定位——查询点了 schema 就在该 schema 查，没点就在连接的 `--database` 查。修复的缺陷：旧实现把带反引号的 `` `db`.`tbl` `` 原样塞进 `` SHOW CREATE TABLE `%s` `` 与 `` INSERT INTO `%s` ``，等于再套一层库名，服务端报 `Error 1146: Table 'db.db.tbl' doesn't exist`（单线程取建表语句即中止），并行路径也会因 `TABLE_NAME` 对不上而退化；剥离后导出的文件恒引用裸表，可在任意目标库回放。

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
| `--progress-interval` | | 定时进度日志的间隔（下限 **100ms**：0 与负值报 `must be > 0`、低于下限报 `must be >= 100ms`，均为用法错误退出码 2；`--quiet` 是不打印进度，二者语义不同） | `5s` | 否 |
| `--batch-size` | | 读→写管道每批行数（范围 1–100000） | `1000` | 否 |
| `--pipeline-buffer` | | 读→写管道队列深度（批数为单位，0 表示同步交接，范围 0–1024），**按给定值执行**；队列与 `--threads` 正交（并行导入只有一条共享队列，深度是驻留、线程是并发），`深度 × 当批字节预算` 可能超过 **1 GiB** 时只记一条 `warn` 报价、不下调深度；库调用方给负深度按 `0` 处理 | `10` | 否 |
| `--statement-size` | | 单条生成 INSERT 的字节上限，取它与 `max_allowed_packet` 派生预算的较小值（**只压低、不抬高**；范围 0–268435456，`0`=不额外压低，见 7.4）；只有服务端报出**大于** 67,108,864 B（64 MiB）的包时，默认预算才再钳到驱动侧插值上限的 9/10 = **60397977** 字节（`limiting_factor=driver_packet_cap`，实测 67,108,864 仍记 `max_allowed_packet`、67,109,888 起才改记 `driver_packet_cap`）；测试床按 OceanBase V4.x 默认 `max_allowed_packet=16M` 时决定者是 `max_allowed_packet`（`batch_bytes=15099494`），显式写出更大的数字则保留原值并记一条 `warn` 说明代价 | `0` | 否 |
| `--threads` | | 并行导入线程数（1=不并行，范围 1–256） | `1` | 否 |
| `--log-level` | | 日志级别（debug/info/warn/error） | `info` | 否 |

**`--columns` 列映射说明：**

支持两种格式，通过检测 `=` 符号自动识别：

1. **位置映射**：`--columns "name,age,email"` - 文件列按位置映射到表列
2. **名称映射**：`--columns "user_name=name,user_age=age"` - 按列名指定文件列到表列的映射；只要求写出来的映射有效——可以只列文件中的一部分列（未列出的字段不导入），书写顺序也不影响结果（INSERT 列清单按文件列头顺序排列）

**位置映射的数量校验：** 位置映射按索引配对，因此 `--columns` 的条目数必须等于文件的列数（列名行给出的列数），条目本身不得为空，且同一目标列不得出现两次。数量不符时工具在写入任何数据前报错，提示改用名称映射导入子集。使用 `--no-col-names`（文件无表头）时列数无从得知，只校验条目非空，列数是否匹配交由服务端报错。

**名称映射的有效性校验：** 只要求写出来的映射有效，但写出来的每一条都逐条校验，且全部在读取任何数据行之前完成：① 文件列必须出现在文件的列名行中（大小写不敏感、忽略首尾空白），否则报错并列出文件实际的列名；② 同一文件列不得写两次；③ 同一目标列不得被两个文件列写入（服务端会以 `Error 1110 Column … specified twice` 拒绝，而 replace 模式此时可能已清表）；④ 文件没有列名行（`--no-col-names`）时名称无从匹配，报错并提示改用位置映射。未写进映射的文件列不导入，导入开始时记一条 `unmapped file columns are not imported`（列出这些列）；未写进映射的目标列由服务端填 DEFAULT/NULL/自增值，NOT NULL 且无默认值时仍由服务端报 `Error 1364`。位置映射不支持子集：它按索引配对，必须一列对一列（见上）。

**无表头文件与 `-c` 的配对（交叉测试暴露的歧义）：** DEL 读取器总是先调用 `ReadHeader`，只有同时给出 `--no-col-names` 才跳过这一步。于是"文件由 `--no-col-names` 导出、导入时只给 `-c`"的组合会把**首条数据行当成列名行丢掉**，且行数少一并不报错（实测 3 行文件只导入 2 行）；同一组合用于带表头的普通导出文件时行为又是对的。文件本身无法区分这两种情况，改名导入（表头列名与目标表列名不同）也走同一条位置映射路径，因此不做任何猜测、不改变导入结果，只把被丢弃的记录说出来：位置 `-c` + DEL + 未加 `--no-col-names` 时，若被当作表头的那一行字段与目标表列名**全不匹配**（目标表列名取自 `information_schema`），记一条 WARN 日志（`--messages` 打开时这条记录只进日志文件）——`the first record of the input file was treated as the column header and not imported (fields: …); none of them is a column of table "…", so if the file was exported with --no-col-names add --no-col-names to the import to load that record as data`。字段名能对上目标表列时（正常表头）不产生任何噪声，改名导入仍然成功、只是多一条提醒。结论：**`--no-col-names` 是成对选项，导出加了、导入也必须加**（`-c` 只补列名，不补"没有表头"这一事实）。

**目标列前置校验（DEL/Parquet 格式）：** 导入开始前，工具通过 `information_schema` 校验目标表存在以及每个目标列名在表中存在（大小写不敏感）。DEL/Parquet 文件的列头取自导出查询的结果集列名，若 `--query` 含未起别名的表达式列（如 `SELECT 1, COUNT(*)`），列头（`1`、`COUNT(*)`）在目标表中不存在，工具会在写入任何数据前立即报错并提示修复方式（为表达式列起别名，或使用 `--columns` 按位置指定目标列），替代原先晦涩的服务端错误 1054（`Unknown column ... in 'field list'`）。校验位于模式处理之前，replace 模式不再出现"先清空表、后报列错误"的数据丢失。SQL 格式不做该校验：create 模式在表建立前执行属正常流程，且其列名来自文件自身的 INSERT 语句。

**失败 SQL 回显（脱敏）：** 导入/导出语句在服务端执行失败时，错误信息与日志会附带失败 SQL 的结构回显（给了 `--messages` 时这条日志在文件里）（表名、列名、占位符完整保留）。数据值默认**不**内联：仅以 `/* args: str(14), num, NULL, bytes(256) */` 形式的类型/长度标签呈现（可定位"哪一列、什么类型、是否超长"，但不泄露密码、个人信息等真实数据）。回显在 2048 字节处截断且保持引号配平。需要内联真实值时显式开启 `--echo-sql-values` 或 `IMEX_ECHO_SQL_VALUES=1`（警告：真实数据将写入 stderr，或写入 `--messages` 文件）。`--query` SELECT 与 CREATE TABLE 语句不含行数据，失败时原文回显。

#### 2.2.4 导入模式（`--mode`）

| 值 | 说明 | 适用场景 |
|----|------|----------|
| `insert` | 追加数据到表，不修改现有数据 | 表已存在，追加新数据 |
| `insert_update` | 插入新行，或更新主键匹配的行 | 表已存在且有主键，同步数据 |
| `replace` | 删除表的所有数据，然后用**普通 INSERT** 写入导入的数据（不是 MySQL 的 `REPLACE INTO`） | 表已存在，完全替换数据 |
| `create` | 创建表并导入数据（仅 SQL 格式） | 表不存在，从 SQL 文件创建 |

**CREATE 模式的表名重写：** 当使用 `--mode create` 导入 SQL 文件时，工具会将 SQL 文件中 CREATE TABLE 语句的表名重写为 `--table` 参数指定的目标表名；`--table` 省略时自动沿用 SQL 文件中的原表名。

**CREATE 模式对“无建表语句”的预检（变更记录 76）：** 文件里没有 `CREATE TABLE` 语句时，`--table` 不能省略（否则报 `create mode requires --table …`）；给了 `--table` 时，工具在读取任何数据之前先探测该表是否存在，不存在则退出 1 并报 `create mode found no CREATE TABLE statement in the SQL file and table "…" does not exist in database "…": re-export with --create, or import with --mode insert into an existing table`。表已存在时照旧按 `INSERT` 导入，因此数据-only 文件配 create 模式在“表已存在”这一既有用法下不受影响；变化只在于失败从服务端的原始 1146（且已读过一批数据）提前到本地诊断（一条 INSERT 都不发）。

**replace 模式的实际语义（全量替换，不是 REPLACE INTO）：** `internal/import/importer.go:225` 的分支先 `SET FOREIGN_KEY_CHECKS=0`，再由 `deleteTable`（:1016）执行一条自动提交的 `DELETE FROM <表>`，随后数据行交由 `buildBatchInsertSQL`（:1071）拼成普通的 `INSERT INTO … VALUES (…),(…)`——该函数只对 `insert_update` 追加 `ON DUPLICATE KEY UPDATE`，全仓库没有任何一处拼 `REPLACE INTO`。因此它与 MySQL `REPLACE INTO` 的语义不同：① 文件里没有的行会被删除（`REPLACE INTO` 只替换主键/唯一键冲突的行，其余行保留）；② 文件内重复键在 `replace` 模式下触发 `1062` 报错（`REPLACE INTO` 会删除冲突行再插入、最后一条生效）；③ `DELETE` 在导入事务之外，之后的任何失败都会留下空表，需要重跑；④ 不触发 DELETE 触发器与 `ON DELETE CASCADE`（`FOREIGN_KEY_CHECKS=0`），`REPLACE INTO` 会触发。选择 `DELETE` 而非 `TRUNCATE`：后者隐式提交、不能回滚，且表被其它表外键引用时可能直接失败。行数统计也建立在这一语义上——表已清空，文件里每一行都是新插入，故 `affected == rows`、全部计入 `inserted`（:547 的注释）。`--help` 的 `--mode` 描述与 README 模式表已按此改写，明确写出"not REPLACE INTO"；需要按键覆盖且保留其它行时用 `insert_update`。

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

配置回显由 `NewExporter`/`NewImporter` 在**建立连接之前**打印，因此连不上服务端时参数照样可见（上一条 `export started` 与它字段重复，已删除）。实测 `-p ***` 打错密码那次：文件 **3 行**、退出 1、输出文件根本没有创建。上面两处样例是 2026-10-24 的快照，当时的回显对三种格式是同一套 26 个字段；现版本按 `--format` 过滤（判据见 5.3 与 `internal/config/formatscope.go`），实测同一命令 `-f del/sql/parquet` 为 25/20/15 个字段，排列也改为「连接 → 输入输出 → 并行与流水线 → 字符集 → 该格式专属布局 → 日志」，`-m` 文件的"一行一条记录"与记录条数不受影响（实测两次成功导出各 3 行）。

开局建连的 ping 只在**连接级**故障上重试（判据 `common.IsTransient`，见 7.7）。服务端自己判定的拒绝——`1045` 凭据、`1049` 库不存在、`1193` 未知系统变量——**一次都不重试**，因此这类运行的日志恰如下（实测 2026-10-28，本机 MariaDB 10.6.23 / 4 核 8 GB / `max_allowed_packet=16777216`；同一命令耗时 5–6 ms）：

```
... INFO imex starting version=dev ... command=export
... INFO export config host=127.0.0.1 port=3306 user=gd database=imex_test ...
... ERROR command failed error="failed to create exporter: failed to ping database after 1 attempt: Error 1045 (28000): Access denied for user 'gd'@'127.0.0.1' (using password: YES)"
```

连接级失败仍按 `DefaultRetryPolicy` 走完三次尝试，但 `database ping failed, retrying` 只在**真的还会再试**时出现，所以三条尝试配两条告警，末条文案仍是 `after 3 attempts`（实测 `-P 3399` 无人在听那次：4 行、退出 1、耗时 2.675 s，此前固定 6.0 s——旧的 3 s 睡眠落在最后一次失败之后，白等）：

```
... INFO imex starting version=dev ... command=export
... WARN database ping failed, retrying attempt=1 error="dial tcp 127.0.0.1:3399: connect: connection refused"
... WARN database ping failed, retrying attempt=2 error="dial tcp 127.0.0.1:3399: connect: connection refused"
... ERROR command failed error="failed to create exporter: failed to ping database after 3 attempts: dial tcp 127.0.0.1:3399: connect: connection refused"
```

退避窗口内服务端起来，这一跑就照常成功：实测在服务端已停、1.5 s 后才拉起的窗口里启动 111,692 B / 5,000 行的导入，`--threads 2` 用掉两次重试后第三次连上，**退出 0、落库 5,000 行、耗时 2.688 s**，日志只有横幅加那两条 retrying（`--log-level warn` 下汇总不打印）。

**行为约定：**

| 项目 | 行为 |
|------|------|
| 打开时机 | 根命令 `PersistentPreRunE` 在打印版本横幅**之前**打开文件（`os.Create`，已存在的同名文件被截断），因此横幅恒为文件首行；参数校验阶段的用法错误也照样落进文件 |
| 终端 | **静默**：文件挂载期间 stderr 不再打印任何记录，stdout 本来就不承载日志。成败只由退出码判断（0 成功、1 运行时错误、2 用法错误，见 3.2）|
| 覆盖范围 | 全部日志记录：横幅、`export/import config`（在建连接之前）、定时进度、WARN 告警、`completed` 统计、收尾的 `command failed`；导出器与导入器内部不再有独立的消息写入路径（原 35 处 `MessageWriter` 调用删除：导出 15、导入 20） |
| 关闭时机 | `cmd.Execute()` 返回前关闭，因此最后那条 `command failed` 也在文件里；关闭自身失败的 ERROR 记录只进 stderr（此时文件已脱离 sink，这条唯一不落文件的记录因此不会丢）|
| 例外 | 选项解析阶段的错误（未知选项、缺少参数值）发生在 `PersistentPreRunE` **之前**，文件尚未打开，因此该错误只出现在 stderr 且**不创建文件**；`--messages` 路径本身无法创建时同样只报 stderr（`failed to create message file <path>: ...`，退出 1）；`--help` 与 `-m` 同时给不产生文件 |
| `--log-level` | 生效：`--log-level error` 下成功导出，文件只剩版本横幅一条（横幅在 `PersistentPreRunE` 打印，早于 `RunE` 应用 `--log-level`，这是既有行为，且这条 INFO 只进文件、不回到终端）|
| `--log-format` | 生效：文件采用所选布局（`console` / `text`），终端不重复输出 |
| `--quiet` | 生效：定时进度被抑制时文件里也没有。旧版"消息文件是显式产物、不受静默影响"的例外随 DB2 报告一起取消 |
| 写入串行化 | 日志 sink 带互斥锁，并行路径（进度监视协程与分片协程）不会把两条记录交叉写进文件 |
| 安全 | 密码不进日志，因而也不进该文件（见 8.4）；`--echo-sql-values` 打开后真实行数据写入 stderr 或该文件（二选一，取决于是否给了 `-m`）|

会出现告警（WARN）的场景：导出侧 `--no-hex-blob` 与非 utf8mb4 字符集组合、单行 INSERT 超过 `--statement-size`、分片边界采样不可用（表统计读不到或探针查询报错，自动回落按值域等宽切分，带 `table`/`shard_key`/`reason`）、单线程运行收到 `--shard-key`（`single-threaded export ignores --shard-key`）、扫描连续 10 秒取不到行（`export scan stalled`）；导入侧读不到 `max_allowed_packet`（按保守值分批）、`--batch-size` 被 65535 占位符上限下调、无表头文件被当作表头丢弃首行（见 7.4）、`insert_update` 命中无唯一键的表、`深度 × batch_bytes` 可能超过 1 GiB 的交接队列（只报价，见 7.4 第 4 条）、worker 数超过 `@@max_connections`、并行重放窗口随 `--threads` 增长、显式 `--statement-size` 超过驱动插值上限、单批 INSERT 超过 5 秒（`slow insert batch`）、单次 COMMIT 超过 5 秒（`slow commit`）。上述配置与状态类场景每次运行至多一条 WARN，三条阈值告警（`export scan stalled`/`slow insert batch`/`slow commit`）则按发生次数各一条（每次停顿、每个慢批、每次提交），都不另写一份 `SQL3107N`。

### 2.4 encrypt 子命令 — 密码加密

把明文密码加密成 `ENC:` 前缀密文，供 `-p/--password` 直接使用，避免明文出现在 `ps` 输出与 shell history 中。

| 项目 | 实现 |
|------|------|
| 语法 | `imex encrypt <password>`，位置参数恰好 1 个；多给或不给是用法错误，退出 2（见 3.3.4）|
| 密钥来源 | 环境变量 `IMEX_KEY`（`common.EnvKey`）；未设置或为空即失败，提示先 `export IMEX_KEY="..."` |
| 算法 | AES-256-GCM，密钥由 **PBKDF2-HMAC-SHA256**（stdlib `crypto/pbkdf2`，go 1.26.10 内置，未新增依赖）从 `IMEX_KEY` 与**每次加密新生成的 16 字节随机盐**派生 32 字节；密文布局 `base64(salt ‖ nonce ‖ ciphertext+tag)`，`ENC:` 前缀与调用方式不变（`internal/common/password.go`）|
| 使用侧 | export/import 在 `RunE` 起始处调用 `common.ResolvePassword`（`cmd/export.go:72`、`cmd/import.go:67`）：值以 `ENC:` 开头则解密，否则原样按明文处理。密钥缺失报 `failed to resolve password: password is encrypted but IMEX_KEY environment variable is not set`，密钥不符报 `failed to resolve password: failed to decrypt: wrong key or corrupted data`，两者都是**运行时错误，退出码 1**（不是用法错误的 2，因为失败发生在参数校验之后）|
| 密文非确定性 | salt 与 nonce 都每次随机，同一明文 + 同一密钥两次加密结果不同，不可把密文当等值键比较或做 diff；加盐还使同一 `IMEX_KEY` 下每条密文派生出**不同的** AES 密钥，破一条不得破全部（旧实现所有密文共用 `sha256(IMEX_KEY)` 一把密钥）|
| | 工作因子 | `kdfIterations = 600000`。实测（4 核沙箱、go1.26.10、单线程）：旧实现"sha256(IMEX_KEY) 直接当 AES 密钥"的离线密钥尝试速率 **939,822 次/秒**（20 万候选 213 ms，只有真密钥能过 AEAD 标签），改为 PBKDF2 后同一测法 **约 3 次/秒**（单次派生 310.8 ms；迭代数取 210000 时为 9 次/秒、单次 108.7 ms），慢约 31 万倍。代价是每次进程起始处的一次性开销：实测带 `ENC:` 口令导出真实库 2 行为 0.35–0.42 s，同命令写明文口令为 0.00 s；解密只在 `RunE` 起始处发生一次，不随 `--threads` 放大 |
| 兼容性 | **不兼容旧密文、不加版本标识**（用户指定）。旧布局 `base64(nonce ‖ ct ‖ tag)` 会按 salt(16) ‖ nonce(12) 错位切分，派生出不同密钥因而通不过 GCM 认证标签，统一报 `failed to decrypt: wrong key or corrupted data`、退出码 1，且不创建输出文件（实测旧串 rc=1、`-o` 目标不存在）。现存所有 `ENC:` 串必须用新版本重新 `imex encrypt` 生成；`password_test.go` 里 `TestLegacyBlobIsRejected` 以复刻旧布局的方式钉住这一点 |
| 输出 | 标准输出仅一行密文；stderr 首行仍是 console 布局的版本横幅（`command=encrypt`），便于日志归属 |
| 密钥传入方式 | **只有环境变量 `IMEX_KEY` 一条**（2026-10-26 用户定案：不提供 `--key-file`，密钥托管属部署侧职责，工具不接管文件权限、路径解析与密钥轮换）。自动化里由运行环境把密钥注入进程环境即可 |
| 安全边界 | 密码不写入日志，因而也不写入 `--messages` 文件（见 8.4）。盐随密文公开存放、不是秘密，它消除的是"同一密钥跨密文复用一把派生密钥"与彩虹式预计算，不是替代密钥强度：**PBKDF2 只抬高猜测成本，`IMEX_KEY` 仍是唯一秘密**，人可记忆的口令在每秒 3 次的成本下依然可被慢慢穷举，因此 `IMEX_KEY` 应当是高熵随机串。且 `IMEX_KEY` 本身就是凭据，明文放在环境变量里与明文密码同级风险，该机制只解决"命令行可见性"，不解决密钥托管 |

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

默认导出、导入均每隔 5 秒打印一次进度日志，间隔由 `--progress-interval` 调整（必须 > 0 且**不低于 100ms**，否则用法错误退出 2：0 与负值报 `must be > 0`，`10ms`/`1ms` 报 `must be >= 100ms`）。下限是实测代价，且 2026-10-30 按用户指令由 10ms 提到 100ms：校验原先只要求"为正"，`--progress-interval 1ns` 因此合法，一次 6400 行导出打出 **1355 条**进度记录（日志 175,557 字节）——间隔短于一次批处理的耗时，进度行就取代了日志本身；10ms 档同样偏短，实测 200,000 行导入分 200 批、发送段 1.391 s，约 **7 ms 一批**，进度行会跟着批次逐条出现，100ms 才回到"心跳"的节奏（同一次运行最多约 19 条）。`--quiet`（或 `IMEX_QUIET=1`，值支持 `1`/`true`/`yes`/`on`，不区分大小写）禁止定时进度日志，但**保留**版本横幅、配置参数、结束统计、WARN 告警（含 `slow insert batch`/`slow commit`/`export scan stalled`）与 ERROR 记录。进度记录本就不产生，因此 `--messages` 文件里同样没有进度行（旧版"消息文件仍记 `SQL3105N`"的例外随 DB2 报告一起取消）。

```bash
imex import --quiet -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
IMEX_QUIET=1 imex export -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

### 3.3.2 日志格式（`--log-format`）

`--log-format` 控制日志记录的布局格式（持久标志，所有子命令可用），也可通过环境变量 `IMEX_LOG_FORMAT` 设置。

**优先级：`IMEX_LOG_FORMAT` 覆盖命令行给的 `--log-format`**，与其余 `IMEX_*` 开关保持一致——环境变量在这套工具里的角色是"覆盖"而不是"补默认值"（布尔类 `IMEX_QUIET`/`IMEX_ERROR_STACK`/`IMEX_ECHO_SQL_VALUES`/`IMEX_NO_SQL_TAG` 同样是 flag 与 env 取并集，env 一侧永远有效）。实测：`IMEX_LOG_FORMAT=console` 加 `--log-format text` 输出 **console 布局**，`IMEX_LOG_FORMAT=text` 加 `--log-format console` 输出 **text 布局**；只设 env 或只写 flag 时各自生效。变更记录 73 曾把这里改成 flag 优先，经用户裁定——应与其余参数的 flag/env 关系一致——已回退，本条描述的即回退后的现行语义。

| 值 | 说明 |
|----|------|
| `console`（默认） | 固定宽度行格式：`2026-01-02 15:04:05.000 [pid] LEVEL message key=value ...`，类似 JUL Formatter，便于 grep 和 tail |
| `text` | slog 原生 key=value 文本格式，适合机器解析 |

值名只接受这两个（变更记录 74）：旧名 `java` 只描述了一个实现语言而不是格式，已作废且**不留别名**——`--log-format java` 与 `IMEX_LOG_FORMAT=java` 都是用法错误（退出码 2，`invalid --log-format "java": must be one of console/text`）。

```bash
imex export --log-format text -h 127.0.0.1 -u root -p pass -D mydb -o /backup/users.del -q "SELECT * FROM users"
IMEX_LOG_FORMAT=text imex import -h 127.0.0.1 -u root -p pass -D mydb -i /backup/users.del -t users
```

日志格式在 `PersistentPreRunE` 中尽早应用，即使用法错误（退出码 2）也使用所选格式输出。

**单行保证：** 两种布局都遵循「一条记录 = 一个物理行」。值为空或含空白、控制字符、`"`、`\`、`=` 时，按 slog 原生规则加引号并转义（`row_del="\n"`、`char_del="\""`、`shard_key=""`）。因此打印 DEL 定界符的配置行不会被真实换行拆开，`grep`/`tail` 每次都能取到完整记录；同一值在 `console` 与 `text` 两种布局下渲染成同一个 token，切换格式不影响字段切分。

**布局切换不改级别：** `ApplyLogFormat()` 只重设布局，`--log-level` 选择的阈值保持不变，版本横幅与随后所有 INFO 记录照常输出。

**消息命名：** 所有经全局 logger 输出的记录统一为小写、单行的 `<对象> <事件>` 短语，主要事件词是 `config` / `progress` / `completed` / `aborted` / `failed`（`started` 只出现在版本横幅 `imex starting` 里），另有 `retrying` / `executing` / `applied` / `derived` 等状态词描述过程事件；诊断告警以 `slow` 开头（`slow insert batch` / `slow commit`）或以 `stalled` 结尾（`export scan stalled`）。因此大小写敏感的 grep 一条 pattern 就能覆盖全链路。同一类记录按发生次数各占一行：配置、进度、完成、中止每次运行至多一条，而每条慢批、每次停顿各一条。主要记录：

| 记录 | 触发时机 |
|------|----------|
| `imex starting` | 每次真实运行的首行（version/build_time/git_commit/go_version/command）；`version` 子命令本身不产生任何日志记录 |
| `export config` / `import config` | 生效参数快照，**只打印所选 `--format` 真正读取的字段**：判据取自 `internal/config/formatscope.go` 这张登记表，与用法错误的拒绝逻辑、`--help` 里的范围标记同源（`-f del` 不打 `statement_size`/`no_create`/`extended_insert`，`-f sql` 不打 DEL 定界符与文本修饰，`-f parquet` 两类都不打；实测 export 为 25/20/15 个字段、import 为 29/20/20）。其余字段按「连接 → 输入输出 → 行流控制 → 并行与流水线 → 字符集 → 该格式专属布局 → 日志」排列，含 DEL 定界符与文本修饰（`date_fmt`/`no_escape`/`no_char_del` 三项此前漏打，现已补齐）、`hex_blob`（导出侧打印**生效值**而非 `--hex-blob` 标志位，与写入器共用 `effectiveHexBlob()`）、`progress_interval`；导入侧的 `statement_size` 与格式无关（它约束本命令生成的语句，不约束输入文件），故三种输入格式都打印。由 `NewExporter`/`NewImporter` 在**建连接之前**打印，因此连接失败时也看得到参数；紧随横幅之后（原先重复它的 `export started`/`import started` 已删除） |
| `session variables will be set on every connection` | 传入了 `--session-var` |
| `export progress` / `import progress` | 每 `--progress-interval`（默认 5 秒）一条定时进度（`--quiet` 抑制；运行短于间隔时一条也不打印） |
| `parallel export detected` / `parallel export ignores pipeline options` / `shard export completed` / `parallel export completed` | 并行导出：分片计划、被忽略的管道选项（CLI 上已改为用法错误，因此该 info 只可能由直接驱动库的调用方触发；级别 `info`，带 `batch_size`/`pipeline_buffer`/`look_ahead_per_shard`/`applies_to`）、每个分片、汇总 |
| `single-threaded export ignores --shard-key` | `warn`：单线程导出从不采样分片键，而它此前是唯一"看得见"的地方——显式给出 `--shard-key` 的 `--threads 1` 运行照样在配置记录里回显 `shard_key=...`，读起来像被采纳，且列名是否存在都不检查（只有分片路径校验）。现在两条进入单线程的路由都出声：`--threads 1`，以及并行请求因复杂查询、表名解析失败或只切出一段而回落。带 `shard_key`（取值）、`threads`（请求值）、`fallback`（`threads>1` 即 true，说明这是一次回落）、`hint`（改用单表查询并 `--threads>1`，或去掉该选项）。配置记录侧：写记录时若 `threads<=1` 且给了键，紧随 `shard_key` 追加 `shard_key_ignored=true`——回落路径写记录时 `threads>1` 仍看着可用，故不带该字段，由本条 `warn` 负责（实测 `--threads 4` + JOIN 的运行只出现 `warn`，`--threads 1` 的两条都在）。退出码与产物不变（同一 5000 行夹具在 `--threads 1` 带键、不带键、以及 `--threads 4` 并行三条路径下产物 md5 均为 `bc78091aa71ef39b73a65268a78fadc8`；回落那条跑的是另一个查询，不参与比对） |
| `shard boundaries sampled from the key distribution` / `no row estimate for the table, splitting the key range by value width` / `shard boundary sampling unavailable, splitting the key range by value width` / `shard ranges fewer than planned` | 并行导出的边界来源：采样成功为 `debug`（带 `table`/`shard_key`/`ranges`/`rows_per_range`），拿不到整表统计为 `debug`（带 `table`）、采样中途失败为 `warn`（带 `table`/`shard_key`/`reason`，两者都回落等宽切分），实得段数少于计划段数（键值域不足或采样提前触顶）为 `info`（带 `planned`/`ranges`）|
| `shard stream failed` / `incomplete output file removed` | 导出写出失败（如 `ENOSPC`、写入被拒）：前者 `error` 仅并行路径出现，带具体错误；后者 `warn` 带被删除的 `output=`，并行与单线程两条路径都会在失败时删除已写出的半截产物（单线程此前不会删除，实测留下 21 MB 残留文件），其余仍在队列上等待的分片此时以 `shard output consumer stopped` 结束 |
| `import batch byte cap resolved` / `could not read max_allowed_packet, assuming conservative batch limit` | 导入首次攒批前解析每批字节预算（每次运行一条，`info`）：带 `max_allowed_packet`（读到的服务端值）、`batch_bytes`（生效预算）、`statement_size`（导入侧的可选压低值，`0`=不额外压低）、`limiting_factor`（`max_allowed_packet` / `statement-size` / `driver_packet_cap`，即谁定的预算；`driver_packet_cap` 只在服务端报出**大于** 67,108,864 B 的包时出现，此时默认预算按 9/10 余量取整为 **60397977** 字节）；后者 `warn` 在读不到该变量时出现，按 4 MiB 保守值继续。测试床按 OceanBase V4.x 默认 `max_allowed_packet=16M` 时实测记 `batch_bytes=15099494 limiting_factor=max_allowed_packet` |
| `--statement-size is above the driver interpolation ceiling, generated statements will fall back to server-side prepared statements` | `warn`：用户显式给出的上限高于 60397977 字节的驱动插值上限。此时**不改写用户的值**（预算仍取 `statement-size`），只说明代价——超过该值的语句不走客户端插值的 `COM_QUERY`，退回服务端预处理语句（实测 80 MiB 数据 2.078 s 对 1.195 s）。带 `statement_size`、`driver_ceiling_bytes`。测试床按 OceanBase V4.x 默认 16M 时默认预算 `15099494` 已低于该上限，这条记录只在用户显式写出更大的数字时出现 |
| `the hand-off queue may retain more than the memory envelope, importing at the configured depth` | `warn`：交接队列按**批数**计深度，而单批字节数等于上面的预算，因此队列驻留的行文本没有硬上界。`深度 × batch_bytes` 可能超过 1 GiB 时**不改深度**，只在任何数据移动之前把价报出来，带 `requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes`/`hint`（测试床按 OceanBase V4.x 默认 16M、预算 `batch_bytes=15099494`：`⌊1 GiB / 15,099,494⌋ = 71`，实测深度 71 静默、深度 72 起出声（`queued_bytes=1087163568`），200 MB / 10 万行文件 `--pipeline-buffer 1024 --threads 32` 记 `requested=1024 batch_bytes=15099494 queued_bytes=15461881856 envelope_bytes=1073741824`，运行按深度 1024 结束、退出 0，峰值 RSS 各重复三轮为 76,616–152,040 KB）。默认深度 10（`≈144 MiB` 名义驻留）不触发该记录；库调用方给负深度按 `0` 处理 |
| `workers exceed the server connection limit, workers that cannot connect will retry and may fail the run` / `... may fail the export` / `parallel import replay window scales with threads` | `warn`，在任何数据移动之前：前者比较 `--threads` 与 `@@max_connections`（导入侧结尾是 `the run`，导出侧是 `the export`），带 `threads`/`connections_needed`（worker 数 + 1 条主连接）/`max_connections`/`hint`（实测 `threads=200 connections_needed=201 max_connections=151`）；后者说明断线重放缓冲按 worker 各自计算，带 `threads`/`per_writer_bytes`/`total_bytes`/`budgeted_total_bytes`（`--threads 256` 时为 256 × 8 MiB = **2147483648** 字节，进程预算仅 67108864）。两条都是告警而非拒绝：DBA 可以现场调高服务端上限，运行本身仍然正确 |
| `worker completed` / `parallel import completed` | 并行导入：每个工作线程（带 `rows`/`duration`/`rate`，实测 4 线程 200,000 行分 48,000/50,000/50,000/52,000，各约 0.96s）、汇总 |
| `shard export completed` | 并行导出的每个分片，带 `rows`/`duration`/`rate`：时长从领到该分片算到它的最后一块交给汇流写入器，包含被前序分片反压的时间（实测 8 分片 × 25,000 行，0.044–0.062s） |
| `export scan stalled` | `warn`：扫描取不到行超过 **10 秒**。每 **64 行**读一次时钟，检查点之间的间隔超阈值才出现（不是逐行计时，避免把计时器塞进最热的循环；1024 行一检查会把「稳定但慢」的扫描误报成卡住）。带 `rows_so_far`、`gap`（实测 `SLEEP(12)` 夹在第 500 行：`gap=12.000s`，`first_row=0.004s write=12.033s`，即这 12 秒算在写入段而不是首行延迟）、并行时另带 `worker`/`shard` |
| `slow insert batch` | `warn`：单批 INSERT 超过 **5 秒**。带 `rows`、`bytes`（语句文本+字符串/字节载荷，与重放缓冲同一套估算）、`duration`，并行时另带 `thread`（实测：`BEFORE INSERT` 触发器每行 `SLEEP(0.002)`、`--batch-size 5000` 的单批 `rows=5000 bytes=937869 duration=10.430s`；默认 `--batch-size 1000` 时同样数据分成 5 批各约 2 秒，因此不告警，说明阈值按**每批**而不是按总量判定） |
| `slow commit` | `warn`：单次 COMMIT 超过 **5 秒**。带 `session`（`single-threaded` 或 `thread N`）、`rows`（本次提交覆盖的行数）、`duration` |
| `export completed` / `import completed` / `import aborted` | 结束统计（中止时仍给出已达进度与 `failed`）。单线程导出把 `duration` 拆成 `connect`（拨号+ping+一致性快照，发生在 `Export` 之前，故不计入 `duration`）、`setup`（`Export` 起点到发起查询）、`first_row`（发起查询到第一行返回，含建文件与写表头）、`write`（第一行到写最后一行）、`finish`（footer+close，parquet 的末段编码也在这里）；四段相加等于 `duration`。单线程导入给出 `connect`、`setup`（连接与元数据、模式准备）、`header`（打开输入文件到解析完表头，parquet 读 footer 建 schema 也算这里）、`read`（读文件并解析的累计，不含等队列）、`send`（执行 INSERT 的累计）、`wait_empty`（写侧等数据）、`wait_full`（读侧等空位）；`read+wait_full` 是读协程视角、`send+wait_empty` 是写侧视角，两组各自接近 `duration`，**不是四段相加**。并行导出汇总带 `connect`/`setup`，并行导入汇总带 `connect`/`setup`/`header` |
| `query failed` / `insert batch failed` / `create table failed` / `shard query failed` / `shard export failed` | 语句级失败，附 `sql` 预览（默认脱敏） |
| `command failed` | 最终失败汇总，`--error-stack` 附带调用栈 |

同一事件在一次运行中只输出一行（配置、进度、完成、中止与失败类记录；`slow insert batch`/`slow commit`/`export scan stalled` 三条阈值告警按发生次数各一行）：原先紧随 `export config`/`import config` 的 `Starting export`/`Starting import` 属于重复记录，已删除（字段全部被 config 记录覆盖，输入文件字节数改由 `import config` 的 `bytes` 承载）。该约定由 `main_test.go` 的 `TestLogMessageStyle` 静态守护：解析全部源文件，检查每个 `logger.Debug/Info/Warn/Error` 的字面量消息首字母小写且不含换行。

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

### 3.5 帮助文案长度规范（用户于 2026-10-29 指定）

`--help` 的每一行都必须能在一屏内读完，不允许靠终端自动换行来“折行显示”。规则：

1. **上限 118 字节/行**，计量范围含选项名列、缩进，以及 pflag 自动追加的 `(default …)`、格式作用域注册表自动追加的 `(only with --format …)` / `(not with --format …)`。取 118 而不是 120，是给 120 列终端留 2 列余量——不同终端对“最后一行恰好占满”的处理并不一致。
2. **文案只写“这个选项做什么、边界在哪”**：实测数字（CPU、字节、耗时）、设计来龙去脉、多步操作说明一律留在本文档选项表与 README；环境变量用 `(also IMEX_XXX=1)` 一句带过；默认值与取值范围交给 pflag 和参数校验表，不在文案里重复。
3. **不手写自动后缀**：`(default …)` 由 pflag 输出，`(only with …)` / `(not with …)` 由 `internal/config/formatscope.go` 的注册表输出；一行放不下时要缩的是 usage 本身，既不能删这些后缀，也不能把它们硬编码进 usage。
4. **新增或修改选项时先量长度**，超限就改文案；判定命令（输出必须为空）：

```bash
for c in "--help" "export --help" "import --help"; do
  ./bin/imex $c 2>&1 | awk 'length($0) > 118 { print length($0) ": " $0 }'
done
```

v0.5.0 收口时的实测上界：root **114**、export **117**、import **116** 字节；规范落地前分别为 159 / 196 / 239，最长单行 259 字节（`--echo-sql-values`、`--parquet-compression`）。
5. 本规范只管**命令行帮助**：代码注释、文档表格不受行宽限制，它们按内容需要换行。

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
| —（DB2 无对应） | `--extended-insert`：SQL 文件的语句布局开关（本工具自有选项，mysqldump 用 `--extended-insert`/`--skip-extended-insert` 表达同一对语义；本工具的默认布局就是后者，因此只保留前一项，曾作为兼容项保留的 `--single-row-insert` 已删除） |

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
```

**`--extended-insert` 模式下的文件结构：**
```sql
/*!40101 SET NAMES utf8mb4 */;
-- Table structure for users
CREATE TABLE `users` (...);

-- Data for users
INSERT INTO `users` (`id`, `name`, `email`, `created_at`) VALUES (1,'Alice','alice@example.com','2026-01-01 10:00:00'),(2,'Bob','bob@example.com','2026-01-02 11:00:00'),(3,'Charlie','charlie@example.com','2026-01-03 12:00:00');
```

**特点：**
- 示例含 CREATE TABLE 语句，对应 `--create`；SQL 导出默认**不带** CREATE TABLE，只写下面的 `INSERT` 数据（详见 2.1.6）
- 默认模式：每行一条 INSERT，文件可按行切分（mysqldump `--skip-extended-insert` 的布局）
- `--extended-insert`：多条 VALUES 合并为一条 INSERT，合并上限由 `--statement-size`（默认 8 MiB）控制。合并只改变文件体积，不改变导入侧打给服务端的语句——导入器把语句拆成行后按 `--batch-size` 重新攒批（见 2.1.6 实测）
- 字符串值按转义规则处理（见 2.1.6），确保每条 INSERT 恰好占一个物理行
- 文件首行包含 `/*!40101 SET NAMES <charset> */;`
- **不生成 `LOCK TABLES`/`UNLOCK TABLES`**（变更记录 72）：任何服务端、任何布局、有无 `--create` 都不写。旧实现生成这一对且锁在 `CREATE TABLE` 之前，于是标准 `mysql` 客户端往空库回放时在第 2 行就 `ERROR 1146 (42S02) Table doesn't exist` 中止；现在同一份产物直接回放成功（实测 3 行表：旧产物回放 exit 1 / 新产物 exit 0 且落库 3 行）
- imex 自身的导入路径仍会跳过 `SET NAMES`/`LOCK TABLES`/`UNLOCK TABLES` 这类前导语句，因此**旧版本生成的 SQL 文件照常可以导入**（新增的产物只是不再产生它们）

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

**页压缩（`--parquet-compression`）：** 默认 `none`，页面原样写盘（parquet-go 的 `WriterConfig.Compression` 缺省即 `Uncompressed`，旧版本行为因此被保留为默认值，产物逐字节不变）。可选 `snappy`（快速、文本类数据约 10× 压缩）与 `zstd`（更小、更耗 CPU 与内存）。编解码器名写在每个列块的 footer 元数据里，`parquet.NewReader` 按 footer 自动解码，因此**导入侧没有、也不需要对应的压缩选项**。三个 codec 都来自已有的 indirect 依赖 `github.com/klauspost/compress v1.18.5`（`parquet-go/compress/{snappy,zstd}` 只是薄包装），引入两者仅使二进制增大约 4 KB。300k 行实测取舍：

| 数据形态 | codec | 文件大小 | 导出耗时 | 峰值 RSS |
|----------|-------|----------|----------|----------|
| 文本类（可压缩） | `none` | 20,379,742 B | 466 ms | 43–47 MB |
| 文本类 | `snappy` | 2,112,909 B（9.6×） | 485 ms | 35–44 MB |
| 文本类 | `zstd` | 349,233 B（58.3×） | 534 ms | 91–164 MB |
| 随机二进制（不可压缩） | `none` | 16,148,845 B | 460 ms | — |
| 随机二进制 | `snappy` | 15,048,253 B（1.07×） | 542 ms | — |
| 随机二进制 | `zstd` | 14,528,888 B（1.11×） | 533 ms | 138 MB |

导入三种编码均约 1.9–2.0 s、峰值 RSS 29–31 MB，差异可忽略——压缩只买体积，基本不买导入时间。

### 5.4 三种格式共有的值处理

| 项 | 行为 | 依据 |
|----|------|------|
| `YEAR` 零值 | 文本协议把 `YEAR(0000)` 发回裸字符串 `0`，直接回导会命中服务端两位 YEAR 规则解释为 2000（静默改值）。导出时按 `DatabaseTypeName()==YEAR` 识别列，把单字符 `0` 重写为 `0000`，DEL/SQL/Parquet 一致；非零 YEAR 原样透传 | `internal/export/exporter.go:242-246`、`370-372`；实测 `--format del` 得 `"2","1999",""`、`"1","0000",` |
| NULL 与空字符串 | DEL（默认带字符串定界符）：NULL 写成两个列分隔符之间的空内容且不加定界符，空字符串写成 `""`，因此可区分。DEL + `--no-char-del`：两者都是空字段，**不可区分**，回读时一律按 NULL 处理。SQL：NULL 写 `NULL` 关键字、空串写 `''`。Parquet：NULL 保留为 Parquet NULL，与空字符串天然区分 | `internal/export/del_writer.go:105-121`；实测 `--no-char-del` 下 `2,1999,` 与 `1,0000,` 形态一致 |
| 二进制列 | DEL/SQL 在 `--hex-blob` 生效时写 `0x…` 字面量、空值写 `0x`（与 NULL 区分）；Parquet 存原始 BYTE_ARRAY，不带 UTF-8 标注 | 见 2.1.6、5.3 |
| 日期/时间戳 | `--timestamp-fmt`/`--date-fmt` 只按列类型（`common.ColumnKind`）修饰文本形态，数值/字符串列不受影响 | 见 2.1.5、2.2.5 |

---

## 六、OceanBase MySQL 模式兼容性

### 6.1 不做服务端类型探测

工具不查询 `version_comment`，也不区分 MySQL 与 OceanBase MySQL 模式：两者走同一条代码路径、发同一批语句、产出同一种文件。此前唯一读过服务端类型的是「MySQL 才加 `LOCK TABLES`」这一条判据，而变更记录 72 已把锁语句整体删除，该判据随之失去调用者；变更记录 73 把残留的 `ServerType`/`DetectServerType` 等死符号一并删除，`internal/common/detect.go` 只保留 `MaxConnections`。将来若要做 OceanBase 专用路径（例如按 tablet 统计切分片），届时按需要重新引入探测，而不是留一个无人调用的判据。

### 6.2 兼容性处理

| 特性 | MySQL | OceanBase | 处理策略 |
|------|-------|-----------|----------|
| 一致性快照 | 支持 | 支持 | 统一使用 |
| `LOCK TABLES` | 支持 | 部分支持 | 导出一律不生成（变更记录 72） |
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
3. 生成分片范围（先采样键分布，采样不可用才按值域等宽切；段数为 `min(2×threads, 64)`，见 7.2.1）
4. 恰好 `--threads` 个 worker 从共享游标**按段序**领取分片，每段独立连接、独立查询，把序列化结果写入自己的字节队列（`shardPipe`）
   - 分片片段不写 CREATE TABLE（writeCreate=false）；锁语句整体不再生成（变更记录 72），随它一起删掉的还有 `SetSkipTableLocks`
   - 分片读完自己的全部行之后**立刻**关闭 `rows` 与该分片连接（结束一致性快照），再写页脚、关队列
5. writeShardStream 流式写出到最终文件：
   - 先由 SQLWriter/DelWriter 写出 header（SET NAMES，`--create` 时还有 CREATE TABLE）
   - 再按分片序号依次消费各自的队列（分片仍在并行产出，只是文件位置由这一个 goroutine 独占）
   - 最后由 SQLWriter 写出 footer（刷出最后一批 INSERT 并 flush）
6. 不产生任何临时文件
7. 停止进度计时器（间隔由 --progress-interval 决定），再写出 `parallel export completed` 汇总（汇总恒为最后一条记录）
```

**并行导出不再写临时文件（`shard_stream.go`）：** 旧实现是每个分片写一份 `<output>.shard_<i>.tmp`，全部分片结束后再 `io.Copy` 拼接、删除临时文件。代价是**每个字节落盘两次**：实测 50 万行表（DEL 输出 124,903,023 B）单次并行导出的写入量为 487,920 × 512 B ≈ 238 MB，约输出体积的 2 倍，SQL 格式为 515,152 块 ≈ 251 MB 对 131,872,590 B（两轮重复测得同一组数字）；容量上意味着并行导出需要两倍的空闲空间，且中途被信号打断会在输出目录留下一堆 `<output>.shard_N.tmp`（实测 4 个共 33 MB）。现在分片直接把 `[]byte` 块交给按分片序号消费的那个 goroutine，写入量降到 243,952 / 257,568 块（等于输出体积按 512 B 向上取整，写放大归零），输出目录里只有最终文件本身。字节序列与旧实现**逐字节一致**（DEL/SQL 两种格式、同一库同一时刻分别与旧二进制比对），导出内容与单线程导出、以及导入回读的行集（count + 顺序无关 CRC 和）也都一致。

队列深度不是细节：单个输出 goroutine 一旦偶尔落后，分片就会填满队列并停下，而它停下的时候其他分片的数据库读取仍在推进，整体变成"读一批、写一批"的停顿。同一台 4 核机器上以 SQL 格式实测（每组 7 次交替、取中位数，消除温漂）：旧实现 1.09 s；每分片 2.5 MiB 前瞻为 1.31 s、8 MiB 为 1.24 s（慢 14~20%）；16 MiB 为 1.09 s，与旧实现持平——写入量减半不再牺牲时间。因此 `shardStreamQueueBlocks` 用一个固定的 64 MiB 总预算按分片数均分，上界 64 块（16 MiB）、下界 2 块，与 `--pipeline-buffer`（按行数计）无关。代价是这部分在途字节从磁盘移到堆上：4 分片并行导出的进程峰值 RSS 由约 33 MB 升到约 111 MB（DEL）、114 MB 升到 215 MB（SQL），8 分片 DEL 为 36→118 MB；`--threads 8` 的产物同样与旧实现逐字节一致、写入量同样是恰好一半。也正因为每段前瞻由这个预算决定，`--batch-size`/`--pipeline-buffer` 在并行路径上没有任何作用（这两个字段只有 `exportSingle` 读取）。2026-10-27 起这条"出声"升级为**拒绝**：`cmd/validate.go:validateParallelExportOptions` 用 `cmd.Flags().Changed()` 判定，`export --threads>1` 时显式给出任一个即 `common.Usagef` 退出码 2，文案点名该选项、说明它在并行路径上不改变任何输出、并给出两条出路（去掉它，或改 `--threads 1`）；判据是"有没有显式传"而不是"值是否等于默认"，所以 `--threads 8 --batch-size 1000`（值恰好是默认值）同样被拒——看起来被采纳却无效正是问题本身。`--threads 1`、完全不给 `--threads`、以及 `import` 侧都不受影响；直接构造 `ExportConfig` 的库调用方不走 CLI 校验，仍收 `parallel export ignores pipeline options` 这条 info。同一笔预算的均分对象也由"分片数"改为"段数"，而段数不再等于线程数（见 7.2.1）。

**分片读完即放连接，不等队列排空：** 上面那种"停下"发生在两种地方，代价完全不同。读到一半停下是背压，是设计要的；而**行已经全部读进内存、只剩把尾部字节交给流式写出 goroutine**时停下，是什么都换不到的空等——这段时间里那条分片连接连同它的 `START TRANSACTION WITH CONSISTENT SNAPSHOT` 一直被持有。旧实现正是这个顺序：`exportShard` 里 `defer rows.Close()` / `defer db.Close()` 排在函数返回之后，而返回之前要先写页脚（SQL 格式还要刷出最多 `--statement-size`，默认 8 MiB 的攒批 INSERT）、再 `pipe.Close()`，这两步都可能阻塞到写出 goroutine 轮到本分片。云环境下这个空等窗口是要命的：连接经过 LB/NAT 时空闲 socket 会被回收，实测本地以 400 万行倾斜表（分片 0 占 360 万行、分片 4 只有 40 万行）`--threads 5` 导出，`SHOW PROCESSLIST` 里一个已完成读取的分片停在 `Writing to net` 约 5 秒、`INNODB_TRX` 快照年龄一路涨到 9 秒——那 5 秒它一行数据都不再产生，只是被前面的分片堵着，而连接和快照都还占着。现在 `exportShard` 在行迭代结束（`rows.Err()` 判过）之后显式调用 `releaseDB()` 关掉 `rows` 与 `db`，页脚与 `pipe.Close()` 挪到释放之后；`releaseDB` 幂等，`defer` 只做兜底，注册顺序保证查询失败、页脚失败等路径下数据库仍是最先被放弃的资源。实测这条**是不变量而不是吞吐**：同一份倾斜数据新旧二进制的产物逐字节一致（DEL 587,806,929 B、md5 `2c9834af64e6` 相同，SQL `--extended-insert` 642,102,879 B、md5 `503ae9219565` 相同），耗时也一致（DEL 8.334/8.167 s 对 8.140/8.050 s，SQL 12.284/12.674 s 对 12.321/13.384 s）——本地写普通文件时"尾部"只有 1~2 个 256 KiB 块（写入器用的就是 `bufio.NewWriterSize(w, 256*1024)`），几乎堵不住；差别在观察到的连接状态上：旧二进制那次跑到中途抓到 `[3 2:270=Sleep:0 268=Query:6 267=Sleep:6]`，即一个读完的分片仍计在打开连接里，新二进制的所有采样里再也没有"已完成但仍打开"的连接，空分片更是毫秒级就释放。回归由 `internal/export/exporter_shard_test.go` 钉住：它用进程内的假 `driver.Connector` 起一个**故意没有消费者**的 `shardPipe`，断言分片的连接已关闭而 `exportShard` 尚未返回（把改动前的 `defer` 顺序还原回去，该用例必然在 5 s 后失败并报 `exportShard still held its connection while waiting on the shard pipe`），另一用例断言空分片既不挂住也不留连接。

**导出的失败与中断语义（两条路径一致）：** 并行导出的流式写法意味着失败时输出文件已经存在并写了部分内容。任一分片报错或写文件报错（如 `ENOSPC`）时，导出整体报错、把已写出的输出文件删除并记一条 `incomplete output file removed` 警告，其余仍在队列上等待的分片由 `shard output consumer stopped` 立即解除阻塞（不会挂死），实测 32 MB tmpfs 目标下 4 分片导出在第 1 个分片写满后整体失败且不留任何文件。旧实现在分片失败时是"不产生输出文件"，在合并阶段失败时同样会留下半截输出，因此新语义在两种情况下都只留一种结果：**要么完整、要么没有**。单线程路径过去并不遵守这一条：`os.Create` 之后的任何失败只报错、不清理，交叉测试实测 20 万行表在末行被 `--no-char-del` 拒绝后留下 21,233,664 字节的半截 DEL 文件（文件名为导出名，看起来像成功产物）。现在 `exportSingle` 在创建输出文件之后注册同一份 `removeIncompleteOutput`，任何后续失败都删除该文件，并行路径改为复用同一个方法；删除只在文件确已创建之后注册，因此创建前的失败（查询报错、`--no-overwrite` 命中已有文件）不会误删用户原有文件，实测 `--threads 1/4` 两种失败都只留日志不留文件。注意：程序未安装信号处理器，`SIGINT`/`SIGTERM` 仍是直接终止进程，此时会留下一个不完整的输出文件（旧实现留下的是等量的分片临时文件），需要用户自行删除。

**并行导出的结果语义：**

- **完整性**：分片边界由精确十进制/整数运算生成（`BIGINT`/`DECIMAL` 超出 float64 精度时不丢行），相邻分片共享同一个边界字符串（前一分为 `<=`、后一分为 `>`），因此每行恰好落入一个分片，不重不漏。
- **行序**：最终文件按分片序号顺序拼接，**不保证**与 `--query` 的全局结果顺序一致；需要稳定全局顺序时请使用单线程导出。
- **一致性**：每个分片使用独立连接并各自执行 `START TRANSACTION WITH CONSISTENT SNAPSHOT`，各分片内部是一致的，但分片之间的快照建立时刻略有差异；导出期间源表仍有写入时，跨分片不构成同一个时间点快照。

**注意：** `--batch-size` / `--pipeline-buffer` 仅作用于单线程导出与导入。并行导出为分片内同步 scan→write，不受管道参数影响，`export --threads>1` 时显式传入任一个即为用法错误（退出码 2）；分片到输出文件之间的字节队列深度是内部常量（64 MiB 进程总预算按**段数**均分，段数为 `min(2×threads, 64)`，见 7.2 与 7.2.1），不通过命令行暴露。

**并行导出的降级触发点：** 以下任一条件命中即回落单线程，并记一条 `warn` 级日志（消息文本即下表右列，`--quiet` 不抑制 warn）：

| 触发条件 | 日志消息 |
|----------|----------|
| `--query` 含 JOIN/派生表/集合运算等复杂特性 | `query has complex features, falling back to single-threaded export`（带 `feature=<识别到的特性>`）|
| 无法从语句提取表名 | `failed to extract table name, falling back to single-threaded export`（带 `error=`）|
| 检测不到可用分片键（**仅限自动检测**：无主键、无唯一键）| `failed to detect shard key, falling back to single-threaded export`（带 `error=`）；用户显式给出 `--shard-key` 而该列不存在时**不再走这条回落**，直接用法错误退出 2：`invalid --shard-key "列名" for table "表名": <检测到的原因>`。显式键在**并行路径**上校验，因此 `--threads 1` 与"并行请求回落到单线程"两条路由仍会带着一个从未被读取（也从未被校验）的键跑完——它们现在各记一条 `single-threaded export ignores --shard-key`，见日志记录表 |
| 分片范围为空或只能切出一段（`MIN()==MAX()`、区间无法精确运算、采样与等宽两种切法都只得到一段）| `only 1 shard range, falling back to single-threaded export` |

前三项与第四项都在 `exportParallel` 的分片计划段里，级别 `warn`。**首列非数值（UUID/字符串主键）不再是降级触发点**——2026-10-27 起该情形走边界采样（7.2.1），实测同一张 `CHAR(32)` 主键表由 1 段变 8 段。`--format parquet` 走的是更早的独立分支，级别是 **`info`** 而非 warn，消息为 `parallel export is not supported for parquet format, using single-threaded export`（`exporter.go:456`，详见 5.3）。

选中键时会先写一条 `debug`：`using primary key for sharding` 或 `using unique key for sharding`，附 `columns=`，便于确认复合键实际用了哪几列。

**复合键与非数值键：** 主键/唯一键可以是复合的（`shard.go` 的 `ShardInfo.Columns` 为切片）。切分只作用于**首列**，其余列保持其全范围，因此每个元组仍被恰好覆盖一次；首列非数值（字符串、UUID、日期之外的类型）时无法计算数值边界，按上表第 4 行降级为单分片。数值边界一律用 `math/big.Rat` 精确运算，整数键保持整数文本形态以命中索引，`DECIMAL` 按 30 位精度输出。

#### 7.2.1 分片边界采样与段数计划（2026-10-27）

**要解决的问题：** 等宽切分只看得见**值域**、看不见**分布**，实测三处后果——① 分片首列非数值（`CHAR(32)`/UUID 主键）时 `GenerateShardRanges` 只能交回一段，`exportParallel` 随即降级为单线程（`only 1 shard range` 告警），`--threads 8` 也一样；② 稀疏或倾斜的整数键被切成空段与巨段：一张 100 万行、`id` 跨度 1…200,000,000 的表在 `--threads 4` 下切出 200,000 / 0 / 0 / 800,000 行，两个 worker 全程空转、最慢段占全表 80%，总时长由那一个 worker 决定；③ 段数与线程数 1:1，收尾段就是关键路径。

**实现（`internal/export/shard_sample.go`）：**

- **段数计划** `shardRangeCount(threads) = min(2×threads, 64)`：段多于线程才有人一直有活干；上限 64 是为了让 64 MiB 的流式预算均分后每段至少 1 MiB（见 7.2）。
- **整表统计**：`SELECT TABLE_ROWS, AVG_ROW_LENGTH FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`。`TABLE_ROWS` 是 InnoDB 估算值，本轮四张基准表与 `COUNT(*)` 逐张比对的偏差为 −1.00%…+3.28%（`bench_lay` −0.17%、`bench_1m` −0.27%、`bench_skew2` −1.00%、`bench_str2` +3.28%）；它只决定"每段跳过多少行"，不参与正确性——段的连续性由边界序列本身保证，估偏只会让某段略大或略小。
- **定位探针**（每段一条，最后一段不需要）：

  ```sql
  SELECT `k` FROM `tbl` WHERE `k` > ? ORDER BY `k` LIMIT 1 OFFSET <每段行数-1>
  ```

  边界值走**绑定参数**（因此对 `CHAR`/UUID 同样成立，字符串不会被当数字重打一遍），`OFFSET` 是工具自己算出的整数、直接内联（占位符在 MySQL 的 `LIMIT` 里不被支持）。总成本约等于沿索引走一遍：实测 100 万行的稀疏表上 7 条探针让端到端导出由 0.84 s 变为 0.90 s（3 次中位数，`--threads 4`），与一次全表扫描（0.128 s）同量级。
- **两条不需要探针的捷径**：(a) **可证的密度**——键是定点整数且值域跨度 `MAX-MIN+1 ≤ 行数 × 3/2` 时，等宽切分与理想切分的偏差不超过 1.5 倍，判据用 `big.Rat` 精确比较，`BIGINT` 满跨度（2^64−1 对 100 万行）也绝不误判；(b) **成本闸门**——稠密整数键只有在 `AVG_ROW_LENGTH × TABLE_ROWS ≥ 256 MiB` 时才值得探针，小表直接等宽。非数值键、`DECIMAL`/浮点键、以及不满足密度判据的稀疏整数键**总是**采样。
- **回落到原行为**：拿不到统计（`DEBUG`）、统计或探针查询报错、探针取到 `NULL`、可用边界不足（均 `WARN`，带 `reason`）一律走 `GenerateShardRanges`；采样绝不是失败来源。
- **连续性不变量**：`rangesFromBounds` 产出的段严格递增、首段从精确的 `MIN`、末段以 `IsLast`（`<=`）收到真实的 `MAX`，相邻段共享同一个边界文本，因此每个元组恰好被覆盖一次——这与等宽切分是同一条性质，也正是产物字节不变的原因。整数边界保持 `shardBoundary` 文本（超过 2^53 不丢精度），到达驱动层时按类型转 `int64` 或精确字符串。分片键可空时，单列键追加一个 `NullOnly` 段（`NULL` 行不丢），复合键含 `NULL` 列时拒绝采样、回落等宽。

**段的领取方式：** `--threads` 个 worker 从共享游标 `atomic.AddInt64(&nextRange, 1)` **按段序**领取。按序是硬要求：写出侧 `writeShardStream` 按段序号依次消费各自的 `shardPipe`，若某个 worker 先去干了靠后的段，它会把队列堵在那里等一个永远不会开始的段。结果收集仍按 `len(shardRanges)` 次读取（第 i 次收到即第 i 段已完成），因此不需要 `WaitGroup`——在按序消费前 `Wait()` 反而会与生产者互相等死。

**实测（本地 MariaDB 10.6.23，与改动前交付的二进制背靠背）：**

| 表 | 键 | 改动前 | 改动后 | 产物 |
|----|----|--------|--------|------|
| `bench_skew2`（100 万行，`id` 1…2 亿稀疏） | `BIGINT` | 4 段：200,000 / 0 / 0 / 800,000 行，2 个 worker 空转 | 8 段：123,749×7 + 133,757 行，最大段 133,757 行（-83.3%） | DEL 141,185,809 B、SQL 196,185,875 B 均 md5 全同；0.84 s → 0.90 s（`--threads 4`）、0.86 s → 0.91 s（`--threads 8`） |
| `bench_str2`（516,377 行，`CHAR(32)` md5 十六进制主键） | `CHAR` | **1 段**，降级单线程并告警 | 8 段（7 条探针 + 1 条统计），无告警 | DEL 79,000,008 B md5 与串行一致；0.36–0.41 s → 0.36–0.40 s（暖缓存，无可测差） |
| `bench_lay`（20 万行稠密 `BIGINT`，11 MB） | `BIGINT` | 等宽 4 段（小数边界） | 等宽，边界**逐项相同**，零探针 | 只多 1 条 `INFORMATION_SCHEMA.TABLES` 查询；DEL 6,624,389 B、单行 SQL 17,224,453 B 在 `--threads 1/4/8` 与改动前 md5 全同 |

**边界对产物的影响面（三条布局逐一实测，3 张表 × `--threads 1/4/8`）：** DEL 与默认单行 SQL 布局下**任意边界集合都产出逐字节相同的文件**（21 组 md5 全同）；`--extended-insert` 每个边界多一条 INSERT 头，实测 `bench_lay` `--threads 4` 多 192 B、`--threads 8` 多 384 B（每条头 48 B）。因此默认产物对采样完全无感，只有显式合并语句的布局会因段数多出若干条语句头。

**已知限制：** ① 统计是**整表**口径，`--query` 带 `WHERE` 时采样可能按未过滤的分布切段（结果仍是不重不漏，只是不均）；② 段数是 `2×threads`，`--extended-insert` 的额外语句头随段数增长；③ 复合键含可空列时仍拒绝采样（与改动前一致）；④ 只在 MariaDB 上实测，OceanBase 的统计口径与 tablet 分布未验证（见 11.2），按 tablet 统计分片的 OceanBase 专用路径（E5）本轮未实现。

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
- 每批字节预算 = 该值的 **9/10**，**不设下限**（变更记录 69 撤掉了此前的 64 KiB 下限：实测服务端 `max_allowed_packet` 生效值 39,936 时旧实现算出 `batch_bytes=65,536`，高于服务端自己刚说出的上限，`--batch-size 20480` 下一次 5,000 行导入退出 1、`Error 1153 (08S01): Got a packet bigger than 'max_allowed_packet' bytes`、`failed=2870`、0 行落库，而记录仍写 `limiting_factor=max_allowed_packet`；改后同一命令 `batch_bytes=35,942`、退出 0、5,000 行全部落库。小上限只意味着小批次，慢而正确）
- 读不到则按 **4 MiB** 保守值并告警
- 字节按值的实际宽度估算：字符串/字节按含转义后的宽度精确计算，标量按最宽文本形式界定

**3) 语句文本一次成型：** 每批下发的 `INSERT INTO ... VALUES (?,?,?) ...` 由一块按精确长度预分配的缓冲区一次写出。归属注释作为最前面的字节进去，不再用 `common.TagSQL` 在成品文本前面拼一遍（那会把整批内容白复制一次）；占位符组 `(?,?,?)` 按列数先拼好、每行只写一次，不再逐列逐字节写。1000 行 × 10 列的一批由 131 µs / 138,160 B / 20 次分配降到 21 µs / 24,792 B / 3 次分配（`BenchmarkBuildBatchInsertSQL`，打开归属注释），产出的文本与改写前逐字节相同，由 `TestBuildBatchInsertSQLTagged` 钉住（含 `insert_update` 的 `ON DUPLICATE KEY UPDATE` 分支）。

**4) 可选压低（`--statement-size`）：** 上面的包预算由服务端决定，用户此前无法主动把批次切小。`import --statement-size <bytes>` 与该预算取**较小值**，因此只能压低、不能抬高（抬高即让一批在服务端中途触发 error 1153）；默认 `0` 表示不额外压低，任何显式数字都照原值执行（哪怕只有一两 KB——这里已没有一条下限会反过来改写它）。压低点在 `batchBudget()`（`applyStatementCap()`）上，单线程与并行导入、DEL/SQL/Parquet 三种输入共用，每次运行只解析一次。与导出侧同名选项的语义刻意不同：导出侧是合并语句的预算（默认 8 MiB，`0` 为用法错误），导入侧是可选上限（默认 `0`，`<0` 或 `>268435456` 为用法错误），校验函数分列为 `validateStatementSize` 与 `validateImportStatementSize`。实测（测试床按 OceanBase V4.x 的默认 `max_allowed_packet=16M`，生效预算 `batch_bytes=15,099,494`；2048 行 × 约 207 B / 424,882 B 的 DEL 文件；`--batch-size 20480` 使行数不触顶；以目标库 `Com_insert` 差值计条数）：默认 **1** 条，`65536` → **7** 条（`--threads 4` 并行导入同样 7 条；SQL、Parquet 文件同样 1→7 条），`1024` → **410** 条，`268435456`（高于预算）→ 仍 **1** 条并记 `limiting_factor=max_allowed_packet`（本床驱动上限轮不到，见下）；四种情形落库行数与 `CRC32` 校验和全部一致（2048 / 4463837020299）。`1024` 那档在 128 MB 床上记的是 342 条，差在行宽而非床：同一预算下每批 6 行对 5 行，语句数就按比例上去。回归见 `internal/import/batchcap_test.go` 的 `TestApplyStatementCap` / `TestBatchBudgetAppliesStatementCapWithoutConnection`、`cmd/cli_matrix_test.go` 的 `TestCLIImportStatementSize`、`test/integration/live_mysql_test.go` 的 `TestLive_ImportStatementSizeCapsStatements`。

**5) 驱动侧插值上限（`driver_packet_cap`）：** 上面的 9/10 余量原本只看服务端 `max_allowed_packet`，但 go-sql-driver/mysql 在客户端插值时还有一道自己的闸门：`connection.go:330` 判定 `len(buf)+4 > mc.maxAllowedPacket` 就返回 `driver.ErrSkip`，`database/sql` 随即**静默**退回 `COM_PREPARE`/`COM_STMT_EXECUTE`（长数据分块下发）。这个默认值是 64 MiB（`const.go:17`，`FormatDSN` 与默认值相同时不写进 DSN，因此连接串里看不到），也就是说服务端预算超过它的那些批次**根本不会走插值路径**——不是失败，只是更慢：实测同一份 80 MiB 数据，插值 1.195 s、服务端预处理语句 2.078 s。因此默认预算现在取 `min(服务端预算, 64 MiB × 9/10)` = **60,397,977** 字节，记录里以 `limiting_factor=driver_packet_cap` 点名是谁定的（128 MB 服务端那轮：改动前 `batch_bytes=120795955 limiting_factor=max_allowed_packet`，改动后 `batch_bytes=60397977 limiting_factor=driver_packet_cap`，两种情形 2048 行都仍是 1 条 INSERT）。**这条钳制只在服务端值大于 67,108,864 B（64 MiB）时才成为决定者**：测试床按 OceanBase V4.x 默认 `max_allowed_packet=16M` 时记的是 `batch_bytes=15099494 limiting_factor=max_allowed_packet`，驱动上限轮不到；边界实测为 67,108,864 B 仍是 `max_allowed_packet`（预算恰等于 60,397,977）、67,109,888 B 起才改记 `driver_packet_cap`。**显式给出的值不改写**：`--statement-size 104857600` 仍按 104,857,600 执行（`limiting_factor=statement-size`），同时记一条 `warn` 说明这些语句将退回服务端预处理语句（带 `driver_ceiling_bytes=60397977`）——同一个数字在默认下是错的、在用户手里是选择，工具只出声不擅自改。回归见 `internal/import/batchcap_ceiling_test.go`（`TestBatchCeilingsClampsToDriverInterpolationCap` 等）与 `test/integration/boundary_test.go`（`TestBoundaryBatchBudgetIsClampedToTheDriverCeiling`、`TestBoundaryExplicitStatementSizeAboveCeilingIsHonouredAndReported`）。

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
- **可容忍的停机窗口（有量值，刻意不可配置）**：三次尝试之间只有两次退避（1s、2s，各带最多 20% 的向下抖动），加上两次重连握手的耗时，**从第一次失败到放弃实测约 2.4–3 秒**（`-P 3399` 端口无人听那次整跑 **2.675 s**：抖动把 1s+2s 压到约 2.4 s，再加三次握手）。也就是说目标库重启、LB 切换这类中断若能在 3 秒内恢复，本次运行自行接上；超过就整跑失败、退出 1。退避参数是编译期常量（`common.DefaultRetryPolicy()`），**不做成命令行选项**：每次重试都要重放当前未提交事务，窗口越长、重复写入的风险面越大，放开它会让人误以为重试能力可以无限延长。实测容错边界：停机 1.5 s 后拉起的一次并行导入（10 万行）exit 0、8 条 `transient database error, retrying`、最高只用到 attempt 2、落库 10 万行且 `COUNT(DISTINCT id)=100000`（不重不漏）。另有一处**未测**边界登记在 §11.2：`INSERT` 已提交但响应丢失（ack loss）时，`COMMIT` 不重试、而语句重试可能撞主键——这条路径至今没有用例。
- **开局建连的 ping 走同一判据**（`common.PingWithRetry`，`internal/common/ping.go`，导出与导入两侧共用）：此前是两个构造器里各写一遍的手写 `for i := 0; i < 3`，不看错误性质，因此上面那份「不重试」清单在开局这一步根本不生效——`1045` 凭据错、`1049` 库不存在、`1193` 未知系统变量（`--session-var` 写错时由服务端在建连阶段抛出）都要跑满三次尝试，实测 **6.0 s**，而且末条错误写成 `failed to ping database after 3 attempts` 把服务端给的真实原因盖在句尾。现改为：瞬态错误仍拿到 `DefaultRetryPolicy` 的三次尝试与同一条 `database ping failed, retrying`，但**该记录只在自己的重试真的被排定时出现**（三次尝试配两条告警，第三次失败不再谎称 retrying），永久拒绝一次即返回；文案里的次数是**实际尝试次数**，所以瞬态耗尽那条仍是 `after 3 attempts`，拒绝那条是 `after 1 attempt`。实测（2026-10-28，同一床）：`-p` 打错密码 / `-D nodb_zz` / `--session-var not_a_variable=1` 三种均为退出 1、**5–6 ms**、0 条 retrying 告警、错误句首即服务端原因；`-P 3399` 端口无人听为退出 1、2.675 s、2 条告警、`after 3 attempts`；服务端已停 1.5 s 后拉起的窗口内启动导入，第三次尝试连上，退出 0、5,000 行落库、2.688 s。仍**不**处理 `ctx` 取消（构造器手里没有 context，退避期间 SIGINT 打不断）——那是 §11 待办里「故障面只覆盖服务端宕机」一条要一起补的。5` 锁等待超时（`innodb_rollback_on_timeout=OFF` 下只回滚语句，盲目回放整个事务可能重复行）；`1153` 包过大（应缩小批次而非重试）。

**4) 交接队列驻留量的报价与重放缓冲的包络**

- **队列驻留量只报价、不改深度**：`--pipeline-buffer` 的单位是**批数**，而单批字节上限就是 7.4 节解析出的预算，所以真正的驻留量是 `深度 × batch_bytes`。测试床按 OceanBase V4.x 的默认 `max_allowed_packet=16M` 时预算为 `batch_bytes=15,099,494`，默认深度 10 的名义驻留 `10 × 15,099,494 ≈ 144 MiB` 落在设计意图内；`--pipeline-buffer 1024` 则等于索要 14.4 GiB 行文本。上一轮为此把深度下调到包络内，本轮按用户指令撤掉：**深度与 `--threads` 是两件正交的事**（并行导入是单读 goroutine 填一条**全部 worker 共享**的 `batchCh`，深度决定驻留批数、线程决定并发），把一个用户选项换算成另一个用户选项不在工具的权限内。现在 `resolveQueueDepth` 照给定深度建队列，只在 `queuedBytes(depth, batch_bytes)` 可能超过 1 GiB 时、于任何数据移动之前记一条 `warn`：`the hand-off queue may retain more than the memory envelope, importing at the configured depth`，字段为 `requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes` 加一条给出路的 `hint`（实测 `requested=1024 batch_bytes=15099494 queued_bytes=15461881856 envelope_bytes=1073741824`，同一条命令 `lowering` 命中 0、`pipeline_buffer=1024` 原样回显、退出 0）。报价线在哪既是算术也是实测：`⌊1 GiB / 15,099,494⌋ = 71`，本机逐档跑出来**深度 71 静默、深度 72 起出声**（`queued_bytes=1087163568`）。库调用方传入的负深度按 `0`（同步交接）处理，不再让 `make(chan, -n)` 崩溃。实测代价（200,688,900 B / 100,000 行 DEL 文件、`--threads 32`、`/usr/bin/time -v -o` 取峰值 RSS，各重复三轮）：深度 1024 为 **76,616–152,040 KB**、3.78–5.07 s，深度 10 为 **122,740–149,412 KB**、4.45–4.83 s——两组重叠，说明预算收窄到 15 MB 之后峰值内存由数据本身与 worker 数量决定，深度不再是主要变量，因此不给单点数字；128 MB 床上那几组（280,288 KB、下调后 112,584 KB、撤掉下调后的 147,340–292,356 KB）只作为「那种床上会发生什么」保留，不再用作对照。回归 `internal/import/batchcap_ceiling_test.go:TestQueuedBytesIsThePriceOfTheDepth` / `TestResolveQueueDepthHonoursTheDepthAndReportsItsPrice`、`test/integration/boundary_test.go:TestBoundaryPipelineBufferIsHonouredAndPriced`（`batch_bytes` 从本次运行的记录里读回再断 `queued_bytes`，小预算的服务端上反向断言不得出声）。
- **重放窗口随线程数增长改为出声**：回放缓冲是**每个 worker** 一份（进程预算 64 MiB 均分、下限 8 MiB），因此 8 个 worker 以上总驻留量随 `--threads` 线性增长：`--threads 256` 是 256 × 8 MiB = **2 GiB**。下限不能取消（单 worker 窗口若小于一批，`--commit-count` 就成了死选项），所以报告而不是收窄：`parallel import replay window scales with threads`（实测 `threads=200 total_bytes=1677721600`、`threads=256 total_bytes=2147483648`，`budgeted_total_bytes=67108864`）。
- **worker 数与 `max_connections` 的矛盾在任何数据移动之前说明**：并行导入与并行导出的每个 worker 全程持有**一条**连接，`--threads` 的静态范围（1–256）对此一无所知。超出服务端上限时，连不上的 worker 重试若干次后会让整跑失败，而此时其他 worker 已经提交了行——这已经不是"慢一点"而是"半途既成事实"。故在启动 worker 之前比较 `threads + 1` 与 `@@max_connections`（`common.MaxConnections`），记 `warn`（实测 `threads=200 connections_needed=201 max_connections=151`，导入侧结尾 `may fail the run`、导出侧 `may fail the export`）。**告警而非拒绝**：DBA 可以现场调高上限，且连接确实够用的那次运行结果仍然正确。
- **并行导入的失败路径不再崩溃**：worker 报错时读端往往还在生产数据。`errCh` 在 `wg.Wait()` 之后关闭，而读端 goroutine 当时**不在**同一个 `WaitGroup` 里且仍会向该通道发送，于是 `panic: send on closed channel`：退出码 **2**（用法类，掩盖了真实的数据错误）、日志截断、已提交的行留在库里。读端加入同一个 `wg` 后通道只能在两阶段都结束时关闭。回归 `test/integration/boundary_test.go:TestBoundaryParallelImportFailureExitsOneWithoutPanic`（20,000 行、第 10,000 行为重复主键，三次运行均退出 **1**，日志含 `insert batch failed` / `import aborted read=20000 inserted=19000 failed=1000` / `command failed`，`panic:`、`send on closed channel`、`goroutine ` 三种崩溃标记命中数为 0）。

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
| 开局建连失败 | 同一判据同样管住 `NewExporter`/`NewImporter` 的第一次 ping（`common.PingWithRetry`）：瞬态错误最多 3 次尝试、退避同 7.7，且 `database ping failed, retrying` 只在还有重试时出现（3 次尝试配 2 条告警）；`1045`/`1049`/`1193` 这类服务端拒绝一次即返回，错误文案按实际次数写（`after 1 attempt`），实测 5–6 ms 而非旧的 6.0 s |
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
│   │   ├── log.go             # 日志模块（console/text 双布局、版本横幅、一条记录一行）
│   │   ├── diag.go            # 诊断辅助（阈值告警 WarnIfSlow、扫描停顿 StallWatch）
│   │   ├── password.go        # 密码加密/解密（PBKDF2-HMAC-SHA256 派生 + AES-256-GCM）
│   │   └── detect.go          # 服务端 max_connections 读取（并行 worker 上限告警）
│   ├── export/
│   │   ├── exporter.go        # 导出调度器（Pipeline 模式、并行分片、writeShardStream 流式写出、分片读完即释放连接）
│   │   ├── configlog.go       # `export config` 回显（按登记表过滤字段、`effectiveHexBlob()` 判定生效值）
│   │   ├── del_writer.go      # DEL 格式写入器
│   │   ├── sql_writer.go      # SQL 格式写入器（默认每行一条 INSERT、`--extended-insert` 才按字节合并/转义）
│   │   ├── parquet_writer.go  # Parquet 格式写入器（列式存储）
│   │   ├── shard.go           # 分片逻辑（主键/唯一键检测、按值域等宽切分）
│   │   ├── shard_sample.go    # 分片边界采样（整表统计 + 定位探针 + 回落，支持非数值键）
│   │   ├── diag.go            # 导出侧诊断常量（扫描停顿阈值 10 秒、分片速率换算）
│   │   ├── shard_stream.go    # 分片到输出文件的字节队列（并行导出流式写出，免临时文件）
│   │   └── sql_parser.go      # SQL 解析器（JOIN 检测、hint 保留）
│   └── import/
│       ├── importer.go        # 导入调度器（Pipeline 模式、列映射、并行导入）
│       ├── configlog.go       # `import config` 回显（按登记表过滤 DEL 布局字段）
│       ├── batchcap.go        # 导入攒批字节上限（max_allowed_packet、--statement-size 压低、65535 占位符）
│       ├── diag.go            # 导入侧诊断常量（慢批/慢提交阈值 5 秒、批字节估算、worker 速率）
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

另有 37 个包内 `*_test.go`（`internal/common` 11、`internal/export` 11、`internal/import` 13、`internal/config` 2），加上 `cmd` 5、仓库根 1、集成 6，全仓库共 49 个测试文件，清单见十一节。

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
| SQL 语句布局 | 默认导出为 SQL → 验证每行一条 INSERT；`--extended-insert` → 验证语句数严格减少且合并后的语句不超 `--statement-size`；`--extended-insert` 配一个低于单行宽度的 `--statement-size` → 验证产物退回每行一条 INSERT 且写入器记一次告警；各布局各自导入 → 数据校验一致，且服务端收到的语句数相同 |
| 导入字节上限 | 宽表/BLOB 大行导入 → 验证不触发 error 1153（`Packet too large`） |
| 巨型 INSERT 导入 | 导入含超长 INSERT 语句的 SQL 文件 → 验证 `recordReader` 无上限读取 |
| 导出/导入转义往返 | 含 `\n`/`\r`/`\t`/`'`/`\`/`\0`/`\x1a` 的数据 → 导出 SQL → 导入 → 数据一致 |
| 占位符上限 | 宽表（columns > 100）导入 → 验证 `batch-size` 自动下调且不报错 |
| 分片边界采样 | 稀疏/倾斜整数键与 `CHAR`/UUID 主键并行导出 → 验证段数达到 `min(2×threads,64)`、产物与单线程逐字节一致、日志无 `only 1 shard range`；稠密小表 → 验证零探针且边界等于等宽结果；统计缺失或探针报错 → 验证回落等宽并记 WARN |
| 告警记录 | 触发 `--no-hex-blob`/超长语句/`max_allowed_packet` 读取失败/无表头首行丢弃/`insert_update` 无唯一键 → 验证对应 WARN 记录写进 `--messages` 文件（未给该选项时在 stderr） |
| `--no-escape` 冲突数据 | `--no-escape` 导出含定界符的数据 → 验证写入前报错且不产生半行残留 |
| SQL 格式多表查询 | `--format sql` + JOIN/派生表/CTE → 验证解析阶段报错并提示改用 del/parquet |
| 位置映射列数 | `--columns` 条目数与文件列数不符、或同一目标列写两次 → 验证写入前报错并提示名称映射 |
| 名称映射子集 | 3 列文件只写 `name=t_name,id=t_id`（`note` 未映射、目标表另一列未映射）→ 验证两列按文件列顺序写入、目标列取 DEFAULT、未映射列不进 INSERT；三格式与并行路径逐值一致；未知文件列/重复文件列/重复目标列/无表头名称映射 → 验证读取数据前报错 |

### 11.1 测试资产清单

上表按"场景"组织，这里按"文件"给出对应的落地位置与规模（当前共 49 个 `_test.go`、272 个 `Test*`（含 1 个 `TestMain`，负责一次性构建被 CLI 测试调用的二进制）、6 个 `Fuzz*`、12 个 `Benchmark*`。计数口径同时纠正一处长期偏差：本表原记 `test/integration/option_matrix_test.go` 为 12，`git grep -c '^func Test' HEAD` 实测为 13，本轮起逐文件按实测复核。

| 文件 | 用例数 | 覆盖点 | 运行方式 |
|------|--------|--------|----------|
| `cmd/cli_matrix_test.go` | 14 | 参数组合矩阵与退出码契约：DEL/SQL/parquet 专属选项互斥、`--log-format` 标志与环境变量的优先级（`TestCLILogFormatEnvOverridesFlag` 四个子用例：都不给时保持内置 `console`、只给 env、只给 flag、两者同时给出时 env 覆盖 flag，断言落在 `common.LogFormat` 上；该用例不建连，靠 `--progress-interval 1ms` 让参数校验先短路）、必填项缺失、`encrypt` 位置参数个数（越界为用法错误 2、密钥缺失仍为运行时 1）、`--progress-interval` 的默认值与取值边界（0 与负值报 `must be > 0`、`1ms`/`10ms`/`99ms` 报 `must be >= 100ms`，均为用法错误 2；`100ms` 与 `30s` 合法）、SQL 语句布局标志（缺省 `ExtendedInsert=false`、`--extended-insert` 落到 config、已删除的 `--single-row-insert` 是未知选项并退出 2、`--extended-insert` 配 `--format del` 与配 `--format parquet` 被拒）、`--parquet-compression` 的三个合法取值 / 两个文本格式越界 / 非法取值（`TestCLIParquetCompression`，拒绝均为用法错误 2）、改名后的 `--no-col-names` 在两侧都是未知选项（`TestCLINoColNamesIsUnknown`，钉住“不留别名”）、同名 `--statement-size` 在两侧相反的合法性（导入接受 `0` 并原样落到 config、导出给 `0` 仍是用法错误 2、两侧共享 268435456 上界）；并行导出对被拒选项的矩阵（`TestCLIParallelExportRejectsPipelineOptions` 九个用例：只给 `--threads` 合法、`--batch-size`/`--pipeline-buffer` 显式给出即拒、**给的就是默认值同样拒**、`--threads 1` 与完全不指定线程数都合法、非法 `--threads` 与非法选项值各自先于冲突报出、所有拒绝都满足 `common.IsUsageError`；`TestCLIImportKeepsPipelineOptionsWithThreads` 钉住 `import --threads 4` 两个选项照旧接受）| `make test` |
| `cmd/validate_fuzz_test.go` | 5 fuzz | 边界校验函数（batch-size、pipeline-buffer、statement-size 的导出与导入两版、threads）对畸形输入不 panic，且两侧的 `0` 判定相反这一事实被分别钉住 | `make test` |
| `cmd/version_log_test.go` | 2 | `version` 子命令不写日志记录（stdout/stderr 捕获后断言），`run` 横幅仍照常记录 | `make test` |
| `cmd/option_scope_test.go` | 3 | 格式适用范围三处同源的命令行侧：登记表里每个 flag 都存在于对应子命令且其帮助文本以范围标记结尾、渲染后的 `--help` 里确实看得到该标记；九个 DEL 布局选项不得漏登记（漏一个就会被静默忽略）；`annotateOptionScopes` 对同一命令重复调用不会追加第二个标记（命令树是包级的，init 与测试会走同一条注册路径）| `make test` |
| `cmd/validate_boundary_test.go` | 1 | `TestCLIDelimiterCollisions`：DEL 四个文本修饰的 11 组取值 × 导出与导入两条命令（5 对同值组合必须拒、`--escape` 与 `--char-del` 同值必须放行、`--no-char-del` 与 `--no-escape` 各自的免除、多字符分隔符合法、`\t` 这类转义写法必须按**解析后**的值比较而不是按字面串），并断言错误顺序取自固定的 char→col→row→escape（Go map 遍历会让同一条命令在不同运行里报不同的冲突）| `make test` |
| `internal/common/*_test.go`（11 个文件）| 48 | **开局建连的重试判据（`ping_test.go`：进程内假连接器 `pingFake` 按脚本回答 `db.Ping()` 并计数，钉住 `1045`/`1049`/`1193`/`1064` 四类服务端拒绝各只发一次 ping、不产生 retrying 记录、文案写 `after 1 attempt`，瞬态故障重连两次后恢复且告警数等于重试数，退避耗尽为三次尝试两条告警配 `after 3 attempts`，`MaxAttempts=0` 的退化上界仍然诚实计数）**；耗时字段固定以秒、三位小数呈现（`logsql_test.go`：`Seconds` 六组边界，交互 SQL 在 info 级静默、debug 级脱敏回显且不含真实值）、瞬态错误判定、日志单行与切布局保持级别、日志文件 sink（挂载后接管 stderr、关闭后归还、并发写入不交叉、替换与失败路径）、进度计时器 stop 排空在飞的记录且二次 stop 不死锁、间隔未到不产出任何进度记录且 stop 不空等一个间隔、重试退避与取消、会话变量解析与 DSN 注入、失败 SQL 脱敏回显、DSN 构建、口令加解密（往返含空串/前缀串/CJK/控制字符、布局长度恰为 salt+nonce+明文+标签、盐与 nonce 每次不同、不同盐派生出不同密钥、错密钥与篡改盐/密文/截断/非法 base64 全部拒绝、旧布局密文必须失败、空密钥拒绝、`ResolvePassword` 四种分支）| `make test` |
| `internal/config/config_test.go` | 2 | `--progress-interval` 的解析结果：导出与导入两侧默认恒为 5 秒，显式取值原样生效，零值与负值回落默认值（`time.NewTicker` 对非正间隔会 panic，库调用方不许把它传进来）| `make test` |
| `internal/config/formatscope_test.go` | 4 | 格式适用范围登记表本身：`AppliesTo` 的格式×选项矩阵（含未登记选项恒等于"与格式无关"、导入侧 `statement-size` 刻意不登记）；`Violation` 的文案与首条命中顺序必须与既有的用法错误逐字相同（把拒绝逻辑合并到一个函数不能改变用户读到的内容）；登记表自洽性（无重复、无"列出全部格式"的伪范围、每个未覆盖格式都拿得到点名该选项的文案）；导出与导入两侧的 DEL 组必须是同一批九个选项，且含此前回显漏掉的 `date-fmt`/`no-escape`/`no-char-del` | `make test` |
| `internal/export/*_test.go`（11 个文件）| 88 | 配置回显按 `--format` 过滤（`configlog_test.go`：三种格式的字段集合、DEL 布局 9 项只在 `-f del` 出现、SQL 布局 4 项只在 `-f sql` 出现、`hex_blob` 三种格式都不越界出现在 parquet，且参数恒为成对 key/value；此前漏打的 `date_fmt`/`no_escape`/`no_char_del` 必须出现；`hex_blob` 取生效值——`gbk` 自动开、`--no-hex-blob` 强制关，`effectiveHexBlob()` 六种组合逐一钉住）、DEL/SQL/Parquet 写入器转义与攒批、Parquet 页压缩编解码器（`parquet_writer_test.go`：`none`/空串→`Uncompressed`、`snappy`→`Snappy`、`zstd`→`Zstd`，经 `parquet.OpenFile` 读列块 `MetaData.Codec` 断言；未知 codec 必须报错）、分片边界精确运算与 hint 保留、hex-blob 判定、并行导出流式写出的 header/LOCK 配对与分块顺序（`exporter_stream_test.go`）、分片字节队列的语义（`shard_stream.go`：任意写入切分后重组逐字节一致、块不超上限、队列满时背压、消费端停止后生产端被 `shard output consumer stopped` 解除阻塞、ctx 取消同样解除、Close 幂等且刷出尾部、队列深度按分片数均分且内存有界）、并行路径对被忽略选项的判定（`shard_stream_test.go` 的 `TestParallelExportPipelineOptionNotice`：默认值不报警、任一值被改动即报警，含 `--pipeline-buffer 0` 这个合法值）；分片资源释放顺序（`exporter_shard_test.go`：进程内假 `driver.Connector` 记录连接关闭，一个用例起一个**故意没有消费者**的 `shardPipe`，断言分片的连接与快照在读完时就结束、而 `exportShard` 仍阻塞在交尾字节上——改动前的 `defer` 顺序下该用例必然超时失败，另一个用例断言空分片既不挂住写出的队列也不留连接）、`--query` 的表名解析（`sql_parser_test.go`：`ExtractTableName`/`SimpleTableName` 对 `` `db`.`tbl` `` 只返回**裸表名**——把 schema 二次拼接的缺陷钉住；新抽出的 `ExtractTableRef` 单独把 schema 与表名拆开，仅供 `SHOW CREATE TABLE` 使用；非 SELECT 语句一律拒绝）；SQL 写入器两种布局的告警对称性（`sql_writer_test.go` 新增 `captureWriterWarnings`，把 `common.Logger` 临时换成 warn 级 `slog` 内存 handler，四个用例分别钉住：`--statement-size` 低于单行宽度时合并布局恰好告警一次且产物逐行、默认尺寸真正合并必须静默、单行文件不得误报合并失败、单行布局仍只告警一次且不得出现合并告警）；分片边界采样（`shard_sample_test.go` 12 个用例，进程内假 `driver` 按查询形态应答并记录收到的语句与绑定参数：段数计划 `min(2×threads,64)` 与边界生成、整数键类型判定、密度可证判据的 7 组取值（含 `BIGINT` 满跨度不误判）、"采样是否值得"的 6 组判定、探针 SQL 形状（`` `k` > ? `` 加 `ORDER BY` 加 `LIMIT 1 OFFSET`）与绑定参数逐段接续上一段边界、重复边界被跳过、稠密小表**一条探针也不发**且结果与 `GenerateShardRanges` 逐项相同、稀疏整数键 8 段连续无缝且整数边界以 `int64` 抵达、五类失败路径（无统计/统计查询报错/探针报错/探针取到 NULL/段数不足）全部回落原行为、单段计划不发任何查询、`rangesFromBounds` 保持 NULL 行仍被覆盖且复合键拒绝采样）、分片键在被忽略时必须出声（`shardkey_ignore_test.go` 2 用例：`warnIfShardKeyIgnored` 的五组路由取值——`--threads 1` 带合法键、`--threads 1` 带不存在的键、`--threads 4` 与 `--threads 16` 两条回落都要报，未给键不报，断言记录里点名 `shard_key`/`threads`/`fallback`；配置记录侧三种格式各三组取值，断言 `shard_key_ignored` 只在"写记录时就已确定用不上"即 `threads<=1` 且给了键时出现）| `make test` |
| `internal/import/*_test.go`（13 个文件）| 57 + 1 fuzz + 12 bench | 配置回显按 `--format` 过滤（`configlog_test.go`：DEL 布局 9 项只在 `-f del` 出现，行流控制 `skip_count`/`row_count`/`commit_count` 与 `statement_size` 三种格式都必须在场——后者约束本命令生成的语句而非输入文件）、攒批字节上限与 65535 占位符（`TestApplyStatementCap` 钉住导入侧 `--statement-size` 只压低不抬高、等于预算与低于 64 KiB 下限时各自的取值，`TestBatchBudgetAppliesStatementCapWithoutConnection` 确认没有连接（`imp.db == nil`，走 4 MiB 保守值）时也在同一处压低预算）、DEL/SQL 读取器回读、无上限记录读取、会话重连回放与空闲提交、重放字节预算按 worker 均分（`replayBudget` 在 1/2/4/8/64/0/−1 线程下的取值与整除性、会话确实持有自己那一份、批次在自己的字节上限处提交而非在进程上限处）、位置映射列数校验、`inserted`/`updated`/`unchanged` 拆分（含 affected 读不到与超限钳制）；DEL 读取器重写以差分方式守住语义——保留改动前的 `parseLine`/`scanRows` 为参考实现，用 28 个手写用例 + 4000 行随机语料 × 6 套定界符配置逐字段比对，配 `FuzzDelReaderParse` 持续模糊，另有空 `--col-del` 的超时守卫用例（`del_reader_equiv_test.go`、`del_reader_test.go`）；`del_reader_bench_test.go` 用 2 万行、贴近导出器真实产物（全字段带引号、空串与 NULL 可区分、二进制列裸 `0x`、引号内含 `||`/`;`）的确定性语料覆盖整行读取、纯分帧、纯解析三段；SQL 语句级重写另有 `sql_reader_bench_test.go`（同一份 2 万行数据按默认单行、`--extended-insert` 多行、跨物理行折行三种布局各一段，每段都断言行数与列数，防止用跳过工作来换速度），`sql_reader_test.go` 补多行语句重组（值内真换行、值内 `;`、双写引号、`;` 单独占一行）与小写关键字 + 非 ASCII 值两个用例，`importer_test.go` 的 `TestBuildBatchInsertSQLTagged` 钉住攒批语句文本与 `common.TagSQL` 逐字节相等（含 `insert_update` 的 `ON DUPLICATE KEY UPDATE` 后缀），`BenchmarkBuildBatchInsertSQL` 在打开归属注释的前提下测每条批次的语句文本构造；`batchcap_ceiling_test.go`（5 个用例）把预算解析里三处"值从哪来"的判定做成纯函数并逐个钉住——`TestBatchCeilingsClampsToDriverInterpolationCap` 六组（大服务端预算被钳到插值上限、上限内的小预算原样保留、显式值低于上限照常生效、显式值高于上限既生效又出声、**显式值大过服务端预算时也不能反过来绕过钳制**、`0` 保持钳后默认值，并逐一断言 `limiting_factor` 的三种取值）、`TestDriverBatchBudgetStaysInsideTheCap`（钳后的预算必须严格低于 64 MiB 且等于共用余量公式的结果，否则修复本身就是 no-op）、`TestQueuedBytesIsThePriceOfTheDepth`（七组：`depth<=0` 与预算 `<=0` 返回 0、按批数×预算线性计价的几组、`1024 × math.MaxInt64` 饱和到 `MaxInt64` 而不是溢出为负）、`TestResolveQueueDepthHonoursTheDepthAndReportsItsPrice`（断 `imp.queueDepth` 恰为配置值 1024、报价记录六个字段齐全、默认深度静默、库调用方 `PipelineBuffer = -1 → 0`；`captureLog` 把 `os.Stderr` 临时指向文件后读字段）、`TestWarnIfReplayWindowScalesReportsEnvelope`（256 线程报出 2 GiB 总量与 64 MiB 预算、4 线程静默）| `make test`（bench 用 `go test -run '^$' -bench . ./internal/import`，fuzz 用 `-fuzz FuzzDelReaderParse`）|
| `main_test.go` | 1 | go/ast 静态守卫：全部 logger 调用消息首字母小写且单行（防日志词汇回归）| `make test` |
| `test/integration/roundtrip_test.go` | 10 | 离线往返（写入器→读取器）：转义文本、每条语句一个物理行、`--statement-size` 攒批、`0x` 二进制字面量、MySQL 文本值与 HEX 一致、DEL 引号/反斜杠两种模式、看似十六进制的文本不被误解码、单行 INSERT | `make test` |
| `test/integration/live_mysql_test.go` | 12 | 真实 MySQL 端到端：三种格式往返、并行导出与并行导入、四种导入模式、列映射、导入行数拆分对真实服务端 `RowsAffected` 的断言；`--messages` 日志文件随用例断言（记录齐全、无 `SQL3xxxN`、每行都是日志）；进度记录不得晚于结束汇总（单线程与并行 × 导出与导入，库内调用把计时器压到 1 ms——校验只在 argv 上，库调用方可自行决定；末段走真实二进制，断言下限值 `--progress-interval 100ms` 生效、配置回显带 `progress_interval=100ms`、查询用六行 `SLEEP(0.05)` 把运行拉到约 300ms 使 100ms 档仍有进度记录、不再有 `started` 记录）；并行导出对 `--batch-size` 的忽略告警三段式（改动过它在并行分片下必须出现、两者保持默认值必须静默、单线程下用了就不得报警）；导入 `--statement-size` 的服务端实测（同一份 2048 行文件、同一 `--batch-size`，只用目标库 `Com_insert` 差值断言默认 1 条语句、`--statement-size 65536` 为 7 条，并复核两表落库行数与 `CRC32` 校验和一致、日志里 `limiting_factor` 随服务端预算取 `max_allowed_packet` 或 `driver_packet_cap`（由 `defaultCapFactor` 现读 `@@max_allowed_packet` 决定，不写死本机）、显式压低时为 `statement-size`） | `IMEX_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/db' make test-live`，未设置该变量时自动跳过 |
| `test/integration/boundary_test.go` | 7 | 运行时选项在**接上真实服务器之后**的边界行为（`cmd/cli_matrix_test.go` 只能钉住校验接受哪些值，而这里的三条上限 `max_allowed_packet`、`max_connections` 与驱动插值上限都在连接的另一侧），每个用例都断言"运行对自己作了什么记录"加上契约承诺的退出码：默认预算被钳到 60,397,977 且 `limiting_factor=driver_packet_cap`（服务端预算低于上限时反向断言不得被钳）、显式 `--statement-size` 只能压低（`TestBoundaryExplicitStatementSizeIsLoweredOnly`：同一张表先跑预算内的 `packetBudget/4` 并断 `limiting_factor=statement-size` 且驱动告警缺席，再跑 CLI 接受的最大值 268435456，第二次按本床实际决定者分三支断言——`packetBudget > 268435456` 走 `statement-size`、`> 60397977` 走 `driver_packet_cap`、其余走 `max_allowed_packet`，两次运行用两张表否则第二次撞 `1062`）、`--pipeline-buffer 1024` 按 1024 执行、`pipeline_buffer=1024` 原样回显、日志里 `lowering` 命中 0，并断言那条报价记录的 `requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes` 四值互相自洽（`batch_bytes` 从本次运行读回，`queued_bytes` 按其 × 深度算出，不由本机常量写死）、`--threads 200` 超过 `@@max_connections=151` 时报出 `connections_needed=201`、20,000 行含重复主键的并行导入三次运行都退出 **1** 且日志里 `panic:`/`send on closed channel`/`\ngoroutine ` 三种崩溃标记命中为 0（同时断言 `insert batch failed` 与 `command failed` 在场——修复前的现场是退出 2 加半截日志）、显式 `--shard-key nosuchcol` 退出 **2** 并点名选项、取值与表名而**不**回落单线程（同表不带 `--shard-key` 的自动检测路径必须仍然正常导出）。服务端取值由 `defaultCapFactor`/`serverMaxAllowedPacket`/`serverMaxConnections` 现读，断言按取值分支而不是按床跳过——因此 OB 默认 16M 床、128 MB 床与更大的床都跑得到 0 SKIP；驱动告警的检索由助手 `warnNeedle` 统一取第一行；`TestBoundarySingleThreadedExportNamesAnIgnoredShardKey` 把 `--shard-key` 的三条路由在同一张 512 行表上跑一遍——`--threads 1 --shard-key nosuchcol` 必须退出 0 且同时出现 `single-threaded export ignores --shard-key`、`shard_key=nosuchcol`、`threads=1`、`fallback=false`、`shard_key_ignored=true`，`--threads 4` 带 JOIN 必须出现回落记录与 `fallback=true` 而**不**出现 `shard_key_ignored`（写记录时还看不出来），`--threads 2 --shard-key id` 必须既无 `ignores` 也无 `ignored` 字段、只回显 `shard_key=id` | 同上 |
| `test/integration/lock_contention_test.go` | 3 | 导入并发与重复主键的锁冲突（真实二进制 + 真实库）。fixture 让每个主键恰好出现两次，且第二次在**相邻批次里反序**（`--batch-size` 与块大小对齐，块序号取模决定升/降序），DEL 与 SQL 两份文件由同一张表按 `ORDER BY seq` 导出。① 单线程：重复键只能是 `1062`/`Duplicate entry`，断言输出里**不出现** `1213`/`Deadlock`（单会话不可能自锁），`--mode insert_update` 则全部键落地；② `--threads 4` + 反序重叠键（sql、del 各一次）：不断言"必须死锁"——是否成环取决于服务端调度，实测同一 fixture 下 del 抓到 1 次、sql 那次没成环——断言的是任何退出码下都不落重复行（`COUNT(*) == COUNT(DISTINCT id)`）、落库行数不超过文件里的键数、失败时只能报 `1213`/`1205` 这类服务端锁错误（`1213/1205` 不在可重试集合内这条由 `internal/common/dberr_test.go` 钉住，因此不会先重放再报成功），服务端死锁计数增量作为观测值打印（变量名 MySQL 为 `Innodb_row_lock_deadlocks`、MariaDB 为 `Innodb_deadlocks`，两者都取不到时降级为只看报错文本）；③ 对照组：同一键集、同样 4 线程、批次一律升序，必须零死锁且全部键落地——把"按键有序"这条规避手段变成被测性质 | 同上 |
| `test/integration/export_cli_test.go` | 4 | 导出 CLI 侧的三项改动固化为真实二进制 + 真实库回归。① `TestLive_ExportSchemaQualifiedQuery`：`--query` 用 `` `db`.`tbl` `` 这种带 schema 的表名，默认（数据）与 `--create` 两种都必须成功、输出不得出现 `1146`/`doesn't exist`，产物只按裸表名写 `INSERT`（断言文件不含 `` `db` `` 且含 `INSERT INTO \`tbl\``），再导入一张 `LIKE` 出来的新表逐行核对——把"schema 二次拼接"这条缺陷钉死；② `TestLive_ExportCreateDefault`：默认 SQL 导出无 `CREATE TABLE`、`--create` 才有、`--no-create` 已是未知选项（退出 2 且文案点名该 flag）、`--create` 配 `-f del` 报范围用法错误退出 2；③ `TestLive_ExportQueryFile`：`--query-file` 读文件（含首尾空白与结尾分号的语料）导出后导入核对，配置回显打印 `query_file=<路径>`，并钉四条用法错误各退出 2——`-q` 与 `--query-file` 互斥、两者都不给、空/仅分号文件、文件不存在；④ `TestLive_ExportQueryFileParquet`：`--query-file` 与格式无关，parquet 也能用它导出且产物非空。fixture 复用 `makePair` 的标准多列表，`snapshotTable`/`assertSameRows` 可直接比对 | 同上 |
| `test/integration/option_matrix_test.go` | 16 | 编译出真实二进制后用 argv 跑交叉矩阵：DEL 布局修饰符的导出/导入成对性、`--no-col-names` 无表头配对歧义、逐字写入模式的拒绝与残留文件清理、SQL 语句布局（默认 10 行 10 条语句、`--extended-insert` 严格减少语句数、`--extended-insert --statement-size 512` 的合并语句不超上限+4 字节、`--extended-insert --statement-size 64` 低于单行宽度时产物退回每行一条 INSERT、单给 `--statement-size` 不改布局，每种布局都逐行往返校验）、并行导出：DEL 与默认 SQL 逐字节等于串行，`--extended-insert` 时只多分片边界的语句头（按头数与字节差精确断言）、日期时间格式的对称性与精度截断、`--create`/`--no-data` × `--mode create`（SQL 导出默认仅数据，四个子用例分别覆盖默认仅数据可直接导入、`--create` 带回建表且保留数据、`--create --no-data` 仅结构建表、默认仅数据配 `--mode create` 必须明确失败；用例名由 `TestOptionMatrixNoDataNoCreate` 改为 `TestOptionMatrixNoDataCreate`）、`--skip-count`/`--row-count` × 线程/批量/提交、`--messages`/`-m` 在三格式与并行路径下等于命令行日志且终端静默（含 `-m` 短选项、`--log-format text`、`--log-level error`、失败记录）、`--no-overwrite` 哨兵文件存活、`--default-charset` 与二进制列 0x 自动切换；**非数值分片键的并行导出**（`TestOptionMatrixTextShardKeyParallelExport`：240 行 `md5` 十六进制 `CHAR(32)` 主键，串行与 `--threads 4` 的 DEL/SQL 产物互比逐字节相同，日志里不得出现 `only 1 shard range` 且必须出现不少于 2 条 `shard export completed`，并行产物导入姊妹表后按键序逐行比对——不比文件字节，因为两份产物各自嵌入自己的表名）；调试级交互 SQL（`TestOptionMatrixDebugEchoesInteractionSQL`：info 级无 `db interaction`，debug 级出现 `SHOW CREATE TABLE`/`information_schema` 与 `worker=`、不得出现攒批 INSERT、汇总耗时匹配三位小数）；**开局建连失败的两条分类（`TestOptionMatrixConnectionRejectionIsNotRetried`：export 与 import 各用错密码跑真实二进制，必须退出 1、日志无 retrying、末条为 `failed to ping database after 1 attempt` 且句中含服务端 `Error 1045`、整跑耗时 < 3 s（旧行为固定 6.0 s）、导出侧不得留下输出文件）** | 同上（`TestMain` 先 `go build` 一次，全部用例复用该二进制） |

`make test` 与 `make test-live` 固定 `CGO_ENABLED=1 go test -race`，这是 `-race` 的要求，与发布构建的 `CGO_ENABLED=0` 无关。

### 11.2 当前环境尚未自动化覆盖的场景

| 场景 | 现状 |
|------|------|
| OceanBase 端到端 | 沙箱内无 OceanBase 实例。仅覆盖到 SQL 层的 OceanBase 分支单测（如 OceanBase 下不写表锁的合并用例），完整链路未验证。分片边界采样同样只在本机 MariaDB 10.6.23 上实测：它只使用 `INFORMATION_SCHEMA.TABLES`、`INFORMATION_SCHEMA.COLUMNS` 与普通的 `ORDER BY ... LIMIT 1 OFFSET` 定位查询，不含窗口函数与方言语法，但 OceanBase 下的统计口径（`TABLE_ROWS` 是否为估算值、`AVG_ROW_LENGTH` 是否可得）与 tablet 级分布未验证；按 tablet 统计分片的 OceanBase 专用路径（E5）本轮未实现 |
| GB 级大文件 | 由 12 个 benchmark 覆盖单点（记录读取器、DEL 整行读取/分帧/解析、SQL 三种语句布局的读取、攒批字节估算、批次语句文本构造），属微基准；8.1 的大文件与吞吐是目标值，没有端到端自动化断言，需在真实环境按压测计划验证 |
| 长时间稳定性 / 断连恢复 | `internal/import/session_test.go` 用模拟重连覆盖回放逻辑，真实网络中断（LB/NAT 回收）未做长时间实验；导出侧只把"已完成读取的分片不再持有连接与快照"这一空等窗口缩到零（`exporter_shard_test.go` 用进程内假连接器钉住释放顺序），真正的 LB/NAT 回收仍需在有云的环境上实测，本仓库只覆盖到"空闲窗口归零"这一层（§8.2 并行导出分片释放）。**开局建连这一层本轮补上了**：`internal/common/ping_test.go` 以进程内假连接器钉住永久拒绝不重试、瞬态故障重试到上界，`TestOptionMatrixConnectionRejectionIsNotRetried` 用真实二进制跑通两侧；服务端在退避窗口内恢复的那次实测为第三次尝试连上、5,000 行落库（见 2.3 末段）。**仍未自动化**的是真实 LB/NAT 回收的长时中断，以及退避期间的 SIGINT——构造器手里没有 context，`PingWithRetry` 的 `time.Sleep` 打不断（§7.7 末条已点名）|
| `COMMIT` 的 ack 丢失 | `COMMIT` 本身刻意不重试（响应丢失时无法判断是否已生效，重放可能重复写入），而语句级重试会回放未提交的事务。「服务端已提交、客户端没收到响应」这一歧义窗口**没有用例**：此时语句重试会撞主键，落库行数与退出码会怎样、是否计入 `failed`，至今未实测。窗口量值见 §7.7「可容忍的停机窗口」 |

---

## 十二、交付物

| 交付物 | 说明 |
|--------|------|
| 源代码 | 完整 Go 项目源码（含 49 个 `_test.go`、`docs/requirements.md`、`docs/cli-oracle.txt`），发布形态为 `imex-<version>-<commit>[-dirty]-src.tar.gz`，排除 `.git/`、`bin/`、`dist/`、`vendor/` |
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

> 本节的沙箱绝对路径与测试库凭据已脱敏：`<workspace>/` 指本地工作区根目录，`<tmp>/` 指临时目录，`<user>:<password>` 指本地测试库账号。自 2026-10-27 起本节的**历史版本**也一并脱敏——原文已从提交历史中移除（做法、提交号映射表与公开仓库侧的残留对象见第 63 行），脱敏前原文不再保留在提交历史中。

### 2026-10-22 本轮改动

| # | 改动项 | 说明 |
|---|--------|------|
| 1 | `--statement-size` 默认值调整 | 从 16 MiB（16777216 字节）降为 **8 MiB（8388608 字节）**，更贴近典型 `max_allowed_packet` 设置，降低导入侧重建语句时的内存峰值 |
| 2 | 导入攒批新增字节上限 | 新增 `batchcap.go`：导入侧按 `max_allowed_packet` 的 9/10 设定每批字节预算（下限 64 KiB），同时按 `rows × columns ≤ 65535` 下调有效 `batch-size`。补上了宽表/BLOB 大行在导入侧可能触发 error 1153 的缺口 |
| 3 | 补齐导出/导入转义与换行往返测试 | SQL 格式转义规则完整覆盖 `\n`/`\r`/`\t`/`\\`/`\0`/`\x1a`/`''`，确保每条 INSERT 恰好占一个物理行；导入侧 `recordReader` 替代 `bufio.Scanner`，无 token 长度上限 |
| 4 | 新增 `--single-row-insert` | SQL 格式专属选项，开启后每行一条 INSERT 语句（等价 mysqldump `--skip-extended-insert`），`--statement-size` 退化为告警阈值。**注：第 47 行把这一布局改为 `--format sql` 的默认值，该选项降级为兼容别名，合并改由 `--extended-insert` 显式开启；第 62 行把它彻底删除，传入即 `unknown flag: --single-row-insert` 退出 2。** |
| 5 | 并行导出 header/footer 统一 | `mergeShardFiles` 由 SQLWriter 统一写出 SET NAMES、CREATE TABLE、LOCK/UNLOCK TABLES；各分片片段设置 `SetSkipTableLocks(true)` 且 `writeCreate=false`，不再各自写 LOCK/UNLOCK **注（变更记录 72，2026-10-28）：本行的 LOCK/UNLOCK TABLES 与 `SetSkipTableLocks` 已整体删除——产物不再生成表锁语句，历史描述照录。** |
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
| 42 | 交叉选项 / 交叉命令行为矩阵，并修复它暴露的两个数据完整性缺陷 | 此前 CLI 侧只有 `cmd/cli_matrix_test.go` 钉住"哪些组合被接受或被拒"，库侧只有 `live_mysql_test.go` 走 API；**"选项被接受之后是否真的生效、导出与导入两侧是否对称"没有覆盖**。新增 `test/integration/option_matrix_test.go`（12 个测试入口、`TestMain` 一次性 `go build` 出真实二进制后用 argv 驱动，故 flag 解析、校验、config 装配、写入器、读取器全程在链路上）：DEL 布局修饰符成对往返（`--char-del`/`--col-del`/`--row-del`/`--escape`/`--no-char-del`/`--no-escape`/`--no-col-names`，含 `\t`、`\r\n` 转义写法）、逐字写入模式对不可表示数据的拒绝、SQL 攒批与单行 INSERT 的语句数/字节上界、`--threads` 的输出确定性、`--date-fmt`/`--timestamp-fmt`、`--no-data`/`--no-create` × `--mode create`、`--skip-count`/`--row-count` × 线程/`--batch-size`/`--commit-count`、`--messages` 在三格式与并行路径下的 SQL3104N/SQL3110N、`--no-replace` 哨兵存活、`--default-charset` × 二进制列 0x 自动切换。矩阵首轮跑出 3 个真缺陷：① **单线程导出失败留下半截产物**（20 万行表被 `--no-char-del` 拒于末行时残留 21,233,664 B 文件，并行路径早已删除），修复为 `exportSingle` 在 `os.Create` 之后注册共用的 `removeIncompleteOutput`（`exportParallel` 改为复用同一方法），注册点在处理之后保证创建前失败不会误删既有文件；② **无表头文件配位置 `-c` 静默丢首行**（`--no-col-names` 导出 + 只给 `-c` 导入时 3 行只进 2 行且退出 0，同一命令用于带表头文件又是对的），因文件自身无法区分两者、改名导入也走同一路径，故不改结果只补齐可见性：被当表头丢弃的那行若与目标表列名全不匹配，记 warn 并写入消息文件，说明加 `--no-col-names` 才不漏行；③ `--columns` 缺失时的报错只提示 `-c`，未说明无表头需成对使用，已在 7.4 与 README 的标志表补"成对使用"。同时确认三项**属既有行为而非缺陷**并写进文档：并行 SQL 输出在分片边界多出 `threads-1` 个 INSERT 头（数据一致，DEL 则逐字节一致，已按头数与字节差精确钉住）、`--timestamp-fmt` 省略 `ss` 会把秒截为 00（用户选定的精度，现固定其余字段不得变动）、非 utf8mb4 连接字符集对不可映射字符由服务端替换为 `?`（文本有损、二进制因 0x 自动切换无损）。另记录两项待用户定级的可用性问题：认证/字符集/库名这类永久错误被 ping 重试 3 次并 6 s 才失败，且错误前缀 `failed to ping database after 3 attempts` 掩盖真实原因；`--no-create` 文件配 `--mode create` 直接撞服务端 1146 而非事先提示文件不含建表语句。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产升至 30 文件 / 161 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`，九节项目结构、11.1 清单、7.2 失败与中断语义、7.4 位置映射、5.3 日志表、README 并行导出与 `--no-col-names` 说明同步**注（变更记录 71，2026-10-28）：本行登记的"认证/字符集/库名这类永久错误被 ping 重试 3 次并 6 s 才失败、且错误前缀掩盖真实原因"这一项已由用户定级并修复——开局建连的 ping 改用 `common.IsTransient` 判据，永久拒绝一次即返回（实测 5–6 ms），文案按实际尝试次数写（`after 1 attempt`），服务端原因不再是句尾配角；本行原文照录，历史数字 6 s 就此失效。另一项"`--no-create` 文件配 `--mode create` 撞服务端 1146"仍未处理。** |
| 43 | `--messages` 改为命令行日志文件，删除 DB2 风格消息报告 | 用户指出 `--messages` 提供的应当是**命令行的日志打印**，而不是 `SQL3xxxN` 样例日志，且带上该选项后所有日志都要进文件。实现落在日志层而不是命令层：`internal/common/log.go` 新增进程级 sink（带互斥锁；`os.Stderr` 在 `Write` 时解析而非构造时捕获，因此既有的 stderr 重定向测试与 `--echo-sql-values` 行为不变），`java` 与 `text` 两种布局都改经该 sink，`OpenLogFile(path)` 把文件挂到 sink 上（同一条记录写完 stderr 再写文件）、`CloseLogFile()` 摘下并关闭；根命令 `PersistentPreRunE` 在应用 `--log-format`、打印版本横幅**之前**打开文件，故横幅恒为文件首行、用法错误也进文件，`cmd.Execute()` 在记录完 `command failed` 之后关闭，故收尾错误同样在文件里。删除 `internal/common/message.go`（`MessageWriter` 及其 `SQL3104N/3105N/3107N/3109N/3110N` 与 `# Completed/# Duration/# Rate` 页脚）与 `message_test.go`，导出器 15 处、导入器 20 处（含 `batchcap.go` 2 处）消息调用全部移除：每条告警与失败本来就有对应的 `common.Logger` 记录，失败还会由 `Execute` 统一记成 `command failed`，因此不丢信息，只去掉重复的第二份；`SQLWriter.SetWarningFunc` 与 `Exporter.warningFunc`/`messageTableName` 一并删除。选项语义变更后 `-m` 短选项沿用（`export`、`import` 两侧均已注册），帮助文本改为「Write every log record to this file as well as to stderr」；`--quiet` 现在同样作用于文件（不再有「消息文件是显式产物」的例外）；`config.ExportConfig.Messages`/`ImportConfig.Messages` 随实现移除，日志文件是进程级 CLI 关注点而非库配置项。实测：`-m` 导出失败时文件与 stderr `diff` 逐字节一致（4 行含版本横幅、`export config`、`query failed` 真实原因与 `command failed` 包装）；`--log-level error` 下成功导出文件只剩横幅；`--log-format text` 布局随 stderr 一起切换。测试：`live_mysql_test.go` 的 `--messages` 断言改为经 `common.OpenLogFile` 走同一 sink，并断言每条告警与失败在日志里都有对应记录、文件里不再出现 `SQL31` 码；`option_matrix_test.go` 的 `--messages` 用例重写为 5 种格式/并行组合下「文件等于命令行日志」（首行为横幅、含 `export config`/`export completed`/`rows=10`、每一行都出现在 stderr 上），另加 `-m` 短选项、`--log-format text`、`--log-level error`、失败记录四个子用例。文档：1.2、导出/导入标志表、2.3 整节改写为「日志文件（`--messages` / `-m`）」、3.3.1 静默、四节 DB2 映射（显式标注语义不同）、导出导入流程步骤、5.3 功能表、九节结构与测试文件计数、11.1/11.2 清单同步，README 同步；`docs/cli-oracle.txt` 保持冻结基线并在头注登记这一处有意差异。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 29 文件 / 161 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`（`internal/common` 少一个文件、少 5 个 `MessageWriter` 用例，新增 5 个 sink 用例：文件与 stderr 逐字节相等、闭包后脱钩且二次关闭无错、早建好的 text 布局也跟随 sink、200 条并发记录不拆行、替换路径只写最新文件、创建失败时报出路径且不影响 stderr）。**本行是中间态**：该实现把记录同时写 stderr 与文件，下一行（44）改为只写文件 |
| 44 | `--messages` 改为独占：带 `-m` 时终端不再打印 | 第 43 行留下的选择题有了答案——用户要求"带 `-m` 就只进文件、终端不再打印"。改动只在 `internal/common/log.go` 的 `sink.Write`：文件挂载时**只写文件**、不再写 stderr，未挂载时照旧写 stderr（`os.Stderr` 仍在写入时解析，logger 的重定向测试不受影响），互斥锁保留（并行路径的两个协程仍会同时写同一个文件，交叉写入会毁掉记录）。`CloseLogFile` 先脱钩再关闭，因此"关闭失败"这条 ERROR 是唯一回到 stderr、不落进文件的记录，不会丢。帮助文本由「Write every log record to this file as well as to stderr」改为「Redirect the command log to this file; with it, nothing is printed to stderr」，`--echo-sql-values` 的告警措辞改为"写入 stderr 或 `--messages` 文件"，导出/导入两处进度注释同步。实测（真实二进制 + 本地 MariaDB）：带 `-m` 的成功导出 stdout 与 stderr 均 0 字节、文件 4 条记录、退出 0；带 `-m` 的失败导出同样 0 字节、文件 5 条含 `query failed` 与 `command failed`、退出码仍为 1；同一条失败命令去掉 `-m` 的 stderr 与该文件去掉时间戳/PID 前缀后 `diff` 逐行相同；`--log-level error` 下文件只剩横幅且横幅不回终端；`--log-format text` 同样只进文件。两类例外如实记录并在文档中说明：未知选项在 `PersistentPreRunE` **之前**失败，文件尚未打开，错误只上 stderr 且不建文件；`-m` 路径无法创建时报 `failed to create message file <path>: ...`（stderr，退出 1）。测试：`TestLogFileMirrorsStderr` 改名 `TestLogFileTakesOverFromStderr`，断言 stderr 为空、文件恰有 3 条、关闭后记录回到 stderr 且文件不再增长；text 布局用例改为"处理器早于挂载也只得写文件"；矩阵 `--messages` 用例把"每一行都出现在 stderr"换成"stdout+stderr 为空"（5 种格式/并行组合 + `-m` 短选项 + 失败记录 + 级别过滤四处各加一条静默断言）；`--no-col-names` 对称用例的 WARN 断言从输出移到文件，并加终端必须为空的断言。测试资产数量不变（29 文件 / 161 `Test*` / 1 `TestMain` / 5 `Fuzz*` / 8 `Benchmark*`）。文档：1.2 特性、导出/导入标志表、2.3 引言与行为约定（新增"终端"与"例外"两行）、3.3.1 静默、4 节 DB2 映射、6/7 节流程步骤、5.3 两处功能表、11.1/11.2 清单，README 同步 |
| 45 | 进度记录不再晚于结束汇总出现，并澄清 `rate` 与 `interval_rate` | 用户实测发现 `completed` 之后还会打印 `progress`，即日志以一个"还没跑完"的记录收尾。根因在计时器的停止方式：`common.StartMonitor` 返回的是 `context.CancelFunc`，`defer stopMon()` 只发出取消信号，goroutine 里**已经开始的那次 report 收不回来**；而汇总在 `Export`/`Import` 返回之前写好，于是"取消之后才落笔"的进度行必然排在 `completed`/`aborted` 之后。两处一起修：① `internal/common/stats.go` 的 stop 改为**排空**——`once.Do(cancel)` 后等待 monitor goroutine 退出（`done` channel），并二次确认 `monCtx.Err()`，避免 select 同时就绪时随机挑到 `ticker.C` 再写一条；stop 幂等，可"汇总前停一次 + defer 兜底一次"。② 五个终点汇总（`export completed`、`parallel export completed`、结构-only `import completed`、`import completed`、`parallel import completed`）与 `import aborted` 之前显式 `stopMonitor()`，停止句柄从 `Export`/`Import` 的局部变量提升为 `Exporter.stopMonitor`/`Importer.stopMonitor` 字段（汇总写在子方法里）。③ 间隔由硬编码 `5*time.Second` 改为 `common.MonitorInterval` 变量（默认值不变，测试压到 1 ms 才能反复触发计时器）。**注：该变量在第 46 行升级为命令行选项 `--progress-interval`，配置字段取代了包级变量。**同一提交顺带澄清两个速率的口径并写进 5.3：`rate` 是自开始以来的**总速率**（已处理 ÷ 已用时长，平滑），`interval_rate` 是**相邻两个进度点之间的区间速率**（两次快照之差，反映当前吞吐与抖动），二者无倍率关系，长跑时以 `interval_rate` 判断"此刻快不快"、以 `rate` 判断"整体多少"。测试：新增 `internal/common/stats_test.go`（2 用例）——把一次 report 钉在"正在写"的状态再 stop，断言汇总之后没有任何记录追加、最后一条是汇总、二次 stop 不死锁；该用例在旧实现下**必然失败**（实测输出 `[completed progress progress]`），是这一缺陷的回归钉子。`test/integration/live_mysql_test.go` 新增 `TestLive_ProgressNeverOutlivesCompletion`：8192 行表（`growTable` 服务端翻倍造数）在 1 ms 间隔下跑单线程导出、并行导出、单线程导入、并行导入四段，断言日志里确有进度记录且最后一条是对应汇总。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 30 文件 / 164 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`（`internal/common` 7 文件 29 个、live 6 个），九节结构与计数、5.3 进度行、6/7 节流程步骤同步，README 补进度段落与修复记录 |
| 46 | 删除与配置回显重复的 `export started`/`import started`，进度间隔改为 `--progress-interval` | 用户确认了这一去重方案并要求间隔可由命令行调整。① 两条 `started` 记录的字段（output/input、format、host、port、database）是紧随其后的 `export config`/`import config` 的子集，重复源于早期"命令启动"与"参数快照"分开的历史。现删除这两条记录，并把配置回显从 `Export`/`Import` 上移到 `NewExporter`/`NewImporter` 的第一句，即**建立连接之前**——原先它打在连接之后，连接失败时反而一个参数都看不到。实测：3 行小表导出共 3 条记录（横幅、`export config`、`export completed`，已无 started）、耗时 6 ms；`-q "SELECT * FROM no_such_table_x"` 的失败导出文件 4 行（横幅、config、`query failed`、`command failed`）、退出 1；密码写错那次 6 行（横幅、config、3 条 `database ping failed, retrying`、`command failed`）、退出 1 且输出文件根本没有创建。② 间隔改为命令行选项：`config.Config.ProgressInterval`（导出与导入共用）+ `config.DefaultProgressInterval = 5s` + `EffectiveProgressInterval()`（零值与负值回落默认值，因为库调用方可直接构造结构体，而 `time.NewTicker` 对非正间隔会 panic）；`export`、`import` 两侧各注册 `--progress-interval`，`cmd/validate.go` 复用既有的 `validatePositiveDuration`，`--progress-interval 0` 报 `invalid --progress-interval 0s: must be > 0` 并退出 2（0 不解释成"关闭进度"，那是 `--quiet` 的语义，一个概念只留一个开关）；第 45 行引入的包级变量 `common.MonitorInterval` 删除，导出器与导入器改用 `cfg.EffectiveProgressInterval()`，配置回显增加 `progress_interval` 字段。口径同时写进 5.3：首个 tick 要等满一个间隔，所以运行短于间隔时**一条进度也不打印**，`stop()` 只等在写的记录落笔、不空等剩余间隔（实测 3 行表 6 ms 结束、日志无 progress 行）；`--progress-interval 200ms` 跑 100 万行导出得 29 条进度记录，最后一条仍是 `export completed`。测试：新增 `internal/config/config_test.go`（2 用例：默认值与取值解析，导出/导入两侧各测）、`cmd/cli_matrix_test.go:TestCLIProgressInterval`（缺省为 5s、1ms/30s 原样生效、0 与 -1s 均为用法错误退出 2）、`internal/common/stats_test.go:TestMonitorSilentBeforeFirstTick`（取代原默认值用例：1 小时间隔下回调 0 次、stop 立即返回）；`TestLive_ProgressNeverOutlivesCompletion` 的四段改由配置的 `progressInterval` 字段驱动（不再改包级变量），并追加第五段用编译出的真实二进制跑 `--log-level info --progress-interval 1ms --messages`，断言汇总仍是最后一条、日志中不含 ` started ` 记录、config 带 `progress_interval=1ms`。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，测试资产 31 文件 / 167 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 8 `Benchmark*`。文档：1.2 特性、导出与导入标志表、四节 DB2 映射（标注无对应）、2.3 实测文件改为 4 行并补连接失败一例、记录来源表（删 `started` 行、config 行注明建连接之前打印）、3.3.1 静默、5.3 进度输出与日志顺序、6/7 节流程步骤、九节与 11.1 计数，README 同步**注（变更记录 71）：本行"密码写错那次 6 行（横幅、config、3 条 `database ping failed, retrying`、`command failed`）"是当时的实测，现已随 `common.PingWithRetry` 的判据修正变为 3 行（横幅、config、`command failed`，见 2.3）；本行原文照录。** |
| 47 | `--format sql` 默认改为每行一条 INSERT，攒批交给导入侧 | 用户选定方案：攒批在导入侧实现，导出默认只出单条单行 SQL。① `config.Config` 的 `SingleRowInsert` 字段换成 `ExtendedInsert`（默认 `false`），`NewExporter` 的两条路径改为 `SetSingleRowInsert(!cfg.ExtendedInsert)`；写入器自身语义不变（`NewSQLWriter` 仍默认合并，供库调用方使用），只是导出器反过来在给了 `--extended-insert` 时才关掉单行。`--statement-size` 在默认布局下退化为告警阈值：一行无法再拆分，超过它只记一次 WARN 并完整写出。② CLI 新增 `--extended-insert`（help：`Merge rows into multi-row INSERT statements bounded by --statement-size`），`--single-row-insert` 保留为已废弃兼容项（help：`Deprecated: one INSERT statement per row is now the default`，传入后不改行为），二者同时给出由新增的 `validateInsertLayout` 报 `--single-row-insert and --extended-insert cannot be used together` 并退出 2；`--extended-insert` 并入 `validateSQLOnlyOptions`，非 SQL 格式照旧拒绝。③ 配置回显在 `statement_size` 之后新增 `extended_insert` 字段。**实测**（本地 MariaDB，`bench_narrow` 100 万行，热缓存，`/usr/bin/time -v` 取峰值 RSS）：导出默认单行 2.43–2.57 s / 35.5–38.5 MB / 278,790,434 B（gzip 28,247,005 B）/ 1,000,000 条语句；`--extended-insert` 2.65–2.69 s / 71.3–74.1 MB / 153,792,809 B（gzip 27,050,101 B）/ 19 条语句，且产物与旧默认**逐字节一致**（md5 `61e8b0714b262972dac8b66dbedb2f90`）；DEL 参考 1.76 s / 25.6 MB / 141,218,462 B（gzip 26,795,207 B）。导入同一份数据：`--batch-size 1000` 单线程下 20,000 条语句的单行文件 12.49–12.87 s / 42–44 MB，1 条语句的合并文件 12.96–13.19 s / 215–224 MB；`--threads 4 --batch-size 20000` 单行 10.00–10.22 s / 67–69 MB，合并 8.90–8.91 s / 273–276 MB；DEL 参考 11.01 s / 35 MB（t1）、7.42 s / 166 MB（t4）。目标库 general log 复核（20,000 行、`--batch-size 1000`）：两种布局都只产生 20 条服务端 INSERT，即文件布局不改变下发的语句。结论：**内存差是结构性的**（导出峰值减半、导入峰值降到 1/5～1/2，因为合并布局要把整条语句物化），**速度不是**（±10% 内，t1 单行略快、t4 大批量合并略快），代价是文件体积 +81%（gzip 后 +4.4%）。测试：`cmd/cli_matrix_test.go` 新增 `TestCLIInsertLayout`（缺省 `ExtendedInsert=false`、`--extended-insert` 落到 config、废弃项解析但不启用合并）与 3 条矩阵用例（`--extended-insert` 合法、配 `--format del` 被拒、与 `--single-row-insert` 互斥）；`test/integration/option_matrix_test.go:TestOptionMatrixSQLBatching` 以“10 行 10 条语句”为新默认基线重写，覆盖旧选项、合并、`--extended-insert --statement-size 512` 的字节上界、单给 `--statement-size` 不改布局四种布局并逐一往返校验（上限放宽 4 字节，新增 `statementCapSlack` 常量说明写入器是先判上限后加 `, ` 与 `;\n`，原 128 字节用例小于任何单行语句因而合并必然为 0，改为 512）；`TestOptionMatrixParallelOutput` 增加“默认 SQL 布局的并行产物逐字节等于串行”，把分片边界头数与字节差的检查移到 `--extended-insert` 路径。文档：1.2 特性、2.1.6 标志表与布局说明、4 节 DB2 映射（`MODIFIED BY STATEMENTSIZE` 语义收窄并新增“无对应”一行）、5.2 文件结构（默认与 `--extended-insert` 两例互换，顺带记录 `LOCK TABLES` 早于 `CREATE TABLE` 使 `mysql < file` 进空库在第 2 行 `ERROR 1146 (42S02)` 中止的既有缺陷，**待用户定级**）、5.3 记录来源表、6/7 节流程步骤、7.4 攒批前提、10 节测试场景行、11.1 计数（31 文件 / 168 `Test*`，此前该行的 30/161 已与逐文件实测不符，一并更正），README 特性、SQL 标志表、导入攒批章节、参数校验表、SQL 格式示例说明、项目结构注释与修复记录同步。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 48 | SQL 导入的语句级改为全程 `[]byte`，去掉字符串物化与平方级扫描 | 用户要求"语句级避免 string 和字节的转换"。改写 `internal/import/sql_reader.go`：① `nextStatement` 返回记录缓冲里的切片，单行语句不再经 `recordReader.Text()` 物化整条语句，只有跨行语句才复制进 reader 自带的 `stmt` 复用缓冲；② 引号状态提为 `stmtState` 跨行携带，追加一行不再从头重扫已积累的文本——正是那次重扫让折行文件的代价随语句长度平方增长；③ 关键字与 `NULL` 的比较改在字节上做（新增 `hasPrefixFold`/`indexFold`/`equalFoldNull`，只折叠 ASCII 区间，多字节字符不再因大写化改变字节长度而使偏移错位），删除整条语句的 `strings.ToUpper` 副本；④ `extractRows` 定出元组边界后把窗口直接交给 `parseValueList([]byte)`，取消每个元组一个 `strings.Builder`，只有留存的值拷贝一次；⑤ `0x` 字面量改走 `hex.Decode` 到预分配切片，不再 `hex.DecodeString(string(...))`，`decodeHexLiteralBytes` 的"畸形则原样返回"分支显式复制一份字面量。`internal/import/importer.go` 的 `buildBatchInsertSQL` 配套改为精确长度 `Grow` 的一次成型：归属注释作为最前面的字节写入（原 `common.TagSQL` 在成品文本前再拼一遍，整批内容白复制），占位符组 `(?,?,?)` 先按列数拼好、每行写一次（原逐列逐字节写）。值的类型仍是 `string`——装箱到 `driver.Value` 必然分配，只减拷贝次数。实测（新增 `sql_reader_bench_test.go`，同一份 2 万行 × 8 列数据三种布局，HEAD 工作树与新树背靠背，`-benchmem`）：默认单行 90.0→42.3 ms（2.1×）/ 36.6→9.3 MB/op / 728,986→340,009 次分配；`--extended-insert` 69.1→32.8 ms（2.1×）/ 32.1→10.2 MB / 629,507→320,368；跨物理行折行 1839.3→32.1 ms（57×）/ 40.8→10.4 MB / 650,226→320,387。`BenchmarkBuildBatchInsertSQL`（打开归属注释，1000 行 × 10 列的一批）131→21 µs、138,160→24,792 B、20→3 次分配。端到端（本地 MariaDB，`--batch-size 1000` 单线程，注入 commit 的 `-trimpath` 构建，`/usr/bin/time -v` 峰值 RSS）：20 万行单行布局 2.61→2.29 s / 41.6→34.6 MB，同数据合并布局 2.85→2.44 s / 187.8→137.6 MB，148 万字节 10 条语句的折行文件 5.98→0.184 s / 37.4→35.4 MB；12 次导入（新旧二进制 × 三种布局 × 注释开关）目标表 `CHECKSUM TABLE` 逐组相同，general log 复核折行文件仍只产生 10 条约 138,556 字节的 INSERT 且以 `/* imex:<commit> */` 开头，说明下发内容不变。**改写过程由测试抓出两处自伤**：占位符组的拼装首次写成 `"(?" + Repeat("?,", n-1) + ")"` 产出 `(??,)`（多一个 `?`），以及纯字面量快路径漏掉 `unescapeSQLString` 使 `\n` 不再还原——前者由 `TestBuildBatchInsertSQLTagged` 的字节相等断言暴露，后者由既有 `TestSQLRoundTrip_PreservesEscapedText` 暴露。测试资产升至 32 文件 / 172 `Test*`（+1 `TestMain`）/ 5 `Fuzz*` / 12 `Benchmark*`，新增 `sql_reader_bench_test.go` 与 `TestSQLReader_MultiLineStatement`（值内真换行、值内 `;`、双写引号、`;` 独占一行）、`TestSQLReader_KeywordCaseAndNonASCII`（小写关键字、U+0131 与中文值、裸 `NULL` 与带引号 `"null"` 的分别）、`TestBuildBatchInsertSQLTagged`；7.4 增"语句文本一次成型"、7.5 增"SQL 语句解析路径"小节，11.1/11.2 与九节注释同步。`make test`（`-race`）与真实库 `go test -race -count=1 ./test/integration` 全通过，`gofmt -l`、`go vet ./...` 干净 |
| 49 | 并行导出的分片在读完后立刻释放连接与快照，不再等尾部字节交给流式写出 | 用户提出云环境下的场景问题：5 线程并行导入/导出，先完成的那几个线程会不会持有空闲连接。实测结论分两侧——**导入侧本来就没有这个问题**：并行导入是单个读取 goroutine + 共享 `batchCh`（容量 `--pipeline-buffer`）的动态派工，没有静态分区，worker 拿到关闭的通道后走 `workerLoop` 退出 → `session.commit()` → `worker completed` → `defer db.Close()`，`--threads 5` 全程 `SHOW PROCESSLIST` 恰好 6 条连接（5 worker + 主池）且结束时全部消失，唯一空闲的是主池（0→32 s `Sleep`，属设计内的常备连接）；**导出侧确实持有**：`exportParallel` 用静态等宽分片，每个分片一条自己的连接与 `START TRANSACTION WITH CONSISTENT SNAPSHOT` 快照，行读完后 `exportShard` 还要写页脚（SQL 格式先刷出最多 `--statement-size`，默认 8 MiB 攒批 INSERT）再 `pipe.Close()`，而 `writeShardStream` 按分片序号消费队列，于是"已经读完的分片"会堵在交尾字节上，连接与快照一起被占着。400 万行倾斜表（分片 0 占 360 万行、分片 4 占 40 万行、`--threads 5 --shard-key bucket`）实测：`duration=9.28s rows=4000000 bytes=587806929`，`SHOW PROCESSLIST` 里一个已完成读取的分片停在 `Writing to net` 约 5 s，`INNODB_TRX` 快照年龄从 1 s 涨到 9 s，另有空分片毫秒级释放（这条本来就对）。改动：`exportShard` 引入幂等的 `releaseDB()`（`rows.Close()` + `db.Close()`），在行迭代结束且 `rows.Err()` 判过之后**立刻**调用，页脚与 `pipe.Close()` 移到其后；`defer releaseDB()` 注册在输出 `Close` 的 defer **之前**，因此查询失败、迭代失败、页脚失败等路径下数据库仍是最先被放弃的资源，`db.Close()` 一次都不会漏。为让这条不变量能在无数据库条件下回归，`Exporter` 新增未导出的 `openShard func() (*sql.DB, error)` 钩子（默认走 `createShardConnection`，测试注入进程内假 `driver.Connector`）。实测这条**是不变量而不是吞吐**：本地写普通文件时尾部只有 1~2 个 256 KiB 块（DEL/SQL 写入器都用 `bufio.NewWriterSize(w, 256*1024)`），几乎堵不出时间，A/B（同一倾斜表、同一 0.4 s 采样观察）为 DEL 8.334/8.167 s 对 8.140/8.050 s、SQL `--extended-insert` 12.284/12.674 s 对 12.321/13.384 s，产物逐字节一致（DEL 587,806,929 B md5 `2c9834af64e6`，SQL 642,102,879 B md5 `503ae9219565`）；差别在连接状态：旧二进制一次采样抓到 `[3 2:270=Sleep:0 268=Query:6 267=Sleep:6]`（含一个读完但仍打开的分片），新二进制全部采样里再没有"已完成但仍打开"的连接。输出设备变慢（网络挂载、写回风暴）时这个等待会被拉长到秒级，而云 LB/NAT 的空闲 socket 回收正打在这个窗口上，因此价值在云链路而不是本地跑分。测试：新增 `internal/export/exporter_shard_test.go`（2 用例、8 文件）——`TestExportShard_ReleasesConnectionBeforePipeDrain` 起一个**故意没有消费者**的 `shardPipe`，断言假连接已关闭而 `exportShard` 仍未返回（100 ms 内不得返回），随后开始消费并断言 `rows=3`、`err=nil`、输出恰 3 行；把改动前的两个 `defer` 顺序还原回去，该用例必然在 5.00 s 超时失败并报 `exportShard still held its connection while waiting on the shard pipe`（已实测验证，非推断）；`TestExportShard_EmptyShardReleasesConnectionAndClosesPipe` 用 0 行分片断言导出返回时连接已释放、队列已关闭且不含任何块。失败路径统一 `p.terminate()`，不留挂死的 goroutine。文档：7.2 流程步骤 4 补一行并新增"分片读完即放连接，不等队列排空"整段、8.2 可靠性表新增"并行导出分片释放"一行、十一节测试资产计数（33 个 `_test.go` / 174 `Test*`，`internal/export` 8 文件 64 用例）、九节结构与交付物计数同步，README 修复记录同步。测试资产升至 33 文件 / 174 `Test*`（含 1 个 `TestMain`）/ 5 `Fuzz*` / 12 `Benchmark*`。`gofmt -l`、`go vet ./...` 干净，`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 50 | 参数审计落地：三处帮助文本改述实际语义，并行导出对忽略的管道选项出声，删除死常量 | 用户要求对全部命令行参数做一次"有用/重复/多余"的排查。结论是没有重复项，需要动的是三类：① **纯兼容项**只有 `--single-row-insert`（`legacySingleRowInsert` 由 cobra 写入后全仓库无人读取，`validateInsertLayout` 与 `validateSQLOnlyOptions` 用的都是 `Changed()` 判存在性），属有意保留，不改行为；② **帮助文本与实际语义不符**——`--statement-size` 在默认单行布局下已经不是攒批上限、只退化成告警阈值，但 help 仍写 "Max bytes per INSERT statement"，`--batch-size`/`--pipeline-buffer` 虽标了 "(single-threaded export)" 却没说明与 `--threads>1` 同时给出时会被忽略。现分别改为 "Max bytes per INSERT statement (only bounds merged statements: without --extended-insert a longer row just warns)"、"Rows per reader->writer hand-off batch (single-threaded export; --threads>1 ignores it and says so)"、"Hand-off queue depth, in batches (single-threaded export; --threads>1 ignores it and says so)"，并登记进 `docs/cli-oracle.txt` 头注（该文件是冻结基线，导出侧的有意差异由六条增至九条）；③ **参数校验管不到的"运行时静默无效"**：格式类越界全部由 `validateSQLOnlyOptions`/`validateDELOnlyOptions`/`validateParquetBinaryOptions` 以 `Changed()` 前置拒绝并退出 2，但 `exportParallel` 根本不读这两个字段（只有 `exportSingle` 读，exporter.go:313/317），前瞻另由 64 MiB ÷ 分片数决定（shard_stream.go:70-83），于是 `--threads 8 --batch-size 50000` 静默等价于默认值。现在 `exportParallel` 在确认按分片并行执行之后（`GenerateShardRanges` 的分片数判定之后，避免降级为单线程时误报）经 `pipelineOptionsIgnoredByParallel()` 判定"用户确实改动过任一个值"，记一条 `info`：`parallel export ignores pipeline options batch_size=5000 pipeline_buffer=10 look_ahead_per_shard="16.0 MiB" applies_to="single-threaded export only"`。判定基准是"与出厂默认值不等"而非"是否传了该 flag"，因此库调用方与常见运行（不动这两个值）完全静默。实测（`bench_narrow` 100 万行，`--threads 4`）：`--batch-size 5000` 那次日志出现该记录且 `look_ahead_per_shard=16.0 MiB`（4 分片 × 64 块 × 256 KiB），全程 `duration=2.396s rows=1000000 bytes=141218462`；不带 `--batch-size` 的并行导出与该记录出现次数为 0；`--threads 1 --batch-size 5000`（真正生效的路径）同样为 0。另删除死常量 `config.DefaultMaxAllowedPacketCap`（全仓库仅定义处一处，`--statement-size` 的 268435456 上界在 `cmd/validate.go:47` 独立成立，导入侧的包上限另有 `internal/import/batchcap.go` 的 `defaultMaxAllowedPacket`=4 MiB 与 `packetSafetyMargin`，与此常量无关）。测试：新增 `internal/export/shard_stream_test.go:TestParallelExportPipelineOptionNotice`（默认值不报警、`--batch-size` 改动报警、`--pipeline-buffer 0` 这一合法取值也算改动）与 `test/integration/live_mysql_test.go:TestLive_ParallelExportReportsIgnoredPipelineOptions`（三段式：并行+改动必须出现、并行+默认必须静默、单线程+改动不得报警，走 `--messages` 文件断言）；`live_env.exportOptions` 增加 `batchSize` 字段用于驱动该场景。测试资产 33 文件 / 176 `Test*`（含 1 个 `TestMain`）/ 5 `Fuzz*` / 12 `Benchmark*`（`internal/export` 8 文件 65、live 7）。文档：README 导出标志表两行与 `--statement-size` 行、流水线生效范围段，requirements.md 5.3 记录来源表、7.2 队列深度段、2.1.3 两行、11.1 计数同步，`docs/cli-oracle.txt` 头注登记三处 reword。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 51 | 导入侧新增 `--statement-size`，把攒批字节预算变成用户可以主动压低的上限 | 用户要求"把该参数实现为 imp sql 类的功能"——`--statement-size` 此前只在导出侧有语义，导入侧每批的字节预算完全由目标库 `max_allowed_packet` 决定，用户无法主动把批次切小。实现：`config.ImportConfig` 新增 `StatementSize`（`internal/config/config.go`），`import` 注册同名选项（默认 `0`），`internal/import/batchcap.go` 新增 `applyStatementCap(packetBudget, statementSize)` 并接在 `batchBudget()` 上，与 `max_allowed_packet × 9/10` 预算取**较小值**——只压低、不抬高（抬高等于让一批在服务端中途触发 error 1153，而那正是该预算要防的事）；默认 `0` 不改任何现有行为；给定值低于 `batchByteLimit` 的 64 KiB 下限时照给定值执行（显式的数字是有意选择）；单行大于上限仍独立成批（`shouldFlush` 在追加之后判定，一行无法再拆）。压低点上选在 `batchBudget()` 而不是某条路径，因此单线程与并行导入、DEL/SQL/Parquet 三种输入一并生效。参数校验刻意分列：新增 `validateImportStatementSize`（接受 `0`，`<0` 或 `>268435456` 为用法错误退出 2），导出侧 `validateStatementSize` 保持拒绝 `0`——同一个名字在两侧含义不同，不能共用一个校验。日志：`import batch byte cap resolved` 增 `statement_size` 与 `limiting_factor`（`max_allowed_packet` / `statement-size`）两个字段以说明预算由谁定的，`import config` 回显增 `statement_size`。实测（本地 MariaDB，`max_allowed_packet=134217728` → 生效预算 120,795,955 B；2048 行 × 约 200 B 载荷；`--batch-size 20480`/`--commit-count 20480` 使行数与提交都不触顶；条数取目标库 `SHOW GLOBAL STATUS LIKE 'Com_insert'` 的差值而非日志自述）：不加选项 **1** 条语句，`--statement-size 65536` **7** 条，`--threads 4` 并行同样 **7** 条，SQL 与 Parquet 文件同样 **1→7** 条，`--statement-size 1024`（低于 64 KiB 下限）**342** 条，`--statement-size 268435456`（高于预算）**1** 条且记 `limiting_factor=max_allowed_packet`；各次落库行数与 `CRC32` 校验和逐组一致（2048 / 4397778945034）。测试：`internal/import/batchcap_test.go` 新增 `TestApplyStatementCap`（0/负值/等于预算/高于预算/真压低/低于下限六种取值）与 `TestBatchBudgetAppliesStatementCapWithoutConnection`（`imp.db == nil` 的保守分支同样压低）；`cmd/cli_matrix_test.go:TestCLIImportStatementSize` 钉住两侧相反的合法性（导入接受 `0`/`1`/`65536`/`268435456` 且原样落到 config，`-1` 与 `268435457` 为用法错误 2，导出 `--statement-size 0` 仍是用法错误 2）；`cmd/validate_fuzz_test.go` 新增 `FuzzValidateImportStatementSize`；`test/integration/live_mysql_test.go:TestLive_ImportStatementSizeCapsStatements` 用 `Com_insert` 断言默认 1 条、加上限后约 7 条，并复核两表落库行与 `limiting_factor` 的切换；`importOptions` 增加 `statementSize` 字段驱动该场景。测试资产 33 文件 / 180 `Test*`（含 1 个 `TestMain`）/ 6 `Fuzz*` / 12 `Benchmark*`（`internal/import` 41、`cmd` 11、live 8）。文档：README 导入标志表新增一行，"导入攒批的两道硬上限"一节改名"导入攒批的硬上限与可选压低"并在末段补上第三道可选上限与上述实测，参数边界表拆成导出/导入两行，关联校验段澄清那串 SQL 专属限制只约束导出侧，项目结构注释更新；requirements.md 2.2.3 导入选项表、7.4 新增第 4) 项、5.3 记录来源表补上此前未登记的 `import batch byte cap resolved` 与 config 回显字段、11.1 总数与四个文件行、九节结构注释同步；`docs/cli-oracle.txt` 头注登记导入侧新增该选项（有意差异 4→5 条）。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 **注：本行引的 `max_allowed_packet=134217728`、预算 120,795,955 与 `1024` 档的 342 条是 128 MB 测试床上的值；变更记录 70 把床按 OceanBase V4.x 默认调成 16M（预算 15,099,494）并以 207 B 行宽的 fixture 重测，同一矩阵为 1 / 7 / 410 / 1 条、`CRC32` 和 4463837020299，342 与 410 之差来自行宽而非床。** |
| 52 | 配置回显与 `--help` 按 `--format` 过滤格式专属选项，判据收敛为一张登记表 | 用户要求"按照命令和不同的文本格式将选项的打印数据更让人使用"。缺陷实测：把 HEAD 二进制 `-f del`/`-f sql`/`-f parquet` 三条 `export config` 去掉 `format=` 后逐字节比对，三者完全相同——同一套 26 个字段里 `-f del` 带着根本不读的 `statement_size`/`no_create`/`no_data`/`extended_insert`、`-f sql` 带着根本不读的 `char_del`/`col_del`/`row_del`/`escape`/`timestamp_fmt`/`no_col_names`、`-f parquet` 两组都带（而这三类组合本来就会被用法错误拒绝），同时 DEL 写入器一直在读的 `date_fmt`/`no_escape`/`no_char_del` 三项**从来没打印过**，`hex_blob` 打的还是 `--hex-blob` 原始标志位而非生效值（`--default-charset gbk` 自动开启时回显仍是 `false`，与紧随其后的 `hex-blob auto-enabled by charset` 自相矛盾）；`--help` 也不说某个选项只对某种格式有效。实现：新增 `internal/config/formatscope.go` 作为唯一的格式适用范围登记表（`ExportOptionScopes`/`ImportOptionScopes`，每项带 `Hint` 与 `Reject`，暴露 `AppliesTo`/`Violation`），三个消费者同源：① `cmd/validate.go` 删去手写的 `validateSQLOnlyOptions`/`validateDELOnlyOptions`+`delOnlyFlags`/`validateParquetBinaryOptions`，合并为 `validateOptionScopes`，登记顺序保持原三函数的检查顺序，**拒绝文案与首条命中逐字不变**；② 新增 `cmd/option_scope.go:annotateOptionScopes`，在 `registerExportFlags`/`registerImportFlags` 末尾把 `Hint` 追加进各 flag 的帮助文本（幂等——命令树是包级的，init 与测试走同一注册路径）；③ 回显从 `NewExporter`/`NewImporter` 的内联调用抽到新增的 `internal/export/configlog.go`/`internal/import/configlog.go`，按 `AppliesTo` 过滤并按「连接 → 输入输出 →（导入另有行流控制）→ 并行与流水线 → 字符集 → 该格式专属布局 → 日志」排列，导出侧 `hex_blob` 改用新抽出的 `effectiveHexBlob()`（原单线程与并行分片各写一遍的重复表达式一并收敛）。回显仍在建连接之前、仍是一条命令一条记录，`--messages` 的"一行一条记录"布局与 5.3 的"同一事件只输出一行"不受影响。实测（本地 MariaDB，`SELECT * FROM allnull` 成功导出）：`-f del` 与 `-f sql` 的回显分别 25/20 个字段，`comm` 差集恰为只在 del 出现的 `char_del col_del date_fmt escape no_char_del no_col_names no_escape row_del timestamp_fmt` 九项与只在 sql 出现的 `extended_insert no_create no_data statement_size` 四项；`export -f del/sql/parquet` 为 25/20/15、`import` 为 29/20/20；`--default-charset gbk` 时回显 `hex_blob=true`；`--format sql --char-del '!'` 仍报 `--char-del can only be used with --format del` 退出 2；`export --help` 16 行、`import --help` 9 行带范围标记；两次成功运行的 `-m` 文件各 3 行且每行都是日志记录。测试：新增 `internal/config/formatscope_test.go`（4 用例）、`cmd/option_scope_test.go`（3 用例）、`internal/export/configlog_test.go`（4 用例）、`internal/import/configlog_test.go`（3 用例），测试资产 33→37 文件、180→194 `Test*`（`internal/config` 2→6、`cmd` 11→14、`internal/export` 65→69、`internal/import` 41→44），`Fuzz*` 6、`Benchmark*` 12 不变；既有钉拒绝文案的 `TestCLIDELFlagFormatValidation`/`TestCLIParquetHexBlobRejected`/`TestCLIDELFlagRejectedExitCode` 未改一行即全通过，作为"文案与顺序不变"的证据。文档：README 日志段改写为按格式过滤并给出实测字段数、"关联校验"段补三处同源口径、项目结构增 4 个文件、修复记录新增一行；requirements.md 2.2.2/2.2.3 两段格式适用性补登记表口径、5.3 记录来源表的 config 行重写、九节结构与测试文件计数、11.1 总数与四个文件行、十二节交付物计数同步；`docs/cli-oracle.txt` 头注登记范围标记带来的帮助文本差异（有意差异 export 9→21 行、import 5→14 行），并把头注里已过时的"Five intentional deltas"改为不带计数的表述。`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 53 | `go.mod` 声明升至 `go 1.26.10` 并重新打包 | 用户要求"gomod 版本升级至 1.26.10，重新打包"。**源码零改动**：只有 `go.mod` 的 `go` 指令由 `1.26.9` 改为 `1.26.10`，本文 1.3 技术栈行与九节项目结构的 `go.mod` 注释同步。沙箱 GOROOT 仍是 go1.26.9，靠 `GOTOOLCHAIN=auto` 自动取到 go1.26.10 后编译（仓库内 `go version` 实测输出 `go version go1.26.10 linux/amd64`），所以这一行记录的是"工具链小版本升级后全量验证通过"，不是行为变更。`docs/cli-oracle.txt` 头注登记一条**环境性有意差异**：`## version` 基线冻结的是 `Go version: go1.26.9`，此后每个构建都会输出 `go1.26.10`，比对 `version` 输出时该行预期不同；5.3 节带日期的样例日志（`go_version=go1.26.9`）与本表第 34 行按历史快照保留、不改写。附带口径提示（本次未改）：`Dockerfile` 用的是浮动次版本 `golang:1.26-alpine`，镜像内 Go 若早于 1.26.10 会触发 `GOTOOLCHAIN` 联网下载，需要固定版本时再改。验证（全部在 go1.26.10 下重跑）：`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过、真实库 `go test -race -count=1 ./test/integration` 86 用例全通过且无 SKIP/FAIL；`make build-all`（`CGO_ENABLED=0`）产出 linux/amd64 与 windows/amd64 静态二进制，`version` 子命令实测输出 `Go version: go1.26.10`；解包发布用的 linux 产物对 3000 行（含 `VARBINARY`）表做三格式往返，`del`/`sql`/`parquet` 目标表的 `SUM(CRC32(...))` 与源表同为 6426483039391，`--threads 4` 并行导出同样通过。因工作区未提交，批次按约定命名 `imex-0.1.0-a716deb-dirty`（src + linux-amd64 + windows-amd64 + `bin-SHA256SUMS.txt`，三个文件 `sha256sum -c` 全 OK） |
| 54 | 把"重复主键 × 导入并发"的锁冲突固化为回归用例 | 用户要求"测试用例可以固化下"，前因是两条实测结论：① 单线程导入重复主键恒为 `1062`/`Duplicate entry`，一个会话不会自锁；② 并行导入没有按键分派——`internal/import/importer.go` 的并行路径是单读取 goroutine 把批次写入带缓冲 channel、N 个 worker 各自 `sql.Open` + `SetMaxOpenConns(1)` 先到先得地领取，谁先拿到哪个批次取决于调度，于是"相邻批次含同一批键且顺序相反"会让两个会话以相反次序申请同一组行锁，构成锁序反转，服务端可能报 `1213`（死锁）或 `1205`（锁等待超时）。而 `internal/common/dberr.go` 刻意不把 1213/1205 归入可重试错误（避免盲目重放把重复数据写进去），所以死锁直接令该批次失败并终止导入——这条"不会重放后谎报成功"依赖用例守卫。实现：新增 `test/integration/lock_contention_test.go`（3 个用例，真实库 + `TestMain` 编译出的真实二进制，沿用 `IMEX_TEST_DSN` 门控与 `option_matrix_test.go` 的 `cliEnv` 复用方式）。fixture 建 `(seq BIGINT PRIMARY KEY, id BIGINT, name VARCHAR(64))`，用 `digits(0..9)` 四次交叉拼出足量 `seq`，令 `id = ((seq-1) DIV batchRows DIV 2) * batchRows + 1 + offset`——每个键在文件里恰好出现两次，`offset` 按块序号奇偶取 `s.seq-1 MOD B` 或其镜像，`invert=true` 即"奇数块降序"，正好制造相邻批次键集重叠且顺序相反；两份数据文件由同一条 `-q "SELECT id, name FROM src ORDER BY seq"` 分别以 `-f sql` 与 `-f del` 导出，保证两种格式走同一份键序。① `TestLive_DuplicateKeysSingleThread`：单线程 `--mode insert` 断言退出码非 0、输出含 `1062` 或 `Duplicate entry`、且**不得**出现 `1213`/`Deadlock`，落库 `COUNT(*) == COUNT(DISTINCT id)`；`--mode insert_update` 断言退出码为 0 且全部键落地（120 行 / 120 键）。② `TestLive_ParallelImportKeyOrderInversion`（sql、del 两个子用例，`--mode insert_update --threads 4 --batch-size 40 --pipeline-buffer 0`）：**不断言必然死锁**——是否成环由服务端调度决定，断言的是一组与退出码无关的不变量：任何情况下不落重复行（`COUNT(*) == COUNT(DISTINCT id)`）、落库行数不超过文件内键数；成功时全部键必须在场，失败时输出只能出现 `1213`/`Deadlock` 或 `1205`/`Lock wait timeout` 这类服务端锁错误；服务端死锁计数增量作为观测值打印（变量名 MySQL 为 `Innodb_row_lock_deadlocks`、MariaDB 为 `Innodb_deadlocks`，两者都取不到时自动降级为只看报错文本）。③ `TestLive_ParallelImportConsistentKeyOrder`：同一键集、同样 4 线程、批次一律升序（`invert=false`），断言零死锁且 400 个键全部落地——把"按键有序 dispatch 可规避死锁"从口头结论升级为被测性质。实测（沙箱 MariaDB 10.6.23，4 核）：上一轮同一 fixture 下 sql 那次没成环（死锁增量 0、400 键全落），del 抓到 1 次死锁并 `import aborted`；本轮复跑 `-run` 分别取 `TestLive_Duplicate` 与 `TestLive_Parallel` 两个前缀 时 sql 与 del 两个子用例各抓到 1 次死锁、均以锁错误终止且落库 0 行（断言的是不变量而非具体调度结果，两种走向都通过）；三个用例耗时 0.07/0.08/0.07 s。文档：九节项目结构增该文件行、测试文件计数 集成 3→4 与全仓库 37→38，11.1 计数说明改 38 个 `_test.go`/196 个 `Test*` 并新增该文件行，十二节交付物计数 37→38。验证：`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过，真实库 `go test -race -count=1 ./test/integration` 全通过 |
| 55 | 导出侧三项：`-q` 里的 schema 不再二次拼接、SQL 导出默认仅数据（`--create` 显式带回建表、`--no-create` 随之删除）、新增与 `-q` 互斥的 `--query-file` | 用户一次提了三点，第一点是缺陷，后两点是行为与易用性变更。① **缺陷：`--query` 带 schema 时建表语句被二次拼接**。复现（HEAD 9389a88 构建的二进制，本地 MariaDB，表 `imex_test.zzq_evi` 三行）：`export -q 'SELECT * FROM \`imex_test\`.\`zzq_evi\`' -f sql` 退出 1，报 `export failed: failed to get CREATE TABLE statement: Error 1146 (42S02): Table 'imex_test.imex_test.zzq_evi' doesn't exist`。根因在 `internal/export/sql_parser.go`：`ExtractTableName`/`SimpleTableName` 返回的是 `sqlparser.String(TableName)`，即**已带反引号的 `` `db`.`tbl` ``**，而 `getCreateTableStatement` 把它当裸名再包一层反引号、并前置连接自己的 `--database`，服务端于是把整串当作默认库里的单个标识符。修复口径是"产物永远只引用裸表名，schema 只在建表查询这一处用"：两处改为断言 `sqlparser.TableName` 后取 `tn.Name.String()`（裸名，不带反引号），新增 `ExtractTableRef(query) (schema, name string, err error)` 返回 `tn.Qualifier.String()` 与裸名，`getCreateTableStatement` 改签名为 `(schema, tableName string)`，只在 `schema != ""` 时加 `` `schema`. `` 前缀；单线程预取与并行分片预取两条路径都换用它。裸名同时供 SQL 写出的 `CREATE`/`INSERT`/`LOCK TABLES` 头与分片键探测使用（`shard.go` 的 `findPrimaryKey`/`findUniqueKey` 本就按 `TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?` 过滤，带前缀反而匹配不上主键）。② **SQL 导出默认不再写 `CREATE TABLE`**（默认仅数据，属破坏性变更，README 与本文同步标注）：`config.ExportConfig` 新增 `Create bool` 与 `WriteCreate() bool`（`Create && !NoCreate`），注册 `--create`（默认 `false`，帮助文本 "Output a CREATE TABLE statement (SQL export is data-only by default)"），`--no-create` 一并删除（注册、`ExportConfig.NoCreate` 字段、格式登记项、回显键四处同时消失；`WriteCreate()` 在失去反向开关后就只剩 `return c.Create`，属无意义间接层，故也删掉，写出侧与回显直接读 `Create`）；`--create` 登记进 `internal/config/formatscope.go` 的 `sqlOnly` 组（SQL 专属组仍是五项——`create`/`no-data`/`statement-size`/`single-row-insert`/`extended-insert`，一增一删，帮助文本里的范围标记行数与既有格式越界口径不变），因此 `--create -f del` 与既有的格式越界同口径拒绝；配置回显由打 `no_create` 改为打**生效值** `create`。导入侧 `--mode create` 依赖建表语句，故其文档示例前补了一条 `export ... --create`。③ **新增 `--query-file`，与 `-q` 互斥**：`ExportConfig` 新增 `QueryFile`，`cmd/export.go:resolveExportQuery` 以 `Flags().Changed()` 判三态——两者都给为用法错误 `--query and --query-file are mutually exclusive`；只给文件则读文件、`TrimSpace` 后去掉行尾分号再 `TrimSpace`，空内容报 `--query-file "..." contains no SQL statement`，读不到报 `cannot read --query-file "..."`；两者都不给报 `--query or --query-file is required`（均为退出 2，与必填项同口径）。回显新增 `query_file` 字段，仅在真正走了文件时出现，避免默认运行多一个无意义字段。实测（同一台沙箱 MariaDB，HEAD 与工作区两个二进制跑同一条命令）：HEAD 退出 1 / 1146，工作区默认退出 0 且产物 243 B、`CREATE TABLE` 行数 0、`INSERT INTO` 行数 3；`--create` 退出 0 产物 472 B，首块为 `CREATE TABLE \`zzq_evi\``，全文件 `imex_test` 出现 **0** 次；删掉源表后用 `import -f sql --mode create` 回灌该文件，退出 0、`inserted=3`、`SELECT COUNT(*)` = 3，证明"带 schema 的查询 + `--create`"这条最容易二次拼接的路径已闭合；`--no-create` 无论单独给还是跟着 `--create` 给都是 `unknown flag: --no-create` 且退出 2；`--create -f del` 退出 2 且文案点名 `--format sql`；`--query-file` 用含首尾空行与结尾分号的语料导出成功，INFO 回显同时出现 `query_file=<tmp>/r3_q.sql` 与 `create=true`，四条负例（与 `-q` 同时给、两者都不给、空文件、文件不存在）全部退出 2；不读文件的默认运行回显里 `query_file` 出现 0 次。测试：`internal/export/sql_parser_test.go` 把 `TestSQLParser_SimpleTableName` 里限定名用例的期望从 `db.users` 改为 `users`（改的就是缺陷本身，注释写明原因），新增 `TestSQLParser_ExtractTableRef`（裸名 / 限定名 / `AS x` 别名三种取值，以及 `UPDATE`、`DELETE`、`INSERT` 三种非 SELECT 一律拒绝）；`internal/export/configlog_test.go` 的 SQL 布局键由 `no_create` 换成 `create`；`internal/config/formatscope_test.go` 的 `AppliesTo` 矩阵把两处 `no-create` 换成 `create`（并补一条 `--create` 对 `del` 为 false）、"首条命中"用例改用 `changedOf("col-del", "create")` 断言 `--create can only be used with --format sql`、导入侧"未登记即恒适用"一例换成 `--mode`；`internal/export/exporter_stream_test.go` 的 `streamTestExporter` 参数由 `noCreate` 反转为 `withCreate`（全部用例走数据-only 分支），`TestWriteShardStream_SQLNoCreateStillLocks` 更名 `TestWriteShardStream_DataOnlyStillLocks`——它守的仍是"没有建表语句也必须成对写 LOCK/UNLOCK"，`internal/export/sql_writer_test.go` 两个用例名里的 `NoCreate` 也改为 `WithoutCreateOrData`/`WithoutCreateSkipsCreateTable`（写出器的布尔参数没动，只是不再用一个已不存在的 flag 给用例命名）；`test/integration/live_mysql_test.go` 的 `exportOptions` 增加 `create` 字段，结构往返用例改为显式 `--create`；`test/integration/option_matrix_test.go` 的 `TestOptionMatrixNoDataNoCreate` 改名 `TestOptionMatrixNoDataCreate` 并重写为四个子用例（默认仅数据可直接导入、`--create` 同时保留结构与数据、`--create --no-data` + `--mode create` 仅结构建表、默认仅数据 + `--mode create` 必须明确失败）；新增 `test/integration/export_cli_test.go`（4 个用例）把三项钉成真实二进制 + 真实库回归，其中 schema 限定用例的两个子用例各自 `CREATE TABLE ... LIKE` 出一张新目标表再逐行比对，避免共用目标表导致主键冲突。测试资产 38→39 个 `_test.go`、196→201 个 `Test*`（`internal/export` 用例 69→70，集成目录 4→5 个文件），`Fuzz*` 6、`Benchmark*` 12 不变。文档：README 导出标志表 `--query` 改述并新增 `--query-file` 与 `--create` 两行、**删掉 `--no-create` 行**并在 SQL 输出说明里写明它已移除（传入为未知选项退出 2），SQL 输出说明段重写为"默认仅数据 + schema 剥离 + `--mode create` 需配 `--create`"，"关联校验"段把 `--create` 并入 SQL 专属清单（不再列 `--no-create`）并写 `-q`/`--query-file` 互斥；requirements.md 2.2.2 导出选项与 SQL 修饰符表（删 `--no-create` 行）、2.2.1 格式适用性段的点名示例、DB2 映射表 `MODIFIED BY NOCREATE` 行改为"默认行为 + `--create` 才附带"、`sql` 格式说明、单表限制一节新增"表名里的 schema 一律剥离"段、导出流程两行、九节结构与计数、11.1 计数与 `internal/export`/`export_cli_test.go`/`option_matrix_test.go` 三行、十二节交付物计数 38→39；`docs/cli-oracle.txt` 的 `## export` 段**整体重新基线**为当前工作区的帮助文本（实测与 `export --help` 逐字节相同，导出侧不再有待对差异），头注记下重新基线日期并说明 `## import`/`## root`/`## version` 三段仍是 4934dd2 快照、SQL 专属组仍是五项（`--create` 顶掉 `--no-create`，范围标记行数不变）。验证：`gofmt -l` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过，真实库 `go test -race -count=1 ./test/integration` 全通过（新增与改写的用例逐条 PASS，含四个中文命名的子用例） |
| 56 | 项目改名 `gomydump` → `imex`（ImEx = Import + Export），模块名、二进制名、环境变量、帮助与日志回显、文档全线统一 | 用户要求把工程名统一改为 imex，覆盖模块名、备注、帮助回显、文档。做法是先做标识符普查，再快照源码树（`<tmp>/imex_prefix_backup.tgz`，304,754 B，排除 `.git`、`bin`、`dist`），然后对全部 `.go`、`.md`、`.txt`、`Makefile`、`Dockerfile`、`go.mod` 施加两条规则 `s/gomydump/imex/g` 与 `s/GOMYDUMP/IMEX/g`，最后逐层复核。改动落点：`go.mod` 模块路径 `github.com/gomydump` → `github.com/imex`（因此 ldflags 注入路径 `cmd.Version`/`cmd.BuildTime`/`cmd.GitCommit` 与全部 import 语句同步）；`cmd/root.go` 的 `Use`、`Long`、版本行与五个环境变量名；`internal/common/password.go` 的 `EnvKey` 由 `GOMYDUMP_KEY` 改为 `IMEX_KEY`；`internal/common/log.go` 启动横幅 `imex starting`；`internal/common/sqltag.go:55` 归因注释 `/* imex:<commit> */`；`Makefile` 的 `BINARY := imex` 与 `make test-live` 提示语；`Dockerfile` 的 ldflags、`-o imex`、`COPY` 与 `ENTRYPOINT ["/app/imex"]`；集成测试的 DSN 环境变量 `IMEX_TEST_DSN` 与测试库 `imex_test`。**改名不是纯文本替换，两处必须实跑才能确认**：① 集成测试表前缀实际写作 `gmydump_it_`（少一个 o），两条规则都不命中，是真实套件失败输出暴露的，已改为 `imex_it_`；② `test/integration/option_matrix_test.go` 在本轮被环境回退成旧变体（`statementCapSlack` 标记计数从 3 归 0，并带回上一轮已删除的 `--no-create` 子用例），`git checkout --` 复原后重放了上一轮的 `TestOptionMatrixNoDataCreate` 四子用例改写与本轮改名。兼容性口径：归因注释是**只写不读**（`sqltag.go` 生成后拼进发给服务端的语句，导入侧解析器不反向识别它），所以旧产物里以 `/* gomydump:<commit> */` 开头的行仍按普通 SQL 注释忽略，**旧导出文件照常可导入**；本轮没有 flag、退出码、文件格式或 schema 变更，因此不留任何旧名兼容层。实测：`imex --help` 首行为 `imex is a table-level MySQL logical backup and restore tool.`、`Usage: imex [command]`，`Available Commands` 与 Global Flags 全部为 `imex`/`IMEX_*`；`imex version` 输出 `imex version 0.1.0`、`Git commit: 9389a88`、`Go version: go1.26.10`；`--log-level debug` 首条记录为 `INFO imex starting version=0.1.0 ... go_version=go1.26.10 command=export`；MariaDB general log 抓到服务端实际收到的 `/* imex:9389a88 */ SHOW CREATE TABLE ...` 与 `/* imex:9389a88 */ SELECT ...` 两条，证明归因注释按新名下发；代码与构建脚本（全部 `.go`、`Makefile`、`Dockerfile`、`go.mod`、`.yml`）对 `gomydump`、`GOMYDUMP`、`gmydump` 三者命中 **0**，文档里剩下的 4 处提及全部是本轮改名记录本身对旧名的引述（README 修复记录 1 行、requirements.md 本行、`cli-oracle.txt` 头注 2 行）。文档：README 的 H1 改为 `# imex` 并补一句名称由来；requirements.md 标题、1.1 名称说明、四节 DB2 映射标题、九节项目结构根目录名、十二节产物前缀 `imex-<version>-<commit>[-dirty]-…` 与 arm64 自行编译命令 `go build -trimpath -o imex .` 同步；**历史变更行与带日期的样例日志里的工具名一并改写**，即第 53 行的批次名等历史记录现在显示 `imex-…`，而该批次实际交付物当时仍是 `gomydump-…` 前缀——记录按当前名称统一，不代表当时的名字；`docs/cli-oracle.txt` 四段体内的命令词与 `IMEX_*` 环境变量随改名更新（体内其余内容零改动），`## export` 段改名后重新与 `imex export --help` 逐字节比对通过（`diff` 为空），并补登记此前头注未列出的 `## root` 两行差异（Features 条目现为 `Command log file (--messages)`、Global Flags 的 `--echo-sql-values` 文案与 import 段一致）。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七个包全通过（包路径均为 `github.com/imex/...`），真实库 `IMEX_TEST_DSN='...tcp(127.0.0.1:3306)/imex_test...' go test -count=1 -v ./test/integration` 全通过（104 条 PASS 含子用例、0 SKIP），测试资产数量不变（39 个 `_test.go`、201 个 `Test*` 含 1 个 `TestMain`、6 个 `Fuzz*`、12 个 `Benchmark*`）——本轮只改名，不增删用例。**本轮范围只到仓库内**：仓库外的约定文件（`<workspace>/AGENTS.md`、`<workspace>/prune-packages.sh`）、工作副本目录名 `<workspace>/gomydump` 与旧名产物 `bin/gomydump*`、`dist/gomydump-*.zip` 在同日的下一行（57）处理。 |
| 57 | 改名收尾：工作副本目录、仓库外约定文件与旧名产物一并统一 | 上一行只做到仓库内，本行把剩下的三层全部对齐。① **工作副本目录** `<workspace>/gomydump` → `<workspace>/imex`（同一文件系统内 `mv`，`.git` 随目录走；改动前后 `git status --porcelain` 都列出 **61** 行，HEAD 仍是 9389a88，工作树内容不变）。代价是平台挂载账本 `<workspace>/.qoder/managed-resources/session-resources.json` 里 **18** 条 `mount_path` 仍指 `<workspace>/gomydump/dist/*`（实测改名前 12:24 就按这些路径重新落回过 20 个旧名包文件，`dist/` 里那些 `gomydump-*` 就是这么来的），旧路径因此可能被再次创建；AGENTS.md 顶部已写明届时以 `<workspace>/imex` 为唯一工作副本、旧目录下只会有重新落回的产物而没有源码，那些包也不在保留脚本的 `<workspace>/work` 额度内、要手工删。当前 `<workspace>/gomydump` 已不存在（`ls -d` 报 No such file directory）。② **仓库内旧名产物删除**：`bin/gomydump`、`bin/gomydump-linux-amd64`、`bin/gomydump-windows-amd64.exe`（约 65 MB）与 `dist/gomydump-*`（20 个文件、约 111 MB，全是旧前缀，`dist/` 现为空目录）；`bin/` 只剩 `make build` 从新路径重出的 `imex`（ldflags 注入 `github.com/imex/cmd.Version` 等，`./bin/imex version` 输出 `imex version 0.1.0` / `Git commit: 9389a88`）。两目录都在 `.gitignore`（`bin/`、`dist/`）内，删除不触碰仓库内容。③ **`<workspace>/AGENTS.md`**：标题改为「工作区约定（imex）」，顶部写明改名范围与「若平台把旧路径 `<workspace>/gomydump` 落回来以 `<workspace>/imex` 为准」的处置；第 3 条源码包命令改为 `tar -czf imex-<version>-<short-hash>-src.tar.gz --exclude=.git --exclude=bin --exclude=dist --exclude=vendor -C /data imex`，`--transform` 由"必需"降级为"在旧路径上打包时的补偿手段"并保留写法；第 5 条 arm64 临时构建输出名改为 `bin/imex-linux-arm64`；第 6 条安全边界改为「以 `imex-` 开头**外加改名前遗留的 `gomydump-` 前缀」并点名脚本里的 `PREFIXES`。④ **`<workspace>/prune-packages.sh`**：头注示例文件名改 `imex-` 并新增前缀段；新增 `PREFIXES=${PREFIXES:-"imex- gomydump-"}`；`key_of()` 从硬编码 `${base#gomydump-}` 改为遍历前缀剥离，新增 `matches_prefix()`，两处目录扫描由 `for f in gomydump-*` 改为 `for f in *` + 前缀过滤——顺带修掉一个原有边界：状态文件 `last-src-package-date` 此前靠"不匹配前缀 glob"侥幸排除，现在由显式前缀判定排除；`REPO` 默认值随目录改为 `<workspace>/imex`。验证：`bash -n` 通过；`--dry-run` 在 37 项上归出 10 个批次（保留 3、删 7，与改名前行为一致）；实跑释放 **86,451,810 B**（84,425 KiB），目录由 37 项 / 129 MB 收敛到 15 项 / 47 MB；另用合成集验证新前缀归组（`imex-<HEAD>-src.tar.gz` 与 `imex-<HEAD>-bin-SHA256SUMS.txt` 归为一批且标注「当前 HEAD（占额度）」）。之所以保留旧前缀：只认 `imex-` 的话，改名前那 34 个文件（约 129 MB）永远清不掉。⑤ **仍留 12 个 `gomydump-*` 文件**，即保留额度内的 3 个改名前批次（`0.1.0-9389a88`、`0.1.0-a716deb`、`0.1.0-a716deb-dirty`）——**故意既不改名也不删**：这些包里的二进制是改名前构建的，`--help` 打印的仍是 `gomydump`，把文件名换成 `imex-` 等于给旧内容挂新名字；按 `KEEP_N`/`KEEP_DAYS` 规则它们会自然退役。⑥ 期间 `test/integration/option_matrix_test.go` 被环境**第二次**回退，这次退回更早的祖先版本（与 HEAD 差 43 增 / 196 删，`statementCapSlack` 标记 3→0，带回已删除的 `--no-create` 子用例以及 `GOMYDUMP_TEST_DSN`、`github.com/gomydump` 导入路径），处置仍是 `git checkout --` 复原 → 重放改名 sed → 重放 `TestOptionMatrixNoDataCreate` 四子用例改写；因此约定死的检查：任何暂存或打包之前 `grep -c statementCapSlack test/integration/option_matrix_test.go` 必须打印 **3**。⑦ 目录改名后的全量验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`go test -race -count=1 ./...` 七个包全通过；真实库 `IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' CGO_ENABLED=1 go test -race -count=1 ./test/integration` 通过（5.014 s），另跑一轮 `-v` 得 **104** 条 PASS（含子用例）、**0** SKIP，输出里 `gomydump_it_`/`gmydump_it_` 命中 **0**（MariaDB 中途再次掉线，按 `mysqld --defaults-file=<tmp>/mysqld/my.cnf` 拉起后 `ping` 与 `ss -ltn` 确认 3306 在听）；源码包顶层目录实测 **99** 个成员全在 `imex/` 下、`gomydump/` 下 **0** 个；`/data` 全树扫描旧名，只剩三处文档引述（README 修复记录 1 行、requirements.md 第 56/57 行、`cli-oracle.txt` 头注 2 行）。⑧ **另外两份仓库外文档一并统一**：早先交付的参数审计报告 `<workspace>/audit_report.md`（8 行旧名 → 0，标题改为「imex 命令行参数只读审计报告」，环境变量同步为 `IMEX_ERROR_STACK`/`IMEX_ECHO_SQL_VALUES`/`IMEX_QUIET`/`IMEX_NO_SQL_TAG`/`IMEX_LOG_FORMAT`）与用户挂载进来的早期需求文档快照 `<workspace>/mysql-backup-tool/docs/requirements.md`（675 行，43 行旧名 → 0，标题改为「imex 技术需求文档」；它是仓库内 1499 行版本的祖先快照，内容早已落后，只统一名称、不回填正文）。两份改前都留了备份：`<tmp>/audit_report.md.bak`、`<tmp>/mbt_req.md.bak`。**两类刻意不动**：`<workspace>/file_*`（32 个文件、105 MB）与 `<workspace>/.qoder/managed-resources/session-resources.json` 是平台侧的交付与挂载账本，记的就是当初以旧名投递出去的包，改名等于篡改投递记录；`<workspace>/work` 里保留额度内的 12 个 `gomydump-*` 文件（3 个改名前批次）内含的二进制确实会打印 `gomydump`，同理不改名。README 的改名条目同步指向本行。 |
| 58 | 导出选项 `--no-replace` 改名为 `--no-overwrite` | 用户建议。① **改名理由**：该选项唯一的语义是"输出文件已存在就拒绝覆盖"，与 REPLACE 没有任何关系，而原名同时撞上了 `--format sql` 生成的 REPLACE 语句与 `import --mode replace`，三个 replace 互相干扰。落点共十处：`cmd/export_flags.go` 注册名、`internal/config/config.go` 字段 `NoReplace` → `NoOverwrite`、`internal/export/exporter.go` 判定与报错文案、`internal/export/configlog.go` 回显项（命令行名 `no-overwrite`、日志键 `no_replace` → `no_overwrite`）、`internal/export/configlog_test.go` 的 `commonKeys` 清单、`test/integration/option_matrix_test.go`（用例名 `TestOptionMatrixNoReplace` → `TestOptionMatrixNoOverwrite`、表前缀参数 `noreplace` → `nooverwrite`、argv 与断言文案、文件头注释的覆盖清单）、README 导出标志表、requirements.md 本表之外的 2.2.2 选项表 / DB2 `MODIFIED BY REPLACE` 映射行 / 7.2 失败与中断语义 / 7.6 导出覆盖语义 / 11.1 测试清单、`docs/cli-oracle.txt`。② **不留别名**（与 `--no-create` 删除时同口径，与仍保留的 `--single-row-insert` 不同）：实测 `imex export ... --no-replace` 报 `unknown flag: --no-replace` 且退出 **2**；DB2 侧 `MODIFIED BY REPLACE` 子句名不变，映射行同时标注两侧现名与改名日期。③ **行为逐条实测**（本地 MariaDB，`imex_test.zz_ow` 两行，工作区二进制）：带 `--no-overwrite` 命中已存在文件 → 退出 **1**、哨兵文件内容原样存活、报错 `output file <tmp>/ow.del already exists, remove --no-overwrite to overwrite`；去掉该选项同命令 → 退出 **0**、文件被两行数据覆盖；配置回显两种情况分别打印 `no_overwrite=true` 与 `no_overwrite=false`；另证实"创建前失败不误删"这条既有语义：查询报错（表不存在）在 `os.Create` 之前，既有文件同样存活。④ **oracle**：`## export` 段按改名后的 `imex export --help` 重新基线，`diff` 为空；`--no-overwrite` 与 `--no-replace` 同为 `n` 开头且排在 `--no-hex-blob` 与 `-o, --output` 之间，列宽未变（最长名仍是 `--socket-*-timeout`），帮助文案一字未改；头注新增 Re-baselined note 2 登记改名与"旧名成为未知选项"的后果。⑤ **旧名清零复核**：代码与构建脚本（全部 `.go`、`Makefile`、`Dockerfile`、`go.mod`）对 `no-replace`、`NoReplace`、`no_replace` 命中 **0**；文档里仍出现旧名的只有三类——历史记录本身（README 修复记录第 799 行、requirements.md 第 42 行这两条带日期的条目按"记录不改写历史"的既有口径保留原名）、本行的改名说明、以及 2.2.2 / DB2 映射 / README 标志表里的"原名 `--no-replace`"标注。⑥ **验证**：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七个包全通过（`github.com/imex/cmd` 37.061 s）；真实库 `IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' CGO_ENABLED=1 go test -race -count=1 -v ./test/integration` 全通过，**104** 条 PASS（37 顶层 + 67 子用例）、**0** SKIP、**0** FAIL，其中 `TestOptionMatrixNoOverwrite/threads=1` 与 `/threads=4` 逐条 PASS；套件运行前按死规矩复核 `grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**（本轮未再被回退）。测试资产规模不变：39 个 `_test.go`、`^func Test` 命中 202（含 1 个 `TestMain`，即 11.1 口径的 201 个用例）、6 个 `Fuzz*`、12 个 `Benchmark*`——本轮只改名，不增删用例。⑦ 本轮改动未提交、未打包。 |
| 59 | 文档与测试注释脱敏：沙箱绝对路径与本地测试库凭据不再入库 | 仓库转为公开后的收尾。① **凭据**：真实测试库账号写死的 DSN（用户名与口令两个明文词）共 4 处（`docs/requirements.md` 变更记录 57、58 两行，`test/integration/option_matrix_test.go` 与 `test/integration/lock_contention_test.go` 的文件头运行说明注释）统一改为 `<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true`；测试文件只动注释、未动任何代码路径。`README.md:59` 与 `live_mysql_test.go:6` 原本就写的是 `root:secret`/`root:pass` 占位串，保持不动。② **路径**：`docs/requirements.md` 内 20 处工作区绝对路径与 9 处临时目录绝对路径，分别改为 `<workspace>/` 与 `<tmp>/` 前缀，并在「十四、变更记录」标题下加一行脱敏说明，声明占位符含义与「历史记录原文保留在提交历史中」。③ **口径**：只脱敏位置与凭据，不改写记录内容——被引用的日志样例里的 `user=gd`、`Access denied for user 'gd'@'127.0.0.1'`、退出码、字节数、时长、用例计数全部逐字保留；单元测试里当作参数占位的 `<tmp>/out.sql`、`<tmp>/a.del` 等 Go 字符串字面量（`cmd/cli_matrix_test.go`、`internal/common/log_test.go`，均为不落盘的假路径）不属于泄露，同样不动。④ **顺带修掉一个既有 Markdown 缺陷**：变更记录某行里 `-run` 的双用例正则选择符写成了裸竖线（`TestLive_Duplicate` 与 `TestLive_Parallel` 的交替，该行未转义竖线因此为 5 个），渲染时把表格单元切断、多出一列，改为 `-run` 分别取 `TestLive_Duplicate` 与 `TestLive_Parallel` 两个前缀；复核脚本由「数所有竖线」收紧为「只数未转义竖线」，因此 1451 行 `--col-del '\|'` 与 1478 行 `imex version \| grep` 两处**已转义**的写法不再误报。实测：58 个变更行的未转义竖线全部恰为 4（三格），README 四格标志表恰为 5。⑤ **清零复核**：真实账号口令串、工作区绝对路径前缀、临时目录绝对路径前缀三者在文档侧命中均为 **0**；token 串 `github_pat_` 在工作树与全部历史命中 **0**（token 只在 `~/.config/gh/hosts.yml`，模式 600）。注意**历史提交仍含脱敏前原文**，本行只改当前版本，不改写历史；如需彻底抹除，要在用户明确同意后重写历史并强推。⑥ 验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`go test -race -count=1 ./...` 七包全通过；真实库 `IMEX_TEST_DSN` 套件 104 条 PASS（37 顶层 + 67 子用例）、0 SKIP、0 FAIL（沙箱 mysqld 中途掉线一次，按 `<tmp>/mysqld/my.cnf` 拉起后重跑通过）；`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**（该文件本轮只改一行注释）。 |
| 60 | 口令加解密的密钥派生改为 PBKDF2-HMAC-SHA256 + 每条密文独立随机盐，不加版本位、不做兼容 | 用户定级：直接改 PBKDF2+salt，不要版本位、不考虑兼容。① **实现**：`internal/common/password.go` 的 `deriveKey` 由 `sha256(IMEX_KEY)` 单次哈希改为 stdlib `crypto/pbkdf2`（`pbkdf2.Key(sha256.New, key, salt, 600000, 32)`，go 1.26.10 内置，未引入新依赖），新增常量 `kdfSaltSize=16`、`kdfIterations=600000`、`kdfKeySize=32`；密文布局 `ENC:` + `base64(salt ‖ nonce ‖ ciphertext+tag)`，盐与 nonce 都每次用 `crypto/rand` 新生成并前置。AES-256-GCM、`ENC:` 前缀、`ResolvePassword` 调用点、两条错误文案（`wrong key or corrupted data`、`password is encrypted but IMEX_KEY environment variable is not set`）与退出码全部不变，CLI 帮助文本一字未改，因此 `docs/cli-oracle.txt` 无需重新基线（实测 `## export` 段 `diff` 仍为空）。② **为什么必须改**：旧派生没有工作因子，而 GCM 认证标签让每一次密钥猜测都能当场验真伪，等于把整套机制的强度完全押在 `IMEX_KEY` 的熵上，且同一密钥下所有密文共用同一把派生密钥。实测（4 核沙箱、单线程、go1.26.10）旧方案的离线密钥候选尝试速率 **939,822 次/秒**（20 万候选 213 ms、命中恰 1 次）；PBKDF2 600000 次迭代后 **约 3 次/秒**（单次派生 310.8 ms），210000 次为 9 次/秒（108.7 ms），故取 600000。加盐同时消除跨密文复用与预计算。③ **代价**：解密只发生在 `RunE` 起始处一次，不随 `--threads` 放大；带 `ENC:` 口令连本地真实库导出 2 行整次进程 0.35–0.42 s，同命令写明文口令 0.00 s；`internal/common` 包的 `-race` 测试由 1.3 s 增至 65.7 s（新用例每次都真跑 600000 次派生，没有为换速度而降低被测迭代数）。④ **不兼容的落地**：`TestLegacyBlobIsRejected` 以复刻旧布局的辅助函数生成旧串并断言必须报错、不得返回任何内容；真实二进制侧另测得旧格式串退出 1、`-o` 目标文件不被创建，错密钥同样退出 1。⑤ **新增 `internal/common/password_test.go`**（首个覆盖该路径的测试文件，9 个 `Test*`）：往返（ASCII、空串、以 `ENC:` 开头的口令、CJK 与 emoji、256 字节长口令、含 NUL 与控制字符）、布局长度恰为 salt+nonce+明文+16 字节标签、同明文两次加密整体不同且盐不同、不同盐派生出不同 32 字节密钥、错密钥、翻转密文一字节、翻转盐一字节、截断、非法 base64、旧布局拒绝、空密钥在加密与解密两端都拒绝、`ResolvePassword` 的明文原样返回 / 空串 / 按 `IMEX_KEY` 解密 / 缺环境变量报错四个分支。⑥ **文档同步**：README 的特性条、`IMEX_KEY` 说明、`encrypt` 子命令节与「密码加密」用法节（两处示例密文重新用新版本生成——旧示例串在新版本下永远解不开，属误导性文档；可解性已实测：按文档写的密钥跑 `export -p 示例串`，得到的是服务端 1045 Access denied 而非解密失败，证明解密链路正常），安全特性与安全事项新增三条（盐与 nonce 随机、`IMEX_KEY` 是唯一秘密、旧密文全部失效需重跑 `encrypt`），项目结构里 password.go 的注释更新；requirements.md 2.4 表格的算法行、密文非确定性行、安全边界行改写并新增工作因子与兼容性两行，九节结构注释、11.1 与十二节的测试计数由 39 个 `_test.go` / 201 个 `Test*` 更新为 **40 / 210**（`internal/common` 由 7 个文件 29 个用例增至 8 个文件 38 个）。⑦ **验证**：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七包全通过，真实库集成套件 **104** 条 PASS（37 顶层 + 67 子用例）、0 SKIP、0 FAIL，`statementCapSlack` 标记 = **3**；`CGO_ENABLED=0` 下 linux/amd64 与 windows/amd64 交叉编译均成功（PBKDF2 是纯 Go，打包矩阵不受影响）。⑧ **本轮未做、仍待定级的同项问题**：AEAD 的 `adata` 仍为 nil（密文未绑定 user、host、port、db，同密钥下可跨配置搬用）；`imex encrypt` 仍把明文当 argv（明文进 `ps` 与 shell history，其 Long 文本"不在进程列表暴露明文"只对结果成立）；密钥只走环境变量——**本条已由用户定案：不加 `--key-file`，密钥托管不是本工具的职责**，故从待办移出、后续不再重提；非 `ENC:` 前缀的明文口令不告警；base64 解码无长度上限；工具整体不支持 TLS（`internal/config/config.go` 已写明要靠外部隧道）。 |
| 61 | 导出 `--extended-insert` 在 `--statement-size` 低于单行宽度时静默退回逐行布局，补一次告警 | 回答「`--batch-size`/`--statement-size` 在两侧各管什么」时顺带查出的缺陷。① **根因**：`internal/export/sql_writer.go` 的合并判定是「当前批非空且加入下一元组将超上限才 flush」（:237），于是当单行语句自身宽度就超过 `--statement-size` 时每条语句永远只装 1 个元组——`--extended-insert` 要的合并一条也没发生，产物与默认单行布局逐字节相同，而改动前**零告警**；同一尺寸下默认单行布局反而会告警一次（`warnOversizedStatement`，:263-273，每文件去重为一条）。② **修法**：写入器为批量化布局补记两个状态（累计行数 `mergedTuples`、是否曾把两个元组放进同一条语句 `everMerged`），`WriteFooter` 在累计 ≥2 行且从未合并时记一次 WARN `--extended-insert never merged rows: every row alone exceeds --statement-size, so the dump is one INSERT per row`（带 `statement_size` 与 `rows`）；单行文件不报——一行无法与任何东西合并，与上限无关；并行导出每个分片各判一次，与既有告警的「每文件一次」同口径。只出声，不改产物。③ **实测**（本地 MariaDB，`bench_lay` 20 万行 3 列，`-f sql`）：`--extended-insert --statement-size 64` → 17,224,453 B / 200,000 条语句，与默认单行布局**逐字节相同**（md5 `01cfcd0ba049a7194be925f1a490e389`），同一条命令在改动前的二进制上告警 **0** 条、改动后 **1** 条，而默认单行布局在同一尺寸下两个二进制都各告警一次；`400` → 8,690,869 B / 22,217 条且**无告警**（证明确实合了）；`1000` → 8,015,845 B / 8,154 条；默认 `8388608` → 7,624,501 B / 1 条（体积 -55.8%，gzip 1,859,386 B 对 1,677,322 B）。④ **顺带把三个选项的语义量化清楚**：导出侧 `--batch-size` 只是 reader→writer 的交接批次（`exporter.go:287-295`），不改变文件内容，且 `--threads>1` 根本不读它（`:441-451` 记一条 info），20 万行 DEL 导出实测 1000/50/50000 为 109/114/166 ms，`--threads 4 --batch-size 50000` 为 122 ms 并记一条 `parallel export ignores pipeline options`（`look_ahead_per_shard` 为 16.0 MiB）；导入侧 `--batch-size` 是每条 INSERT 的行数，受 65535 占位符按列数钳制（`batchcap.go:33-48` 与 `:179-187` 的一次性 WARN，实测 `requested=50000 effective=21845 columns=3 limit=65535`），CLI 上界 100000 与该钳制语义重复，同一份单行文件（逐次 TRUNCATE 后重导，每次都是 200,000 行）实测 100/200/500/1000/5000/10000/21845 → 1022 ms / 901 ms / 821 ms / 813 ms / 739 ms / 756 ms / 807 ms，500 以上进入平坦区，最小的 100 比最优点慢约 38%，默认 1000 已在平坦区、无需调整；`--batch-size 50000` 触发上述钳制并成功导入，`--batch-size 200000` 报 `invalid --batch-size 200000: must be >= 1 and <= 100000` 退出 2；`--statement-size` 在导入侧默认 0 表示不加额外限制，给值只可能压低 `max_allowed_packet` 的 9/10 预算（`batchcap.go:126-146`，64 KiB 下限）；文件布局不影响导入速度（同一份数据单行文件 766 ms 对合并文件 814 ms，差异在噪声内，`internal/import/sql_reader.go:15-18` 把元组拆回行后重新攒批），`--extended-insert` 的价值只在体积与第三方工具直接消费。⑤ **测试**：`internal/export/sql_writer_test.go` 新增 `captureWriterWarnings`（临时把 `common.Logger` 换成 warn 级 `slog` 内存 handler）与 4 个用例——退化尺寸恰好 1 条合并告警且产物逐行、默认尺寸真正合并必须静默、单行文件不得误报合并失败、单行布局仍只告警一次且不得出现合并告警；`test/integration/option_matrix_test.go:TestOptionMatrixSQLBatching` 把旧选项用例替换为退化布局的产物形态断言（顶层用例数不变）。文档：README 导出标志表 `--extended-insert` 行、requirements.md 2.2.2 选项表该行、SQL 布局默认值说明新增一条带实测数字的退化说明、10 节测试场景行、11.1 两处计数（`internal/export` 70→74、总量 210→214）。本轮改动未提交、未打包 |
| 62 | 删除导出选项 `--single-row-insert` | 用户确认删除，不做兼容层。① **删除前的状态**：`cmd/export_flags.go` 的 `legacySingleRowInsert` 由 cobra 写入后全仓库无人读取，唯一引用它的是 `cmd/validate.go` 的 `validateInsertLayout`，判的仍是它与 `--extended-insert` 的互斥；布局本身早已由 `config.ExportConfig.ExtendedInsert`（默认 `false`）经 `internal/export/exporter.go:264` 与 `:624` 的 `SetSingleRowInsert(!cfg.ExtendedInsert)` 决定，与该 CLI 选项无关。② **落点四处**：死变量与注释、`registerExportFlags` 的注册、`validateInsertLayout` 及其在 `validateExportConfig` 里的调用点、`internal/config/formatscope.go` 的 `sqlOnly` 登记项；SQL 专属组由五项减为四项，`--help` 的范围标记少一行。内部 API `SQLWriter.SetSingleRowInsert` **保留**——它是默认布局的真实开关。③ **口径一致**：与第 55 行直接删除 `--no-create`、第 58 行不给 `--no-replace` 留别名同一条「不保留兼容层」策略。④ **实测**：`imex export ... -f sql --single-row-insert` 报 `unknown flag: --single-row-insert` 且退出 **2**，且不产出输出文件（改动前的同一命令退出 **0**）；与 `--extended-insert` 同时给出同样退出 **2**，文案由互斥改为未知选项；`--extended-insert -f parquet` 退出 **2** 并报 `can only be used with --format sql`（parquet 越界的覆盖已转到该选项上）；`imex export --help` 与 `docs/cli-oracle.txt` 的 `## export` 段 diff 为空——重新基线只少那一行，列对齐未变（最长名仍是 `--socket-*-timeout`）；代码与构建脚本（全部 `.go`、`Makefile`、`Dockerfile`、`go.mod`）里 `single-row-insert` 命中 **0**，仅「已删除选项是未知选项」这一条用例名与其 argv 还含该名。⑤ **测试**：`cmd/cli_matrix_test.go` 删三条（旧选项合法、与 `--extended-insert` 互斥、配 `--format parquet` 越界）并加一条未知选项用例，parquet 越界的覆盖转由 `--extended-insert` 承担，`TestCLIInsertLayout` 去掉废弃项段落（该文件 `Test*` 数不变）；`internal/config/formatscope_test.go` 的 `AppliesTo` 矩阵把该行换成 `extended-insert` 两行（SQL 为 true、parquet 为 false）；`test/integration/option_matrix_test.go` 的旧选项用例换成第 61 行那条退化布局用例；`test/integration/roundtrip_test.go` 的注释不再点名已删除的选项（该用例走内部 API，保留）。⑥ **文档**：README 导出标志表与参数校验表各删一行、SQL 输出说明段补删除理由与未知选项后果、关联校验段更新 SQL 专属清单；requirements.md 2.2.2 选项表与适用性说明、4 节 DB2 映射行、选项演进表第 4 行补注、10 节场景行、11.1 三处描述同步；`docs/cli-oracle.txt` 重新基线 `## export` 并在头注新增 Re-baselined note 3，同时把 nine DEL plus five SQL 与 the fourteen marked options 改为 nine DEL plus four SQL 与 the thirteen marked options。测试资产 40 个 `_test.go` / 214 个 `Test*`（含 1 个 `TestMain`）、6 个 `Fuzz*`、12 个 `Benchmark*`。验证见第 61 行末尾的同一轮 QA。本轮改动未提交、未打包 |
| 63 | 按用户授权重写提交历史并强推，把第 59 行只改了当前版本的脱敏前原文从历史中抹除 | 用户指令「重写历史和强推」，推翻第 59 行「本行只改当前版本，不改写历史」的口径。① **暴露面实测（改写前）**：对全部 32 个提交逐版本扫描，真实测试库口令串命中 **16** 个文件版本（`test/integration/option_matrix_test.go` 12、`test/integration/lock_contention_test.go` 2、本文件 2），工作区绝对路径前缀命中 **2** 个版本（本文件）；提交信息命中 **0**；当前工作树两者命中均 **0**（第 59 行的成果），即泄露只存在于历史版本；访问令牌串在工作树与全历史命中 **0**。② **工具**：pip 安装的 `git-filter-repo`（a40bce548d2c 构建）的 `--replace-text` 实测是**空操作**——在合成小仓库上跑一次，blob 内容、提交信息、提交 SHA 三者全不变，`--help` 只输出 5 行，故弃用；改用 `git filter-branch -f --tree-filter` 加 sed 三条规则（明文「用户:口令」串 → `<user>:<password>`、口令单词 → `<password>`、工作区绝对路径前缀 → `<workspace>/`），作用域限定上述三个文件；`<tmp>/` 那类前缀刻意**不**脱敏（单测里是不落盘的假路径字面量，与第 59 行同口径）。改写全部在一处临时克隆里做，主仓在验证通过前未动。③ **改写结果**：提交数 32 不变，提交信息与顺序逐行 diff 为空（一一对应）；`main^{tree}` 改写前后同为 `396c7dbb05d7…`，即**当前发布内容逐字节未变**，只重写旧版本对象；最早发生内容变化的提交是 `5bf4458` → `fa3fa3b`（`option_matrix_test.go` 1 增 1 删），含它在内共 **15** 个提交的 SHA 发生变化，映射表：`5bf4458→fa3fa3b`、`b4102ad→e357986`、`8136242→1a468d0`、`40129e7→aeb6035`、`86f3116→c7b1cde`、`d1b1ee2→f6d40b7`、`bc9210e→e30257d`、`fbfff64→337be30`、`a716deb→f55c5ad`、`9389a88→4cceac9`、`0d505a0→58db5fa`、`894f2f5→e2195a1`、`3224942→df9e7aa`、`f8856dc→bec9eed`、`cef64a7→48dc684`。**后果**：第 53、55、56、57 行里的 `9389a88`（本文件 7 处）与 `a716deb`（3 处）、以及 README 修复记录里的 2 处 `9389a88` 从此不可解析，按本表读取即可——历史行里的实测日志样本不做回溯替换，把样本换成「当时并未打印过的提交号」等于伪造证据。另测得 `4934dd2`、`2c9834af64e6`、`503ae9219565` 三个提交号在改写前后的历史里**都**不可解析（前者是 `docs/cli-oracle.txt` 的基线提交号，本文件第 55 行 1 处、该文件 4 处），属本轮之前就已存在的失效引用，不由本轮负责。④ **清理与强推**：filter-branch 自动留下的 `refs/original/*` 备份与过期的 `refs/remotes/origin/*` 必须删掉才算零命中——此前那 16 次命中正是备份 ref 贡献的；本地分支 `backup/pre-reword`（`0a674a0`）实测其自身与 9 个祖先版本两个串命中均 **0**，与主线同源、无独有内容，已删；无 tag。强推结果 `+ cef64a7...48dc684 main -> main (forced update)`，之后 `git ls-remote origin` 只剩 `HEAD` 与 `refs/heads/main`，fork 接口测得 **0** 个 fork。主仓随后对齐：从临时克隆 fetch → `main` 指向 `48dc684` → `reset --hard` → `reflog expire --expire=now --all` → `gc --prune=now --aggressive`；用 `git cat-file --batch-all-objects` 扫描**全部剩余对象（含未引用对象）**，口令串出现次数 **0**，包体由 505.78 KiB 收敛到 485.94 KiB，reflog 条目 **0**，工作树 0 脏。⑤ **备份**：`<tmp>/imex-pre-rewrite-20261027.bundle`（479,620 B，`bundle verify` okay，含改写前 4 个 ref）——**这份备份自带脱敏前原文**，只存在于临时目录、不入库，沙箱回收即消失；要长期留存须自行加密保管。⑥ **公开仓库侧残留（诚实边界）**：强推只让旧对象悬空，不等于删除。`GET /repos/.../commits/cef64a7` 仍返回 **200**；以 `option_matrix_test.go` 为例，旧历史该文件的 8 个不同 blob 版本中 **6** 个仍能按 SHA 从 blobs 接口取回且内容仍含口令（`4413bd2e600e…`、`814e1dd2bc4b…`、`94a8bede7d1b…`、`dceb4f781739…`、`e0459d0652b6…`、`e55c984b3e93…`），另 2 个（`3a4385e81f56…`、`e11584a0df3d…`）是改写后保留的版本、命中 **0**。沙箱内 `raw.githubusercontent.com` 被连接重置，以上均经 REST API 测得（未认证请求会间歇返回 403 限流，带 token 重试即为 200）。要彻底抹除只能向 GitHub Support 提删除请求；任何在本轮之前 clone 过的人本地仍持有原文。因此该测试库口令按**已外泄**处理：它是沙箱内本地测试库账号，仓库内已不再出现，若在他处复用请轮换；令牌的撤销与轮换仍是用户的动作，本轮未代做。⑦ **同轮文档一致性**：本节头注原写「历史记录原文保留在提交历史中」，已被本行推翻并同步改写；第 59 行「本行只改当前版本，不改写历史」及其尾注、第 61 与 62 行末尾的「本轮改动未提交、未打包」同样被其后的提交与本轮强推取代——历史记录按「写入当时的事实」保留，不回溯修改，以本行为准。⑧ **验证**：本轮零代码、零测试改动（`main^{tree}` 与第 62 行提交时逐字节相同，而该树当时已过全量 QA：7 包 `-race` 全绿、真实库 104 条 PASS 含子用例、0 SKIP、0 FAIL），故只重跑 `gofmt -l`（空）、`CGO_ENABLED=1 go build ./...`（通过）与死规矩检查 `grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**；11.1 与十二节的测试资产计数不变；本节变更行由 62 行增至 **63** 行，逐行未转义竖线仍恰为 4（三格）。本行为事后补记，未提交、未打包 |
| 64 | 并行导出改为采样分片边界并按段数超配 worker，导入重放预算改为进程级均分，并行导出对无效管道选项由静默忽略改为拒绝 | 用户指令「实现第 1、2、3 条」，即上一轮并行导出/导入分析里三条：第 1 条 = E1+E2+E3（采样边界、支持非数值键、段数与线程数解耦且产物字节不变），第 2 条 = I1+I2（重放字节预算按 worker 均分、把 `--commit-count` 默认值抬到实测拐点），第 3 条 = E4（去掉并行路径上那两个无效选项的假象）。改动先在临时分支 `feat/parallel-optimize` 上完成（基线 `2dc30e7`），提交后按用户「单一 main 分支」指令快进合入 `main` 并删除临时分支。**① 采样边界（E1+E2）**：新增 `internal/export/shard_sample.go`（约 300 行）——`shardRangeCount(threads) = min(2×threads, 64)` 定段数，`planShardRanges` 依次判「段数<2 直接等宽 → 读 `INFORMATION_SCHEMA.TABLES` 的 `TABLE_ROWS`/`AVG_ROW_LENGTH`，读不到记 `debug` 回落等宽 → `samplingWorthwhile` 不值得则等宽 → 探针采样，任何错误记 `warn` 回落等宽」；探针是 ``SELECT `k` FROM `tbl` WHERE `k` > ? ORDER BY `k` LIMIT 1 OFFSET 每段行数-1``，边界走绑定参数（因此 `CHAR`/UUID 主键天然可用，不需要窗口函数或任何方言语法，MySQL 与 OceanBase 同一句话），`OFFSET` 由工具自算后内联；两条捷径省掉探针——定点整数键且值域跨度 ≤ 行数×3/2 时可证等宽偏差不超过 1.5 倍（`big.Rat` 精确比较，`BIGINT` 满跨度不误判），稠密整数键仅在 `AVG_ROW_LENGTH × TABLE_ROWS ≥ 256 MiB` 时才花探针；`rangesFromBounds` 严格保持「首段从精确 MIN、末段以 `IsLast` 收到真实 MAX、相邻段共享同一边界文本」的连续覆盖不变量，整数边界维持 `shardBoundary` 文本（超过 2^53 不丢精度），单列可空键追加 `NullOnly` 段、复合键含可空列拒绝采样。**② 段数与线程解耦（E3）**：`exportParallel` 由「每段一个 goroutine」改为恰好 `threads` 个 worker 经 `atomic.AddInt64(&nextRange, 1)` 从 −1 起**按段序**领取（写出侧 `writeShardStream` 按段序消费，乱序领取即死锁），结果收集仍读满 `len(shardRanges)` 条因此不需要 `WaitGroup`（实现过程中一度加了 `wg.Wait()`，与按序消费互相等死，已删除并写进注释）；`len(shardRanges) < numRanges`（采样提前触顶或可空键多一段）时记一条 `info` `shard ranges fewer than planned`，判据刻意用 `<` 而不是 `!=`。**③ 重放预算（I1）**：`internal/import/session.go` 新增 `replayBudget(threads)` 与 `minReplayBudget = 8 MiB`，`DefaultMaxPendingBytes = 64 MiB` 的语义由「每会话」改为「每进程」并按 `cfg.ImportThreads` 均分（8 线程整除为 8 MiB，再多的线程数取下限），`maybeCommit` 保持「行数或字节任一触顶即提交」不变；改动动机是既有注释与实测的口径：4 线程宽行导入峰值 RSS 208 MB 几乎全是重放缓冲。本轮另测窄行不受影响——100 万行窄表 DEL `--threads 4` 峰值 RSS 改动前 29,284/28,168 KiB、改动后 28,988/28,104 KiB，因为 `--commit-count` 的行数上限先触发，字节上限根本不参与。**④ `--commit-count` 默认值（I2）经实测否决**：与候选 20000 在本地 MariaDB 对跑、每组 3 次取中位数、每次落库行数都核对为 1,000,000——1M 行窄表 `--threads 4` 为 3.56 s（3.50/3.56/3.79）对 3.66 s（3.66/3.71/3.49），1M 行平均 168 B 的表 `--threads 1` 为 8.52 s（8.52/9.04/8.46）对 8.47 s（8.19/8.47/8.48）；两侧中位数之差 ≤ 0.1 s 且小于组内 ±0.6 s 抖动，加大提交窗口不构成拐点、只会拉长掉线重放范围，故**默认值保持 1000**，导入 CLI 无需重新基线。**⑤ 无效选项改为拒绝（E4）**：`cmd/validate.go` 新增 `validateParallelExportOptions`，登记 `parallelExportIgnoredOptions = {batch-size, pipeline-buffer}`，`--threads>1` 时用 `cmd.Flags().Changed(name)` 判定并 `common.Usagef` 退出码 2，文案点名该选项、说明「并行时每个分片把自己的行直接流式写进输出，该选项不改变任何东西」、给出两条出路（去掉它 / 改 `--threads 1`）；调用点紧跟 `validatePipelineBuffer`，因此顺序是 `--threads` → `--batch-size` → `--pipeline-buffer` → 路径冲突 → 其余，非法值先于冲突报出。判据是「是否显式传入」而非「是否等于默认值」，故 `--threads 8 --batch-size 1000` 同样被拒。`cmd/export_flags.go` 两行帮助文本同步改为 `(single-threaded export only; passing it with --threads>1 is a usage error)`；库调用方（手工构造 `ExportConfig`）不走 CLI 校验，仍收 `parallel export ignores pipeline options` 这条 `info`，注释已改写以免读成「CLI 也会忽略」。**⑥ 产物字节不变是硬约束，逐条实测**：与改动前交付的二进制背靠背跑 3 张表 × 三种布局 × `--threads 1/4/8`——DEL 与默认单行 SQL 共 18 组 md5 全同（`bench_lay` 6,624,389 B `580c2f5d21`、`bench_skew2` 141,185,809 B `b0579eece7`、`bench_str2` 79,000,008 B `c01b546581`，SQL 布局 17,224,453 / 196,185,875 / 102,500,078 B 同样全同），`--extended-insert` 的 `--threads 1` 三组也全同，只有 `--extended-insert` 的 `--threads 4/8` 因边界增多而各多一条 INSERT 头（`bench_lay` 7,624,645→7,624,837 即 +192 B、7,624,837→7,625,221 即 +384 B，每条头 48 B；根因是默认布局「一条语句一行」使任意边界都不改变字节，而合并布局在每个段边界必须 flush）。**⑦ 结构收益实测**：`bench_skew2`（100 万行，`id` 1…200,000,000 稀疏）改动前 4 段为 200,000 / 0 / 0 / 800,000 行（general log 里能看到 `>= 150000000.25 and <= 200000000` 这类等宽小数边界），改动后 8 段为 123,749×7 + 133,757 行、最大段 133,757 行（-83.3%）、空段为零；`bench_str2`（516,377 行 `CHAR(32)` md5 十六进制主键）改动前只有 1 段并记 `only 1 shard range` 降级单线程，改动后 8 段、7 条探针、1 条统计查询、无告警。稠密小表 `bench_lay` 实测零探针且边界与 `GenerateShardRanges` 逐项相同，代价仅是多一条 `INFORMATION_SCHEMA.TABLES` 查询（general log 计数：改动前 0、改动后 1 条统计 + 0 条探针）。**⑧ 速度诚实记账**：这台 4 核沙箱上导出不是分布不均的瓶颈，采样收益表现为结构而非吞吐——`bench_skew2` 141 MB DEL `--threads 4` 为 0.84 s（改动前中位数）对 0.90 s（改动后），`--threads 8` 为 0.86 s 对 0.91 s，增量与 7 条探针的成本同量级（一次全表扫描实测 0.128 s）；`bench_str2` 在暖缓存下 0.36–0.41 s 对 0.36–0.40 s 无可测差（首轮测得的 5.29 s 对 0.39 s 是冷缓冲池造成的假象，已按 A/B 交替 3 次复测推翻）。段数翻倍后每段前瞻由 16 MiB 降到 8 MiB（`--threads 4`）/4 MiB（`--threads 8`），实测无可测损失，与 7.2 那条「2.5 MiB 时慢约 20%」的历史测量并不矛盾——后者远低于本次的最浅档位。**⑨ 测试**：新增 `internal/export/shard_sample_test.go`（12 个 `Test*`，进程内假 `driver`（`sampleFake`/`sampleConnector`/`sampleConn`/`sampleStmt`/`sampleRows`）按查询形态应答并记录语句与绑定参数——段数计划、整数键类型判定、密度判据 7 组、`samplingWorthwhile` 6 组、`probeArg` 装箱规则、探针 SQL 形状与绑定参数逐段接续、重复边界跳过、稠密小表零探针且等值、稀疏整数 8 段连续且边界以 `int64` 抵达、5 类失败回落、单段不发查询、NULL 段与复合键拒绝）；`cmd/cli_matrix_test.go` 新增 2 个 `Test*`（`TestCLIParallelExportRejectsPipelineOptions` 9 用例含「给默认值也拒」「`--threads 1` 合法」「非法 `--threads` 先报」「非法 `--batch-size` 先报」「所有拒绝必须满足 `common.IsUsageError`」，`TestCLIImportKeepsPipelineOptionsWithThreads` 钉住导入侧不受影响）；`internal/import/session_test.go` 新增 3 个 `Test*`（`TestReplayBudgetDividesAcrossWriters` 覆盖 1/2/4/8/64/0/−1 与整除性、`TestWriteSessionRetainsItsShareOfTheReplayBudget`、`TestWriteSessionCommitsAtItsOwnByteCap`）；`test/integration/option_matrix_test.go` 新增 `TestOptionMatrixTextShardKeyParallelExport`（240 行 md5 十六进制 `CHAR(32)` 主键：串行与 `--threads 4` 的 DEL 与 SQL 产物互比逐字节相同、日志不得出现 `only 1 shard range` 且 `shard export completed` 不少于 2 条、并行产物导入姊妹表后按键序逐行比对）。两条集成侧既有用例随之调整：`TestOptionMatrixParallelOutput` 的段数上界由「threads-1」这一错误假设改为按 `maxRanges = 2×threads` 表达（`extra > maxRanges-1` 才失败，且「字节差 == 头数 × 头宽」这一精确断言原样保留），原因是段数与线程数解耦后边界数就是 `2×threads-1`（该用例首轮真实失败为「8 条 INSERT 头对 1 条串行，extra=7 越界」）；`TestOptionMatrixTextShardKeyParallelExport` 首版比的是两份文件的字节，实测 28426 对 28426 仍不相等——两份产物各自嵌入自己的表名，故改为直接按键序比对落库行（新增 `textKeyRows` 辅助函数）。**⑩ 文档**：`docs/cli-oracle.txt` 的 `## export` 段按新帮助文本整体重新基线（`diff` 逐字节为空，export 段现居 99–185 行，文件 293 行），两条被改的帮助行都在基线里，头注新增 **Re-baselined note 4（2026-10-27）** 登记「静默忽略 + 一条 info」改为「显式传入即用法错误、退出码 2」、只拒显式传入、导入侧两个选项照旧、库调用方仍记 info；README 的并行导出说明（降级触发点不再含非数值键、新增采样与段数说明、64 MiB 预算改为按段均分并给出 `--threads 4/8/64 段`的前瞻值）、流水线生效范围、参数校验表两行、关联校验段、并行导入新增的重放预算段、批量导入控制新增的 `--commit-count` 实测段，共 6 处；修复记录补一行并在两条历史记录尾部加「本行提到的 info 已被变更记录 64 升级为用法错误」的注（历史记录不回改原文）。requirements.md 侧新增 7.2.1 小节（采样设计、探针 SQL、两条捷径、回落、连续覆盖不变量、按序领取、实测三表对照、产物影响面、四条已知限制）、2.2.2 两行选项说明、WARN 场景清单、日志记录清单新增一行（`shard boundaries sampled from the key distribution` / `no row estimate for the table, splitting the key range by value width` / `shard boundary sampling unavailable, splitting the key range by value width` / `shard ranges fewer than planned`）、7.2 队列深度段与注意段、降级触发表第四行及其后说明、并行导出流程第 3/4 步、项目结构补 `shard_sample.go`、10 节新增「分片边界采样」场景行、11.2 OceanBase 行补采样未在有实例上验证、十二节源码包计数。**⑪ 计数复核**：11.1 与十二节的测试资产由 40 个 `_test.go` / 214 个 `Test*` 更正为 **41 / 233**（`internal/export` 9 个文件 74 → 10 个文件 86，`cmd/cli_matrix_test.go` 9 → 11，`internal/import` 44 → 47，`test/integration/option_matrix_test.go` 12 → 14），并顺带纠正一处长期偏差——`git grep -c '^func Test' HEAD` 实测 `option_matrix_test.go` 在改动前就是 13 而非表中所记 12，故本行起逐文件按实测复核；`Fuzz*` 6 个、`Benchmark*` 12 个不变。**⑫ 验证**：`gofmt -l .` 空；`CGO_ENABLED=1 go vet ./...` 干净；`CGO_ENABLED=1 go test -race -count=1 ./...` 七个包全通过（提交前的最后一次实跑：`github.com/imex/cmd` 31.058 s、`github.com/imex/internal/common` 47.746 s——后者因 PBKDF2 用例每次真跑 600000 次派生，同轮三次实跑在 47.7–56.5 s 之间波动，取哪一次都不影响结论）；真实库 `IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' CGO_ENABLED=1 go test -race -count=1 -v ./test/integration` **105 条 PASS**（38 顶层 + 67 子用例）、**0 SKIP**、**0 FAIL**（基线 104，本轮 +1 条真实库用例）；死规矩复核 `grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**。**⑬ 诚实边界**：采样用的是**整表**统计，`--query` 带 `WHERE` 时仍可能按未过滤的分布切段（不重不漏的性质不变，均衡性可能失效）；`--extended-insert` 的产物会随段数多出语句头；复合键含可空列仍拒绝采样（与改动前一致）；性能数字全部来自本机 MariaDB 10.6.23 的 4 核 / 8 GB 沙箱，OceanBase 无实例未验证，E5（按 tablet 统计分片的 OceanBase 专用路径）不属于本轮三条、未实现；本轮改动经用户「仅提交」指令落 commit `10f66e3`（12 files changed / 1363 insertions(+) / 56 deletions(-)），随后按「单一 main 分支」指令 `git merge --ff-only` 快进合入 `main` 并删除临时分支 `feat/parallel-optimize`，`main` HEAD 现为 `10f66e3`；未 push、未打 tag、未打包，`Makefile` 版本号未动，**用户定级：Y 档**（版本号第二位 +1、第三位归零）。定级依据第 7 条：本轮动了五个对外契约面中的三处——① CLI 帮助文本（两条选项说明改写，`docs/cli-oracle.txt` 的 `## export` 段已重新基线）、③ 退出码语义（新增一条退出码 2 的拒绝路径，原为静默忽略）、⑤ 发给服务端的语句（并行导出新增 1 条 `INFORMATION_SCHEMA.TABLES` 统计查询与最多 `2×threads-1` 条定位探针）；但**没有**删除或改名任何选项、没有改产物文件格式与 `ENC:` 布局、没有动环境变量，新增的拒绝只挡住「本来就不起作用」的组合（`--threads>1` 加那两个管道选项），既有可用命令行全部照旧，故按字面归第二档而非第一档。落地方式：该轮提交并 push、工作树干净、QA 全绿之后，以附注标签 `v0.2.0` 落在该提交上，且 `Makefile` 第一行 `VERSION ?= 0.2.0` 必须同一次提交内同步（第 8 条）；若第 9 条的 `v0.1.0` 基线决定回填，`v0.2.0` 就是回填之后的第一轮进位；若决定不回填，`v0.2.0` 将是本仓库第一个 tag。本轮结束时已按用户「仅提交」指令落 commit `10f66e3`，未 push、未打 tag、未打包；因项目工作流规定单一 main 分支，随即以 `git checkout main && git merge --ff-only feat/parallel-optimize && git branch -d feat/parallel-optimize` 快进合入并删除临时分支，最终 `main` HEAD = `10f66e3`、工作树干净。随后用户下「继续自动完成剩余工作」指令：本轮追加一次 release 提交把 `Makefile` 首行 `VERSION ?= 0.1.0` 同步升至 `0.2.0`（与本行 tag 名同提交，符合第 8 条），并在该 release 提交上打附注 tag `v0.2.0`；push 主线与 tag 后本轮闭环。release 提交前置 QA 再次全绿：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七包全通过（`cmd` 31.062 s、`internal/common` 45.626 s——同一次派生负载本轮实测 45.6–56.5 s 波动，把 §⑫ 那句「47.7–56.5 s」的下限顺带拉低到 45.6 s，不影响结论），真实库 `IMEX_TEST_DSN=... go test -race -count=1 -v ./test/integration` 复算 **105 条 PASS / 0 SKIP / 0 FAIL**、`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**。回填 `v0.1.0` 到 `2dc30e7` 属规则 9 的开放问题、本轮未代决策，因此 `v0.2.0` 是本仓库**第一个 tag**。 |
| 65 | 日志的耗时字段单位随量级漂移（`4.3205s`、`1m2.3s`、`842ms` 混用），正常跑完的导出/导入看不到工具究竟向库发了哪些语句，并行日志又分不清是哪个 worker 出了问题 | 用户指令「1. 最后日志统计总耗时单位为 s，保留三位小数。 2. exp/imp 与数据库交互的 SQL 要打印出来，但具体数据 SQL 太多就不要。 3. 并行日志要有 worker 编号」，定级 Z（纯缺陷修复）。① **固定单位**：新增 `internal/common/logsql.go:Seconds`（`fmt.Sprintf("%.3fs", d.Seconds())`），导出单线程/并行完成、导入结构-only/单线程/并行/中止、以及两侧的进度 `elapsed` 共 8 处耗时字段统一走它（842ms→`0.842s`、2.5s→`2.500s`、90s→`90.000s`），字段名与值语义不变、只换取值格式；`TestSecondsAlwaysPlainSeconds` 钉住 6 组边界。② **交互 SQL 回显**：新增 `common.LogInteractionSQL`，只在 `--log-level debug`（库调用方为 debug handler）下记一条 `db interaction`，参数经 `SQLPreview` 脱敏成 `num`/`str(len)`/`bytes(len)`/`NULL`（真实值仍只在 `--echo-sql-values` 下出现）。覆盖面：导出侧 `SHOW CREATE TABLE`、分片统计与列类型查询、MIN/MAX、`LIMIT 0` 列探测、采样探针（`... ORDER BY 键 LIMIT 1 OFFSET n`，绑定参数逐段接续）、`version_comment`；导入侧 information_schema 的表/列/唯一键校验、表头判定、列类型查询、`SHOW VARIABLES max_allowed_packet`、`CREATE TABLE`、`DELETE`、`SET FOREIGN_KEY_CHECKS`。**行数据语句一律不打**：攒批 INSERT/REPLACE 走 `session.go` 的 `tx.ExecContext(insertSQL, values...)`，只在失败时经既有脱敏错误路径出现；因此 debug 日志的条数只随表/分片/批次规模增长，与行数无关。③ **worker 编号**：并行导出的 `shardResult` 增 `workerID`，`shard export completed`/`shard export failed` 带 `worker` 与 `shard`，分片查询失败的错误前缀改为 `(worker N, SQL: ...)`，并行汇总补 `workers`；并行导入原有的 `thread` 字段保持不变。**实测**（本地 MariaDB 10.6.23）：100 万行倾斜表 `--threads 4 -f sql --create --log-level debug` 得到 22 条 `db interaction`（1 条统计 + 1 条列类型 + 1 条 MIN/MAX + 7 条探针 + `SHOW CREATE TABLE` 等），`worker=0` 到 `worker=3` 全部出现，汇总 `parallel export completed rows=1000000 ... workers=4 duration=4.320s`；同一条命令降到 info 级后 `db interaction` 为 0。DEL 导入 5 行到姊妹表：4 条校验类 `db interaction`、0 条攒批 INSERT、`import completed ... duration=0.002s`。**契约面**：无 flag/help/退出码/输出文件格式/env 变更，`docs/cli-oracle.txt` 无需重新基线（`## export`/`## import` diff 为空）。测试：新增 `internal/common/logsql_test.go`（3 用例：固定三位小数、info 级静默、debug 级脱敏且不含真实值）与 `test/integration/option_matrix_test.go:TestOptionMatrixDebugEchoesInteractionSQL`（info 级无 `db interaction`，debug 级有 `SHOW CREATE TABLE` 与 `information_schema`、无 `db interaction.*INSERT INTO`、有 `worker=`、至少 2 条分片完成、`duration=\d+\.\d{3}s`）；测试资产 42 个 `_test.go`、236 个 `Test*`。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过、真实库 **106 条 PASS / 0 SKIP / 0 FAIL**、`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3。 |
| 66 | `--mode replace` 这个名字指向 MySQL 的 `REPLACE INTO`，实现却是「先 DELETE 全表、再发普通 INSERT」，而 `--help` 只列了四个值、文档也只写「删除表所有数据后插入」，用户无法从名字或帮助判断它是**全量替换**还是**按键覆盖** | 用户选定 A 方案：**不改行为、只正名**。① `cmd/import_flags.go` 的 `--mode` 描述由枚举名改为语义说明：`Import mode: insert, insert_update (upsert), replace (delete all rows then plain INSERT, not REPLACE INTO), create (from SQL file)`。② README 模式表把 `replace` 改为「删除表所有数据后**普通 INSERT**（不是 MySQL 的 `REPLACE INTO`）」，并在表后补一段写明四处实质差别：文件里没有的行会被删掉、文件内重复键仍报 `1062`、`DELETE` 是导入事务之外的自动提交语句（之后任何失败都留下空表）、不触发 DELETE 触发器与 `ON DELETE CASCADE`；同时说明清空用 `DELETE` 而非 `TRUNCATE` 的原因，以及需要按键覆盖时改用 `insert_update`。③ `docs/requirements.md` 2.2.4 的表格行同步，并新增一段写明实现位置（`importer.go:225` 的 replace 分支、`deleteTable` 的 :1016、`buildBatchInsertSQL` :1071 只对 `insert_update` 追加 `ON DUPLICATE KEY UPDATE`，全仓库无一处拼 `REPLACE INTO`）、四处语义差别与行数统计依据（:547 的「表已清空故 affected == rows」）。④ `docs/cli-oracle.txt` 的 `## import` 仍是冻结快照，新增 Re-baselined note 6 记录这一行有意差异，import 侧有意 delta 由十四行改为十五行，`## export` 仍空 diff（实测 `export` 0 行、`import` 新增 15 行，其中 `--mode` 一行）。**实测**（本地 MariaDB 10.6.23，`probe2.rpl` 原有 5 行；DEL 文件 3 行）：`--mode replace --log-level debug` 退出 0，落库恰为 `1:new1,2:new2,3:new3`（原有第 4、5 行被删除，证明是全量替换而非按键覆盖），debug 日志里出现 `DELETE FROM \`rpl\`` 的 `db interaction`、`REPLACE INTO` 命中 0，汇总 `inserted=3`；同一张表再导一份含重复主键 `1` 的 2 行文件：退出 1、`Error 1062 (23000): Duplicate entry '1' for key 'PRIMARY'`，且表被留成 0 行——正是文档里写明的失败后果。**契约面**：仅帮助文本一行改写与文档，无 flag 增删、退出码、文件格式与 env 变化，行为零改动。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全过、真实库 **106 条 PASS / 0 SKIP / 0 FAIL**、`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3。 |
| 67 | 参数边界审计的七项后果全部修复：并行导入失败会 panic、默认字节预算越过驱动插值上限、交接队列没有字节上界、`--progress-interval` 允许 1ns、`--threads` 与运行环境的关系无人出声、DEL 定界符可以两两同值、显式 `--shard-key` 失败被静默降级 | 用户指令「全部修」。① **并行导入失败即崩溃**（`internal/import/importer.go:importParallel`）：`errCh` 在 `wg.Wait()` 之后关闭，而读端 goroutine 不在同一个 `wg` 里、它的错误与取消路径仍要向该通道发送，于是 worker 先报错时 `panic: send on closed channel`——实测退出码 **2**（用法类，掩盖了真实数据错误）、日志截断、其他 worker 已提交的行留在库里。读端加入同一个 `wg`（`defer wg.Done()` 与 `defer close(batchCh)` 同处一个 goroutine），通道只能在两阶段都结束后关闭。② **驱动侧 64 MiB 插值上限**（`internal/import/batchcap.go` 新增 `driverInterpolationCap`、`driverBatchBudget()` 与纯函数 `batchCeilings()`，`internal/import/session.go` 的 `replayBudget` 注释同步纠正）：go-sql-driver 在 `connection.go:330` 判定插值结果 `len(buf)+4` 超过自己的 `maxAllowedPacket`（默认 64 MiB，`const.go:17`）就返回 `driver.ErrSkip`，`database/sql` 静默退回 `COM_PREPARE`/`COM_STMT_EXECUTE`，实测同一份 80 MiB 数据 1.195 s 对 2.078 s。默认预算由 `min(服务端预算)` 改为 `min(服务端预算, 64 MiB × 9/10)`，本机 128 MB 服务端从 120,795,955 B 变为 **60,397,977 B**，记录里以 `limiting_factor=driver_packet_cap` 点名是谁定的；**显式值不改写**，`--statement-size 104857600` 仍按 104,857,600 执行并记一条 `warn`（带 `driver_ceiling_bytes=60397977`）。③ **交接队列的字节上界**（新增 `maxQueuedBatchBytes = 1 GiB`、`effectiveQueueDepth()`、`Importer.queueDepth`，`importSingle` 的 `rowCh` 与 `importParallel` 的 `batchCh` 容量都改用解析后的深度）：`--pipeline-buffer` 的单位是批数而单批字节数等于 ②的预算，实测 200,688,900 B / 100,000 行文件在深度 1024 下峰值 RSS **280,288 KB**、4.09 s，下调为 `effective=17` 后 **112,584 KB**、4.11 s（内存 −60%、速度不变），深度 1 为 39,264 KB、5.76 s，默认深度 10 为 80,480 KB、4.23 s 且永不被下调（包络取 1 GiB 正是为了让默认值落在里面）。④ **`--progress-interval` 下限**（`internal/config/config.go` 新增 `MinProgressInterval = 10ms`，`cmd/validate.go:validateProgressInterval` 两侧共用）：校验原先只要求为正，实测改动前 `1ns` 合法且一次 6,400 行导出记 **1,355 条**进度、日志 175,557 字节；现在 0 与负值报 `must be > 0`，`1ns`/`1ms`/`9ms` 报 `invalid --progress-interval 1ms: must be >= 10ms`，`10ms` 起合法（库调用方不受该下限约束，回归用例仍用 1 ms 压计时器）。⑤ **`--threads` 与运行环境**（`internal/common/detect.go` 新增 `MaxConnections`，`batchcap.go` 新增 `warnIfThreadsExceedCapacity` 与 `warnIfReplayWindowScales`，导出侧在 `internal/export/exporter.go` 的 `parallel export detected` 之后同样检查）：本机 `@@max_connections=151`，`--threads 200` 在数据移动前记 `workers exceed the server connection limit…`（`connections_needed=201`，导入结尾 `may fail the run`、导出 `may fail the export`）；`--threads 256` 记 `parallel import replay window scales with threads`（`total_bytes=2147483648`，`budgeted_total_bytes=67108864`）。两条都是告警而非拒绝——DBA 可以现场调高上限，连接真够用时运行仍然正确。⑥ **DEL 定界符同值**（`cmd/validate.go:validateDelimiters` 重写为固定顺序 char→col→row→escape 的五对冲突表，`--no-char-del`/`--no-escape` 各自免除相关项，`--escape` 与 `--char-del` 同值这一默认组合仍合法；此前遍历 map 会让同一条命令报出不同的冲突，改为切片后顺序确定）：实测改动前 `--col-del , --row-del ,` 导出 200 行得到 14,501 B、**0 个行分隔符**、402 个逗号的文件，用同样参数回读读出 401 行、**落库 0 行**（全部失败）且只报 `import aborted`；现在两侧都退出 **2** 并报 `invalid --col-del ",": it is the same as --row-del, which makes the file layout ambiguous`。⑦ **显式 `--shard-key` 不再静默降级**（`internal/export/exporter.go`：检测失败且 `ShardKey != ""` 时包成 `common.Usagef`，自动检测失败仍照旧回落单线程）：实测退出 **2** 且文案点名选项、取值与表名（`invalid --shard-key "nosuchcol" for table "doc_probe": failed to get column type for nosuchcol: …`），改动前退出 **0** 只记一条 `failed to detect shard key, falling back to single-threaded export` 并报告成功；同表不带该选项的自动检测路径仍退出 0。**测试**：新增 `internal/import/batchcap_ceiling_test.go`（5 个用例：`batchCeilings` 六组、`driverBatchBudget` 自身在限内、`effectiveQueueDepth` 七组、下调必须出声且静默路径不报、256 线程报出重放包络）、`cmd/validate_boundary_test.go`（1 个用例 × 11 组取值 × 两条命令的定界符矩阵）、`test/integration/boundary_test.go`（6 条真实库边界用例，含三次重跑的失败路径）；`cmd/cli_matrix_test.go` 的 `TestCLIProgressInterval` 与 `test/integration/live_mysql_test.go` 的两处断言按新契约改写（`limiting_factor` 由 `defaultCapFactor` 现读服务端值决定，不写死本机）。**契约面**（五面逐一）：CLI flag 与 help——import 的 `--pipeline-buffer`、`--statement-size` 与两侧的 `--progress-interval` 共三行帮助文本改写，无 flag 增删；输出文件格式——不变，只是拒绝写出无法回读的文件；退出码与错误文本——三类此前被接受且结果错误的取值改为退出 2（低于下限的间隔、同值定界符、不存在的显式分片键），并行导入数据失败由错误的 2 修正为契约里的 1；`IMEX_*` env——不变；发往服务端的语句——导入默认批次边界由 120,795,955 B 变为 60,397,977 B（同一文件实测语句条数与 `CRC32` 落库校验和不变），`--threads>1` 路径新增一条 `SELECT @@max_connections`。文档：`docs/cli-oracle.txt` 新增 Re-baselined note 7、`## export` 按新帮助重新基线（实测 diff 为空，87 行逐字节相同）、`## import` 有意 delta 由十五行改为 **十六** 行（新增 `--pipeline-buffer` 那行）；README 的导出与导入标志表、参数校验表、关联校验段、「导入攒批的硬上限与可选压低」新增一条驱动插值上限；本节 2.2.x 的进度节、日志记录表、7.4 新增第 5 条、连接加固新增「4) 交接队列与重放缓冲的内存包络」、并行导出退化表按显式分片键改写、测试资产表新增三个文件并把规模改记为 45 个 `_test.go` / 248 个 `Test*`。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全过、真实库 **112 条 PASS（45 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**、`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3。 **注：本行 ② 的 120,795,955 → 60,397,977（`limiting_factor=driver_packet_cap`）与 ③ 的 280,288 KB / 112,584 KB / 深度 17，都是在 `max_allowed_packet=128M` 的测试床上取的；变更记录 69 撤掉了 ③ 的深度下调、变更记录 70 把床按 OceanBase V4.x 默认调成 16M（默认预算 15,099,494，决定者改记 `max_allowed_packet`），那些数字只作为「那种床上会发生什么」保留。** |
| 68 | 交叉测试查出 `--shard-key` 的生效面与回显面不一致：单线程导出把用户显式给出的分片键照原样回显、既不读取也不校验 | 用户指令「交叉测试下，确认是否有缺陷」，本轮只修这一项（缺陷 B、C 见本行末尾，未动）。**现场**（同一张 5000 行 / 111,692 B 的 `x_src`，改动前二进制）：`--threads 1 --shard-key nosuchcol` 退出 **0**、产物 111,692 B，且 `export config` 回显 `shard_key=nosuchcol`；`--threads 1 --shard-key '!!!'` 同样退出 0 并回显 `shard_key=!!!`；`--threads 4 --shard-key nosuchcol -q "带 JOIN 的查询"` 记一条 `query has complex features, falling back to single-threaded export` 后退出 0；改动前 `--threads 1` 带 `nosuchcol`、带 `!!!`、以及不带该选项的三次导出产物逐字节一致（111,692 B，md5 `bc78091aa71ef39b73a65268a78fadc8`），改动后 `--threads 4 --shard-key id` 的并行产物与之一致——同表同查询下"该选项在这条路径上不改变任何东西"是实测事实而非猜测（带 JOIN 的回落那条是另一个查询、产物 46 B，不参与比对）。同一非法键在 `--threads 2` 下退出 **2**（第 67 项的 ⑦），所以缺陷不是"没校验"这么简单，而是**同一个选项的合法性取决于运行最后走了哪条路，而日志说的却是另一回事**——用户从配置记录读不出它已被丢弃。仓库里已有处理这一类问题的机制（`cmd/validate.go:parallelExportIgnoredOptions` 对并行路径上不读取的 `--batch-size`/`--pipeline-buffer` 显式给出即退出 2），`--shard-key` 是同一毛病的反方向却不在清单内。**修法选择**：本轮采用"出声"而不是"拒绝"，因为拒绝会把此前返回 0 的调用改成 2，按五面契约即不兼容变更（要把整轮从 Z+1 推到 X+1）；而出声足以消除"看起来被采纳"这一实际代价。三处改动：① `internal/export/exporter.go` 新增 `warnIfShardKeyIgnored()`，在 `exportSingle` 入口调用——它是所有单线程路由的唯一汇聚点（`--threads 1`、复杂特性回落、表名解析失败回落、检测失败回落、只切出一段），因此 `--threads 4` 配 JOIN 这种"先按并行计划、后回落"的现场也会出声，实测 `WARN single-threaded export ignores --shard-key shard_key=id threads=4 fallback=true hint="..."`；② `internal/export/configlog.go` 在写 `shard_key` 之后，仅当"写记录时就能确定用不上"（`ShardKey != ""` 且 `Threads <= 1`）追加 `shard_key_ignored=true`——回落路径此时 `threads>1` 仍看着可用，宁可不出字段也不出一条将来会被推翻的断言；③ `cmd/export_flags.go` 的 `--shard-key` 帮助文本改写为说明只有并行导出读取并校验它，`internal/config/config.go` 的字段注释同步。**测试**：新增 `internal/export/shardkey_ignore_test.go`（2 用例，五组路由取值 + 三种格式的记录字段）、`test/integration/boundary_test.go:TestBoundarySingleThreadedExportNamesAnIgnoredShardKey`（三条路由，512 行表，断言字段与退出码）；既有 `TestBoundaryExplicitShardKeyIsRefused` 与 `TestOptionMatrixTextShardKeyParallelExport` 未改一行仍通过，作为"并行路径行为不变"的证据。**契约面**（五面逐一）：CLI flag 与 help——`--shard-key` 一行帮助文本改写，无 flag 增删，此前所有合法调用仍然合法；输出文件格式——不变，实测三条路径产物 md5 与不带该选项一致；退出码与错误文本——不变（新增记录级别为 `warn`，`--log-level error` 下不出现）；`IMEX_*` env——不变；发往服务端的语句——不变，单线程路径一条查询都不多发。`docs/cli-oracle.txt` 新增 Re-baselined note 8，`## export` 按新帮助重新基线（实测 diff 为空，87 行逐字节相同），`## import` 有意 delta 仍为十六行。验证：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全过、真实库 **113 条 PASS（46 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**。**同一轮交叉测试另查出两项，本轮未修、待定级**：缺陷 B 为 1 GiB 队列包络可把深度压到低于 `--threads`（`--threads 32 --pipeline-buffer 1024` 实测 `requested=1024 effective=17`，与 README「并行导入建议 ≥ `--threads`」矛盾，下调记录不提这个冲突）；缺口 C 为导出侧队列 `make(chan rowBatch, pipelineBuffer)`（`internal/export/exporter.go:297`）仍按批数定深、没有字节包络（本机 2000 行 / 200 MB 表实测深度 1024 峰值 RSS 140,416 KB、深度 10 为 150,464 KB，限制驻留量的是数据本身而不是代码，因此只在数据量大于内存时才显现）。 **注：本行末尾两项（缺陷 B、缺口 C）已由变更记录 69 按用户「不要做转换，要根据用户输入来」的指令处置完毕。** 同一轮末尾缺口 C 引的 140,416 KB / 150,464 KB 也取自那张 128 MB 床（变更记录 70 之后本床为 16M）。
| 69 | 用户指令「我的理解，thread 是线程，控制的是并发。buffer 控制的是队列深度。两者并不矛盾，**不要做转换，要根据用户输入来。其他参数也是类似**」——上一轮为控制内存而把 `--pipeline-buffer` 按 1 GiB 包络换算成另一个深度（实测 `requested=1024 effective=17`），并把 `--threads` 与深度之间的关系写进文档建议；同时审计其余参数是否也有同类换算 | 原则定为「**协议与服务端事实保留，用户选项之间的换算撤掉**」，并先核实拓扑再下结论：并行导入只有一个读 goroutine（`internal/import/importer.go:847`）填**一条全部 worker 共享的** `batchCh`（容量 = `imp.queueDepth`，:835），worker 只消费（:949）。深度决定「读侧可以跑在写侧前面多远」，线程决定「同时有多少写在跑」，二者正交；换算等于替用户改他刚写下的数字。**① 撤队列夹紧**：删 `effectiveQueueDepth()`，新增纯函数 `queuedBytes(depth, maxBatchBytes)`（`depth<=0` 或预算 `<=0` 返回 0，乘法饱和到 `math.MaxInt64` 而非溢出为负），`resolveQueueDepth` 取配置值建队列，仅当 `深度 × batch_bytes > 1 GiB` 时在任何数据移动之前记 `warn`（`the hand-off queue may retain more than the memory envelope, importing at the configured depth`，字段 `requested`/`batch_bytes`/`queued_bytes`/`envelope_bytes`/`hint`）。`maxQueuedBatchBytes` 的注释改写为「队列只是按它报告，深度不受它影响」，并把上一轮那组单点 RSS 换成"去掉夹紧后重复多轮为 147,340–292,356 KB（深度 1024）/ 126,972–207,392 KB（深度 10），故不给单点值"，`importer.go` 三处结构体与通道注释同步。库调用方的负深度钳到 `0`（同步交接），不再让 `make(chan rowBatch, -n)` 崩溃——这是本轮唯一保留的"改写"，因为负数无法作为队列存在。**② 不加线程下限**：曾拟「深度不得低于 `--threads`」，那是同一毛病的反方向（用一个选项换算另一个），按指令不实现；文档里那句「并行导入建议 ≥ `--threads`」随下调逻辑一并撤掉。**③ 撤字节预算的 64 KiB 下限**（`batchByteLimit` 现为纯 `packet × 9 / 10`）：审计其余参数时发现的唯一一条真正的反向换算——下限会把预算抬到高于服务端刚说出的上限。实测（本机 `SET GLOBAL max_allowed_packet=40000` → 生效 39,936，5,000 行 / 111,692 B 的 DEL 文件，`--batch-size 20480` 让字节而非行数决定批次，与改动前二进制背靠背）：旧实现 `batch_bytes=65536` → 退出 **1**、`Error 1153 (08S01): Got a packet bigger than 'max_allowed_packet' bytes`、`failed=2870`、**0 行落库**，且记录把决定者写成 `limiting_factor=max_allowed_packet`；改后 `batch_bytes=35942` → 退出 **0**、5,000 行全部落库。`applyStatementCap` 的注释同步去掉下限。**④ 其余上限分类结论**：`max_allowed_packet × 9/10`（服务端事实）、`maxPlaceholders = 65535`（协议 2 字节参数计数，压的是行数并出声）、驱动 64 MiB 插值上限（只钳**默认**预算，显式值原样执行并记 `driver_ceiling_bytes` 的 warn）、并行导出 `min(2×threads, 64)` 段数计划与按段均分的 64 MiB 前瞻/重放预算（工具内部规划量，不读用户的 `--batch-size`/`--pipeline-buffer`）——都不是把用户甲选项换算成乙选项，保留。**⑤ 缺口 C 按同一原则不改代码**：导出侧交接队列 `internal/export/exporter.go:319`（变更记录 68 记的是当时的 :297）仍按批数定深、无字节包络，即"按给定值执行"，只补文档说明。**⑥ 实测**（`/tmp/imex-gate`，本机 MariaDB 10.6.23，`max_allowed_packet=128M` → `batch_bytes=60397977`）：`--threads 32 --pipeline-buffer 1024` 导 5,000 行退出 0、记录回显 `pipeline_buffer=1024`、`lowering` 命中 0、报价记录在场；200,688,900 B / 100,000 行文件按深度 1024 重复多轮峰值 RSS 147,340–292,356 KB、4.0–8.1 s，深度 10 为 126,972–207,392 KB——两组重叠、抖动来自这台 4 核 / 8 GB 沙箱，故本行不给单点值，上一轮的 280,288 KB / 112,584 KB 对照随下调逻辑失效、不再引用。**⑦ 测试**：`internal/import/batchcap_ceiling_test.go` 的 `TestEffectiveQueueDepth` → `TestQueuedBytesIsThePriceOfTheDepth`（7 组，含 `1024 × MaxInt64 → MaxInt64` 饱和与负深度/零预算返回 0）、`TestResolveQueueDepthReportsReduction` → `TestResolveQueueDepthHonoursTheDepthAndReportsItsPrice`（断 `imp.queueDepth == 1024` 与六个字段在场、默认深度静默、`PipelineBuffer = -1 → 0`）；`internal/import/batchcap_test.go` 的 `TestBatchByteLimit` 换掉两条下限用例、钉住 `65536 → 58982` 与 `1024 → 921`（注释保留 39,936 现场），`TestApplyStatementCap` 子用例改名「远低于预算的显式上限仍然生效」；`test/integration/boundary_test.go` 的 `TestBoundaryPipelineBufferIsLoweredToBoundQueuedBytes` → `TestBoundaryPipelineBufferIsHonouredAndPriced`（`pipeline_buffer=1024` 回显 + `lowering` 缺席 + 用新助手 `logField` 从本次运行读回 `batch_bytes` 再断 `queued_bytes`，小服务端预算时反向断言不得出声）。测试资产 46 个 `_test.go` / 251 个 `Test*`（另有 1 个 `TestMain`）、6 个 `Fuzz*`、12 个 `Benchmark*`，本轮改名 4 个用例、净增 0 个文件。**⑧ 契约面**（五面逐一）：CLI flag 与 help——仅 import 的 `--pipeline-buffer` 一行帮助文本改写（`Hand-off queue depth, in batches (0 = synchronous hand-off; a depth whose batches could exceed 1 GiB of retained rows is imported as given and reported)`），无 flag 增删；输出文件格式——不变；退出码与错误文本——不变（本轮把此前被静默压低的组合改为照给定值运行，若该深度真的耗尽内存，结果是 OOM 而非新的退出码，这一点由报价记录提前说明）；`IMEX_*` env——不变；发往服务端的语句——批次边界本身不变（仍由服务端与驱动定），变化在于用户写下的大深度现在真的被执行，以及服务端上限低于 72,817 B 的库上不再发出高于其上限的语句。**⑨ 文档**：`docs/cli-oracle.txt` 的 `## export` diff 实测逐字节为空（87 行）、`## import` 有意 delta 实测仍为 16 行（`--pipeline-buffer` 那行内容更新、行数不变），新增 Re-baselined note 9 并声明 note 7 对该行的描述已被取代；README 的导入标志表、参数校验表、流水线与内存控制段新增「1 GiB 是报价线不是夹紧值」、攒批上限段去掉下限并给出 1153 现场、变更记录新增本行对应的行并给两条历史记录各加一条「已被 69 取代」的注（历史记录原文不回改）。**⑩ 定级**：Z+1（纯缺陷修复）——没有任何此前正确的调用被拒绝或改写，被恢复的正是用户显式写下的数字；版本号第三位进位（`0.2.1 → 0.2.2`），tag 与提交待用户明示。验证：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过（末次实跑：`github.com/imex/cmd` 31.056 s、`github.com/imex/internal/common` 42.716 s，后者因 PBKDF2 用例每次真跑 600000 次派生，本轮四次实跑落在 42.7–43.1 s 与历史 56.5 s 之间，取哪一次都不影响结论），真实库 **113 条 PASS（46 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**（`go test -v` 实数，非推断），`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3，`## export` diff 为空（87 行逐字节相同）、`## import` 有意 delta 实测 16 行。 **注：本行 ⑥ 的 `batch_bytes=60397977`、`queued_bytes=61847528448` 与 RSS 区间 147,340–292,356 KB / 126,972–207,392 KB 是在 `max_allowed_packet=128M` 的床上取的；变更记录 70 把床按 OceanBase V4.x 默认调成 16M 后重测为 `batch_bytes=15099494`、`queued_bytes=15461881856`、深度 1024 为 76,616–152,040 KB、深度 10 为 122,740–149,412 KB，报价线随之从深度 10 一档移到 71/72。** **落地记录（2026-10-28）：本行与变更记录 70 一并落 commit `9f0d81e`（16 files changed 之外另含 4 个新增测试文件），版本号在其后的 release 提交 `90c3cf3` 由 `0.2.1` 升至 `0.2.2`，附注标签 `v0.2.2` 落在该 release 提交并已 push（`origin/main` = `90c3cf3`，`origin` 新增 tag `v0.2.2`）；打包未做，等用户明示。** |
| 70 | 测试床的 `max_allowed_packet=128M` 是这台 4 核 / 8 GB 沙箱自己的值，OceanBase 租户从来不是这个数：官方文档里 V2.1.x/V2.2.x/V3.1.x/V3.2.x 的租户变量默认值为 4,194,304，V4.x 的调优文档写 16,777,216，诊断文自己也承认「有的版本为 4M，有的版本为 16M」。用 128 MB 取的那组数——`batch_bytes=120795955` 钳到 `60397977`、`limiting_factor=driver_packet_cap`、`--statement-size 1024` 得 342 条语句、深度 1024 峰值 RSS 最高 292,356 KB——拿到真实 OB 租户上一条都不成立，而 README、本文档与 `docs/cli-oracle.txt` 里引用它们的句子有十几处 | 按用户指令「b 16m」：**代码逻辑一行不改，把测试床按 OceanBase V4.x 默认设成 `max_allowed_packet=16M` 并重新实测，正文数字全部换成 OB 租户上真正会出现的那一组**。① **床**：`/tmp/mysqld/my.cnf` 改 `max_allowed_packet=16M` 后重启，`SELECT @@global.max_allowed_packet` = **16777216**，`@@max_connections` = **151**（后者本来就与 OB 默认不同，属环境事实，只在文档里点名）。② **预算与决定者**：默认解析记录变成 `batch_bytes=15099494 limiting_factor=max_allowed_packet`——驱动侧 64 MiB 插值上限在这个床上**根本轮不到**。为了让「谁在决定」不依赖某一台机器，补了两条边界实测：服务端 67,108,864 B（恰为 64 MiB，9/10 预算 = 60,397,977）时仍记 `max_allowed_packet`，67,109,888 B 起才改记 `driver_packet_cap`；据此把 README 标志表、参数校验表、7.4 第 5 条与选项表里所有「默认预算钳到 60,397,977」的说法统一改写为「只有服务端值**大于** 67,108,864 B 时才轮到它」，128 MB 床上那组数字降级为「那种床上会发生什么」保留。③ **报价线**：`⌊1 GiB / 15,099,494⌋ = 71`，逐档实测**深度 71 静默、72 起出声**（`queued_bytes=1087163568`）；深度 1024 记 `requested=1024 batch_bytes=15099494 queued_bytes=15461881856 envelope_bytes=1073741824`；默认深度 10 的名义驻留从 576 MiB 降到 **144 MiB**，仍在 1 GiB 之内，所以「默认值落在包络里」这条设计意图在新床上依然成立。④ **内存**：200,688,900 B / 100,000 行 DEL 文件、`--threads 32`、`/usr/bin/time -v -o` 取峰值 RSS，各重复三轮——深度 1024 为 **76,616–152,040 KB**（3.78–5.07 s），深度 10 为 **122,740–149,412 KB**（4.45–4.83 s），两组重叠。结论比 128 MB 床更清楚：预算收窄到 15 MB 之后单个批次就小了，读侧供不满那么深的队列，**深度不再是峰值内存的主要变量**，驻留量由数据本身与 worker 数量决定。旧对照（280,288 KB、下调后 112,584 KB、撤下调后的 147,340–292,356 KB）全部标注为 128 MB 床的历史测量并停止引用。⑤ **`--statement-size` 矩阵重测**（新 fixture 424,882 B / 2,048 行 × 约 207 B，`--batch-size 20480` 使行数不触顶，以目标库 `Com_insert` 全局计数差计条数）：不加 **1** 条、`65536` **7** 条（`--threads 4`、SQL 输入、Parquet 输入各 7 条）、`1024` **410** 条、`268435456`（高于预算）**1** 条且 `limiting_factor=max_allowed_packet`；四种情形落库 2,048 行、`CRC32` 和 **4463837020299** 全部相同。342 → 410 的差来自行宽（每批 6 行 vs 5 行）而不是床，正文写明。⑥ **真实库用例**：`TestBoundaryExplicitStatementSizeAboveCeilingIsHonouredAndReported` 在 16M 床上必然跳过（它要求服务端预算容得下 100 MiB 这个显式值），而 1 SKIP 会让「全绿」失去意义，故改写为 `TestBoundaryExplicitStatementSizeIsLoweredOnly`——第一条腿跑「预算内的显式值原样生效且不出驱动告警」（`packetBudget/4`，本床 3,774,873），第二条腿跑「CLI 接受的最大显式值 268435456 绝不高抬预算」，第二条按本床实际决定者分三支断言（`packetBudget > 268435456` 走 `statement-size`、`> 60397977` 走 `driver_packet_cap` 且不报错、其余走 `max_allowed_packet`）：OB 默认床走第三支、128 MB 床走第二支、更大的床走第一支，谁都不必 skip；两条腿用两张表（第二条腿此前撞 `1062 Duplicate entry '1'`），断言值全部由 `SELECT @@max_allowed_packet` 现读，不写死本机。驱动告警的检索由新助手 `warnNeedle` 统一取第一行。⑦ **文档与注释**：README 的导入标志表（`--statement-size`、`--pipeline-buffer` 两行）、流水线与内存控制段（1 GiB 报价线一档）、攒批上限段（驱动钳制阈值）、参数校验表、修复日志新增本行对应的行并给三条历史记录各加一条「数字取自 128 MB 床」的注（历史记录原文不回改）；本文档 2.2.x 选项表、日志记录表三行、7.4 第 2)/4)/5) 条、7.4 交接队列档、测试资产表两行按新床改写；`docs/cli-oracle.txt` 新增 Re-baselined note 10 说明本轮只换测量环境、`## export` diff 实测仍逐字节为空（87 行）、`## import` 有意 delta 实测仍 16 行（**本轮没有任何帮助文本变化**），并注明 note 9 引的记录字段取自 128 MB 床。代码注释同步：`internal/import/batchcap.go` 的 `maxQueuedBatchBytes` 包络注释、`internal/import/batchcap_ceiling_test.go` 与 `test/integration/boundary_test.go` 里引用本机 RSS 的注释按新床重写并标明来源床。⑧ **兜底常量不动**：`defaultMaxAllowedPacket = 4 MiB` 保持原值——它恰等于 OB V2/V3 的租户默认值，是「读不到 `max_allowed_packet` 时」最保守的那个假设；而且 `common.DetectServerType` 走的正是同一条 `SHOW VARIABLES`（`internal/common/detect.go:49`），被屏蔽时工具无从知道对面是哪个 OB 版本，故该常量做成版本感知没有信息来源。⑨ **契约面**（五面逐一）：CLI flag 与 help——**不变**；输出文件格式——不变；退出码与错误文本——不变；`IMEX_*` env——不变；发往服务端的语句——**不变**（`max_allowed_packet` 从来不是 imex 的选项，本轮改的是测试床里服务端的取值以及文档引用它的数字）。⑩ **定级**：Z+1（纯缺陷修复档内的一次重测与文档纠偏）——没有任何调用面的形状发生变化，被修正的是「把沙箱值当成 OB 值」这个错误陈述；版本号第三位进位仍为 `0.2.1 → 0.2.2`，tag、提交、打包均待用户明示。验证（全部在 16M 床上重跑）：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `CGO_ENABLED=1 go test -race -count=1 ./...` 全通过（`github.com/imex/cmd` 31.072 s、`github.com/imex/internal/common` 54.353 s），真实库 `go test -v -race -count=1 ./test/integration` **113 条 PASS（46 顶层 + 67 子用例）/ 0 SKIP / 0 FAIL**（`grep -c '^--- PASS'` = 46、`grep -cE '^\s+--- PASS'` = 67 实数），`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3，`## export` diff 为空、`## import` 有意 delta 16 行。 **落地记录（2026-10-28）：本行的文档与代码注释随变更记录 67-70 一起落 commit `9f0d81e`，`Makefile` 首行在 release 提交 `90c3cf3` 改为 `VERSION ?= 0.2.2`，附注 tag `v0.2.2` 随主线一起 push；提交前末次实跑 `gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净，七包 `-race` 全通过、真实库 113 条 PASS / 0 SKIP / 0 FAIL 见本行末尾与本表 69 行末；打包未做。** |
| 71 | 开局建连的那次 `db.Ping()` 不看错误性质：`NewExporter` 与 `NewImporter` 各写一遍 `for i := 0; i < 3 { Ping; WARN; sleep(i+1 秒) }`，于是服务端自己判定的永久拒绝——`1045` 凭据错、`1049` 库不存在、`1193` `--session-var` 写了服务端不认识的变量——也要跑满三次尝试、固定等 **6.0 s** 才失败，且末条文案 `failed to ping database after 3 attempts` 把服务端给的真实原因挤到句尾（实测四种失败各 3 条 retrying 告警，最后一次失败之后还要白睡 3 s）。变更记录 42 当时把这一项登记为「待用户定级的可用性问题」，而 `internal/common/dberr.go` 的注释早就写明 1045 属「credentials will not heal themselves」、明确排除在瞬态之外——**同一仓库里两套判据互相矛盾** | 按用户指令「可以。永久错误了不要重试」修复。① **判据同源**：新增 `internal/common/ping.go` 的 `common.PingWithRetry(db)`，重试与否交给语句路径一直在用的 `common.IsTransient`，退避沿用 `DefaultRetryPolicy()`（1s→2s、上限 4s、20% 向下抖动），两处构造器改为一行调用，重复的手写循环随之删除。② **永久拒绝一次即返回**：非瞬态错误不排定任何重试，实测三种拒绝均为 5–6 ms。③ **文案讲真话**：`failed to ping database after %d %s` 里的次数是**实际尝试次数**（单数写 `attempt`），所以瞬态耗尽那条仍是 `after 3 attempts`（对外可见文本一字不变），永久拒绝那条改为 `after 1 attempt`，服务端原因重新成为句子的主角。④ **告警不再撒谎**：`database ping failed, retrying` 只在重试真的被排定时打印，因此三次尝试配两条告警，第三次失败由末条 ERROR 独自交代；永久拒绝则一条都不打。⑤ **实测**（床：本机 MariaDB 10.6.23、4 核 / 8 GB、`@@global.max_allowed_packet=16777216`、`@@max_connections=151`，2026-10-28）——改前（`/tmp/perf_reg4.log`、`/tmp/perf_reg5.out`）：1045 / 1049 / 1193 / 端口 3399 无人听 **四种全部 6.0 s、各 3 条 retrying 告警**；改后：三种永久拒绝 **5 / 6 / 6 ms、0 条告警、`after 1 attempt`、退出码仍 1**，端口无人听 **2.675 s、2 条告警、`after 3 attempts`**（1s+2s 退避，不再多睡 3 s）；另测「服务端已停、1.5 s 后拉起」这一窗口内启动 111,692 B / 5,000 行导入（`--threads 2`），第三次尝试连上、**退出 0、落库 5,000 行、2.688 s**，证明改的只是分类而不是重试能力。⑥ **测试**：`internal/common/ping_test.go`（新增 5 个 `Test*`，进程内假连接器 `pingFake` 按脚本回答 ping 并计数——故意不用 `driver.ErrBadConn`，因为 `database/sql` 会对它自行重试而模糊本次要钉的次数）钉住四类永久拒绝（1045/1049/1193/1064）各只发一次 ping 且无告警、成功路径一次即过、瞬态两次重试后恢复（告警数 == 重试数）、耗尽为上界三次两条告警、`MaxAttempts=0` 的退化上界仍诚实计数；`test/integration/option_matrix_test.go` 新增 `TestOptionMatrixConnectionRejectionIsNotRetried`（真实二进制、export 与 import 两条命令各用错密码跑一遍：退出码必须为 **1**（运行时错误而非用法错误）、日志无 retrying、末条含 `failed to ping database after 1 attempt` 与 `Error 1045`、整跑 < 3 s、导出侧不得留下输出文件），把「永久错误不重试」与「退出码归类」同时钉在 CLI 面上。⑦ **文档**：2.3 的连接失败实测样例按两类重画（永久拒绝 3 行 + 瞬态 4 行，各附实测耗时，并给出「退避窗口内服务端起来则整跑成功」一段）；5.3 之后那节（配置回显）里「密码写错那次文件 6 行」改为实测 **3 行**；7.7 的瞬态重试小节新增「开局建连的 ping 走同一判据」一条，写明改前 6.0 s 与改后两类的数字；10 节场景表新增「开局建连失败」一行（原「连接重连」行照旧）；11.1 计数 46 个 `_test.go` / 251 个 `Test*` → **47 / 257**，`internal/common` 行 9 个文件 41 → **10 个文件 46**、`option_matrix_test.go` 15 → **16**，两行各补本行覆盖点；十二节源码包计数 42 → **47**（与本表 11.1 同源）；11.2「长时间稳定性 / 断连恢复」行补上本轮已自动化到哪一层、仍未覆盖什么；变更记录 42 与 46 各在尾部加一条「本行数字已被本行推翻」的注，原文不回改；README 的「云环境连接加固」瞬断条目补写同一判据，并新增修复记录一行。⑧ **刻意不动的三处**：并行导出**分片连接**那次 `db.Ping()`（`exporter.go` 的 shard 路径）本来就只有单次尝试、不重试，无需改；`PingWithRetry` 仍**不**接受 `context`——两个构造器签名里没有 ctx（CLI 是在构造器返回之后才建 ctx 的），为加它而改导出/导入构造器的签名属另一档改动，故退避期间的 SIGINT 仍打不断，这一点连同「真实 LB/NAT 长时中断」一起留在 11.2；`dberr.go` 的瞬态清单一字未动（本轮是让 ping 服从它，不是改它）。⑨ **五面契约**：① CLI flag 与 help——**不变**（实测本轮 `## export` 与实机帮助 diff 逐字节为空、76 行，`## import` 有意差异仍 16 行、`## root` 仍 2 行，与变更记录 70 记录的口径一致，无需重新基线）；② 输出文件格式——不变；③ 退出码——**不变**（永久拒绝与瞬态耗尽都仍是 1），错误文本有两处可见变化：永久拒绝路径的 `after 3 attempts` → `after 1 attempt`、以及 `database ping failed, retrying` 告警在永久路径消失 / 在瞬态路径由 3 条变 2 条；④ `IMEX_*` 环境变量——不变；⑤ 发往服务端的语句——**成功路径与全部数据语句一字不变**，仅失败路径少发那两次注定无用的重连握手。⑩ **定级**：**Z+1**（纯缺陷修复档）——它不新增能力、不动文件格式与环境变量、不改任何退出码，被改的是「对同一份错误该不该重试」这一实现自相矛盾之处；若你判定面③那两句文本的变化（`after 3 attempts` 的字面出现次数、retrying 告警条数）算对外契约，则应升 **Y+1**，版本号第三位进位与 tag 均待你明示。**验证（全部在 16M 床上实跑）：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七包全通过（`github.com/imex/internal/common` 46.938 s 为 PBKDF2 用例真跑 600000 次派生，其余六包 1.017–3.545 s），真实库 `IMEX_TEST_DSN=... CGO_ENABLED=1 go test -race -count=1 -v ./test/integration` **116 条 PASS（47 顶层 + 69 子用例）/ 0 SKIP / 0 FAIL**（5.986 s，基线 113 = 46 + 67，本行 +1 顶层 +2 子用例），死规矩复核 `grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**；`make build` 路径本轮未重跑（版本号仍是 `0.2.2`，未提交、未打 tag、未打包） |
| 72 | SQL 产物的 `LOCK TABLES … WRITE;` 写在 `CREATE TABLE` 之前，导致产物在标准 `mysql` 客户端里**必然回放失败**；两次登记（变更 47、README 修复记录）都停在「待用户定级」 | 按用户指令「SQL 产物的 LOCK TABLES 一类的东西不要再生成了」——不是调顺序，而是**整体不再生成**这一对语句。① 代码：`internal/export/sql_writer.go` 删掉 `WriteHeader` 里的锁块与 `WriteFooter` 里的解锁块，随之删除只为此存在的 `skipTableLocks` 字段与 `SetSkipTableLocks()` 方法；`serverType` 字段的唯一用途就是「MySQL 才加锁」，故 `NewSQLWriter` 去掉该参数，`exporter.go` 的 `exportShard`/`writeShardStream` 参数链、以及两处 `common.DetectServerType(e.db)` 调用一并删除。② **实测**（本机 MariaDB 10.6.23，`imex_test.lockfix` 3 行；旧二进制 = 变更 71 的构建，新二进制 = 本轮工作区）：旧产物 455 B、首行 SET NAMES、**第 2 行 `LOCK TABLES`**、末行 `UNLOCK TABLES;`；新产物 411 B、无锁语句，其余**逐字节相同**，差值恰为两条语句的 44 B。标准 `mysql` 客户端回放到空库：**旧 exit 1**（`ERROR 1146 (42S02) at line 2: Table 'imex_lock_old.lockfix' doesn't exist`，0 张表），**新 exit 0**、3 行落库；imex 自身 `import -f sql --mode create` 回灌新产物 exit 0、3 行。仅数据导出（不带 `--create`）为 SET NAMES 后直接 INSERT；并行 `--threads 2` 实测 4 条 `shard export completed`（确认确实走了并行路径），产物 0 条锁语句、内容与单线程逐字节相同。③ **测试**：`TestSQLWriter_SkipTableLocks` 改写为 `TestSQLWriterWritesNoTableLocks`（表驱动 `create=false` / `create=true`，断言产物不含 LOCK/UNLOCK 且 SET NAMES 与 INSERT 存活）；`TestWriteShardStream_SQLWritesOneLockPair` 改写为 `TestWriteShardStreamSQLHasNoTableLocks`（断言 0 条锁、恰 1 条 SET NAMES、分片顺序不变）；删除 `TestWriteShardStream_DataOnlyStillLocks` 与 `TestWriteShardStream_OceanBaseSkipsLocks`——它们守卫的行为已不存在，「任何服务端都不写锁」由前一条覆盖。`internal/export` 88 → **86**，全仓库 257 → **255** 个 `Test*`，`_test.go` 仍 47 个。④ **文档**：2.1.6 的 SQL 文件结构改写并点名 `ERROR 1146`、5.2 两份样例去掉两行锁语句、特点两条重写（旧产物仍可导入）、六节兼容表改为「导出一律不生成」、7.2 并行导出流程去掉锁相关两条；变更记录 5 追加注说明其描述的机制已删除；README 同步。⑤ **五面契约**：① CLI flag 与 help——**不变**（实测 `## export` 与实机帮助 diff **逐字节为空**，87 行；`## import` 有意差异仍 16 行、`## root` 仍 2 行）；② 输出文件格式——**变**：SQL 产物少两条语句（每文件 44 B 级别）；③ 退出码——不变（成功导出与各失败路径都不依赖这两条语句），错误文本不变；④ `IMEX_*` 环境变量——不变；⑤ 发往服务端的语句——**少一条** `SHOW VARIABLES LIKE 'version_comment'`（原先每次导出在建连后无条件执行一次，只为决定要不要加锁），同时消失的还有 `DetectServerType` 那条永远为 nil 的 error 分支。⑥ **刻意不动**：导入侧「跳过 SET/LOCK/UNLOCK 前导语句」的逻辑**保留**，因此旧版本产出的文件照常可以导入；`internal/common/detect.go` 的 `ServerType`/`DetectServerType` 目前**已无调用者**，保留的理由是 OB/E5 路线图（按 tablet 统计分片）迟早要用同一份 `version_comment` 判据，若下次审计判定为死代码可直接删除。⑦ **定级**：按语义是**缺陷修复**（产物此前对任何标准工具都不可用），实质影响只在面②，建议 **Z+1**；若把「SQL 产物字节变化」严格算作面②的契约破坏，则应升 **Y+1**——与变更 71 的定级一并由你裁决。**验证**：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七包全通过（`internal/common` 46.570 s、`internal/import` 3.830 s、`internal/export` 1.623 s），真实库 `./test/integration` **116 条 PASS（47 顶层 + 69 子用例）/ 0 SKIP / 0 FAIL**（6.424 s），`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**；未提交、未打 tag、未打包。 **注：本行 ⑥ 保留的 `ServerType`/`DetectServerType` 等死符号已由变更记录 73 删除，`internal/common/detect.go` 现只剩 `MaxConnections`。** |
| 73 | 用户给出三条评审决定——「A-7 删掉这三个死符号 / A-4 掉线重试窗口 只写量化文档 / A-5 改成 flag 优先」：① `internal/common/detect.go` 的 `ServerType`/`ServerMySQL`/`ServerOceanBase`/`String()`/`DetectServerType()` 自变更 72 删掉锁语句后已无任何调用者，是纯死代码；② 掉线重试的「能挺过多长停机」只在正文零散写着 1s→2s 退避，没有给出可引用的窗口量值；③ `IMEX_LOG_FORMAT` 会**无条件覆盖**显式 `--log-format`，即命令行写下的值可被一个环境变量悄悄否决，与其余「flag OR env」开关的语义不一致 | 三条一并落地，代码、测试、文档各自有实测。**① A-7 删死符号**：`internal/common/detect.go` 由 61 行降到 25 行，删除 `ServerType` 类型、`ServerMySQL`/`ServerOceanBase` 两个常量、`String()` 方法与 `DetectServerType()`（后者含 `SHOW VARIABLES LIKE 'version_comment'` 及只为它存在的 `strings` 导入）；用户说的是「三个死符号」，实际连带方法与常量共五个名字、36 行，因为删定义不影响任何调用者——全仓 `*.go` 对这五个名字的命中实测为 **0**。**行为零变化有背靠背证据**：与变更 72 的构建（`/tmp/imex-lockfix`）对同一张 3 行表 `a7probe` 各导出四种组合（DEL/SQL × `--threads 1/2`），SQL 产物 **419 B、md5 `689be3eda0fb…`**、DEL 产物 **41 B、md5 `172d4645512a…`**，两个二进制逐字节相同、四次退出码均为 0；`--log-level debug` 的运行日志里 `version_comment` 命中 **0**（这条查询自变更 72 起就不再发出，本轮删的是无人调用的定义）。**② A-4 只写量化文档、零代码改动**：7.7 的瞬态重试小节写明窗口是**有量值的**——退避 1s→2s、上限 4s、20% 向下抖动，实测一次 `-P 3399` 无人听的整跑在 **2.675 s** 放弃，故能挺过的停机窗口约 **2.4–3 秒**；同段给出边界实测：服务端停 1.5 s 后拉起，同时启动的一次 10 万行并行导入 exit 0、8 条 `database ping failed, retrying`、最高只到 attempt 2、落库 10 万行且不重不漏；并明确**不做成命令行选项**（窗口由退避策略决定，做成选项等于把「重试几次」交给用户猜）。11.2 新增一条**未测**边界：`COMMIT` 的 ack 丢失——「服务端已提交、客户端没收到响应」这一歧义窗口没有用例，重放是否会产生重复行取决于导入模式，留给后续。**③ A-5 改成 flag 优先**：`cmd/root.go` 的 `PersistentPreRunE` 改为仅当 `!cmd.Flags().Changed("log-format")` 时才读 `IMEX_LOG_FORMAT`（并 `TrimSpace`），注释点明区别——布尔开关（`IMEX_QUIET`/`IMEX_NO_SQL_TAG`/`IMEX_ERROR_STACK`/`IMEX_ECHO_SQL_VALUES`）只能是「flag OR env」，因为它们只负责把行为打开；带值的 `--log-format` 必须让命令行能选，否则改配置只能靠改环境。**④ 实测矩阵（8 次运行，错口令触发 1045 永久拒绝，均在 5–6 ms 内退出 1、3 行日志）**：旧二进制 `IMEX_LOG_FORMAT=java` + `--log-format text` → **java 布局**（env 压过 flag）、`IMEX_LOG_FORMAT=text` + `--log-format java` → **text 布局**（同样被压过）；新二进制同两组分别为 **text**、**java**（flag 胜出）；两侧「只给 env」都取 env、「只给 flag」都取 flag，证明 env 仍是没有显式选项时的默认值，只失去了否决权。**⑤ 测试**：`cmd/cli_matrix_test.go` 新增 `TestCLILogFormatFlagBeatsEnv`（四个子用例：都不给保持内置 `java`、只给 env、只给 flag、两者同时给出 flag 胜出），断言落在 `common.LogFormat` 而不是日志文本上；argv 末尾加 `--progress-interval 1ms` 让参数校验先于建连短路，因此该用例 **0.00 s** 完成、不再依赖 mysqld 是否存活（此前同类用例在库挂掉时会先等一轮退避）。测试资产 255 → **256 个 `Test*`**，`cmd/cli_matrix_test.go` 11 → **12**，`_test.go` 仍 47 个。**⑥ 刻意不动**：`--log-format` 的帮助文本一字未改，故 `docs/cli-oracle.txt` 无需重新基线（实测 `## export` help 侧新增 0 行 87 行逐字节相同、`## import` 有意差异仍 16 行、`## root` 仍 2 行）；其余布尔类 `IMEX_*` 的「flag OR env」语义保持不变——它们没有「命令行否定环境」的表达需求，改了反而是另一档行为变更。**⑦ 文档**：3.3.2 新增优先级段（env 是默认值、不是否决）、6.1 由「自动检测」改写为「不做服务端类型探测」（说明探测的唯一消费者随锁语句一起消失，将来做 OB 专用路径时再引入）、项目结构两行（`detect.go` 改为 `max_connections` 读取、`sql_writer.go` 去掉已不存在的「LOCK 抑制」）、7.7 的量化窗口、11.2 的 ack 丢失行、11.1 与十二节计数；README 的特性行（OceanBase「自动检测并适配」改为共用同一路径）、运行时 SQL 归因清单删去 `SHOW VARIABLES version_comment`、项目结构、参数表的 `--log-format` 行；变更记录 72 追加一条注说明其保留的死符号已被本行删除（历史原文不回改）。**⑧ 五面契约（逐面）**：① CLI flag 与 help——**不变**（实测 0/16/2，未重新基线）；② 输出文件格式——**不变**（419 B / 41 B 逐字节相同）；③ 退出码与错误文本——退出码不变，文本不变；唯一可见差异是当 env 与 flag 同时给出且不同时，日志**布局**此前由 env 决定、现在由 flag 决定；④ `IMEX_*` 环境变量——**变**：`IMEX_LOG_FORMAT` 从「无条件覆盖」降为「未显式给出该选项时的默认值」，既有脚本若依赖 env 压过命令行，日志布局会翻转（记录集合、字段与退出码不变）；⑤ 发往服务端的语句——**不变**（`version_comment` 自变更 72 起已停发，本轮 debug 日志检索命中 0；A-7 删的是无人调用的定义）。**⑨ 定级**：A-7 属死代码清理（无外部可见变化，Z 档内）；A-4 只补文档（无契约变化）；A-5 落在面④且**是破坏性变化**——按本仓库「X+1 = 不兼容」的口径应为 **X+1**，与用户此前对 A-2 的「纯缺陷修复 = Z+1」不能同时成立：A-2+A-5 若合并发布，版本号应为 **1.0.0** 而非 0.2.3。`Makefile` 首行仍是 `VERSION ?= 0.2.2`，未提交、未打 tag、未打包，等用户裁决版本号。**⑩ 验证（本轮实跑）**：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七包全通过（`internal/common` 55.165 s 为 PBKDF2 用例真跑 600000 次派生，`internal/import` 4.611 s、`internal/export` 1.650 s、根包 1.186 s、`cmd` 1.064 s、`internal/config` 1.015 s、`test/integration` 1.043 s），真实库 `IMEX_TEST_DSN=... go test -race -count=1 -v ./test/integration` **116 条 PASS（47 顶层 + 69 子用例）/ 0 SKIP / 0 FAIL**（7.238 s），`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**，帮助侧 delta 实测 export **0**（87 行）/ import **16** / root **2**。**用户裁决（2026-10-28）：按 Y 档处理**——版本号第二位 +1、第三位归零，`0.2.2 → 0.3.0`，`Makefile` 首行在同一次提交内改为 `VERSION ?= 0.3.0`，README 修复记录章节标题同步为 v0.3.0。采纳理由：A-5 的破坏面只在「环境变量与命令行同时给出且取值冲突」时可见，文件格式、退出码、错误文本与发往服务端的语句都不变；A-7 与 A-4 无外部可见变化。故按「兼容的行为变更 = Y 档」落地，而非本行 ⑨ 按字面推得的 X 档。**同日回退（2026-10-28，尚未提交前）：用户裁定 `--log-format` 与 `IMEX_LOG_FORMAT` 的关系应与其他参数保持一致——env 是覆盖而不是补默认值——故本行 ③④⑤⑦ 中 A-5 的改动已整体回退：`cmd/root.go` 恢复 env 覆盖 flag，`TestCLILogFormatFlagBeatsEnv` 改写为 `TestCLILogFormatEnvOverridesFlag`（四子用例，`both given, env wins`），3.3.2 与 README 参数表按回退后的语义描述，`VERSION` 也退回 `0.2.2`。A-7 与 A-4 保留。因此本批的契约面变化为零，定级随之退回 Z 档（`0.2.3`），是否发版仍待用户明示。** |
| 74 | `--log-format` 的默认值名叫 `java`：它说的是实现语言而不是布局，两个合法值一个拿语言命名（`java`）、一个拿格式命名（`text`），名不对称也不自解释；用户指出「`IMEX_LOG_FORMAT=java` 这个值比较土，换个名字」并选定 `console`，旧名作废 | 值名**硬改不留别名**（与 `--no-create`、`--no-replace`、`--single-row-insert` 同一政策）。**① 代码**：`internal/common/log.go` 的常量 `LogFormatJava` → `LogFormatConsole`（值 `"console"`）、`javaHandler` → `consoleHandler`、全局默认 `LogFormat = "console"`；`cmd/root.go` 的默认值与帮助行改为 `Log layout: console (default: human-readable fixed-width) or text (slog key=value), also via IMEX_LOG_FORMAT`；`cmd/validate.go` 的报错清单改为 `console/text`。两个标识符都只在包内与测试里使用，没有导出到包外的调用方。**② 实测**（本机 MariaDB 10.6.23，`imex_test`，`SELECT 1`）：`--log-format java` → **退出 2**、`invalid --log-format "java": must be one of console/text`；`IMEX_LOG_FORMAT=java` → 同样 **退出 2**（环境变量走同一条成员检查）；`--log-format console` 与 `--log-format text` 均退出 0，前者输出定宽行、后者输出 `time=... level=...`；**不带任何选项**退出 0 且输出定宽行，证明默认值本身换了、不只是帮助文本换了；`IMEX_LOG_FORMAT=console` 配 `--log-format text` 输出定宽行，证明回退后的「env 覆盖 flag」在新值名下同样成立。**③ 测试**：`cmd/cli_matrix_test.go`、`cmd/version_log_test.go`、`internal/common/log_test.go` 十余处引用同步改名，用例名 `TestJavaHandlerKeepsOneRecordPerLine` → `TestConsoleHandlerKeepsOneRecordPerLine`；非法值用例的取值由 `javaness` 改为 `consoleness`（仍钉「近似拼错必须被拒且是用法错误」），用例数量与文件数不变（47 个 `_test.go` / 256 个 `Test*`）。**④ 文档与基线**：3.3.2 的取值表由 `java` 改为 `console` 并新增一段说明作废与无别名、二节选型表、5.3 之后配置回显节、项目结构、README 参数表与项目结构同步；`docs/cli-oracle.txt` 三个携带该持久标志的段落（export/import/root）同步替换那一行，并新增 **Re-baselined note 11** 登记改名、无别名、两个旧值调用均退出 2。**⑤ 五面契约**：① CLI flag 与 help——`--log-format` 一行帮助文本与默认值显示改变，无 flag 增删，实机 `## export` diff 仍逐字节为空（87 行）、`## import` 有意差异仍 16 行、`## root` 仍 2 行；② 输出文件格式——不变；③ 退出码与错误文本——**新增一类退出 2**（此前合法的 `java` 现在被拒），错误文本的合法值清单改为 `console/text`；④ `IMEX_*` 环境变量——`IMEX_LOG_FORMAT` 的取值集合收窄，`java` 由合法变非法；⑤ 发往服务端的语句——不变。**⑥ 定级**：删除一个此前被接受的取值属**不兼容变更 → X+1**。本批三项（变更 72 用户判 Z、73 的 A-7/A-4 属 Z、本行 X）合起来按第 7 条应为 **1.0.0**；`Makefile` 首行仍未动（`0.2.2`），提交、tag、打包等明示。**验证（本轮实跑）**：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过（`internal/common` 50.135 s、`internal/import` 4.224 s、`internal/export` 1.627 s、`internal/config` 1.021 s、根包 1.168 s、`cmd` 1.058 s、`test/integration` 1.040 s），真实库 **116 条 PASS（47 顶层 + 69 子用例）/ 0 SKIP / 0 FAIL**（7.618 s），`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**，帮助侧 delta 实测 **0 / 16 / 2**。 |
| 75 | Parquet 导出没有压缩选项：`internal/export/parquet_writer.go` 构造 `parquet.WriterConfig` 时只设 `Schema` 与 `MaxRowsPerRowGroup`，parquet-go 的 `Compression` 缺省即 `&format.Uncompressed{}`（`writer.go:707`），因此所有 Parquet 产物都是未压缩的；用户要求在此前评估的基础上把选择权交出来——「--parquet-compression none|snappy|zstd ，默认 none。版本y档」 | **① 实现**：`internal/config/config.go` 新增 `ParquetCompressionNone/Snappy/Zstd` 三个常量、`ValidParquetCompression()` 与 `ExportConfig.ParquetCompression`（`NewExportConfig()` 默认 `none`）；`cmd/export_flags.go` 注册 `--parquet-compression`（help 说明三值、代价与仅 parquet 生效）；`cmd/validate.go` 新增 `validateParquetCompression`（非法值报 `invalid --parquet-compression %q: must be one of none/snappy/zstd`）并接在 `validateFormat` 之后；`internal/config/formatscope.go` 新增 `parquetOnly("parquet-compression")` 登记项，因此越界报 `--parquet-compression can only be used with --format parquet`、`--help` 自动追加 `(only with --format parquet)`、配置回显只在 `-f parquet` 出现；`internal/export/parquet_writer.go` 的 `NewParquetWriter` 增加 `compression` 参数，`parquetCodec()` 把 `""`/`none` 映射为 nil（Uncompressed）、`snappy`/`zstd` 映射为 `parquet-go/compress` 的薄包装，未知值返回错误；`internal/export/exporter.go` 传入 `e.config.ParquetCompression`；`internal/export/configlog.go` 新增 `parquet_compression` 字段。三个 codec 均来自既有 indirect 依赖 `github.com/klauspost/compress v1.18.5`，**不新增模块**；两个 codec 链接进二进制仅 +4,096 B（22,274,313 → 22,278,409）。**② 实测（本机 MariaDB 10.6.23，`imex_test.pqsrc` 20,000 行，含 TEXT 与 VARBINARY；导出 `-f parquet`，再 `import -f parquet` 回灌三张 `LIKE` 表）**：默认（不带选项）与 `--parquet-compression none` 的产物**逐字节相同**（`cmp` 无差异）；三档文件 2,363,430 / 997,410 / 759,240 B，footer 列块 codec 实测为 `UNCOMPRESSED` / `SNAPPY` / `ZSTD`（`parquet.OpenFile` + `Metadata().RowGroups[].Columns[].MetaData.Codec`），行数均为 20,000；回灌后三张目标表 `COUNT(*)=20,000`、`SUM(CRC32(...))=42900513945642` 与源表一致。退出码：`-f del --parquet-compression snappy` 与 `-f sql` 同组合均退出 **2**；`--parquet-compression gzip` 退出 **2**、错误文本逐字为 `invalid --parquet-compression "gzip": must be one of none/snappy/zstd`；三个合法值均退出 0，配置回显随值变化（如 `parquet_compression=zstd`）。300k 行取舍矩阵（此前评估测得，已写入 5.3）：文本类 none 20,379,742 B/466 ms/43–47 MB、snappy 2,112,909 B（9.6×）/485 ms/35–44 MB、zstd 349,233 B（58.3×）/534 ms/91–164 MB；随机二进制 none 16,148,845 B、snappy 15,048,253 B（1.07×）、zstd 14,528,888 B（1.11×）；导入三档均约 1.9–2.0 s、RSS 29–31 MB。**③ 测试**：`internal/export/parquet_writer_test.go` 新增 `TestParquetWriterAppliesTheRequestedCodec`（`none`/空串→`format.Uncompressed`、`snappy`→`format.Snappy`、`zstd`→`format.Zstd`，经 `parquet.OpenFile` 读列块 `MetaData.Codec` 断言）与 `TestParquetWriterRejectsUnknownCodec`（`gzip` 必须报错）；`internal/export/configlog_test.go` 新增 `parquetLayoutKeys` 组，断言 `parquet_compression` 只出现在 `-f parquet`；`cmd/cli_matrix_test.go` 新增 `TestCLIParquetCompression`（三值合法、`-f del`/`-f sql` 越界、非法值各退出 2）。测试资产 47 个 `_test.go` / **259** 个 `Test*`（另有 1 个 `TestMain`）/ 6 个 `Fuzz*` / 12 个 `Benchmark*`，`internal/export` 86→**88**、`cmd/cli_matrix_test.go` 12→**13**。**④ 文档与基线**：README 导出标志表新增该行并补一段压缩取舍、参数校验表新增一行、修复记录新增一行；`docs/cli-oracle.txt` 重新基线 `## export`（87→**88** 行，只多这一行，diff 重新为空），新增 **Re-baselined note 12**，`## import` 有意差异仍 16 行、`## root` 仍 2 行。**⑤ 五面契约**：① CLI flag 与 help——**变**：新增一个选项、帮助多一行（导出 87→88 行），旧命令行不受影响；② 输出文件格式——**默认不变**（`none` 与旧行为逐字节相同），仅显式选择 `snappy`/`zstd` 时页压缩改变；③ 退出码与错误文本——**新增两条退出 2**（非法取值、配合非 parquet 格式），原有路径不变；④ `IMEX_*` 环境变量——**不变**，未新增环境变量；⑤ 发往服务端的语句——**不变**（导出仍只发原查询与既有元数据语句，压缩发生在本进程内）。**⑥ 定级**：新增选填选项、默认值与既有产物逐字节一致、旧命令行全部照旧——属**兼容的功能新增 → Y+1**（用户亦已指定「版本y档」）。`Makefile` 首行仍是 `VERSION ?= 0.2.2`；本批工作区未提交、未打 tag、未打包。**版本号提示**：变更 74（`--log-format java` 移除）按字面是 X+1，若它与本行合并发布，按第 7 条「本轮最高档」应为 `1.0.0` 而非 `0.3.0`，发版口径待用户明示。 |
| 76 | `--mode create` 只给了 `-t`、而 SQL 文件不含 `CREATE TABLE` 时，工具不预检目标表是否存在：直接进入读循环并把第一批 INSERT 发给服务端，最终以驱动的原始 `Error 1146 (42S02): Table … doesn't exist` 收场。实测（本机 MariaDB，`SELECT id,name FROM pqsrc LIMIT 5` 的 data-only 导出，`import -f sql -M create -t no_such_target`）：退出 1、`import aborted read=5 inserted=0 updated=0 unchanged=0 failed=5`，错误文本只有服务端的 1146，既没说是“文件没有建表语句”，也没说怎么补；此前变更记录 42 末尾自己登记了“仍未处理” | **① 实现**：`internal/import/importer.go` 把表存在性探测抽成 `tableExists(ctx)`（与 `validateTargetColumns` 共用同一条 `SELECT COUNT(*) FROM information_schema.TABLES`，错误包装也一致），并在 create 分支里、执行任何 CREATE 之前加一条判据：文件里没有 CREATE 语句时先探测 `-t` 指定的表是否存在，不存在就在读取数据之前返回 `create mode found no CREATE TABLE statement in the SQL file and table %q does not exist in database %q: re-export with --create, or import with --mode insert into an existing table`。表存在时行为不变（照旧按 INSERT 导入），`-t` 缺省时的旧报错（`create mode requires --table …`）逐字保留。**② 实测（改动前后同一命令）**：缺失表——改前退出 **1**、读 5 行、5 条 INSERT 全部失败、错误里是 1146；改后退出 **1**、**一条 INSERT 都没发**、日志里 `INSERT INTO` 命中 0、错误文本为上面那句并点名 `--create` 与 `--mode insert`；已存在的表——改后退出 **0**、`read=5 inserted=5 failed=0`，5 行落库（改动前同样成功），即失败路径提前、成功路径不变。**③ 五面契约**：① CLI/help——不变（无新选项，oracle 三段 diff 仍 0/16/2）；② 产物文件格式——不变；③ 退出码与错误文本——退出码仍 1，失败文案从服务端 1146 换成可操作的本地诊断；④ `IMEX_*`——不变；⑤ 发往服务端的语句——失败路径从“1 条批量 INSERT（5 元组）”变成“1 条 information_schema 探测、0 条 INSERT”，成功路径（`-t` + 无 CREATE + 表已存在）多一条同样的探测。**④ 定级**：失败语义与文案是缺陷修复，但成功路径多了一条可被 general_log 观察到的探测，按第 7 条面⑤“任一变了至少第二档”记 **Y**。**⑤ 测试**：`test/integration/option_matrix_test.go` 的“默认仅数据 + mode create 必须明确失败”由“退出 1 或 2 均可”收紧为“退出必须 1 + 文案含三处关键信息 + 不得出现 1146 + 不得创建目标表”，并新增子用例“数据-only + mode create 对已存在的表照旧导入”守住兼容面。真实库 117 条 RUN（47 顶层 + 70 子用例）/ 0 SKIP / 0 FAIL。**⑥ 文档**：README 修复记录新增一行；2.2.4 的 create 模式说明补该预检。 |
| 77 | 表头开关叫 `--no-col-names`：名字描述的是 DB2 的 `MODIFIED BY NOCOLNAMES` 遗产而不是“关掉表头行”，在导入侧尤其容易和“列名映射”混淆；用户要求“第二条需完善，用 no-header”。实际代价有实测：`--no-col-names` 导出的 3 行文件、导入只给 `-c` 时首行被当表头丢掉，退出 **0**、只落 2 行（变更记录 42 起只有一条 WARN） | **① 实现**：两侧同名叫 `--no-header`，**硬改名不留别名**（与 `--no-create`、`--no-replace`、`--single-row-insert` 同政策）：`config.DelModifier.NoColNames` → `NoHeader`；`cmd/export_flags.go`/`cmd/import_flags.go` 的注册与帮助改为 `Do not write the header row` / `The input file has no header row (its first line is data)`；`internal/config/formatscope.go` 两侧登记项由 `delOnly("no-col-names")` 改为 `delOnly("no-header")`（越界文案随之变成 `--no-header can only be used with --format del`）；导出/导入的配置回显键由 `no_col_names` 改为 `no_header`；导入侧那条“首行被当表头丢弃”的 WARN 文本同步改为 `if the file was exported with --no-header add --no-header to the import`。**② 实测**（MariaDB，3 行 DEL 文件）：`export --no-header` 产物 3 行、首行直接是 `"1","user-1"`；`import --no-header -c id,name` 退出 **0**、落库 **3** 行（改动前同样的文件只给 `-c` 只有 2 行）；两侧旧的 `--no-col-names` 均退出 **2**、`unknown flag: --no-col-names`；配置回显在两侧均为 `no_header=true`；忘记新开关时仍为 2 行，但 WARN 已点名 `--no-header`（同一文件实测）。**③ 五面契约**：① CLI 与 help——**变**：两个子命令各改一行选项名与说明（`## export` 重新基线后 88 行、diff 0；`## import` 有意差异仍 16 行；`## root` 仍 2 行）；② 产物文件格式——不变（同样的调用只换 flag 名）；③ 退出码与错误文本——旧名从“合法”变“未知选项”，退出 **2**，属不兼容；④ `IMEX_*`——不变（该选项没有环境变量）；⑤ 发往服务端的语句——不变。**④ 定级**：删除/改名 CLI 选项 → **X+1**。本批四项（74 X、75 Y、76 Y、77 X）按“取最高档”合并发布应为 **1.0.0**；`Makefile` 首行仍是 `0.2.2`，提交、tag、打包等明示。**⑤ 测试**：`cmd/cli_matrix_test.go` 的 DEL 矩阵改用新名，并新增 `TestCLINoColNamesIsUnknown`（两侧旧名都必须是未知选项）；`cmd/option_scope_test.go`、`internal/config/formatscope_test.go`、`internal/export/configlog_test.go`、`internal/import/configlog_test.go`、`internal/export/exporter_stream_test.go` 与 `test/integration/option_matrix_test.go`（用例名改为 `TestOptionMatrixNoHeaderSymmetry`）同步；测试资产 47 个 `_test.go` / **260** 个 `Test*`（另有 1 个 `TestMain`）/ 6 个 `Fuzz*` / 12 个 `Benchmark*`，`cmd/cli_matrix_test.go` 13 → **14**。**⑥ 文档**：README 两处标志表、成对说明、示例与参数校验表改用新名并注明“原名已作废、不留别名”；2.1.5 / 2.2.5 的选项表、`--skip-count` 说明、无表头配对段、DB2 映射行同步；`docs/cli-oracle.txt` 重新基线 `## export` 并新增 **Re-baselined note 13**；历史变更行（15/17/29/42/44/52）里的旧名保留原文，本行即其改名说明。**⑦ 验证**：`gofmt -l` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过（`internal/common` 55.687 s、`internal/import` 4.168 s、`internal/export` 1.630 s），真实库 117 条 RUN（47 顶层 + 70 子用例）/ 0 SKIP / 0 FAIL（7.039 s），`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**，帮助侧 delta 实测 **0 / 16 / 2**。 **注：本行的改名已由变更记录 78 撤回（用户复核认为 `--no-col-names` 更清晰明了），现行状态以 78 行为准。** |

| 78 | 变更 77 把表头开关改名为 `--no-header`，用户复核后认为原名更好：「`--no-col-names` 还是比 `--no-header` 清晰明了」——它直说“不输出列名 / 首行不是列名”，而 `no-header` 容易被读成“文件没有头部（前导区）” | **撤回改名**（改动从未提交、从未发布，因此无兼容成本、无需别名）。**① 代码**：`config.DelModifier.NoHeader` 恢复为 `NoColNames`，两侧帮助文本恢复 `Do not output column names` / `First line is not column names`，`internal/config/formatscope.go` 两侧登记项恢复 `delOnly("no-col-names")`（越界文案回到 `--no-col-names can only be used with --format del`），导出/导入配置回显键恢复 `no_col_names`，导入侧“首行被当表头丢弃”的 WARN 恢复原文 `... so if the file was exported with --no-col-names add --no-col-names to the import to load that record as data`。**② 实测（本机 MariaDB 10.6.23，3 行表 `imex_hdr.src`）**：`export --no-col-names` 产物 3 行、首行即 `"1","aaa"`，配置回显 `no_col_names=true`；`import --no-col-names -c id,name` 退出 0、`read=3 inserted=3 updated=0 unchanged=0 failed=0`、落库 3 行（1/aaa、2/bbb、3/ccc）；同一文件只给 `-c` 时退出 0、只落 2 行，WARN 逐字点名 `--no-col-names`；两个子命令的 `--no-header` 均退出 **2**、错误文本为 `unknown flag: --no-header`。**③ 五面契约（相对 v0.2.2）**：① CLI 与 help——**净变化为零**（选项名、说明、范围标记全部回到基线，`docs/cli-oracle.txt` 的 `## export` 二次重新基线后 88 行、diff **0**；`## import` 有意差异仍 **16** 行；`## root` 仍 **2** 行）；②③④⑤ 无变化。**④ 定级**：77 与 78 在面①上互相抵消，本轮净变化只剩 74（X，`--log-format java` 作废）、75（Y，parquet 压缩）、76（Y，create 预检）；按“取最高档”合并发布仍为 **1.0.0**，`Makefile` 首行未动（`0.2.2`）。**⑤ 测试**：`TestCLINoColNamesIsUnknown` 改写为 `TestCLINoHeaderIsUnknown`（两侧 `--no-header` 必须是未知选项，钉住“只有一个名字”），DEL 矩阵恢复 `{"--no-col-names", ""}`；`cmd/option_scope_test.go`、`internal/config/formatscope_test.go`、`internal/export/configlog_test.go`、`internal/import/configlog_test.go`、`internal/export/exporter_stream_test.go`（`streamTestExporter` 参数名回到 `noColNames`）、`test/integration/option_matrix_test.go`（用例名回到 `TestOptionMatrixNoColNamesSymmetry`，文件名 `no_col_names.del` / 日志 `no-col-names.log`）全部恢复；测试资产不变：47 个 `_test.go` / **260** 个 `Test*`（另有 1 个 `TestMain`）/ 6 个 `Fuzz*` / 12 个 `Benchmark*`，`cmd/cli_matrix_test.go` 仍 14。**⑥ 文档**：README 两处标志表、无表头配对说明、示例、参数校验表与关联校验段恢复原名；2.1.5 / 2.2.5 选项表、`--skip-count` 说明、无表头配对段、DB2 映射行（恢复为 `MODIFIED BY NOCOLNAMES \| --no-col-names`）同步；历史变更行（29/42/52 等）里本来就是旧名，本轮确认未被动过；`docs/cli-oracle.txt` 的 **note 13** 改写为“改名当天即撤回”，`## export` 体二次重新基线。**⑦ 验证**：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 干净、七包 `-race` 全通过，真实库 117 条 RUN（47 顶层 + 70 子用例）/ 0 SKIP / 0 FAIL，`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**；未提交、未打 tag、未打包。 **用户裁决（2026-10-28）：「提交，tag y 变化，打包」——按 Y 档发版**，版本号 `0.2.2 → 0.3.0`（第二位 +1、第三位归零），`Makefile` 首行在同一次提交内同步并打附注标签 `v0.3.0`；README 修复记录章节标题同步为 v0.3.0。采纳理由：本轮净契约面变化是 75（新增 `--parquet-compression`，新增选项属兼容的功能新增）与 76（create 预检，成功路径多一条 `information_schema` 探测），改名 77/78 相抵且从未发布；同一未发布提交里的 74（`--log-format java` 作废）按字面属 X 档，但用户明示取 Y 档，与本仓库 0.x 期「不兼容先记在第二位」的惯例一致（AGENTS.md 第 9 条把该口径列为待用户决定，本条即用户决定）。 |
| 79 | 目标表带生成列（`VIRTUAL`/`STORED GENERATED`）时，导入必然在第一批失败：导出把生成列当普通列写进 `INSERT` 列清单，服务端在严格模式下回 `Error 1906 (HY000): The value specified for generated column 'gv' ... has been ignored`，实测 3 行 `read=3 inserted=0 failed=3`，DEL / SQL / Parquet 与 `--mode create` 四条路径全部一样；对照实测本机 `mysqldump --skip-extended-insert --no-create-info` 的产物（`INSERT INTO t_gen VALUES (1,10,20,21);`）在同一服务端同样报 1906，所以不是 imex 独有的回退，但完全可以做得更好。**① 修法**：新增 `internal/import/generated_columns.go`——`generatedColumnQuery` 查 `information_schema.COLUMNS ... AND EXTRA LIKE '%GENERATED%'`（只认 `EXTRA`：MySQL 5.7/8.0、MariaDB 10.2+、OceanBase MySQL 模式都写 `VIRTUAL GENERATED`/`STORED GENERATED`；不选 `GENERATION_EXPRESSION` 是因为缺这一列的服务端会让整条查询失败，把“顺手排除”变成“导入中断”）；`splitGeneratedColumns` 把列清单拆成保留列表 + 基于**原始位置**的 `dropCols` 掩码；`appendRowValues` 在攒批扁平化时按掩码删值，无生成列时仍是一次 `append(dst, row...)` 快路径。接入点在 `import()` 的建表/校验之后、DEL 列类型解析之前；DEL 的 `columnInfoFor` 改吃过滤前的 `readerColumns`，否则生成列在中间时 kinds/binaries 会整体错位（单元测试钉住中列场景）。全列都是生成列时直接报错并提示“导入基列”。读 `information_schema` 失败只 WARN 并按原样发送，行为退回改动前，不会因权限不足拒绝一次可能无害的导入。**② 实测（MariaDB 10.6.23，`imex_edge2.t_gen` = `id,v,gv AS (v*2) VIRTUAL,gs AS (v*2+1) STORED`，3 行）**：修复后 DEL / SQL / Parquet 三格式回导 `rows=3/3 mismatch=0 missing=0 extra=0`，`sql --create` 同样 `3/3`；重算值实测 `1/10/20/21`、`2/20/40/41`、`3/NULL/NULL/NULL`，与服务端定义一致；服务端语句实测为 `INSERT INTO \`t_gen_nn\` (\`id\`, \`v\`)`（用一个多出 `NOT NULL` 列的同构表逼出错误回显才看到语句），日志同时给出 `generated columns excluded from INSERT table=t_gen_dbg columns="[gv gs]"`。**③ 五面契约（相对 v0.3.0）**：①②③④ 不变；⑤ **有变化**——每次导入多一条 `SELECT UPPER(COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND EXTRA LIKE '%GENERATED%'`（普通表也照发，只回 0 行），且生成列表的 `INSERT` 列清单少列（服务端语句可见）。按第 7 条“⑤ 变了至少进第二档”，本轮定级 **Y（0.4.0）**，`Makefile` 首行未动（仍 `0.3.0`）。**④ 测试**：新增 `internal/import/generated_columns_test.go`（9 个用例：`splitGeneratedColumns` 5 例含中列/大小写/表外生成列、`appendRowValues` 4 例含短行不越界）与 `test/integration/live_mysql_test.go` 的 `TestLive_GeneratedColumnsExcluded`（del/sql/parquet/sql-create 四个子用例，断言 `inserted=3` 且生成列逐值一致）；统计 `^func Test` 264（HEAD 261，含 1 个 `TestMain`）、`_test.go` 48 个（HEAD 47）、`cmd/cli_matrix_test.go` 仍 14。**⑤ 验证**：`gofmt -l .` 空、`go vet ./...` 干净、七包 `go test -race -count=1 ./...` 全通过，实库（`IMEX_TEST_DSN` 指向本机 10.6.23）**122 RUN / 0 SKIP / 0 FAIL**（基线 117 + 新增 1 顶层 + 4 子用例），`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**。**⑥ 未做的两件**：`t_big` 两行各约 8 MB 仍因批字节预算“append 后判断”溢出到 `Error 1153`（0 行入库），空表 `-f sql`（无 `--create`）产出的 31 B 文件仍被 `the input file contains no importable data` 拒绝；两者本轮未动。未提交、未打 tag、未打包。 |
| 80 | 承接 79 ⑥：批字节预算仍是「append 之后判断」，两行各自没超预算、合计却超过 `max_allowed_packet` 时会被凑成一条语句。实测（本机 16 MiB 上限、预算 15,099,494 B）`t_big` 两行 8,388,608 + 8,388,606 B 合成 16,777,214 B 的语句，服务端回 `Error 1153 (08S01): Got a packet bigger than 'max_allowed_packet' bytes` 并 reset 连接，`read=2 inserted=0 failed=2`，DEL / SQL / Parquet 与 `sql --create` 四条路径全一样；只有「单行本身就超预算」这一种情况被 `shouldFlush` 的 append 后判断救了回来。**① 修法**：新增 `wouldExceedBatch(batchBytes, rowBytes, maxBatchBytes)`，在 append 之前预判，超预算就先把当前批发出去再收这一行；空批永不预判（单行超预算仍单独成批，避免空语句白跑一趟），`shouldFlush` 继续负责行数上限与单行超预算两种 append 后刷新。单线程与并行两条读线程都改（`importer.go` 各一处）。**② 实测**：`t_big` 三格式 + `sql --create` 全部 `rows=2/2 mismatch=0 missing=0 extra=0`（值为 8 MiB / 8,388,606 B LONGTEXT，`HEX(MD5())` 逐字节一致）；打开 `general_log`（`log_output=TABLE`）回放同一导入，`mysql.general_log` 里该表只有**两条** `INSERT INTO t_big_gl` 记录，即两批各一行，而修复前是一条 16.7 MB 的语句直接 1153。**③ 五面契约**：①②③④ 不变；⑤ 有变化——同一文件拆成两条语句发出（批边界变化），无新增语句种类。**④ 测试**：新增 `TestWouldExceedBatch`（7 个用例，含「两行只在合计时溢出」「单行超预算不预判」「预算为 0 不预判」）与实库 `TestLive_BatchSplitsBeforePacketLimit`（按服务端 `max_allowed_packet` 的 55% 造行，`threads1` / `threads4` 两个子用例，断言 `inserted=2` 且 MD5 逐行一致）。 |
| 81 | 承接 79 ⑥：空表用 `-f sql` 且不带 `--create` 导出的文件只有 31 B（`/*!40101 SET NAMES utf8mb4 */;`），导入被 `the input file contains no importable data ...` 拒绝、退出 1，等于「空表自己导出的数据文件无法回导」。**① 修法**：`SQLReader` 新增 `unrecognized` 标记与 `OnlyPreamble()`，`ReadHeader` 的 default 分支只在语句是 dump 前导（`SET`/`LOCK`/`UNLOCK`/`USE`/`START`/`BEGIN`/`COMMIT`/`ROLLBACK`/`DELIMITER`，整词匹配，`SETX` 不算）时静默跳过，其余无法归类的语句（含未以 `;` 结束的残留文本）记为 unrecognized；`Import` 在 `ReadHeader` 返回 `io.EOF` 且 `OnlyPreamble()` 为真时走 `finishNoData`，以 `import completed read=0 ... note="empty SQL file: no INSERT statement and no CREATE TABLE, nothing to import"` 结束、退出 0。结构-only（`--no-data`）的汇总与它共用同一个 `finishNoData`，输出字段保持原样。**② 实测**：`t_empty.sql`（31 B）导入 `-t t_empty_sql` 退出 0、`read=0 inserted=0 failed=0`，目标表未被触碰；故意写的 `hello world\nnot sql`（无 `;`）仍退出 1、错误仍是 `the input file contains no importable data ...`；`SELECT 1;` 同样退出 1（`OnlyPreamble()` 假）。空文件在 `--mode replace` 下同样按无操作处理、不清表（实测目标表 2 行保持不动），与 mysql 客户端回放空 dump 的行为一致。**③ 五面契约**：①②④⑤ 不变；③ **有变化**——空 dump 从「退出 1 + 错误」变为「退出 0 + 0 行汇总」，属于退出码/文案语义变化。**④ 测试**：新增 `TestSQLReader_OnlyPreamble`（7 个用例：SET 头部、多条前导、小写+注释、无分号垃圾、`SELECT 1;`、`SETX`、空文件）与实库 `TestLive_EmptySQLFileImportsZeroRows`（空表 SQL 导出 → 回导 0 行、目标已有行保持不动）。**⑤ 定级**：80 只动面⑤、81 动面③，与 79 合并本轮为 **Y（0.4.0）**；`Makefile` 首行仍 `0.3.0`。**⑥ 验证**：`gofmt -l .` 空、`go vet ./...` 干净、七包 `go test -race -count=1 ./...` 全通过，实库 **126 RUN / 0 SKIP / 0 FAIL**（117 基线 + 79 的 5 + 80 的 3 + 81 的 1）；`^func Test` 268（含 1 个 `TestMain`）、`_test.go` 48 个、`cmd/cli_matrix_test.go` 仍 14、`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**；两套边界矩阵 28 项比对全部 `mismatch=0`（`t_float` 仍是已知的服务端 6 位有效数字文本限制，与 mysqldump 一致，未变）。未提交、未打 tag、未打包。 **用户裁决（2026-10-29）：「提交 tag push」——按 Y 档发版**，版本号 `0.3.0 → 0.4.0`（第二位 +1、第三位归零），`Makefile` 首行在同一次提交内同步并打附注标签 `v0.4.0`；README 修复记录章节标题同步为 v0.4.0。采纳理由：79/80 命中面⑤（新增生成列探测语句、批边界变化），81 命中面③（空 dump 的退出码与文案语义变化），三条都是向后兼容的完善，没有删改选项、产物格式或 `ENC:` 布局，故按最高档取 Y；本轮不含不兼容变更，与 0.x 期惯例不冲突。 |
| 82 | 标准指令「只要求写出来的映射有效」：名称映射（`--columns "file=table,..."`）此前要求覆盖文件全部列——`applyColumnMapping` 先比 `len(mapping) == len(fileColumns)`，再逐列要求 `mapping[fileCol]` 存在。于是「3 列文件只要 2 列、第 3 列本就不该进目标表」这种最常见的子集需求被直接拒绝：旧二进制（f0c2549 构建）对同一命令退出 1、报 `column mapping count mismatch: file has 3 columns, but mapping specifies 2 columns`、目标表 0 行。同时 `dropCols []bool` 只能表达「保留/丢弃」、不能表达「输出顺序 ≠ 行顺序」，而名称映射本来就允许书写顺序与文件列头不同 | **① 修法**：`ColumnMapping` 由 `TargetColumns []string` 改为三元组——`Columns`（INSERT 列清单，按文件列头顺序）、`Projection []int`（`Projection[i]` 是喂给 `Columns[i]` 的行内位置，nil 表示行序即列序的快路径）、`ReaderColumns []string`（按输入位置对齐，未映射位为空串，供 DEL 的 `columnInfoFor` 解析 kinds/binaries 时保持位置不错位）；全仓把 `dropCols []bool` 替换为 `projection []int`：`splitGeneratedColumns(cols, projection, generated)`、`excludeGeneratedColumns(ctx, cols, projection)`、`appendRowValues(dst, row, projection)` 都在「列 + 投影」对上工作，因此**子集映射与生成列排除可以叠加**（生成列即便被写进映射也会在发语句前剔除，其余列的行位置不变）。名称映射的每条写出项都在读取任何数据行之前校验：文件列必须出现在列名行（大小写不敏感、忽略首尾空白，否则报错并列出文件实际列名）、同一文件列不得写两次、同一目标列不得被两个文件列写入（服务端 `Error 1110 Column ... specified twice` 提前到客户端，replace 模式不再可能先清表再失败）、无列名行时提示改用位置映射；未写进映射的文件列记一条 INFO `unmapped file columns are not imported`，未写进映射的目标列取 DEFAULT/NULL/自增值。位置映射保持「条目数 = 文件列数」不变（按索引配对，无法命名要跳过的列），并补上重复目标列校验。**② 实测（MariaDB 10.6.23，`imex_map2`）**：DEL 文件 3 列 `id,name,note`，目标表 `(t_name,t_id PK,untouched DEFAULT 'dflt')`，命令 `-c "name=t_name,id=t_id"`——新二进制退出 0、3 行 `1|one|dflt / 2|two|dflt / 3|three|dflt`（`note` 未导入、未映射目标列取默认值）；同一命令旧二进制退出 1、0 行。general_log（`log_output=TABLE`）实测服务端只收到 `INSERT INTO \`m_tgt\` (\`t_id\`, \`t_name\`) VALUES ('1','one'),('2','two'),('3','three')`：文件 3 列只写 2 列，且列清单按文件列头顺序（id 在前）而非 `--columns` 书写顺序（name 在前）。错误路径全部退出 1、目标表 0 行：未知文件列 → `file column "nope" is not in the input header (file columns: id, name, note)`；重复目标列 → `target column "A" is fed by two file columns ... Error 1110 Column 'A' specified twice`；重复文件列 → `file column "ID" is mapped twice`；无表头名称映射 → `column mapping matches file columns by name, but the input file has no column header ...`；位置映射重复目标列在读取前退出 1。生成列 × 名称映射组合（`gv=gv,id=id,gs=gs,v=v`，目标表含 `gv VIRTUAL`/`gs STORED`）回导 `inserted=3` 且生成列逐值一致，服务端语句仍只有 `id,v` 两列。**③ 五面契约**：② 文件格式、④ 环境变量不变；① `--columns` 帮助文本改为 `Target column list (comma-separated, one per input column) or a name mapping (file_col=table_col,...; may name a subset)`；③ 新增四类映射校验文案与位置映射重复列文案，名称映射不再出现 `column mapping count mismatch`；⑤ 同一命令发出的 INSERT 列清单可为子集（general_log 可见）。按第 7 条「任一契约面变化至少进第二档」，本轮定级 **Y（0.5.0）**；`Makefile` 首行升至 `0.5.0`。**④ 测试**：`internal/import/importer_test.go` 新增 `TestApplyColumnMapping`（4 个成功场景：子集、全列文件序快路径、子集乱序、大小写/空白；7 个拒绝场景：未知列、错误中列出列名、重复文件列、重复目标列、空侧、缺 `=`、无表头），`TestValidatePositionalColumns` 补重复目标列用例；`internal/import/generated_columns_test.go` 改为投影语义并新增「映射投影穿越生成列过滤」「子集投影与过滤叠加」两例、`appendRowValues` 补重排子集用例；实库新增 `TestLive_ColumnMappingSubset`（del `--threads 4` / sql / parquet 三个子用例，断言两列按文件列顺序写入、`note` 被忽略、`untouched` 取默认值）与 `TestLive_GeneratedColumnsExcluded/mapped`。统计：`^func Test` 268→**270**、`_test.go` 48 个、`^func Fuzz` 6、`^func Benchmark` 12，`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**。**⑤ 验证**：`gofmt -l .` 空、`go vet ./...` 干净、七包 `go test -race -count=1 ./...` 全通过；实库 `go test -race -count=1 ./test/integration` **131 RUN / 0 SKIP / 0 FAIL**（含三个 mapping/生成列用例的新子用例），退出码 0。**⑥ 未做**：本轮未打 tag、未打包（已随本次提交落地）。 |
| 83 | 用户指令「echo-sql-values 的注释太长，精简些。尽量不要超过屏幕自动换行。parquet-compression，shard-key 同样的问题，精简」：三条 flag 说明把设计说明与实测宣称写进了命令行，`--help` 中单行分别为 **259 / 259 / 219** 字节（120 列终端必然折行） | **① 修法**：只改 flag usage 文案，行为零改动——`--echo-sql-values` → `Echo real row values in failed SQL, not redacted tags (also IMEX_ECHO_SQL_VALUES=1)`（“真实数据写进 stderr 或 `--messages` 文件”的告警已在需求文档 3.4 与 8.x 安全表，不再重复进 help）；`--parquet-compression` → `Compression: none, snappy, zstd`（去掉 snappy/zstd 的 CPU×字节实测宣称，`(only with --format parquet)` 与 `(default "none")` 仍分别由 scope 注册表与 pflag 自动追加）；`--shard-key` → `Column used to shard a parallel export; defaults to the primary key`（`--threads 1` 忽略、回落后忽略的细节留在选项表）。**② 实测**：`imex --help`/`imex export --help`/`imex import --help` 三行长度 259/259/219 → **111 / 117 / 107** 字节（含前缀、自动 scope 标记与 default，均 ≤120）；`--parquet-compression` 的 `(only with --format parquet) (default "none")` 仍完整显示，`--shard-key` 的 auto-detect 语义改为一句话后仍可按名找到。**③ 五面契约**：① 帮助文案变化；②④⑤ 不变（无新选项、默认值/退出码/下发语句/文件格式/环境变量均未动）。与 82 同属本轮 **Y（0.5.0）**；本轮未打 tag、未打包（已随本次提交落地）。**④ 测试**：`gofmt -l cmd` 空、`go vet ./cmd ./internal/config` 干净、`go test -race -count=1 ./cmd/ ./internal/config/` 通过（含 `TestScopeMarkersReachTheHelpText`，证明缩短后 scope 标记仍在 help 内）。 |
| 84 | 承接 83 的指令「看下是否有其他的选项注释过长的？精简下」：逐行量 `imex --help` / `export --help` / `import --help`，除 83 处理的三条外仍有 **19 行超过 118 字节**，最长是 `import --statement-size` 的 **239** 字节、`import --pipeline-buffer` 205、`export --statement-size` 196、`--session-var` 两侧各 191、`import --mode` 189 | **① 修法**：只改 usage 文案，行为零改动，删掉实现细节与实测宣称（保留在需求文档选项表与 README）、保留每条必须知道的语义：`--quiet` → `Suppress progress logs; summaries and ERROR records still print (also IMEX_QUIET=1)`；`--log-format` → `Log layout: console or text (slog key=value); also IMEX_LOG_FORMAT`；`--no-sql-tag` → `Do not prepend the /* imex:<commit> */ comment to statements (also IMEX_NO_SQL_TAG=1)`；`export --batch-size` → `Rows per hand-off batch; with --threads>1 it is a usage error`；`--create` → `Output a CREATE TABLE statement`；`--default-charset` → `Connection & output-file charset (utf8mb4 default)` / `Connection & input-file charset (utf8mb4 default)`；`--extended-insert` → `Merge rows into multi-row INSERTs`；`--hex-blob` → `Binary columns as 0x hex literals`；`--no-hex-blob` → `Force raw binary passthrough (may corrupt data)`；`--pipeline-buffer` → `Hand-off queue depth in batches; --threads>1 is a usage error`（导出）/ `Hand-off queue depth in batches (0 = synchronous)`（导入）；`--progress-interval` → `Periodic progress log interval (min 10ms; --quiet drops it)`；`--session-var` → `Set a session variable (repeatable), e.g. sql_mode=STRICT_TRANS_TABLES`；`export --statement-size` → `Max bytes per merged INSERT`；`import --statement-size` → `Byte ceiling per generated INSERT (0 = server packet budget)`；`import --columns` → `Target columns (one per input column) or file=table mapping (subset ok)`；`import --mode` → `insert, insert_update, replace (clears the table), create`。顺带修正一处**语义**措辞：`import --batch-size` 原写 `Rows per reader->writer hand-off batch`，与变更记录 61 实测的「导入侧 = 每条 INSERT 的行数」不符，改为 `Rows per INSERT batch`。**② 实测**：三份 help 的行长上界 root **159 → 114**、export **196 → 117**、import **239 → 116**，超过 118 字节的行为 **0**；逐条前后：`--quiet` 159→111、`--log-format` 156→114、`--no-sql-tag` 143→113、`export --batch-size` 169→116、`--create` 133→96、`--default-charset` 130→90、`--extended-insert` 136→98、`--hex-blob` 150→101、`--no-hex-blob` 151→115、`export --pipeline-buffer` 161→114、`--progress-interval` 151→112、`--session-var` 191→110、`export --statement-size` 196→110、`import --columns` 160→111、`import --default-charset` 129→89、`import --mode` 189→116、`import --pipeline-buffer` 205→102、`import --statement-size` 239→100、`import --batch-size` 93→76。`(only with …)`/`(not with …)`/`(default …)` 由 scope 注册表与 pflag 自动追加，未重复写进文案。**③ 五面契约**：① 帮助文案变化；②③④⑤ 不变（无新选项、默认值/退出码/下发语句/文件格式/环境变量均未动）。与 82/83 同属本轮 **Y（0.5.0）**；本轮未打 tag、未打包（已随本次提交落地）。**④ 测试**：`gofmt -l cmd` 空、`go vet ./cmd ./internal/config` 干净、`go test -race -count=1 ./cmd/ ./internal/config/` 通过（`TestScopeMarkersReachTheHelpText` 仍绿，说明缩短后 scope 标记仍在）；help 文案无快照测试，故无基线需重录。 |
| 85 | 用户指令「注释长度作为规则写入到文档中」：82-84 两轮把帮助从 259 字节压到 ≤117，但规则只存在于那三次改动里，下一个新选项照样会写成长句 | **纯文档轮，无代码改动**。① 新增 `docs/requirements.md` **3.5 帮助文案长度规范（用户于 2026-10-29 指定）**：① 上限 **118 字节/行**，计量范围含选项名列、缩进、pflag 自动追加的 `(default …)` 与作用域注册表自动追加的 `(only with …)` / `(not with …)`（取 118 而非 120 是给 120 列终端留 2 列余量）；② 帮助只写“选项做什么、边界在哪”，实测数字/设计来龙去脉/多步说明放选项表与 README，环境变量用 `(also IMEX_XXX=1)` 一句带过，默认值与取值范围交给 pflag 与校验表；③ 自动后缀不得手写、不得为省长度删除，超长时缩 usage；④ 判定命令 `for c in "--help" "export --help" "import --help"; do ./bin/imex $c 2>&1 \| awk 'length($0) > 118'; done` 输出必须为空；⑤ 本规范只管命令行帮助，代码注释与文档表格不受行宽限制。② `/data/AGENTS.md` 新增顶层「帮助文案长度规范（用户于 2026-10-29 指定）」三条，把同一条规则写进跨轮生效的工作区约定并指向 3.5。**实测**：判定命令对当前二进制输出为空，三份 help 上界 root 114 / export 117 / import 116 字节（规范落地前 159 / 196 / 239，最长单行 259）。**五面契约**：①②③④⑤ 全部不变（本轮不触碰任何 flag、默认值、语句、格式与环境变量）。**定级**：纯文档轮，五个契约面全不变，不进第 7 条第二档；按用户「提交 tag push」指令，版本号取第三位 **Z（0.5.1）**，`Makefile` 首行升至 `0.5.1` 并与附注 tag `v0.5.1` 落在同一提交；已提交、已打 tag、已 push；未打包。 |
| 86 | 用户指令「考虑到包大小 依赖cgo等问题，集成duckdb需求搁置不做」+「缓存清空。结论入文档」：此前评估「`imex parquet sql` 复用 `imex` 的参数风格，支持一次性执行 + 交互 shell」 | **纯文档轮，无代码改动**（spike 已整体回退，工作树在 `078ac3c` 干净）。**① 评估范围**：封装 `github.com/marcboeker/go-duckdb/v2 v2.4.3`（引擎 **v1.4.1**，绑定 `duckdb-go-bindings v0.1.21`），新增 `imex parquet sql`（`-q/--query`、`-c`、`--file alias=path`、`--allow-dir`、`-o`、`-f`、`--threads`、`--memory-limit`、`--overwrite`）与基于 readline 的交互 shell（多行、Ctrl-C/Ctrl-D、历史、`.mode/.headers/.maxrows/.nullvalue/.separator/.timer/.output/.once/.read/.tables/.databases/.schema/.shell/.help/.quit`），做过 8 个单测与 pty 实测。**② 实测成本（linux/amd64，`-trimpath -ldflags "-s -w"`）**：纯 Go 基线 22,245,538 B（UPX `-1` 后 9,216,076 B）；接入后 75,243,264 B（含 shell 75,633,984 B），约 **+238%**；UPX `-1` 32,732,040 B、`--best` 26,446,376 B（单进程约 16 分钟 CPU）、xz -9 18,829,628 B、gzip -9 26,604,236 B；UPX 自解压启动 ~346 ms，原生 17 ms。模块缓存 ~1.2 GB（du：539M 引擎静态库 + 462M 绑定 + 126M 引擎下载缓存 + 101M 绑定下载缓存）。**③ 不做的理由**：必须 cgo（`CGO_ENABLED=1`），与「单静态二进制 + Windows 免 cgo 交叉编译」的发布方式冲突，Windows 只能出 stub，用户已裁决「Windows 下就放弃该特征」；体积 +238%（UPX 后仍 +255%）与小工具定位不符；引擎版本被绑定钉死在 v1.4.1，升级节奏受第三方控制；能力上只覆盖 DuckDB CLI 的 **16/66** 个点命令、**7/18** 种输出模式，参数面结构也不同（原生 CLI 是位置参数 + dot 命令，`imex` 是子命令 + flag），`-f` 语义还与现有导出格式冲突，做不到「和原生一样」。**④ 归档**：补丁与结论存于仓库外 `/data/work/duckdb-spike/`（`tracked.diff` 12,940 B，已用 `git apply --check --reverse` 验证可逆；`untracked.tar.gz` 14,275 B 含 6 个新增源文件；`README.txt` 73 行含还原步骤），还原命令为 `git apply tracked.diff && tar xzf untracked.tar.gz -C /data/imex`；本轮已 `git checkout -- go.mod go.sum` 并删除对应源文件。**⑤ 五面契约**：①②③④⑤ 全部不变（spike 从未提交，`imex parquet` 现按未知命令退出 2），版本仍 0.5.1。**⑥ 缓存清理**：删除 `/go/pkg/mod/github.com/{duckdb,marcboeker}` 与 `/go/pkg/mod/cache/download/github.com/{duckdb,marcboeker,chzyer}`（du 合计 ~1.2 GB；`/go/pkg/mod` 现 2.5 G，根分区可用 3.7 G → 4.9 G），删除后 `make build` 与 `CGO_ENABLED=1 go test ./...` 七包全通过，证明仓库不依赖这些缓存。**⑦ 定级**：纯文档轮，五个契约面全不变，按 Z 位 **0.5.2**，`Makefile` 首行须与提交同批同步；本轮未提交、未打 tag、未打包。**其后（2026-10-30）**：本行已以 `5f1df86` 提交并 push 到 `origin/main`，附注 tag 与打包按用户「打包不要编译windows了」之后的指令另行处理（源码包 + Linux amd64，见 README 修复记录）。 |
| 87 | 用户指令「检查下关键节点，添加一些日志供日常排查性能或诊断使用。添加前让我确认」→ 确认「a+b」：此前日志只有端到端 `duration`，出问题看不出时间花在连接、元数据、首行等待、扫描、解析、发送还是落盘 | **A 组阶段计时 + B 组阈值告警，五面契约零变化**。**① 导出（单线程）**：`export completed` 增加 `connect`（`NewExporter` 里拨号+ping+一致性快照的耗时，`Export` 之前发生，故不计入 `duration`）、`setup`（`Export` 起点到发起查询）、`first_row`（查询到第一行，含建文件与写表头）、`write`（第一行到最后一行写完）、`finish`（footer+close；为此把文件改为在汇总前显式 `Close`，避免 close 时间丢掉，defer 的 Close 成为二次关闭的兜底），四段之和即为 `duration`（毫秒取整）；并行路径 `parallel export completed` 增加 `connect`/`setup`，`shard export completed` 增加 `rows`/`duration`/`rate`（时长从领到分片算到最后一块交给汇流写入器，含被前序分片反压的时间）。**② 导入（单线程）**：`import completed` 增加 `connect`、`setup`、`header`（打开输入到解析完表头，parquet 读 footer 建 schema 也算这里）、`read`（读文件+解析，不含等队列，按批括起来计时，不是逐行 `time.Now()`）、`send`（执行 INSERT 的累计）、`wait_empty`（写侧等数据）、`wait_full`（读侧等空位）；`read+wait_full` 是读协程视角、`send+wait_empty` 是写侧视角，两组各自接近 `duration`，不是四段相加（文档已写明）。并行路径 `parallel import completed` 增加 `connect`/`setup`/`header`，`worker completed` 增加 `rows`/`duration`/`rate`。**③ 阈值告警**：新增 `export scan stalled`（扫描连续 **10 秒**取不到行，每 **64 行**读一次时钟，避免把计时器塞进最热循环；1024 行一检查会把「稳定但慢」误报成卡住，已改小）、`slow insert batch`（**单批** >5s，带 `rows`/`bytes`/`duration`，并行带 `thread`）、`slow commit`（单次 COMMIT >5s，带 `session`/`rows`/`duration`，在 `writeSession.commit` 一处收口）。阈值与检查间隔是包内常量，**不加 flag、不加 env**。**④ 实测（MariaDB 10.6.23，20 万行 DEL/SQL/Parquet）**：导出 `duration=0.160s connect=0.001s setup=0.000s first_row=0.004s write=0.152s finish=0.003s`（SQL `0.237/0.006/0.226/0.005`、Parquet `0.287/0.004/0.262/0.021`，三段相加与 `duration` 一致（毫秒取整））；并行 4 线程 8 分片 `rows=25000 duration=0.044–0.062s rate≈40–57 万行/秒`、汇总 `setup=0.001s`。导入 `duration=1.875s connect=0.000s setup=0.001s header=0.000s read=0.409s send=1.391s wait_empty=0.002s wait_full=1.364s`（读侧 `0.409+1.364≈1.77s`、写侧 `1.391+0.002≈1.39s`）；并行 4 线程每 worker `rows=48000/50000/50000/52000 duration≈0.957s`，合计 200,000 行。告警实测：`SELECT id, payload, IF(id=500, SLEEP(12), 0)` 导出得到 `WARN export scan stalled rows_so_far=448 gap=12.000s`，且这 12 秒落在 `write=12.033s` 而不是 `first_row=0.004s`；`BEFORE INSERT` 触发器每行 `SLEEP(0.002)` 配 `--batch-size 5000` 得到 `WARN slow insert batch rows=5000 bytes=937869 duration=10.430s`，同一数据改用默认 `--batch-size 1000` 分 5 批各约 2 秒则**不告警**（说明阈值按单批判定）；`slow commit` 在本机 20 万行单事务（`--commit-count 200000`）未超过 5 秒，**未能在实库触发**，其门槛与字段顺序由单测覆盖。**⑤ 五面契约**：①②③④⑤ 全部不变——不新增下发语句、不加 flag/env、退出码与错误文案不动、产物字节不变，只在既有记录上加字段并新增两条阈值 WARN＋一条扫描 WARN。**⑥ 测试**：新增 `internal/common/diag_test.go` 的 `TestStallWatch`（5 个子用例：未到检查点不读时钟、健康检查点只计量不告警、超阈值告警、锚点随报告前移、零值静默）与 `TestWarnIfSlow`（阈值门槛、`duration` 排最后、零阈值静默）；`gofmt -l .` 空、`go vet ./...` 干净、`CGO_ENABLED=1 go test -race -count=1 ./...` 七包全通过；`^func Test` 270→**272**、`_test.go` 48→**49**、`^func Fuzz` 6、`^func Benchmark` 12、`cmd/cli_matrix_test.go` 仍 14、`grep -c statementCapSlack test/integration/option_matrix_test.go` = **3**，三份 help 超 118 字节的行仍为 **0**。**⑦ 定级**：五个契约面全不变，按 Z 位 **0.5.3**，`Makefile` 首行须与提交同批同步；本轮未提交、未打 tag、未打包。**其后（2026-10-30）**：本行已以 `c73bf1e` 提交（`Makefile` 首行同批升至 0.5.3），尚未 push、未打 tag、未打包；随后的文档-实现核对修正见下一行。 |
| 88 | 用户指令「提交后，检查下文档和现在实现是否匹配？」：变更记录 87 已以 `c73bf1e` 提交后，逐项核对文档与代码、实测数字 | **纯文档轮 + 一处代码注释纠错，五个契约面零变化**。**① 计数**：文档里的资产计数停在 47 个 `_test.go` / 260 个 `Test*`（变更 87 提交时代码实为 48/270），现按实测改为 **49** / **272**（§11.1 与 §十二两处、11.0 的拆分句、包内计数 `internal/common` 10 文件 46→**11/48**、`internal/import` 12 文件 52→**13/57**），与 `find . -name '*_test.go' \| wc -l`、`^func Test` 实测一致；另修 `test/integration/live_mysql_test.go` 8→**12**，11.1 的 `TestMain` 口径由「另有 1 个」改为「含 1 个」（272 这个数本就含它）；`grep -c statementCapSlack test/integration/option_matrix_test.go` 仍 **3**，三份 help 超 118 字节仍 **0** 行。**② 消息命名规则**：3.3.2 与 README 原写「事件词固定为 `config`/`progress`/`completed`/`aborted`/`failed`」，与代码里既有的 `retrying`/`executing`/`applied`/`derived` 以及本轮新增的 `slow insert batch`/`slow commit`/`export scan stalled` 冲突；改为「主要事件词是……」并补状态词与诊断告警的命名口径，同时把「同一事件只输出一行」限定为配置/进度/完成/中止/失败类，阈值告警按发生次数各一行（3.3.2、3.3 表后说明）。**③ WARN 清单**：2.3 与 README 的两份场景清单补上三条诊断告警，并把 `--quiet` 的保留项统一为「配置、结束统计、WARN 与 ERROR」（README 两处选项表、3.3.1、7.x 日志段各一处；代码实测 `common.Quiet` 只包住两条进度闭包，WARN 不受影响）。**④ README 修复记录**：标题 v0.5.2→**v0.5.3**，补一行记录 87 的阶段计时与三条阈值告警（实测数字与「`slow commit` 未能在实库触发、由单测覆盖」照录）。**⑤ 文件树**：README 与需求的 `项目结构` 各补 `internal/common/diag.go`、`internal/export/diag.go`、`internal/import/diag.go` 三行。**⑥ 代码注释纠错**：`internal/import/importer.go` 中 `setupDur` 上方原写「四段相加等于总数」，与实现（`read+wait_full` 为读侧视角、`send+wait_empty` 为写侧视角，两组各自接近 `duration`）矛盾；改为说明各字段是同一运行的两种视角、不得相加。**⑦ 核对后**：`gofmt -l .` 空、`go vet ./...` 干净，`go test -count=1 ./internal/common ./internal/import ./internal/export` 三包通过（9.145 s / 0.417 s / 0.466 s）；本轮无行为改动，未重跑整套测试与真实库。**⑧ 发现但未改（需用户定夺）**：`--quiet` 的帮助文本仍是「Suppress progress logs; summaries and ERROR records still print」，未点名 WARN（面①，改动需代码提交）；§十二与 README 的发布矩阵仍写 Linux + Windows 两个平台，而本次打包按用户指令只出源码包 + Linux amd64（属发布实践，`make build-all` 能力未变）。**⑨ 定级**：纯文档（含一处注释），按 Z 位 **0.5.4**，`Makefile` 首行须与提交同批同步；本轮未提交、未打 tag、未打包。本行与下一行同处一个工作区，若合并提交则按两行中较高的定级。**用户裁决（2026-10-31）：「提交 tag」——本行与下一行同批提交，取下一行的 **1.0.0**。** |
| 89 | 用户指令「`--progress-interval` 最小100ms单位」：把进度间隔的下限从 10ms 提到 **100ms**（参数仍是 `time.Duration`，`100ms`/`500ms`/`5s` 都合法，不是整数毫秒） | **① 实现**：`internal/config/config.go` 的 `MinProgressInterval` 10ms→**100ms**（`cmd/validate.go:validateProgressInterval` 直接引用该常量，比较与文案不变）；两侧 `--progress-interval` 帮助文本改为 `(min 100ms; --quiet drops it)`；`cmd/cli_matrix_test.go:TestCLIProgressInterval` 的合法档由 `10ms` 改为 **`100ms`**，并把 `10ms` 移入拒绝档；`test/integration/live_mysql_test.go:TestLive_ProgressNeverOutlivesCompletion` 的 CLI 段改用 **100ms**（配置回显断言同步为 `progress_interval=100ms`），查询改为 `SELECT SLEEP(0.05) AS s, id ... WHERE id <= 6`——六行各睡 50ms 把运行拉到约 300ms，否则 8192 行导出只需 10ms、100ms 档一次进度都不打。**② 实测（本机 MariaDB 10.6.23 + 工作区构建）**：`100ms`/`1s`/`5s` 均退出 **0**；`10ms`、`1ms` 退出 **2** 且文案逐字为 `invalid --progress-interval 10ms: must be >= 100ms`（导出与导入两侧相同）；`0` 与 `-1s` 仍是 `must be > 0`（`0` 解析为 `0s`）；默认值仍是 5s。100ms 档的节奏：262,144 行 DEL 导出 `duration=0.18s` 打出 **1** 条进度（`rows=0` 那条先到），六行 `SLEEP(0.05)` 查询 `duration=0.301s` 打出 **3** 条；作为对照，10ms 档在 200,000 行导入上等于每批一条（200 批 / 发送段 1.391 s，约 7ms 一批），`1ns` 档历史上一次 6400 行导出打出 1355 条（日志 175,557 字节）。**③ 五面契约**：① CLI/help——**变**：下限与帮助文本，`docs/cli-oracle.txt` 的 `## export` 体按新帮助重新生成（88 行，diff 为空），并另吸收 2026-10-29 帮助精简遗留的 15 行差异（第 16 行是本轮的进度行）；`## import`/`## root` 仍为冻结快照，实测差异 **22**/**5** 行（旧记的 sixteen/two 是同一批改写之前的数），新增 note 14 说明；② 文件格式——不变；③ 退出码——**变**：`10ms`–`99ms` 由此前的合法变为用法错误 **2**，文案 `must be >= 100ms`，0/负值文案不变；④ `IMEX_*`——不变（该选项没有环境变量）；⑤ 发往服务端的语句——不变。**④ 测试**：`gofmt -l .` 空、`go vet ./...` 干净、`go test -count=1 ./...` 七包全通过（`internal/common` 9.656 s，其余 ≤0.53 s）；真实库 `IMEX_TEST_DSN=... go test -run TestLive_ProgressNeverOutlivesCompletion ./test/integration` **PASS（0.59 s）**；三份 help 超 118 字节仍 **0** 行。**⑤ 定级**：拒绝此前合法的取值属不兼容变更，按本仓库口径为 **X**；0.x 期按「不兼容记在第二位」处理，修正上一条建议：按 `AGENTS.md` 第 7/9 条**字面执行**——「退出码或错误语义变更」属不兼容，进第一位，故为 **1.0.0**（此前写的 0.6.0 是沿用了 0.x 期「不兼容记在第二位」的旧惯例，第 9 条明确该惯例尚未经用户改口径，未改前不采用）。`Makefile` 首行 0.5.3→**1.0.0** 与本轮同批提交，并打附注 tag **`v1.0.0`**；未 push、未打包。 |