# CONTEXT — imex 工程状态快照

> 快照时间 **2026-11-03（UTC）**。这一页只回答四件事：现在在哪、已经定了什么、接下来等什么、怎么自证。
> 权威文档仍是 `README.md`（使用者视角）与 `docs/requirements.md`（设计与取证视角）。只有那两处的
> 功能记录 / 变更记录 受 append-only 约束；**本文件不受**，状态变了就地改，不留历史。

## 一、当前坐标

- **仓库**：模块 `github.com/imex`，远端 `https://github.com/yyt030/imex`，分支 `main`（工作副本 `<workspace>/imex`）。
- **HEAD**：本轮（变更记录 **128**：用户指令「commit tag push」，`Makefile` 第 1 行 0.17.1 → **0.18.0** 并打附注 tag `v0.18.0`）只交**一笔**提交——`Makefile` 一行、`docs/requirements.md` 新增 128 行、本文件状态更新，按 standing 指令直推 `main`，tag 单独推（见六节）。本行刻意不写这一笔的提交号（写法依据是 `157f2b9`）：**判收口的口径是 `ls-remote --heads origin` 与本地 `git rev-parse HEAD` 实测一致，且 `git rev-parse v0.18.0^{commit}` 等于同一个号**，而不是比对某个自己刚写下的号。本轮提交之下，链往回数依次是：`3f0a861` 与 `d73a6f3`（127 轮两笔：代码 + 测试 + 三份文档一笔、本文件状态补记一笔）、`0095741`（126 轮，测试-only：给 `internal/list/render_test.go` 补 CR、LF 两条转义用例）、`157f2b9` 与 `263effd`（CONTEXT 两笔状态补记）、`5c219df`（125 轮：`--engine mysql|pg` 与 PostgreSQL 支持，35 个文件、4,114 增 323 删，其中 9 个新建文件）、`c6ef115`（CONTEXT 补两条真库环境约束）、`78e4f93`（补记 124 的真库复跑取证）、`5103318`（变更记录 124：移除 `--socket`，连接只剩 TCP）、`ccf2929`（变更记录 123）、`e1fa753`（转义轮，122 轮）、`ec88e9f`（变更记录 9 行结构修复，121 轮）、`fc1844a`（`Makefile` 进位 0.17.1 + 本文件状态更新，tag `v0.17.1`）、`6587ad1`（脱敏轮，变更记录 119）、`96f1f30`（cli-oracle note 25，118 轮）、`7e61e72`（变更记录 117 补注）、`29f8b25`（原 HANDOFF.md 所在，tag `v0.17.0`）。**注（2026-11-02 脱敏轮，变更记录 119）**：`HANDOFF.md` 已从仓库移除——它的八节内容被本文件第三、五节与变更记录 117 的「注（发布收尾）」逐条覆盖，118 轮对它做的 10 行改动（`4934dd2` 的处理过程）也已由 118 正文与本文件 2.3、4.4 承载，无独有信息；同一轮把本文件第五节 QA 命令里的真实测试库凭据换成占位串。
- **已推送**：`v0.18.0` 收口的正是 `v0.17.1` 之后那 **12** 个提交（`ec88e9f`、`e1fa753`、`ccf2929`、`5103318`、`78e4f93`、`c6ef115`、`5c219df`、`263effd`、`157f2b9`、`0095741`、`d73a6f3`、`3f0a861`；`git log v0.17.1..HEAD` 实测，累计 **56** 文件 4,717 增 1,032 删），它们与本轮的 bump 提交一起按 standing 指令直推 `main`，最后一次推完再测 `ls-remote --heads origin` 与 `git rev-parse HEAD` 一致即算收口。**tag 不随 `git push` 走**，`v0.18.0` 要在 `main` 推成之后单独 `git push origin v0.18.0`。纯文档轮与代码轮本身**不进位、不打 tag**——进位只在用户点名时做一次（本轮就是那次）；`v0.17.1`（→ `fc1844a`）、`v0.17.0`（→ `29f8b25`）与 `v0.16.0`（→ `08ffa85`）不动、不移、不重打。推 `main` 在这套环境里要靠 `git -c http.version=HTTP/1.1` 绕开 HTTP2 framing / GnuTLS -110，clone 同理。**125 轮对这条纪律作过一处更正并沿用至今**：连用 `HTTP/1.1` 两次都仍报 `GnuTLS recv error (-110)`，第三次带 `-c http.version=HTTP/1.0`（git 会 warning `unknown value`、回落默认值）才推上去，而同一次会话里 `ls-remote` 用 `HTTP/1.1` 是通的、下一轮补记用 `HTTP/1.1` 也一次通过——所以 `-110` 更像 push 路径上的偶发断连，而不是 HTTP2 framing 的必然结果；实操口径保持「先 `HTTP/1.1`，失败就原样重试两三次」，别把一次 `-110` 当成网络策略已变。
- **已发布**：最新 tag 由 `v0.17.1` 进位到 **`v0.18.0`**（2026-11-03 按用户「commit tag push」执行；档位依据与前置四件的逐条核对记在变更记录 **128**，纯文档轮收口 `v0.17.1` 的先例是 `v0.5.1` 与 `v0.5.2`，见三节第 2 条与变更记录 120 ①）。`git tag -l` 实测由 **20** 个增至 **21** 个。远端 heads 只有 `main`。
- **工作树**：本轮的 bump 提交推完后以「`git status --short` 为空 + `ls-remote --heads origin` 与 `git rev-parse HEAD` 一致 + `v0.18.0^{commit}` 等于同一个号」为收口判据（打 tag 前实测为空、跟踪文件 **137** 个）。本轮**零代码、零依赖改动**——`go.mod` 与 `go.sum` 一字未动，改的只有 `Makefile` 第 1 行与两份文档。更早那条遗留已随 125 轮了结：`go get gitee.com/chunanyong/dm`（达梦评估轮）带进来的三条 indirect **不在工作树里了**（`grep -c chunanyong go.mod go.sum` 均 **0**，HEAD 的 `go.mod` 也从来没有它们）——`go mod tidy` 因无人 import 而摘除，达梦支持仍未落地，将来随真实引用一起进。125 轮 `go.mod` 改的是 pgx：新增直接依赖 `github.com/jackc/pgx/v5 v5.12.0`、`pflag` 升为直接引用、五条新 indirect。2026-11-02 换新沙箱后按 `https://github.com/yyt030/imex` 重新 clone，完整历史、非浅克隆；`bin/`、`dist/`、`*.shard_*.tmp` 由 `.gitignore` 覆盖。
- **本地开发库**：**本沙箱有两个真库床（2026-11-03）**——① MariaDB **10.6.23**（`1:10.6.23-0ubuntu0.22.04.1`）：`apt-get install -y --no-install-recommends mariadb-server mariadb-client`，装前用 `/usr/sbin/policy-rc.d`（`exit 101`）挡住自动启服务，datadir 沿用 `/var/lib/mysql`，配置自建于 `/tmp/mysqld/my.cnf`（`user=mysql`、`socket=/tmp/mysqld/mysql.sock`、`bind-address=127.0.0.1`、`port=3306`、`skip-name-resolve`、`general_log_file` 只设路径不设 `general_log`，实测 `@@general_log=0`；`innodb_buffer_pool_size` 留 **256M**）。库：`imex_it`（集成测试床）与 `imex_test`；账号 `gd`@`127.0.0.1`，口令 20 字符随机、只存 `/tmp/mysqld/imex_test_pw`。② PostgreSQL **14.24**：`postgres://pgprobe:<20 字符随机>@127.0.0.1:5432/pgprobe?sslmode=disable`，口令只存运行环境；该角色**不能** `CREATE DATABASE`、在 `public` 上有 CREATE，所以 pg 床建在 `pgprobe` 库内：`internal/list/queries_pg_test.go` 的 `TestPGLiveServer` 在同库开两个 schema（`public` 与 `imex_list_it`）断言「带 `-D` 只回 `current_schema()` 的对象」「不带 `-D` 时两个非系统 schema 都出现」，而**换 schema 本身（改角色 `search_path`）没有用例**——工具不给 schema 选项，这条只能靠环境构造（已写进 requirements 11.2）。两库口令都不写进仓库（口径同变更记录 59 / 119）。上一沙箱的 `imex_ddl*` 等夹具库没有随迁移带来，也不需要——集成用例自建表。

## 二、已完成事项

### 2.1 v0.17.0 轮（代码在 `c03f2c9`，已发布：tag `v0.17.0` 落在 `29f8b25`）

实现 `docs/requirements.md` 3.6 的设计定稿，并修掉 116 之后 CLI 规约审计里的 Z 档缺陷。以下数字除标注外均取自变更记录 117 的当轮实测。

- **新增 `show` 命令**：六个 kind `tab` / `vws` / `trg` / `evt` / `prc` / `fcn`，英文整词（`table`、`view`…）为别名。产物只有服务器返回的 DDL 原文加结尾 `;` 与换行，不重写文本；六 kind 实测 230 / 174 / 120 / 146 / 105 / 116 B。代码在 `internal/show/show.go` + `cmd/show.go`。
- **`list` 与 `show` 共用一套输出通道**（`cmd/output_channel.go`）：八条 `list` 叶子与六个 `show` kind 各加 `-o, --output` 与 `--no-overwrite`，`-m` 文案统一为一个共享常量。`show tab sh_t -D imex_test` 四格实测：`-o` 230 B + stderr 385 B；`-o` 230 B + 日志 388 B 且屏幕 0 B；stdout 230 B + stderr 376 B；同一文件 606 B（日志 376 + 结果 230）且屏幕 0 B。
- **同路径守卫** `validateDistinctPaths`：`-o` 与 `-m` 指向同一文件时退出 **2**（覆盖写与追加写不可共存于一途），`list`/`show` 侧附退路文案、`export`/`import` 必填侧不给退路。它是 `validateExportConfig`/`validateImportConfig` 的第一条语句，import 侧只有 `-M create` 才够得着。
- **`--no-overwrite` 命中已存在文件是运行时错误退出 1**，文案与 `export` 逐字相同，原文件不动；`-m` 的追加语义跨运行不变。
- **审计 Z 档四条修复**：三条超时选项的帮助去掉 JDBC 括注（并同步改 2.1.2 与 README 的同一说法）、`list` 的 `-t` 帮助补 `(required)`、`list --help` 首句改写成通道口径、`-m` 文案随实测更新。审计里另一条**刻意不改**：`--default-charset` 不再枚举可取字符集是 2026-10-29 帮助收敛轮有意为之，已作为 intentional delta 记进 cli-oracle note 24。
- **由单测查出的真实缺陷**：`SplitName` 原先用 `strings.Index` 找第一个反引号，双反引号转义会把名字提前切断；改为成对消费的 `scanQuoted`，读不懂的输入整串交服务器而不猜切分点。
- **两条复测更正**（写进 3.6 / 2.6 / README，并在已提交的变更记录 116 行内加带日期的「注」）：① 设计文里「简单语句触发器可回放」不成立——`internal/import/sql_reader.go:ReadHeader` 只认 `CREATE TABLE`/`CREATE TEMPORARY` 与 `INSERT`，其余语句跳过并置 `unrecognized`，纯触发器 DDL 文件被 `the input file contains no importable data` 拒绝；② 视图 `CREATE OR REPLACE` 幂等那两条退出 0 是用 mysql 客户端跑的、不经本 reader，不作为 imex 能力宣称。
- **文档面**：3.6 标题与首段改为「已落地」并补「落地状态」段；新增 2.6 `show` 使用节；3.4.1 必覆盖清单补 `list` 八对象与 `show` 六 kind；2.5 输出契约表补五行；2.3 补「结果与日志同文件」；3.2 重写 `-?` 适用范围；1.1 补 `list` 与 `show`；九节结构树补齐缺失文件并校正连接符；十一节场景表补六行、资产表补九行；十二节计数纠正。
- **cli-oracle 重基线**：13 段 / 796 行 → **20 段 / 1,079 行**，且**每个 help 体与二进制逐字节相同**（`## import` 首次整体重取，折叠 note 6–14/21 的累计 delta；改动前的 13 段基线可用 `git show v0.16.0:docs/cli-oracle.txt` 取回）。
- **测试资产**：`_test.go` 55 → **61**，`^func Test` 319 → **378**，集成文件 7 → **9**（Fuzz 6、Benchmark 12 不变）。
- **本轮唯一的行为破坏**：`imex list tab -D d -m log.txt > rows.txt` 这类既有调用今后拿到空文件（无 `-o` 且带 `-m` 时结果并进 `-m` 文件末尾），已按 114 ② 的先例记进变更记录而不升 X 档。

### 2.2 里程碑（**21** 个 tag，摘要取自各 tag 的附注信息；`git tag -l` 在 `v0.18.0` 之前实测 **20** 个，本表此前只列到 `v0.16.0`，本轮补上 `v0.17.0`/`v0.17.1`/`v0.18.0` 三行，计数随之从 19 更正）

| tag | 一句话 |
|---|---|
| `v0.18.0` | 双引擎与仅数据 SQL 导出：新增 `--engine mysql\|pg`（PostgreSQL 14 实床，八类能力明确拒绝），删 `--socket`/`-S`/`IMEX_SOCKET` 与 `--create`/`--no-data`，SQL 导出从此只写数据、建表语句归 `imex show tab` |
| `v0.17.1` | 纯文档轮收口（117 补注 + cli-oracle note 25 + 脱敏复核），五面契约与代码零改动，取 patch 位 |
| `v0.17.0` | 新增 `show` 对象定义导出（tab/vws/trg/evt/prc/fcn）与 `list`/`show` 共用输出通道模型（`-o`/`--no-overwrite`/同路径拒绝），修 CLI 规约审计 Z 档，并附工程状态快照 |
| `v0.16.0` | 交互 SQL 回显默认 info 级可见并补齐 `list` 与快照语句覆盖，单条记录截到 512 字节 |
| `v0.15.0` | 缺口令提示点名 `--password`/`IMEX_PASSWORD`；按指令移除 `scripts` 批量导出脚本 |
| `v0.14.0` | 六个连接选项支持 `IMEX_*` 环境变量 |
| `v0.13.0` | `-q` 的 `schema.table` 走并行导出且点名的 schema 优先于 `-D` |
| `v0.12.1` | 限定库名的并行导出不再发非法分片 SQL、不再挂起（缺陷档） |
| `v0.12.0` | 导入分块游标与导出分配/转换优化：1M 行导入 RSS 141→45 MB，产物逐字节不变 |
| `v0.10.0` | 导出读路径池化与时钟采样：300 万行 SQL 合并 −28%，分配 −72% |
| `v0.9.0` | SQL 数值裸字面量 + 写入器字节追加：300 万行实测 −15%/−25%，产物逐字节不变 |
| `v0.8.0` | 删除 `--quiet` 与 `list --format`、`--query-file`，`-q` 支持 `@file` |
| `v0.7.0` | `-m` 文件默认追加写，交付 `list` 命令与 `tab` 别名 |
| `v0.6.0` | **版本纠正轮**：1.0.0 → 0.6.0（X 保持 0），行为与纠正前一致 |
| `v0.5.2` / `v0.5.1` | 纯文档轮（DuckDB 集成评估搁置结论；≤118 字节帮助规范入册） |
| `v0.5.0` | 名称映射只要求写出来的映射有效（可子集、顺序任意），帮助文案精简 |
| `v0.4.0` | 导入排除生成列、批字节预算改 append 前预判、空 SQL 文件按 0 行成功 |
| `v0.3.0` | 新增 `--parquet-compression`（none/snappy/zstd），`--mode create` 补建表预检 |
| `v0.2.2` / `v0.2.1` | 纯缺陷轮：参数边界七项与被忽略选项出声；日志耗时固定三位小数、debug 级回显 SQL |
| `v0.2.0` | 并行导出改采样分片边界并按段数超配 worker（仓库**第一个 tag**，`v0.1.0` 从未回填） |

三处需要留意的历史事实（现查所得，别按 README 段落名去反推 tag）：**没有 `v0.11.0` 这个 tag**——README 的「修复记录（v0.11.0）」段是随 `76a20fb`（升到 0.12.0 那一轮）一起补上的文档分段，仓库里也没有任何一次提交把 `Makefile` 写成 `VERSION ?= 0.11.0`；`v0.6.0` 是对更早那次 X 档（1.0.0）的纠正，说明 0.x 期不兼容变更的口径当时已改为记在 Y 上（AGENTS.md 第 9 条仍把这点列为待定项）；**今天 `git tag -l` 里也没有 `v0.6.0` 这个对象**（2026-11-03 现查：`v0.5.2` 与 `v0.7.0` 之间为空，共 20 个），而 README 与本表都按 `v0.6.0` 记着那次纠正——要么纠正轮当时未留 tag，要么后来被删，本轮只把这条现查记下，历史文本一字未改。

### 2.3 v0.17.0 之后的文档轮（2026-11-02，当时未进位、未打 tag）

变更记录 **118** 行。只做一件：把 4.4 那条「基线提交号 `4934dd2` 解析不到」的文档缺陷按已给的方向处理掉——`docs/cli-oracle.txt` 第 1 行的基线说明补一句可解析指向，文末加 **Provenance note 25**。历史文本一处未删、未替换：oracle notes 1–24 的 9 处、`## version` 段那条 `Git commit: 4934dd2` 输出快照、README 功能记录 2 处、本表历史行 8 处全部原样保留（沿用变更记录 63 行的裁定）。oracle 由 1,079 → **1,106 行**，仍 20 段，20 个 help 体未变。版本仍 `0.17.0`（`Makefile` 第 1 行未动），按三节第 2 条纯文档轮不单独打 tag，`v0.17.0` 不动。

**本沙箱 QA 实况**：`gofmt -l .` 空、`go vet` 净、9 包 `-race` 全 ok、`make build` 通过、20 个帮助面 118 字节守卫 0 行超限、`statementCapSlack` = 3；**只有真库 206 用例未复跑**（环境无 MariaDB，见一节）。

### 2.4 v0.17.1 收口 tag（2026-11-02）

变更记录 **120** 行。用户指令「commit tag push」，并选定进位到 **v0.17.1**——覆盖三节第 2 条「纯文档轮不单独打 tag」，先例是 `v0.5.1` 与 `v0.5.2`。`Makefile` 第 1 行 0.17.0 → 0.17.1，与 tag 名同一次提交同步；被 tag 收口的三个提交是 `7e61e72`（117 补注）、`96f1f30`（note 25 / 118 轮）、`6587ad1`（脱敏 / 119 轮）。五面契约全部未变、代码零改动，所以取 patch 位。

**该轮 QA 实况**（同环境，无 MariaDB）：`make build` 退出 0、`imex version` 报 `0.17.1`、`gofmt -l .` 空、`go vet` 净、9 包 `-race` 全 ok、20 面 118 字节守卫 0 行超限、`statementCapSlack` = 3、`_test.go` 61 / `^func Test` 378；真库 206 用例仍未复跑，「QA 全绿」按「除真库外全绿」放行（120 ⑤）。

### 2.5 文档表格收口三轮（2026-11-02，未进位、未打 tag）

变更记录 **121 / 122 / 123** 三行，全是 Markdown，代码零改动。121 把变更记录里 9 条违反三节第 3 条「一个物理行、恰好 4 个竖线」的历史行修好（6 行补转义、3 行补格与并格，逐行做「还原后与改前逐字节相同」的证真）；122 与 123 按用户指令「两次提交」处理 121 ④ 剩下的普通表格缺陷——判据收紧为「表格块表头定格数、其后每个以竖线开头的行都比」后实测 10 个物理行有问题，先转义 4 行里的 7 处内容竖线（122），再补格 3 行、并格 1 行、删虚格 1 行、合行 1 段（123，含把代码段里的真控制符改成可见转义串，取序依据 `docs/requirements.md` 2.5 输出契约表第 615 行与 `internal/list/render.go:15-20`，属推断已写明）。

**收口后实测**：编号变更记录 **123** 条、未转义竖线不为 4 的行为 **0** 条；两份文档普通表格单元数不匹配行 **0**；表格行里裸制表符与回车 **0**；`README.md` 1,270 行、`docs/requirements.md` 1,932 行。**本轮 QA 实况**（同环境，无 MariaDB）：`gofmt -l .` 空、`go vet` 退出 0、`make build` 退出 0、`./bin/imex version` 报 `0.17.1`、工作树凭据两种形态命中 0。123 ⑤ 另记一条**已查明未改**的内容缺陷：11.1 资产表里 `internal/list` 那行宣称四个转义，`render_test.go` 实际只断言反斜杠与制表符两例，已挂进 4.1 等裁定。

### 2.6 移除 socket 传输（2026-11-03，变更记录 124，未进位、未打 tag）

用户指令「先把--socket去掉，用处有限。」。`--socket` / `-S` 与 `IMEX_SOCKET` 整体删除、不留兼容层（口径同 101 删 `--quiet`、102 删 `list -f/--format`、62 删 `--single-row-insert`），`internal/common/util.go:BuildDSN` 从此无条件 `cfg.Net = "tcp"`。改动规模：Go 侧 14 文件 **75 增 167 删**（含测试），文档 3 文件。`cmd/conn_env.go` **117 → 93 行**，`connEnvReport.skipped` 作为死字段一起删；三条超时选项（`--connect-timeout`/`--socket-read-timeout`/`--socket-write-timeout`）**名字与默认值都不动**——它们约束的本来就是 TCP 上的单次读/写。

**实测拒绝形态**：`export --socket`、`show tab --socket` 报 `unknown flag: --socket`，`import -S`、`list tab -S` 报 `unknown shorthand flag: 'S' in -S`，四条退出码全 **2**；设了 `IMEX_SOCKET` 后 INFO 记录只列五个变量、连接仍 `dial tcp 127.0.0.1:3307`；必填文案变成 `--host is required`（`-h ""` 实测退出 **2**），`connection variable ignored` 那条 WARN 整体消失。`docs/cli-oracle.txt` 走 **Re-baselined note 26**：十六行离开文件（export/import 各一、八个 `list <object>`、六个 `show <kind>`），**19 个活体与本树二进制的 help 全部 diff EMPTY**，`## version` 仍冻结，1,106 → **1,105** 行、仍 20 段、0 行超 118 字节。测试资产：`_test.go` **61** 不变、`^func Test` **378 → 377**（`cmd/conn_env_test.go` 10 → 9，新增 `TestSocketTransportIsGone` 钉住「profile 里残留的 `IMEX_SOCKET` 永不生效」）、Fuzz **6**、Benchmark **12**。

**本沙箱 QA 实况**：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 退出 0、`CGO_ENABLED=1 go test -race -count=1 ./...` **九包全通过**（`cmd` 13.983 s、`internal/common` 44.211 s、`test/integration` 1.030 s = 当时还没真库、用例全跳过）、`make build` 退出 0（`bin/imex` 22,368,418 B、`imex version` 报 `0.17.1`）、20 面 118 字节守卫 0 行超限、`statementCapSlack` = **3**。真库在同一轮搭起来之后补跑过了：集成 **206 RUN / 206 PASS（70 顶层 + 136 子用例）/ 0 SKIP / 0 FAIL**（39.053 s），取证见变更记录 124 ⑨ 的注。**按用户 standing 指令直接推 `main`，不打 tag**；版本号按 113 的先例留给下一次进位指令。

**顺带发现、本轮未动**：变更记录的子表里有 **3** 处空行插在**同一个子表的两个数据行之间**（现 `docs/requirements.md` 第 **1816** 行分隔 32 与 33、第 **1883** 行分隔 77 与 78、第 **1908** 行分隔 101 与 102），Markdown 渲染时会把一张表断成两张。判据按「空行的上一行与下一行都以竖线开头」实测，非本轮引入，也不是 121/122/123 修的竖线数与格数问题，等裁定。

### 2.7 PostgreSQL 引擎（2026-11-03，变更记录 125，提交 `5c219df` 已推 `main`，未进位、未打 tag）

用户指令「实现支持pg数据库。」。新增 `--engine mysql|pg`（默认 `mysql`，**没有 `IMEX_ENGINE`**——面④ 一字不变），把散在 22 个文件里的 83 处 MySQL 专有 SQL 收进方言层 `internal/backend`（`Backend` 接口 **24** 个方法 + `mysql.go`/`postgres.go` 两实现），`list` 另分 `queries.go`/`queries_pg.go` 两支。规模：新增非测试 **1,623** 行、新增测试 **1,827** 行（四文件），20 个既有 Go 文件 432 增 283 删；`make build` 产物 **22,368,418 → 26,890,402 B**。

**pg 侧放行**：`export -f del`/`-f parquet`（单线程）、`import -f del`/`-f parquet`（`--mode insert`）、`list` 七个对象。**八类拒绝**（全部退出 2、发生在建连之前）：`show` 六 kind、`list evt`、两侧 `-f sql`、两侧 `--threads>1`、`--session-var`、`--no-hex-blob`、`--mode replace|create|insert_update`、`client_encoding` 表外的 `--default-charset`。两级命名空间规则：`-D` 归连接，对象范围取 `current_schema()`，系统 schema 默认排除、`--all` 取消。

**MySQL 不变的取证**（本轮最硬的一条）：HEAD 用 `git worktree` + 同一条 `make build` 造基线二进制，同床 43 个命令形态、每趟 **404** 条 `general_log` 语句，顺序敏感逐条比对 → **41** 全同 + **2**（两条 `--threads 4`）按连接分组后全同 + **0** 差异；200,000 行表 DEL 与 Parquet 产物 md5 全同；三项耗时不劣化。**方法学教训两条**：`general_log_file` 的目录必须 `mysql` 用户可写（否则 1231、比对跑在空集上却报「全部 IDENTICAL」），以及基线二进制的 provenance 要现查（`/tmp/imex-before` 是 `ccf2929`、还带 `--socket`）。

**pg 真库取证**：PostgreSQL 14.24 / `pgprobe` / `public` schema；11 列 × 12 行 fixture 含 NULL、空串、`-`、引号、TAB/LF、尾随空格、反斜杠、CJK+emoji、`infinity`/`-infinity`、`±Inf`/`NaN`、带裸 `0x00` 的 `bytea`；实测裸 NUL 走文本协议报 `SQLSTATE 22021`（这就是恒开 hex 的理由），认证失败 `SQLSTATE 28P01` 不重试退 1。快照步骤两边不同（MySQL 两步 vs `BEGIN ISOLATION LEVEL REPEATABLE READ` 一步），`max_connections` 读法不同，无连接时的批次预算 pg 取 `1<<30`。

**QA 实况**：`gofmt -l .` **0 行**、`go vet` 无输出、`go test -race -count=1 ./...` **十包全 ok**、两引擎真库合跑 **219** `--- PASS`（76 顶层 + 143 子用例；MySQL 206、PG 13）/ **0** FAIL / **0** SKIP（38.047 s）、20 面 118 字节守卫 0 行超限、`statementCapSlack` = 3、oracle `sections=20 identical=20 drift=0`。资产：`_test.go` **65**、`^func Test` **410**、Fuzz **6**、Benchmark **12**。oracle 走 **Re-baselined note 27**：1,105 → **1,150** 行、16 个面各进一行 `--engine`、0 行离开。

**本轮自纠的三条断言**（都是刚写下的错话，改回后已复核）：① 2.5 初稿说 pg 的 `LIKE` 模式串走 `LOWER()`——实际 `queries_pg.go` 里没有 `LOWER`，折叠只在导入侧列名匹配；② `cmd/engine.go`/`postgres.go` 注释说默认端口「会被记录」「`list config` 里看得到」——实测两处都不成立；③ 2.1.11 规模段的 1,620/537/196 与「23 文件 502 增 286 删」是改注释前的数，重取为 1,623/538/198 与 20 文件 432 增 283 删。**顺带修掉两处 README 旧错**：缺口令文案是四条命令一致（不是三条）、连接选项是五个（不是六个）。

**遗留**：pg 的 `-f sql`/并行/`show` 是明确拒绝而非半成品；11.2 那行点名的三个 pg 缺口不补（`Open()` URL 四参数无用例、pg parquet 只有小表窄类型、只在 14.24 验过）；达梦驱动依赖已被 `go mod tidy` 摘掉，实现轮再随真实引用进来；`docs/requirements.md` 3.7 仍未合进达梦方案与本轮 PG 结论。

### 2.8 11.1 的两条转义用例（2026-11-03，变更记录 126，测试-only，未进位、未打 tag）

用户指令「11.1 补两个案例」，把 123 ⑤ 挂起的那条裁定了结：选**补用例**而不是改文案，因为 `internal/list/render.go` 的 `escaper` 本来就映射了 `\`、`\t`、`\r`、`\n`，缺的是断言不是能力。新增 `TestRenderTableEscapesCarriageReturn` 与 `TestRenderTableEscapesLineFeed`（后者在同一格叠一个数据反斜杠，一次钉住「数据反斜杠翻倍、转义引入的不翻倍」两半规则），只动一个测试文件 +53 行、生产代码零改动，`make build` 尺寸与 125 轮逐字节同为 **26,890,402 B**。

**变异复跑**证明新用例不空转、也证明 123 ⑤ 的判断成立：删掉 `escaper` 的 `"\r"`/`"\n"` 两条映射后，失败的恰是新增两例、原三例全过；删掉 `"\\"` 那条则 `TestRenderTableEscapesLineFeed` 也失败。两次变异都从 `/tmp/render.go.bak` 还原后复跑通过。规模：仓库 `_test.go` 仍 65 个、`^func Test` **410 → 412**、`Fuzz*` 6 / `Benchmark*` 12 不变、`internal/list` 22 → 24（`render_test.go` 3 → 5），11.1 表内本行、首句与逐行加总三处同步更正。

**环境事件**：本轮第一次真库复跑拿到 64 FAIL，签名全是 `connect: connection refused`——`uptime -s` 查到 **08:00:24**，即沙箱虚拟机重启把 MariaDB 与 PostgreSQL 一起带走（`/tmp/mysqld/mysqld.pid` 还是 05:08 的死文件）。按第五节原命令重启两个库（PG 走 redo 后 ready），夹具数据无损，复跑即 219/0/0。**教训：真库大面积 connection refused 先排环境，别改断言。**

### 2.9 SQL 导出改为仅数据（2026-11-03，变更记录 127，未进位、未打 tag）

用户指令「export -f sql 修改成只导出数据。具体的ddl操作全部移到show tab中。」，追问后裁定「--create 与 --no-data 是直接删掉」。`-f sql` 的产物从此恒为 `SET NAMES` 一行加 INSERT，`CREATE TABLE`、`DROP TABLE` 与两类节注释一律不再出现；全仓库只有 `internal/show` 读 `SHOW CREATE TABLE`。两个选项**无别名、无弃用兼容层**，给了就是 `unknown flag` 且退出 **2**（口径同 124 删 `--socket`、101 删 `--quiet`、102 删 `list -f/--format`、62/100 删 `--single-row-insert`/`--no-create`）。规模：非测试 Go **11** 文件 **46 增 158 删**（204 个改动行），测试 **10** 文件 **261 增 250 删**（511 行），`make build` 产物 **26,890,402 → 26,882,210 B**。主体在 `internal/export/sql_writer.go`（结构体失去三个 DDL 字段、构造函数七参降五参、`WriteHeader` 只剩字符集行）与 `exporter.go`（删预取块与 `getCreateTableStatement`）。

**取证四条**：① 默认路径逐字节不变——基线二进制（`git worktree` 检出 HEAD 后同一条 `make build`）与本轮跑同一条 `-f sql` 导出，都是 **271 B**、md5 **9952746dccb3e3b82b68d6a7d4d5f74d**，`--threads 4` 同一 md5，说明删掉的只是 opt-in；② 差额可算——基线 `--create` 产物 **544 B**，本轮等价物是 `show tab` 的 **223 B** 加数据 **271 B** = **494 B**，差的 **50 B** 恰是两条节注释（30 + 19）与一个空行（1）；③ 面⑤ 只少一条语句——`general_log`（TABLE 输出、滤掉开关语句）抓到 **9** 行对 **8** 行，逐行对齐后差异恰为那条 `SHOW CREATE TABLE`；④ 闭环仍在——`cat` 两份成一个文件喂 `import -f sql --mode create -t cmp127_copy`，退出 0、`COUNT(*)=3`、两侧 `CHECKSUM TABLE` 同为 **1846113088**。`import --mode create` 刻意保留，改的只是「结构从哪来」；它那条找不到建表语句的报错随之改写（面③），仍退出 **1**、仍在发出首批 INSERT 之前失败、不会退化服务端 `1146`。配置回显字段数实测：导出 del **25**、parquet **16** 不变，sql **20 → 18**；导入三格式 **29 / 20 / 20** 逐格式不变。`imex export --help` **86 → 82** 行。资产：`_test.go` 仍 **65**、`^func Test` **412 → 409**（减量全在 `internal/export` **101 → 98**，且全在 `sql_writer_test.go` **26 → 23**：删五例、增两例）、Fuzz **6**、Benchmark **12**；真库合跑 **217** 条 `--- PASS`（**76** 顶层 + **131** 一级 + **10** 二级）/ 0 SKIP / 0 FAIL（32.517 s），对照同床 HEAD 的 **219**（76 + 133 + 10），差值就是被合成一条的那两个子用例。oracle 走 **Re-baselined note 28**：**1,150 → 1,146** 行（走掉 `## export` 的两条选项行与示例四行改两行）加 note 自身 **27** 行，现 **1,173** 行、20 段、`sections=20 identical=20 drift=0`。

**本轮两条自纠**：① note 28 初稿写「本条加 25 行」并在代码段里多打了转义反引号，按实测 1,146 + 27 = 1,173 改掉；② **批量补丁脚本里的空替换等于误删**——给 requirements.md 打补丁的 python 里一条 `rep('把"schema 二次拼接"这条缺陷钉死；','')` 把 11.1 中 `test/integration/export_cli_test.go` 行的那句话连带删掉（同形的 `internal/export` 行那句没受影响），改写该行时按原文恢复。教训：脚本里的删除项要逐条过目，别把「顺带清理」写进同一个替换列表。

### 2.10 v0.18.0 进位与 tag（2026-11-03，变更记录 128，本轮就是 tag 轮）

用户指令「commit tag push」。本轮开始时没有任何待提交的代码改动——124/125/126/127 四轮都已提交并推送，所以交付物只有版本号与 tag：`Makefile` 第 1 行 0.17.1 → **0.18.0**，与 tag 名 `v0.18.0` 在**同一次提交**里同步（纪律 2，先例 `fc1844a` / 变更记录 120），附注标签，先推 `main`、再打 tag、再单独 `git push origin v0.18.0`。

**档位口径**：`v0.17.1` 之后命中契约面的是 124（面①③④，删 `--socket`/`-S`/`IMEX_SOCKET`）、125（面①③⑤ 追加，`--engine mysql|pg`）、127（面①②③⑤，删 `--create`/`--no-data` 且 SQL 导出改仅数据），三轮当轮各记 **Y**；121/122/123 纯 Markdown、126 纯测试，不动任何一面。0.x 期不兼容记在 Y 位（先例见 2.2 的 `v0.6.0` 纠正轮与 AGENTS.md 第 9 条那个待定项），所以进位取 **0.18.0**、X 保持 0。被这个 tag 收口的是 `git log v0.17.1..HEAD` 的 **12** 个提交，累计 **56** 文件 4,717 增 1,032 删。

**前置四件（实测）**：提交已 push（打 tag 前 `ls-remote --heads origin` = 本地 HEAD = `3f0a86151a061a36861025c37e900619d735f690`）、工作树干净（`git status --short` 空、跟踪文件 **137** 个）、QA 全绿（见五节本轮那段：两引擎真库 **217 PASS / 0 FAIL / 0 SKIP**）、`statementCapSlack` = **3**。bump 之后重新 `make build`，`imex version` 报 **0.18.0**，产物仍是 **26,882,210 B**（与 bump 前同字节 = 零代码改动），oracle 复跑仍 **sections=20 identical=20 drift=0**——`## version` 段按 note 5 冻结，不随进位改。

**本轮两条现查（都只记录、未改历史文本）**：① README 的发布记录里 `v0.17.1` 之后是空的——124/125/127 的 README 改动全部就地写进正文，没起 `## 功能记录（v0.18.0）` 一节，本轮按 120 的先例也不回溯补写，等点名；② `git tag -l` 里没有 `v0.6.0` 对象（`git rev-parse -q --verify refs/tags/v0.6.0` 退出 **1**），而 README 与 2.2 都按 `v0.6.0` 记着那次版本纠正。另外变更记录 125 那句「生成列与 `ALWAYS IDENTITY` 不进产物」与本轮双引擎对比现测冲突（两引擎的 DEL 产物都含其值，只有 INSERT 排除），按纪律 3 该在 125 行内加带日期的「注」更正——同样等点名，本行只挂进 4.1。

## 三、关键决策（长期有效，改动需用户点名）

1. **对外契约固定为五面**：① CLI 选项集合与帮助文本（`docs/cli-oracle.txt` 是冻结面，重新基线即契约变更）；② 产物文件格式（DEL/SQL 布局、Parquet、`ENC:` 密文布局）；③ 退出码与错误/告警文案（0 成功、1 运行时、2 用法）；④ `IMEX_*` 环境变量集合；⑤ 发给服务端的语句与事务、锁边界（用户可从 `general_log` 观察）。任一面变化至少进第二位 Y，不兼容进第一位 X。
2. **档位与 tag 纪律**：一轮交付一个 tag，按命中的最高档进位；一律附注标签，名字 = `Makefile` 第 1 行的 `VERSION`，两处同一次提交里同步改；**不自动打、不自动推**，tag 不随 `git push` 走，需单独 `git push origin vX.Y.Z`；已推送的 tag 不可移动、不可删除重打；**纯文档 / 纯测试轮不单独打 tag**。tag 前置四件：提交已 push、工作树干净、QA 全绿、`grep -c statementCapSlack test/integration/option_matrix_test.go` = 3。
3. **历史只追加**：`README.md` 功能记录与 `docs/requirements.md` 变更记录不得回改；发现写错就在原行内加带日期的「注」。变更记录每行必须是**一个物理行、恰好 4 个竖线**。
4. **帮助单行 ≤118 字节**（含 pflag 自动后缀），守卫面当前为 20 个 = oracle 的 20 段。`## encrypt` 与 `## help` 仍未入册；oracle 与二进制的一致性**目前靠人工重取比对，没有自动断言**。
5. **通道模型的裁定**：结果与日志严格分路——结果进 `-o` 文件或 stdout，日志进 stderr，带 `-m` 时屏幕完全静默（含错误），结果在「无 `-o` 且有 `-m`」时追加到 `-m` 文件末尾；`-m` 恒为追加写；完成记录（`list completed`/`show completed`）在 `sink.Flush()` **之前**写，所以文件里日志在前、结果在后。
6. **`show` 的产物规则**：逐字 DDL + `;\n`，不带 `sql_mode` 之类会话属性；定义列**按列名取**并以「值以 `CREATE` 开头」兜底，因为六个语句的列位不统一（表/视图第 2 列、触发器/例程第 3 列、事件第 4 列）；明确不用 `information_schema`（触发器丢头、例程丢参数与 `RETURNS`、视图是规范化文本）；`--raw` 按裁定 ④ 不做。
7. **回放边界**：**只有 `tab` 的产物能直接回放**；`DELIMITER` 支持动的是 import reader（面⑤），属单独一轮，不进 0.17.0。
8. **`-?` 的注册面**：只给 16 个叶子（那里 `-h` 让给 `--host`）；root 与 `list`/`show`/`version`/`encrypt`/`help` 保持 cobra 的 `-h, --help`，所以 `imex -?` 是 unknown-shorthand 用法错误（退出 2）。
9. **命名**：`show` 的 kind 用三个字母简称，英文整词作别名（用户 2026-11-02 裁定）。
10. **数字必须实测**：文档里的字节数、行数、PASS 数只有在实跑过的服务器上量出来才写。本轮就因夹具内容变化重测并纠正了 5 处上一轮遗留的漂移（`-m` 文件 2,395 B、show 文件 606 B、缓冲三档 433/2,698/12,848 B、同路径 312 B、语句 24–42 B）。
11. **打包纪律**（`<workspace>/AGENTS.md`）：提交后不自动打包；每天第一次回复自动出源码工程包（未提交改动加 `-dirty`）；产物只 amd64；`<workspace>/work` 只保留最近 3 批。

## 四、待办与待裁定

### 4.1 等用户裁定，未动

主体来自变更记录 117 ⑯，后来现查补的条目在行内注明出处；未点名一律不动：

- 枚举记法统一。
- `-?` 是否扩到 root 与 `list`/`show`/`version`/`encrypt`/`help`。
- `## encrypt` 是否入 oracle，以及 118 字节守卫是否随之从 20 面扩面。
- import 的 `DELIMITER` 支持 / 非 `CREATE TABLE` DDL 的回放通道（单独一轮）。
- 父级那句误导性的 `unknown shorthand flag`（kind 或对象拼错时拿不到 `Did you mean`）。
- 是否把 `scripts/` 写进 `.gitignore`。
- **README 与 Makefile 里那串示例 DSN 要不要一并换成纯占位串**（2026-11-03 变更记录 127 ⑫ 现查）：`README.md` 第 63 行与 `Makefile` 第 79、82 行写着 `IMEX_TEST_DSN='root:secret@tcp(127.0.0.1:3306)/imex_it'`，共 **3** 处，本轮沙箱的 root 口令恰好也是 `secret`。两个真随机口令在仓库内命中 **0**，这串是历轮占位写法（`git log -S` 追到 `a52fa6b`），只在 `127.0.0.1` 与临时沙箱内有意义；未点名不动。
- `export` 在 `SHOW CREATE TABLE` 里对标识符的朴素加引号。**（2026-11-03 变更记录 127 之后此条已无处可改）**：导出侧不再发那条语句，`getCreateTableStatement` 连同它的朴素加引号一起删掉（`grep -rn getCreateTableStatement` 现命中 **0**），剩下的同类代码只有两处且都已是成对转义——`internal/backend/mysql.go:35` 的 `QuoteIdent`（`list` 与 import 走它）与 `internal/show/show.go:166` 的 `quoteIdent`，二者都把名字里的反引号翻倍，不存在「提前切断引号span」的注入面。原条目按历史不改写，此处只记状态：**因被点名对象消失而了结，不是通过实现改动**。
- **变更记录 125 的生成列那句要不要加「注」更正**（2026-11-03 双引擎对比现查，变更记录 128 ⑫ 记下）：125 写「`GENERATED ALWAYS` 与 `ALWAYS IDENTITY` 两列不进产物也不进 INSERT」，实测两引擎的 **DEL 产物都含这两列的值**，被排除的只有 INSERT 的列清单——排除发生在导入侧（`internal/import/generated_columns.go:excludeGeneratedColumns` 加两支 `GeneratedColumnsSQL()`，MySQL 读 `information_schema.COLUMNS.EXTRA`、PG 读 `pg_attribute`）。按纪律 3 只能在那一行内加带日期的「注」，未点名不动。
- **README 要不要补 `## 功能记录（v0.18.0）` 一节**（2026-11-03 tag 轮现查，见 2.10 与变更记录 128 ⑨）：README 每个 tag 一段发布记录的先例到 `## 功能记录（v0.17.0）`（第 **1067** 行）为止，124/125/127 的改动都就地写进正文，所以 `v0.17.1` 之后没有发布记录段。本轮按 120 的先例只改 `Makefile` + 变更记录 + 本文件，回溯补写要用户点名。

### 4.2 与 mydumper/mysqldump 的已知功能差距

整库一致快照的批量导出/导入、`--drop-existing`/`--add-drop-table`、gzip + 体积切片、`--updated-since`、`--ask-password`、长查询护栏。当前多表场景由调用方自己循环，**每张表各开一次快照，表与时点不一致**（变更记录 112 ⑦ 有语句流取证）。

### 4.3 沙箱清理（2026-11-03 现查）

- **MySQL 开发库**：`information_schema.schemata` 现 **6** 个（`imex_it`、`imex_test` 加 `information_schema`/`mysql`/`performance_schema`/`sys`），`imex_test` 仍是空库。`imex_it` 现 **23** 张表（127 轮记 20，之后双引擎对比测试又留下 `cmp127_mx` 0 行、`cmp127_full` 3 行、`gc_mx` 2 行三张；`cmp127` 与 `cmp127_copy` 各 3 行仍在，`cmp127_new` 未留下），`big_table` 实数 `count(*) = 200000`（`table_rows` 此刻对 `big_table` 与 `imex_perf_dst` 都报 198,194 的估算值，别照抄），`imex_perf_dst` 是集成床夹具。这些都是历轮取证手造的表，留着不碍事——集成用例自建表且带随机后缀，不会撞名。
- **PostgreSQL 开发库**：库 `pgprobe`，两个非系统 schema 共 **13** 个对象（127 轮记 9，双引擎对比测试新增 `public` 的 `rt_mx` 5 行、`gc_probe` 2 行、`id_probe` 2 行、`id_dst` 2 行四张）：`public` 现有 `imex_doc_demo` 2 行、`rt` 5 行、`rt_mx` 5 行、`rt_probe` 0 行、`gc_probe`/`id_probe`/`id_dst` 各 2 行与视图 `v_probe`；`imex_list_it` 的 `t1` 5 行、`t2`/`rt`/`"MixedCase"` 各 0 行与视图 `v1`——`imex_list_it` 是 `TestPGLiveServer` 为跨 schema 覆盖建的夹具，两个视图由 `internal/list/queries_pg_test.go:492` 与 `:508` 的 `CREATE OR REPLACE VIEW` 留下，跑完都不删。库大小现 **9,507 kB**（`pg_database_size` = 9,735,515 B；上轮记 9,243 kB）。**重启后 `pg_stat_user_tables.n_live_tup` 全为 0**，那是统计视图没跑过，不是数据没了——行数一律用 `count(*)`。角色 `pgprobe` **没有 CREATEDB**（`ERROR: permission denied to create database`），所以 pg 侧所有夹具只能铺在 schema 层，`CREATE` 权限只有 `public` 一处。
- **本机**：2026-11-02 列的 `<tmp>/wt-0120`、`<tmp>/wt-0121`、`<tmp>/wt-bench`、`<tmp>/wt-pprof` 四个工作树和 `scripts/export-all-tables.sh` 随旧沙箱一起没了（`scripts/` 目录也不存在）。取证工作树都已在各自收口前 `git worktree remove` 清掉（125 轮的 `/tmp/imex-base`、127 轮的 `/tmp/base127`——后者检出 HEAD `0095741`，用来造基线二进制并在同一对库上跑对照真库），`git worktree list` 只剩 `/data/imex`。`<tmp>` 下留着的取证产物：`/tmp/imex-doc`（工作树明文构建 38,373,289 B）、`/tmp/imex-baseline`（125 轮 HEAD `make build` 22,368,418 B）、`/tmp/imex_base`（127 轮基线，`0095741` 的 `make build`，**26,890,402 B**，与被删选项同一次构建）、`/tmp/oracle.py`（oracle 比对脚本，用法 `oracle.py <binary> [--write]`，漏传参数会 `IndexError`）、`/tmp/qa127.sh`（本轮门禁脚本，全量输出留档 `/tmp/qa128_raw.txt`）、`/tmp/rec125.txt`/`/tmp/rec126.txt`/`/tmp/rec128.txt`（变更记录的行文本）、`/tmp/live126.txt`、`/tmp/live127.txt` 与 `/tmp/live127_final.txt`（双引擎真库输出，**217** 条 `--- PASS`）、`/tmp/live_base127.txt`（同一对库上 HEAD 的对照跑，**219** 条——两次差值就是变更记录 127 ⑥ 说的那两条被合成一条）、`/tmp/cmp127.sh`（基线对拍脚本）、`/tmp/xmatrix.sh` 与 `/tmp/xmatrix2.sh` 加 `/tmp/xmatrix/`（两引擎 export/import/list/show 支持矩阵的脚本与产物，含 `m.del`/`p.del`/`m.sql`/`m.ddl`/`full.sql`，128 ⑫ 那条生成列冲突的取证出处）、`/tmp/gl127_base_create.log` 与 `/tmp/gl127_new_data.log`（`general_log` **文件**形态的两次抓取，13 行对 12 行；正式取证改用 TABLE 输出并滤掉开关语句，按 **9 行对 8 行**计，见 127 ⑤）、`/tmp/cfg_*.txt` 与 `/tmp/bcfg_*.txt`（新旧二进制的配置回显样本）、`/tmp/render.go.bak`（126 轮变异复跑前存下的原文件备份）。**两个库都不是 systemd 起的**（`policy-rc.d` 挡着），整机重启之后必须手工按本节命令重起：126 轮遇到 **08:00:24**、127 轮 **11:07:56**、本轮（tag 轮）第三次 **14:24:16**，三次签名都是 `connection refused` 与大面积 FAIL；`/tmp/mysqld/mysqld.pid` 会留死文件、不影响重起。`/tmp` 现 **209** 项 / **723 MB**，绝大多数是历轮取证产物，不在任何保留脚本的额度内。

### 4.4 一处文档缺陷：仓库引用的基线提交号解析不到（**已处理**，见 2.3 与变更记录 118）

`docs/cli-oracle.txt` 的头注把基线写成「extracted from shipped binary commit **4934dd2**」，全文共 **11** 处引用这串（其中 `## version` 段里那处 `Git commit: 4934dd2` 是当年 `imex version` 的输出快照，按 note 5 属于该保留的内容），`README.md` 另有 2 处、`docs/requirements.md` 8 处。但这串**在本地与远端都解析不到**：`git cat-file -t 4934dd2` → `fatal: Not a valid object name`，`git log --all` 里没有以 `4934` 开头的提交，仓库可见历史只有 72 个提交（2026-11-02 在新沙箱重新 clone 后复测，前缀为 `4934dd2` 的对象数为 **0**，远端 heads 只有 `main`）。`v0.16.0` 及更早的 oracle 就一直这么写着，属长期遗留而非本轮引入。

**处理结果（2026-11-02）**：按「只加可解析指向、不回溯替换」做完——头注第 1 行补了「该 id 在本仓库不可解析，见 note 25；13 段体可由 `git show v0.16.0:docs/cli-oracle.txt` 取回」，文末新增 **Provenance note 25** 把实测数字与 63 行的裁定写进 oracle 自己。上面列的 21 处历史引用一处没删，`## version` 快照未动，版本仍 `0.17.0`、未打新 tag。

## 五、怎么自证（本轮全部实跑过）

```bash
cd <workspace>/imex
make build                                                   # bin/imex
gofmt -l .                                                   # 期望空
CGO_ENABLED=1 go vet ./...                                   # 期望无输出
CGO_ENABLED=1 go test -race -count=1 ./...                   # 期望 10 个 ok
IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_it' \
IMEX_TEST_PG_DSN='postgres://<user>:<password>@127.0.0.1:5432/<db>?sslmode=disable' \
  CGO_ENABLED=1 go test -race -count=1 -v ./test/integration # 期望 0 FAIL / 0 SKIP（217 条 --- PASS = MySQL 204 + PG 13，其中 76 顶层 + 131 一级 + 10 二级）
grep -c statementCapSlack test/integration/option_matrix_test.go   # 期望 3
```

`IMEX_TEST_DSN` 里的 `<user>:<password>` 是占位串，真实账号口令只在运行环境里给、不写进仓库（口径同变更记录 59，取值以裸形态为准复核，见变更记录 119 ②bis）。

帮助长度守卫（20 个面，输出必须为空）：

```bash
for c in "--help" "export --help" "import --help" "list --help" "show --help" "version --help" \
         "list databases --help" "list tables --help" "list views --help" "list columns --help" \
         "list indexes --help" "list triggers --help" "list routines --help" "list events --help" \
         "show tab --help" "show vws --help" "show trg --help" "show evt --help" "show prc --help" "show fcn --help"; do
  ./bin/imex $c 2>&1 | awk -v cmd="$c" 'length($0) > 118 {print "OVER118", cmd, NR}'
done
```

两个坑：`test/integration` 的 `TestMain` 在 **`IMEX_TEST_DSN` 或 `IMEX_TEST_PG_DSN` 任一存在时**才构建 CLI 二进制（125 轮从「只认 MySQL」改宽，否则 pg-only 环境拿不到二进制），直接驱动二进制的用例必须按 DSN 判空 `t.Skip`（现为 `show_test.go` 两条 help-only 与 `option_matrix_test.go` 一条），否则 `go test ./...` 会报 `exec: no command`；`IMEX_TEST_PG_DSN` 还必须是 `postgres://` URL、带口令、指定库，三项缺一即 `t.Fatalf`（这些用例要把口令放上命令行）。沙箱重启后 3306 不在了，起法：`nohup setsid /usr/sbin/mariadbd --defaults-file=<tmp>/mysqld/my.cnf ><tmp>/mysqld/runNN.log 2>&1 </dev/null &`，再用 `ss -ltn | grep ':3306 '` 确认。**新沙箱（连 mariadbd 都没有时）的完整重建法**（2026-11-03 实测走通，含真库 206 用例）：`printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d` 先挡住自动起服务，再 `DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends mariadb-server mariadb-client`（jammy 的候选版本恰好还是 `1:10.6.23`，与历轮取证同版本，结论可直接对比）；datadir 由包初始化好，不必 `mariadb-install-db`；自建 `<tmp>/mysqld/my.cnf` 时必须给 `user=mysql`、并把 `<tmp>/mysqld` 目录 `chown mysql:mysql`（`general_log_file` 那类由服务端自建的落盘文件才写得出来），配置里 `bind-address=127.0.0.1` + `skip-name-resolve`，于是授权要按 `gd`@`127.0.0.1` 写而不是主机名；起来之后 `mariadb -S <tmp>/mysqld/mysql.sock -u root` 建 `imex_it`/`imex_test` 与 `gd`@`127.0.0.1`（`GRANT ALL ON *.* … WITH GRANT OPTION`，因为 `live_mysql_test.go` 有一条 `CREATE DATABASE` 路径，权限不够它会 `t.Skipf` 而不是失败）。
     两条环境约束是 2026-11-03 现查的：① **`max_connections` 必须留在默认 151**——把它抬到 400 之后 `TestBoundaryWorkersOverConnectionLimitAreReported` 会 `t.Skipf("server accepts %d connections, the over-limit case cannot be produced")`（它要 `--threads 200` 打不过上限才成立），QA 的「**0 SKIP**」口径当场破掉；改回默认后同一跑恢复 **206 RUN / 206 PASS（70 顶层 + 136 子用例）/ 0 SKIP / 0 FAIL（38.502 s）**。② `innodb_buffer_pool_size` 从 512M 降到 **256M**：512M 时 mariadbd 在同沙箱又起一个 PostgreSQL 与若干 Go 链接任务的时段里被**无声 SIGKILL**（`run01.log` 空、error log 停在 04:04:21 的两条 `Access denied`——那是集成套件故意测 1045 路径的正常记录，不是故障原因），重启后按新值跑到 38.5 s 全绿且不再掉线。

**PostgreSQL 床的重建法（2026-11-03 实测走通，含真库 13 条 pg 用例）**：`DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends postgresql-14`（jammy 候选即 `14.24`），同样先靠 `/usr/sbin/policy-rc.d` 挡自动启动；起法（本轮实际在跑的那条，`ps -eo args` 现查）`nohup setsid su postgres -c '/usr/lib/postgresql/14/bin/postgres -D /var/lib/postgresql/14/main -c config_file=/etc/postgresql/14/main/postgresql.conf' >< /tmp/pg/run.log 2>&1 </dev/null &`，`ss -ltn` 复核只监听 `127.0.0.1:5432`。建床：`su postgres -c 'psql -c "CREATE ROLE pgprobe LOGIN PASSWORD '"'"'<随机口令>'"'"'" CREATEDB"'` 之后 `CREATE DATABASE pgprobe OWNER pgprobe`——**注意沙箱里这条 `CREATEDB` 会因权限被拒**，125 轮的实际形态是角色不能建库、只能在 `public` 上 CREATE，所以 pg 床只有 `pgprobe` 这一个库；`queries_pg_test.go` 用同库里的第二个 schema（`imex_list_it`）钉住「带 `-D` 只回 `current_schema()`」「不带 `-D` 两个非系统 schema 都在结果里」，但「换个 schema 去连」这条没有任何用例——工具不给 schema 选项（已写进 requirements 11.2）。口令只进运行环境与 `/tmp`，不进仓库。

**QA 现状（2026-11-02 在 `c03f2c9` 上复跑，本机 MariaDB 10.6.23）**：`gofmt -l` 0 行、`go vet` 净、9 包 `-race` 全 ok、集成 **206 通过（70 顶层 + 136 子用例）/ 0 失败 / 0 跳过**（16.331 s）、20 个帮助面 0 行超 118 字节、`statementCapSlack` = 3。

**沙箱换了之后（2026-11-02 变更记录 118 轮实测）**：上面那串除了集成那一行都能跑：`gofmt -l .` 空、`CGO_ENABLED=1 go vet ./...` 净、`CGO_ENABLED=1 go test -race -count=1 ./...` 9 包全 ok（`test/integration` 在无 DSN 时 1.027 s 通过 = 真库用例全跳过）、`make build` 通过（`bin/imex` 22,368,418 B，ldflags 打进 `GitCommit=7e61e72`）、20 面 118 字节守卫 0 行超限、`statementCapSlack` = 3。帮助守卫与构建都不依赖数据库，照常复跑。**这个沙箱里没装 MariaDB**，`/usr/sbin/mariadbd` 与 `mysqld` 都不存在，所以起库那条命令在此不可用，206 用例与 20 个帮助面的守卫要等环境补回来才能复跑；`go` 本体是 go1.22.5，靠 `GOTOOLCHAIN=auto` 拉 `go1.26.10` 才能按 `go.mod` 编译（首次 `go build` 会下载依赖与工具链）。

**QA 现状（2026-11-03，变更记录 125 轮，两引擎同床）**：`gofmt -l` 0 行、`go vet` 净、**10 包** `-race -count=1` 全 ok（新增的 `internal/backend` 是第 10 个；最慢 `internal/common` 56.429 s）、双 DSN 集成 **219 条 `--- PASS`（76 顶层 + 143 子用例）/ 0 失败 / 0 跳过**（38.047 s，其中 MySQL 206、PG 13）、20 个帮助面 0 行超 118 字节、`statementCapSlack` = 3、oracle 复跑 `sections=20 identical=20 drift=0`（1,150 行）。`make build` 产物 `bin/imex` 26,890,402 B（同轮 HEAD 构建 22,368,418 B，两者同为 `-s -w -trimpath`，可直接比）。`version` 仍 `0.17.1`，未打新 tag。

**QA 复跑（2026-11-03，变更记录 126 轮，测试-only）**：同一串再跑一遍仍全绿，只有一处耗时变化——`gofmt -l` 0 行、`go vet` 净、10 包 `-race -count=1` 全 ok（`internal/common` 56.290 s、`cmd` 14.237 s、`internal/list` 1.025 s）、双 DSN 真库 **219 条 `--- PASS`（76 顶层 + 143 子用例）/ 0 FAIL / 0 SKIP**（40.282 s）、20 面 118 字节守卫 0 行超限、`statementCapSlack` = 3、oracle `sections=20 identical=20 drift=0`、`make build` 仍是 **26,890,402 B**（本轮没动生产代码，尺寸不变即证据）。**新增一条环境坑**：沙箱虚拟机可能整机重启（本轮实测 `uptime -s` = 08:00:24，重启后 3306 与 5432 双双 `connection refused`，第一次真库复跑因此报 **64 FAIL**）——两个库都不在了，先按上面两条重建法起服务再判用例成败，别把环境没落直接读成回归。

**QA 复跑（2026-11-03，变更记录 127 轮，两引擎同床）**：`gofmt -l .` **0 行**、`CGO_ENABLED=1 go vet ./...` 无输出、`CGO_ENABLED=1 go test -race -count=1 ./...` **十包全 ok**、真库合跑 **217** 条 `--- PASS`（76 顶层 + 131 一级 + 10 二级 = MySQL 204 + PG 13）/ **0** SKIP / **0** FAIL（**32.517 s**，收尾复跑同样 **217** 条 / **33.483 s**）、20 个帮助面 **0** 行超 118 字节、`statementCapSlack` = **3**、oracle `sections=20 identical=20 drift=0`（现 **1,173** 行）；`make build` 产物 **26,882,210 B**、`./bin/imex version` 仍报 **0.17.1**。与上一轮的 219 条差 **2** 条不是丢用例而是合并：`TestLive_ExportSchemaQualifiedQuery` 原来的两个子用例（数据、`--create`）在 DDL 移除后只剩一条数据侧断言——同一对库上把 HEAD（`0095741`）用 `git worktree` 检出单独跑一遍实测 **219** 条 / 33.460 s 为证，逐条比对 `--- PASS` 名单即可看到差异只在那两条。**本轮又遇到一次整机重启**（`uptime -s` = **11:07:56**），签名与 126 轮那次一模一样，按上面两条重建法起库后复跑即全绿——`connection refused` 先排环境这条已经二次验证。

**QA 复跑（2026-11-03，变更记录 128 轮 = tag 轮，两引擎同床）**：门禁第一次跑就撞上**本沙箱第三次整机重启**——真库那段报 **67 FAIL / 14 PASS**（其中 **64** 条 `connection refused`），`ss -ltn` 里 3306 与 5432 都不在，`uptime -s` = **14:24:16**；按上面两条重建法把 `mariadbd` 与 postgres 拉起（PG 日志 `automatic recovery in progress` 后 ready）、复核两个端口在听、`count(*)` 抽查夹具无损，**断言一字未动**原样复跑即全绿：`gofmt -l .` **0** 行、`CGO_ENABLED=1 go vet ./...` 无输出、`CGO_ENABLED=1 go test -race -count=1 ./...` **十包全 ok**（根 1.228 s、`cmd` 1.124 s、`internal/backend` 1.014 s、`internal/common` 46.632 s、`internal/config` 1.011 s、`internal/export` 1.590 s、`internal/import` 3.829 s、`internal/list` 1.020 s、`internal/show` 1.035 s、`test/integration` 1.035 s）、双 DSN 真库 **217** 条 `--- PASS`（76 顶层 + 131 一级 + 10 二级）/ **0** FAIL / **0** SKIP（**32.701 s**）、20 面 118 字节守卫 **0** 行超限、`statementCapSlack` = **3**、oracle `sections=20 identical=20 drift=0`（1,173 行），留档 `/tmp/qa128_raw.txt`。进位与两份文档改完后**只复跑不依赖数据库的那几项**：`make build` → `imex version` **0.18.0**、`Git commit: 3f0a861`（构建早于提交，stamped 的是父提交，与 120 ④ 同现象）、产物 **26,882,210 B** 与 bump 前逐字节同尺寸，20 面守卫仍 **0** 行超限、oracle 对新二进制仍 `drift=0`（`## version` 段按 note 5 冻结，不随进位改）、`gofmt -l .` 仍 **0** 行、凭据扫描仍 **5** 处（本行刻意不重抄那串示例 DSN，否则会变 6）。文档结构两项也实测：`/tmp/tblcheck.py` 三份 Markdown 表格单元数不匹配行 **0**；编号变更记录 **128** 条、未转义竖线不为 4 的行为 **0** 条。

## 六、发布执行记录

**`v0.17.0`（2026-11-02 已完成）**：按用户「commit tag push」的指令依次执行完毕：`git push origin main`（`c03f2c9`→`67b06e5`→`29f8b25`）→ `git tag -a v0.17.0`（打在 `29f8b25`）→ `git push origin v0.17.0`。发布前复核过四件事：提交已 push、工作树只剩 `?? scripts/`、上面那串 QA 全绿、`Makefile` 第 1 行是 `0.17.0`。产物（源码包 / amd64 二进制）仍只在用户明确要时才做。

**`v0.17.1`（2026-11-02 已完成）**：同样按「commit tag push」执行，纯文档轮收口，一笔提交（`fc1844a`：`Makefile` 进位 + 变更记录 120 + 本文件），tag 打在该提交上并单独推送；细节见 2.4 与变更记录 120。该轮真库 206 用例未复跑，「QA 全绿」按「除真库外全绿」放行。

**`v0.18.0`（2026-11-03 已完成）**：按用户「commit tag push」执行，收口 `v0.17.1` 之后的 12 个提交（121–127 轮 + 四笔 CONTEXT 补记），一笔提交（`Makefile` 0.17.1 → 0.18.0 + 变更记录 **128** + 本文件状态更新），按 standing 口径带 `-c http.version=HTTP/1.1` 推 `main`（失败就原样重试两三次，见一节「已推送」那条）→ `git tag -a v0.18.0` → 单独 `git push origin v0.18.0`。收口判据是 `ls-remote --heads origin` 与 `git rev-parse HEAD` 一致、`ls-remote --tags origin` 里有 `refs/tags/v0.18.0`、`git status --short` 为空。发布前四件前置**全部实测**（120 那轮缺的那一条这次补上了）：提交已 push、工作树干净、两引擎真库 **217 / 0 / 0** 全绿、`statementCapSlack` = 3；`make build` 的 `imex version` 报 **0.18.0**、产物 **26,882,210 B** 与 bump 前同字节、oracle `drift=0`。源码包与 amd64 产物未做（用户未点名，纪律 11）。

## 七、文档索引

| 要看什么 | 去哪 |
|---|---|
| 命令怎么用、选项含义 | `README.md`（`### list`、`### show`，list 那节里 list/show 共用的「输出通道」表，以及「**PostgreSQL 引擎（`--engine pg`）**」一节） |
| 设计来龙去脉、实测取证、分级理由 | `docs/requirements.md` 二节（2.1.5 与 2.1.6 的 SQL 导出仅数据 / 2.1.7 `show tab` 取结构 / 2.1.11 引擎与 PG 范围 / 2.3 日志 / 2.5 list / 2.6 show）、3.4.1 SQL 回显、3.6 通道与 show 定稿、七节 7.1 方言层 |
| 每一轮改了什么、为什么 | `docs/requirements.md` 十四节变更记录（现行最新行 **128**：`v0.18.0` 进位与 tag 的档位依据、前置四件核对、tag 轮 QA 与第三次整机重启处置；上一行 127 是 SQL 导出仅数据、DDL 全归 `show tab`、删 `--create` 与 `--no-data`） |
| 版本号与 tag 怎么定、发过哪些版本 | 本文件 2.2 里程碑表（**21** 个 tag）、2.10（本轮进位口径）、六节（三次「commit tag push」的执行记录）、`docs/requirements.md` 变更记录 120 与 128 |
| 帮助文本的冻结基线 | `docs/cli-oracle.txt`（**20 段 / 1,173 行**，头注 note 1–28 记录了每次重基线、漂移处理与基线提交号 `4934dd2` 的取回路径；note 28 是本轮 `## export` 的第四次重基线） |
| 测试矩阵与资产清单 | `docs/requirements.md` 十一节（11.1 资产表、11.2 未自动化场景） |
| 打包/tag/文档纪律 | `<workspace>/AGENTS.md`（工作区约定，不在仓库内） |
