# gomydump

基于 Go 语言开发的表粒度 MySQL 逻辑备份与恢复工具，功能设计参考 IBM DB2 的 EXPORT/IMPORT，同时支持 MySQL 和 OceanBase MySQL 模式。

## 特性

- **多种导出格式**：支持 DEL（定界 ASCII）、SQL 和 Parquet 格式
- **并行分片导出**：基于主键/唯一索引的多线程并行导出，大幅提升大表导出性能
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
- **消息文件**：DB2 风格的操作日志记录
- **流式处理**：支持 GB 级大文件，不在内存中缓存全量数据
- **批量优化**：导入使用批量 INSERT（1000 行/批次），提升写入性能
- **密码加密**：支持 AES-256-GCM 加密密码，避免在 ps 命令中泄露明文
- **单一二进制**：纯 Go 实现，无 CGO 依赖

## 安装

### 从源码编译

```bash
git clone <repository-url>
cd mysql-backup-tool
go build -o gomydump
```

### 使用 Makefile

```bash
make build          # 编译当前平台二进制 → bin/gomydump
make linux          # 交叉编译 Linux（amd64 + arm64）
make build-all      # 交叉编译全平台（Linux / macOS / Windows）
make build COMPRESS=1  # 编译后自动 upx 快速压缩（-1，需安装 upx；未装则跳过）
make compress       # 单独对 bin/ 现有产物运行 upx -1
```

### 交叉编译

工具为纯 Go 实现、无 CGO 依赖，构建时强制 `CGO_ENABLED=0`，产物为静态链接二进制，可在任意 Linux 发行版直接运行，无需目标平台 C 工具链：

```bash
# 仅 Linux
make linux
# 产物：bin/gomydump-linux-amd64、bin/gomydump-linux-arm64

# 指定单平台
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o gomydump .
```

版本信息通过 ldflags 注入 `cmd` 包，`gomydump version` 可打印版本、Git commit 与构建时间。

### Docker

```bash
docker build -t gomydump:latest .
```

## 快速开始

### 导出数据

```bash
# 导出整张表到 DEL 格式
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"

# 导出为 SQL 格式
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.sql -f sql \
  -q "SELECT * FROM users"

# 导出为 Parquet 格式
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.parquet -f parquet \
  -q "SELECT * FROM users"

# 自定义分隔符（制表符分隔）
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.tsv \
  --col-del "\t" --no-char-del \
  -q "SELECT * FROM users"
```

### 导入数据

```bash
# 从 DEL 文件追加数据
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  -t users

# 使用 insert_update 模式（插入或更新）
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  --mode insert_update \
  -t users

# 从 SQL 文件创建表并导入（create 模式也需 -t 指定新表名）
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.sql -f sql \
  --mode create -t users
```

## 命令说明

### export - 数据导出

```bash
gomydump export [连接选项] [导出选项] [修饰选项] --query "<SELECT SQL>"
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
gomydump export -h ob-host -u "root@sys" -p password -D mydb -o /backup/users.del -q "SELECT * FROM users"

# OceanBase 三段式用户名
gomydump export -h ob-host -u "root@sys#obcluster" -p password -D mydb -o /backup/users.del -q "SELECT * FROM users"

# OceanBase 带集群 ID 的用户名
gomydump export -h ob-host -u "root@sys#obcluster:id" -p password -D mydb -o /backup/users.del -q "SELECT * FROM users"
```

**会话变量（`--session-var`）：**

在导出/导入开始前为数据库连接设置会话变量（如 `sql_mode`、`foreign_key_checks`、OceanBase 的 `query_timeout` 等），export 与 import 子命令均支持，可重复传入：

```bash
gomydump export ... --threads 4 \
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

SQL 格式还会在文件头写出**自描述字符集**语句 `/*!40101 SET NAMES <charset> */;`（mysqldump `--set-charset` 风格）：标准 `mysql` 客户端在恢复时据此设定连接字符集；gomydump 自身的导入会跳过该行，改由 `--default-charset` 对齐字符集。导入端按目标列类型对 `0x` 值做十六进制还原后写入。

**导出选项：**

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--output` | `-o` | 输出文件路径 | 无（必填） |
| `--format` | `-f` | 输出格式（del/sql/parquet） | `del` |
| `--query` | `-q` | SQL SELECT 语句（支持 OceanBase / MySQL 优化器 hint） | 无（必填） |
| `--messages` | `-m` | 消息文件路径 | 空 |
| `--no-replace` | | 拒绝覆盖已存在的输出文件（默认允许覆盖） | `false` |
| `--threads` | | 并行导出任务数（1=不并行；上限 256） | `1` |
| `--shard-key` | | 分片键列名（空=自动检测主键/唯一键） | 空 |
| `--batch-size` | | 读→写交接每批的行数（仅单线程导出流水线） | `1000` |
| `--pipeline-buffer` | | 读→写交接队列深度（批数；0=同步交接，仅单线程导出） | `10` |
| `--default-charset` | | 连接与输出文件字符集（见上方说明；utf8mb4/utf8/latin1/ascii/gbk/gb18030） | `utf8mb4` |
| `--hex-blob` | | 二进制列强制写为 `0x` 十六进制字面量（非 utf8mb4 时自动开启） | `false` |
| `--no-hex-blob` | | 强制二进制列原样透传（非 utf8mb4 下可能损坏，会打印告警）；与 `--hex-blob` 互斥 | `false` |

> 提示：DEL/Parquet 文件的列头取自查询结果集的列名。若 `--query` 含未起别名的表达式列（如 `SELECT 1, COUNT(*)`），导入时该列头（`1`、`COUNT(*)`）在目标表中不存在会报错，请为表达式列起别名（`SELECT 1 AS flag`）或导入时用 `--columns` 按位置指定目标列。

**DEL 格式修饰选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--char-del` | 字符串定界符 | `"` |
| `--col-del` | 列分隔符 | `,` |
| `--row-del` | 行分隔符 | `\n` |
| `--escape` | 转义符 | `"` |
| `--no-escape` | 不使用转义 | `false` |
| `--timestamp-fmt` | 时间戳格式（作用于 DATETIME/TIMESTAMP 列） | `yyyy-MM-dd HH:mm:ss` |
| `--date-fmt` | 日期格式（作用于 DATE 列） | `yyyy-MM-dd` |
| `--no-char-del` | 不使用字符串定界符 | `false` |
| `--no-col-names` | 不输出列名 | `false` |

**全局选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--log-level` | 日志级别（debug/info/warn/error） | `info` |
| `--error-stack` | 失败时打印完整调用堆栈（可用 `GOMYDUMP_ERROR_STACK=1`） | `false` |
| `--echo-sql-values` | 失败 SQL 回显内联真实数据值（默认脱敏，可用 `GOMYDUMP_ECHO_SQL_VALUES=1`） | `false` |
| `--quiet` | 禁止定时（5s）进度日志，仅保留配置、结束统计与 ERROR 记录（可用 `GOMYDUMP_QUIET=1`） | `false` |
| `--no-sql-tag` | 不在工具主动执行的语句前加 `/* gomydump:<commit> */` 归因注释（默认加，可用 `GOMYDUMP_NO_SQL_TAG=1`） | `false` |

**SQL 格式修饰选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--statement-size` | 单条 INSERT 语句最大字节数（超出则另起一条语句） | `8388608`（8MiB） |
| `--single-row-insert` | 每行一条 INSERT（等价 mysqldump `--skip-extended-insert`）；开启后 `--statement-size` 不再参与攒批，仅在单行语句超限时告警一次 | `false` |
| `--no-create` | 不输出 CREATE TABLE 语句 | `false` |
| `--no-data` | 不输出 INSERT 语句 | `false` |

### import - 数据导入

```bash
gomydump import [连接选项] [导入选项] [修饰选项]
```

连接选项与 export 子命令相同（`--host`/`--port`/`--user`/`--password`/`--socket`/`--database`/`--session-var`/`--default-charset` 以及 `--connect-timeout`/`--socket-read-timeout`/`--socket-write-timeout`），`--host`（或 `--socket`）、`--user`、`--password`、`--database` 为必填。导入时 `--default-charset` 须与当初导出所用的字符集一致（文件编码 == 连接字符集），否则文本列会被误解码；SQL 文件首行的 `SET NAMES` 头仅供标准 `mysql` 客户端使用，gomydump 导入以本参数为准。

**导入选项：**

| 选项 | 短选项 | 说明 | 默认值 |
|------|--------|------|--------|
| `--input` | `-i` | 输入文件路径 | 无（必填） |
| `--format` | `-f` | 输入格式（del/sql/parquet） | `del` |
| `--mode` | `-M` | 导入模式 | `insert` |
| `--table` | `-t` | 目标表名（insert/replace/insert_update 必填；create 模式可省略，自动取 SQL 文件中 CREATE 语句的表名，指定则作为新表名重写） | 无 |
| `--columns` | `-c` | 列映射（见下方说明） | 空 |
| `--messages` | `-m` | 消息文件路径 | 空 |
| `--threads` | | 并行导入任务数（1=不并行；上限 256） | `1` |
| `--batch-size` | | 读→写交接每批的行数（单线程与并行导入均生效；实际还受字节上限与占位符上限约束，见下） | `1000` |
| `--pipeline-buffer` | | 读→写交接队列深度（批数；0=同步；并行导入建议 ≥ `--threads`） | `10` |
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
| `replace` | 删除表所有数据后插入 |
| `create` | 创建表并导入数据（仅 SQL 格式） |

**DEL 格式修饰选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--char-del` | 字符串定界符 | `"` |
| `--col-del` | 列分隔符 | `,` |
| `--row-del` | 行分隔符 | `\n` |
| `--escape` | 转义符 | `"` |
| `--no-escape` | 不使用转义 | `false` |
| `--timestamp-fmt` | 时间戳格式（导入时按目标列类型回转换） | `yyyy-MM-dd HH:mm:ss` |
| `--date-fmt` | 日期格式（导入时按目标列类型回转换） | `yyyy-MM-dd` |
| `--skip-count` | 跳过前 n 行 | `0` |
| `--row-count` | 最多导入 n 行 | `0`（无限制） |
| `--commit-count` | 每 n 行提交一次事务 | `1000` |
| `--no-char-del` | 不使用字符串定界符 | `false` |
| `--no-col-names` | 输入文件第一行不是列名（无表头时须用 `-c/--columns` 指定目标列，否则报清晰错误） | `false` |

**全局选项：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--log-level` | 日志级别（debug/info/warn/error） | `info` |
| `--error-stack` | 失败时打印完整调用堆栈（可用 `GOMYDUMP_ERROR_STACK=1`） | `false` |
| `--echo-sql-values` | 失败 SQL 回显内联真实数据值（默认脱敏，可用 `GOMYDUMP_ECHO_SQL_VALUES=1`） | `false` |
| `--quiet` | 禁止定时（5s）进度日志，仅保留配置、结束统计与 ERROR 记录（可用 `GOMYDUMP_QUIET=1`） | `false` |
| `--no-sql-tag` | 不在工具主动执行的语句前加 `/* gomydump:<commit> */` 归因注释（默认加，可用 `GOMYDUMP_NO_SQL_TAG=1`） | `false` |

### version - 版本信息

```bash
gomydump version
```

### encrypt - 密码加密

```bash
gomydump encrypt <password>
```

加密数据库密码，生成 `ENC:` 前缀的密文，可在 export/import 命令中安全使用。

**参数：**
- `password`：要加密的明文密码（必填）

**环境变量：**
- `GOMYDUMP_KEY`：加密密钥（必填，用于 AES-256 加密）

**示例：**

```bash
export GOMYDUMP_KEY="my-secret-key"
gomydump encrypt "my-password"
# 输出: ENC:CiQ0mGHx0IasPOwmBKX99/9ZyjeG+QMl0Ee0uH3tYg+84S3BPOR5RqtS8w==
```

**注意：** 解密功能仅在 export/import 命令内部使用，不提供公开的 decrypt 命令，以降低安全风险。

## 高级用法

### 并行导出

```bash
# 自动检测主键并并行导出（4线程）
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "SELECT * FROM users"

# 指定分片键进行并行导出
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/orders.del \
  --threads 8 \
  --shard-key order_id \
  -q "SELECT * FROM orders"

# JOIN 查询自动降级为单线程（智能检测）
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/user_orders.del \
  --threads 4 \
  -q "SELECT u.name, o.order_id FROM users u JOIN orders o ON u.id = o.user_id"
# 注意：检测到 JOIN，自动使用单线程导出
```

> Parquet 格式不支持并行导出（分片文件无法拼接），`--threads>1` 时会自动降级为单线程。

### 流水线与内存控制

导出/导入内部是「读 goroutine → 缓冲 channel → 写 goroutine」的两段流水线（并行导入再加 N 个写 worker）。`--batch-size` 与 `--pipeline-buffer` 控制这条交接管道的**粒度**，从而**可预测地**影响峰值内存：

- 在途行数 ≈ `batch-size × (pipeline-buffer + 线程数)`，峰值内存 ≈ 在途行数 × 平均行字节。宽表/BLOB 大字段时，调小两者即可压低峰值。
- 这是**比例控制，不是字节硬上限**：Go 堆、数据库驱动的读缓冲（≈ `max_allowed_packet`）以及每条巨型 INSERT 的解析，都在本参数之外。因此**无法**用它对任意输入保证「进程 RSS ≤ 某值」。
- 需要进程级内存天花板时，用 Go 运行时软上限：`GOMEMLIMIT=6GiB gomydump import ...`（配合 `GOGC` 调整回收力度）。这才是真正约束 Go 进程占用的杠杆。
- 生效范围：`--batch-size/--pipeline-buffer` 作用于**单线程导出**、**单线程与并行导入**；**并行导出**是分片内同步流式 `scan→write`，内存几乎不随数据量增长，不受这两个参数影响。SQL 导入会把单条语句一次性解析，其规模由 `--statement-size` 约束。

### 导入攒批的两道硬上限

导出侧按**字节**拼语句（`--statement-size`），而导入侧把语句拆成行后按**行数**（`--batch-size`）重新攒批，两者并不对称：宽表或含大字段的表，几百行就可能在导入侧拼出一个超过服务端 `max_allowed_packet` 的包，在事务中途报 `error 1153 (Packet for query is too large)`。因此导入攒批额外受两道协议层上限约束，与 `--batch-size` 取最小值生效：

- **字节上限**：首次攒批前读取目标库 `SHOW VARIABLES LIKE 'max_allowed_packet'`，每批预算取该值的 9/10，下限 64 KiB；读不到（权限受限或代理屏蔽 `SHOW VARIABLES`）时按 4 MiB 保守值并打印告警。字节按值的实际宽度估算——字符串/二进制按含转义后的宽度精确计，其余标量按其最宽文本形式界定。行数或字节任一触顶即提交一批；单行自身就超过预算时独立成批（一行无法再拆分，不丢弃）。
- **占位符上限**：MySQL 预处理语句的参数个数是 2 字节无符号整数，故 `行数 × 列数 ≤ 65535`。超出时自动下调有效批大小并打印一次告警（例如 2000 列的表，批大小被压到 32 行）。

这两道上限只影响导入侧自己构造的批，不改变文件内容，也不需要新增选项。

### 优化器 Hint（`--query`）

`--query` 完整保留 SQL 中的优化器 hint，可用于控制 OceanBase / MySQL 的执行计划：

```bash
# OceanBase 并行 hint（放在 SELECT 之前）
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  --threads 4 \
  -q "/*+ parallel(8) */ SELECT id, name FROM users"

# hint 写在 SELECT 之后同样支持
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/orders.del \
  -q "SELECT /*+ INDEX(orders idx_status) */ * FROM orders WHERE status='active'"
```

单线程直接原样执行 `--query`，hint 天然保留；并行导出为注入分片条件会解析重写 SQL，重写前剥离语句最前面的 `/*+ ... */` hint，注入分片条件后再拼回，确保下发给数据库的分片查询（含分片键探测语句）仍携带 hint。

### 并行导入

```bash
# 4线程并行导入
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  --threads 4 \
  -t users
```

### 列映射导入

```bash
# 方式1：位置映射（文件3列 → 表的name,age,email列）
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/data.del \
  --columns "name,age,email" \
  --no-col-names \
  -t users

# 方式2：名称映射（显式指定文件列到表列的映射）
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/data.del \
  --columns "user_name=name,user_age=age,user_email=email" \
  -t users
```

### 无包裹符模式

```bash
# 导出无包裹符文件（纯分隔符）
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.tsv \
  --col-del "," --no-char-del \
  -q "SELECT * FROM users"

# 文件内容示例：
# name,age,email
# 张三,25,zhang@example.com
# 李四,30,li@example.com

# 导入无包裹符文件
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.tsv \
  --no-char-del \
  -t users
```

### 云环境连接加固

在云上（LB/NAT 网关、SNAT 空闲超时等），长时间没有网络往返的 TCP 连接可能被中间设备静默回收，工具在读写时会得到 `broken pipe` / `connection reset` / `invalid connection` / `gone away` 等瞬断错误。三个层面同时加固：

- **驱动层 socket 超时**（对应 JDBC `connectTimeout`/`socketTimeout`）：`--connect-timeout`（默认 5s）、`--socket-read-timeout`/`--socket-write-timeout`（默认 300s）。这三个参数以 DSN 字段（`timeout` / `readTimeout` / `writeTimeout`）形式传入 go-sql-driver，作用于**每次 socket 读/写**；由于是 DSN 级参数，`--session-var` 无法覆盖（同名会被拒绝）。
- **连接池 TTL**：并行 worker 每线程 `SetMaxOpenConns(1)` + `SetConnMaxIdleTime` + `SetConnMaxLifetime`，让连接在 LB 空闲阈值前主动老化重建，而不是被动被中间设备切断。
- **事务空闲提交 + 瞬断重试**：并行导入的 worker 在 batch 长时间不到达时（默认 30s）主动 `COMMIT` 当前部分事务，释放未提交窗口；对 `driver.ErrBadConn` / `io.EOF` / 网络超时 / `invalid connection` / `broken pipe` / `gone away` 这类瞬态错误，重连后**回放**该批未提交数据（默认 3 次尝试，指数退避 1s–4s）。`context.Canceled` 与语义错误不重试。

对导出而言，主连接使用 `SetConnMaxLifetime(0)`（不因 TTL 关闭，保证一致性快照期间连接稳定），worker 使用 TTL 池；读写超时对每条连接独立生效，长时间零字节的响应（如大 result set 起始阶段）会命中 `socket-read-timeout`，请按需调大。

### 参数边界校验

所有参数校验前置到真正读写文件/连库之前，越界或冲突一律以用法错误退出（退出码 `2`，见 3.6），不会留下半成品文件或副作用。主要边界：

| 选项 | 合法范围 | 越界行为 |
|------|----------|----------|
| `--port` | `1`–`65535` | `invalid --port`，退出码 2 |
| `--threads` | `1`–`256`（每任务一条物理连接+协程，防止打爆 FD/`max_connections`） | `invalid --threads`，退出码 2 |
| `--statement-size` | `>0` 且 ≤ `268435456`（256MiB，防止 SQL writer 拼接到 OOM） | `invalid --statement-size`，退出码 2 |
| `--batch-size` | `1`–`100000`（读→写交接每批行数） | `invalid --batch-size`，退出码 2 |
| `--pipeline-buffer` | `0`–`1024`（交接队列深度，批数；`0`=同步交接） | `invalid --pipeline-buffer`，退出码 2 |
| `--connect-timeout` / `--socket-read-timeout` / `--socket-write-timeout` | 均 `>0` | 退出码 2 |
| `--commit-count` | `≥1` | `invalid --commit-count`，退出码 2 |
| `--skip-count` / `--row-count` | `≥0`（`row-count=0` 表示无限制） | 退出码 2 |
| `--log-level` | `debug`/`info`/`warn`/`error`（大小写与首尾空格容错） | `invalid --log-level`，退出码 2（此前非法值会被静默降级为 info） |
| `--format` / `--mode` | 各自枚举白名单 | 退出码 2 |
| `--default-charset` | 字符集白名单；非 `utf8mb4` 不可用于 `parquet`；`gbk`/`gb18030` 不可用于 `sql`（按字节转义不安全） | `invalid --default-charset` / 格式冲突，退出码 2 |
| `--hex-blob` + `--no-hex-blob` | 二者互斥 | `mutually exclusive`，退出码 2 |

关联校验：`--no-create`/`--no-data`/`--statement-size`/`--single-row-insert` 仅对 `--format sql` 生效，显式传入且格式不符时报错而非静默忽略；`--mode create` 仅支持 `--format sql`。SSL/TLS 选项已移除，`--ssl-*` 现为未知选项（退出码 2）。OceanBase 租户选项 `--tenant` 已移除（见下节）；`--strip-lzeros` 与 `--dec-pt`（数值列始终输出 MySQL wire text，小数点恒为 `.`）亦已移除。早期的 `--codepage` 已移除——它只描述文件码页且对二进制列有损；连接/文件字符集改由 mysqldump 惯用法的 `--default-charset` 掌管，二进制列的字符集安全由 `--hex-blob`（非 utf8mb4 自动开启）保证（见上文"字符集与二进制编码"）。

### OceanBase 租户连接

OceanBase 的租户通过**用户名**指定，OBProxy 在握手认证阶段据此路由，支持 `user@tenant`、`user@tenant#cluster` 等形式：

```bash
gomydump export -h oceanbase-host -u 'root@my_tenant#obcluster' -p password -D mydb \
  -o /backup/users.del \
  -q "SELECT * FROM users"
```

### 自定义分隔符

```bash
# 使用十六进制分隔符（制表符 0x09）
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.tsv \
  --col-del 0x09 --no-char-del \
  -q "SELECT * FROM users"

# 使用多字节分隔符
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  --col-del "||" --row-del "\r\n" \
  -q "SELECT * FROM users"
```

### 批量导入控制

```bash
# 跳过前 1 行（列名），导入 1000 行，每 100 行提交一次
gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del \
  --skip-count 1 --row-count 1000 --commit-count 100 \
  -t users
```

### 消息文件

```bash
# 导出时记录消息文件
gomydump export -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del \
  -m /var/log/export_users.msg \
  -q "SELECT * FROM users"
```

导出/导入的运行日志同时打印**行数**与**文件字节数**，行数据全部来自服务端/驱动的真实反馈：导出侧行数按**实际写入文件的行数**统计；导入侧 `read` 按**进入写入管道的文件行数**统计（不含 `--skip-count` 跳过与被 `--row-count` 截断的行），并按 `RowsAffected` 拆分 `inserted` / `updated`（`insert_update` 模式下 `updated = affected - rows`，插入相同值时 `updated` 记 0、按新增呈现），批次执行失败的行计入 `failed`。导入、导出均每隔 **5 秒**打印一次进度（已处理条数、总速率 `rate`、区间速率 `interval_rate`、`elapsed`），结束日志打印总条数、`bytes`、`duration` 与总速率；运行中止时打印 `Import aborted` 汇总（含 `failed`）。

版本横幅（`gomydump starting`，含 version/build_time/git_commit/go_version/command）始终作为运行的**首行**日志输出，随后才是命令启动与配置参数。加 `--quiet`（或 `GOMYDUMP_QUIET=1`）可**禁止定时进度日志**，仅保留配置、结束统计与 ERROR 记录；此时 `--messages` 消息文件仍按 DB2 兼容格式记录行进度（`SQL3105N`）与统计，因为消息文件是显式产物不受静默影响。

### 错误堆栈开关

命令失败时统一以 **ERROR 级日志记录**输出（`level=ERROR msg="command failed" error="..."`），与其它诊断信息格式一致，不再单独打印裸 `Error:` 行。默认只含错误链文本；开启错误堆栈后会附带 pkg/errors 记录的完整调用栈（每一层 wrap 都带自身栈帧），便于排障。两种开启方式：

```bash
# 方式1：命令行标志（持久标志，所有子命令可用）
gomydump export --error-stack -h 127.0.0.1 -u root -p password -D mydb \
  -o /backup/users.del -q "SELECT * FROM nope"

# 方式2：环境变量（适合脚本/容器）
GOMYDUMP_ERROR_STACK=1 gomydump import -h 127.0.0.1 -u root -p password -D mydb \
  -i /backup/users.del -t users
```

输出示例（默认，无堆栈）：

```
time=... level=ERROR msg="command failed" error="failed to create exporter: Error 1146 (42S02): Table 'mydb.nope' doesn't exist"
```

开启 `--error-stack` 后，`error=` 字段内追加 pkg/errors 各层 wrap 的完整调用栈：

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

### 失败 SQL 回显与脱敏

导入/导出语句在服务端执行失败时（列不存在、类型不符、数值超长、约束冲突等），错误信息、日志和消息文件都会附带失败 SQL 的结构回显，便于定位。为防止敏感数据泄露，**默认不内联真实行数据值**，`?` 占位符只以类型/长度标签呈现：

```
Error: import failed: failed to insert batch (SQL: INSERT INTO `u` (`id`, `pw`) VALUES (?,?),(?,?) /* args: str(1), str(14), str(1), str(12) */): Error 1406 Data too long for column 'pw' at row 1
```

标签足以判断"哪一列、什么类型、多长、是否 NULL"，而不会把密码/个人信息写进长期留存的日志与消息文件。回显超过 2048 字节自动截断。确需内联真实值排障时显式开启（警告：真实数据将出现在 stderr、日志与消息文件）：

```bash
gomydump import --echo-sql-values ...
GOMYDUMP_ECHO_SQL_VALUES=1 gomydump import ...   # 环境变量方式
```

`--query` 的 SELECT 原文与 CREATE TABLE 语句不含行数据，失败时始终原文回显。

### 运行时 SQL 归因注释

默认情况下，工具在自己**主动执行**的语句前追加一段自描述注释 `/* gomydump:<commit7> */`（commit 为构建经 ldflags 注入的 git 短哈希前 7 位）。目的是在数据库 `general_log`、慢查询日志或审计流里，按该前缀即可过滤出"gomydump 执行的语句"，并据 commit 定位到具体二进制版本。

加注释的范围是**工具对数据/结构/元数据主动发起的语句**：

- 导出：主 `SELECT`、并行分片查询、列探测（`LIMIT 0`）、`SHOW CREATE TABLE`、主键/唯一键/列类型/min-max 探测、`SHOW VARIABLES version_comment`
- 导入：`INSERT` / `REPLACE` / `INSERT … ON DUPLICATE KEY UPDATE` / `DELETE` / `CREATE TABLE`、目标表与列的存在性/类型校验查询

**不加**注释的是工具的连接期与事务期内部控制语句：`SET NAMES` 与 `--session-var`（均由 DSN 在建连时发出、不经执行路径）、`SET FOREIGN_KEY_CHECKS`，以及 `START TRANSACTION` / `SET TRANSACTION` / `COMMIT` / `ROLLBACK`。

可用 `--no-sql-tag` 或 `GOMYDUMP_NO_SQL_TAG=1` 关闭该注释。dev 构建（`GitCommit=unknown`）或注入值非合法十六进制哈希时不加注释，保持日志零噪音；注释内容限定为 `[0-9a-f]`，不会提前闭合或注入。该注释**只作用于运行时发往服务端的语句，导出文件内容不受影响**（导出的 SQL 文件不写此注释）。若 `--query` 含优化器 hint `/*+ … */`，前导普通块注释不影响 hint 解析（hint 仍紧跟 `SELECT`）。

### 密码加密

为避免在 `ps` 命令中泄露明文密码，可以使用 AES-256-GCM 加密密码：

```bash
# 1. 设置加密密钥（环境变量）
export GOMYDUMP_KEY="your-secret-key"

# 2. 加密密码
gomydump encrypt "my-password"
# 输出: ENC:CiQ0mGHx0IasPOwmBKX99/9ZyjeG+QMl0Ee0uH3tYg+84S3BPOR5RqtS8w==

# 3. 使用加密后的密码执行导出/导入
gomydump export -h 127.0.0.1 -u root -p "ENC:CiQ0mGHx0IasPOwmBKX99/9ZyjeG+QMl0Ee0uH3tYg+84S3BPOR5RqtS8w==" \
  -D mydb -o backup.sql -q "SELECT * FROM users"
```

**自动化脚本示例：**

```bash
#!/bin/bash
# 设置密钥（从安全存储获取）
export GOMYDUMP_KEY="my-secret-key"

# 加密密码（只需执行一次，保存结果）
ENCRYPTED_PWD=$(gomydump encrypt "actual-password")

# 批量导出多个数据库
for db in db1 db2 db3; do
    gomydump export -h 127.0.0.1 -u root -p "$ENCRYPTED_PWD" -D "$db" -o "${db}.sql" -q "SELECT * FROM users"
done
```

**安全特性：**
- ✅ `ps` 命令看不到明文密码（只显示 `ENC:` 开头的密文）
- ✅ AES-256-GCM 加密（每次加密使用随机 nonce，相同密码加密结果不同）
- ✅ 密钥通过环境变量传递（不暴露在命令行）
- ✅ 自动解密（export/import 命令自动识别 `ENC:` 前缀并解密）

**注意事项：**
- 加密密钥必须通过 `GOMYDUMP_KEY` 环境变量设置
- 密钥丢失将无法解密密码，请妥善保管
- 建议在自动化脚本中使用密钥管理服务（如 Vault）存储密钥

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

## 修复记录（v0.1.0）

对照需求文档的系统自测发现以下缺陷，均已修复并通过端到端回归（HEX 字节指纹 + 单位/整表往返校验）：

| 缺陷 | 修复 |
|------|------|
| create 模式省略 `-t` 时 INSERT 因空表名失败并遗留脏表 | 自动取 SQL 文件首条 CREATE 语句表名回填，再按 `-t`（若提供）重写 |
| 导出侧 `--date-fmt`/`--timestamp-fmt` 解析但不生效 | DEL 写入按列类型（`common.ColumnKind`）应用文本修饰，默认值下输出与旧行为逐字节一致（`--dec-pt` 后来随参数精简移除） |
| 导入侧 `--date-fmt`/`--timestamp-fmt` 未回转换 | 查询目标表列类型后，仅对日期/时间戳列做反向转换，字符串与数值列不受影响（`--dec-pt` 后来随参数精简移除） |
| `--no-col-names` 不带 `-c` 报错晦涩 | 新增前置校验，给出清晰错误并提示使用 `--columns` |
| Parquet 非 UTF-8 二进制回导失败 | 二进制列存为原始 BYTE_ARRAY（去 UTF-8 标注），非 UTF-8 内容完整往返 |
| DEL/Parquet 导入未校验目标列，表达式列头（如 `SELECT 1` 产生的 `"1"`）报 `Unknown column '1' in 'field list'`；replace 模式还会先清空表再失败 | 导入前用 `information_schema` 校验目标列存在性（大小写不敏感），缺失时立即给出含修复建议的错误（起别名或用 `--columns`），表不存在同样明确报错；校验在 mode 处理之前执行，replace 模式不再发生"先清表后报错"的数据丢失 |
| 语句执行失败时错误信息不含 SQL，难以定位 | 失败 SQL 随错误回显到 stderr/日志/消息文件；默认脱敏（占位符保留，值以 `str(14)/num/NULL/bytes(256)` 类型标签呈现），2048 字节截断并保持引号配平；`--echo-sql-values` / `GOMYDUMP_ECHO_SQL_VALUES=1` 可显式内联真实值 |
| 失败时以裸 `Error:` 行输出、与其它日志格式不一致；版本信息不在首行；进度日志无法关闭 | 命令失败统一走 `level=ERROR msg="command failed"` 日志记录（`--error-stack` 追加调用栈）；版本横幅 `gomydump starting` 提前为运行首行；新增 `--quiet` / `GOMYDUMP_QUIET=1` 禁止定时进度日志（消息文件进度不受影响） |

## 项目结构

```
mysql-backup-tool/
├── main.go                    # 程序入口
├── cmd/
│   ├── root.go                # 根命令和版本命令
│   ├── export.go              # export 子命令（RunE 与参数校验）
│   ├── export_flags.go        # export 子命令 flag 注册
│   ├── import.go              # import 子命令（RunE 与参数校验）
│   ├── import_flags.go        # import 子命令 flag 注册
│   ├── validate.go            # 共享参数边界校验（port/threads/statement-size/log-level/分隔符解析）
│   └── encrypt.go             # encrypt 子命令（密码加密）
├── internal/
│   ├── config/
│   │   └── config.go            # 配置定义与默认值（连接/超时/字符集/DelModifier/导出导入选项）
│   ├── common/
│   │   ├── util.go              # DSN 构建（超时/字符集/会话变量注入）与工具函数
│   │   ├── colkind.go           # 列类型分类（DEL 文本修饰与回转换、二进制列判定）
│   │   ├── sessionvars.go       # 会话变量解析与 DSN 注入（--session-var）
│   │   ├── sqlpreview.go        # 失败 SQL 脱敏回显（--echo-sql-values）
│   │   ├── sqltag.go            # 运行时 SQL 归因注释 /* gomydump:<commit> */（--no-sql-tag）
│   │   ├── usage.go             # 用法错误分类（退出码 2）
│   │   ├── log.go               # 日志模块（java/text 两种格式、版本信息、启动日志）
│   │   ├── stats.go             # 吞吐统计与速率计算
│   │   ├── message.go           # 消息文件处理
│   │   ├── password.go          # 密码加密/解密（AES-256-GCM）
│   │   └── detect.go            # 服务器类型检测
│   ├── export/
│   │   ├── exporter.go          # 导出调度器（单线程流水线、并行分片与合并、hex-blob 判定）
│   │   ├── del_writer.go        # DEL 格式写入器（双写/反斜杠转义、字段缓存优化）
│   │   ├── sql_writer.go        # SQL 格式写入器（按字节攒批、单行模式、快速路径优化）
│   │   ├── parquet_writer.go    # Parquet 格式写入器（列式存储）
│   │   ├── shard.go             # 分片逻辑（主键/唯一键检测）
│   │   └── sql_parser.go        # SQL 解析器（JOIN 检测）
│   └── import/
│       ├── importer.go          # 导入调度器（单线程流水线、并行 worker、列映射、攒批）
│       ├── batchcap.go          # 攒批上限（max_allowed_packet 字节预算 + 65535 占位符）
│       ├── record_reader.go     # 无上限记录读取器（自定义 SplitFunc，零拷贝别名）
│       ├── del_reader.go        # DEL 格式读取器
│       ├── sql_reader.go        # SQL 格式读取器
│       └── parquet_reader.go    # Parquet 格式读取器
├── test/
│   └── integration/             # 离线往返测试（不连库，随 go test ./... 执行）
│       └── roundtrip_test.go    # 导出写入器→导入读取器：转义/换行/0x/NULL 与空串/单行 INSERT/DEL 修饰符
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
