# CONTEXT — imex 工程状态快照

> 快照时间 **2026-11-02（UTC）**。这一页只回答四件事：现在在哪、已经定了什么、接下来等什么、怎么自证。
> 权威文档仍是 `README.md`（使用者视角）与 `docs/requirements.md`（设计与取证视角）。只有那两处的
> 功能记录 / 变更记录 受 append-only 约束；**本文件不受**，状态变了就地改，不留历史。

## 一、当前坐标

- **仓库**：模块 `github.com/imex`，远端 `https://github.com/yyt030/imex`，分支 `main`（沙箱内工作副本 `/data/imex`）。
- **HEAD**：`c03f2c9` —「feat: 新增 show 对象定义导出与 list/show 输出通道模型，修 CLI 规约审计 Z 档，版本升至 0.17.0」，19 files changed / +2,842 / −128。
- **未推送**：`main...origin/main [ahead 1]`，远端顶端还停在 `03b4828`（变更记录 116 的补注轮）。
- **未打 tag**：`Makefile` 第 1 行已是 `VERSION ?= 0.17.0`，但仓库最新 tag 是 `v0.16.0`（→ `08ffa85`）。上一轮指令只到 `commit`，tag 与 push 都未点名，因此都没做。
- **工作树**：除未跟踪的 `scripts/export-all-tables.sh`（7,834 B，变更记录 114 裁定从仓库移除后本机残留的 scratch）外干净。`bin/`、`dist/`、`*.shard_*.tmp` 由 `.gitignore` 覆盖。
- **本地开发库**：沙箱内 MariaDB 10.6.23，仅监听 `127.0.0.1:3306`，测试库 `imex_it`、夹具库 `imex_test` / `imex_ddl*` 等。

## 二、已完成事项

### 2.1 v0.17.0 轮（HEAD `c03f2c9`，已提交、未发布）

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

### 2.2 里程碑（19 个 tag，摘要取自各 tag 的附注信息）

| tag | 一句话 |
|---|---|
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

两处需要留意的历史事实（现查所得，别按 README 段落名去反推 tag）：**没有 `v0.11.0` 这个 tag**——README 的「修复记录（v0.11.0）」段是随 `76a20fb`（升到 0.12.0 那一轮）一起补上的文档分段，仓库里也没有任何一次提交把 `Makefile` 写成 `VERSION ?= 0.11.0`；`v0.6.0` 是对更早那次 X 档（1.0.0）的纠正，说明 0.x 期不兼容变更的口径当时已改为记在 Y 上（AGENTS.md 第 9 条仍把这点列为待定项）。

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
11. **打包纪律**（`/data/AGENTS.md`）：提交后不自动打包；每天第一次回复自动出源码工程包（未提交改动加 `-dirty`）；产物只 amd64；`/data/work` 只保留最近 3 批。

## 四、待办与待裁定

### 4.1 等用户裁定，未动

逐条来自变更记录 117 ⑯，未点名一律不动：

- 枚举记法统一。
- `-?` 是否扩到 root 与 `list`/`show`/`version`/`encrypt`/`help`。
- `## encrypt` 是否入 oracle，以及 118 字节守卫是否随之从 20 面扩面。
- import 的 `DELIMITER` 支持 / 非 `CREATE TABLE` DDL 的回放通道（单独一轮）。
- 父级那句误导性的 `unknown shorthand flag`（kind 或对象拼错时拿不到 `Did you mean`）。
- 是否把 `scripts/` 写进 `.gitignore`。
- `export` 在 `SHOW CREATE TABLE` 里对标识符的朴素加引号。

### 4.2 与 mydumper/mysqldump 的已知功能差距

整库一致快照的批量导出/导入、`--drop-existing`/`--add-drop-table`、gzip + 体积切片、`--updated-since`、`--ask-password`、长查询护栏。当前多表场景由调用方自己循环，**每张表各开一次快照，表与时点不一致**（变更记录 112 ⑦ 有语句流取证）。

### 4.3 沙箱清理（2026-11-02 现查）

- **开发库上的夹具**：`imex list databases` 现有 19 个库，其中 `imex_it`（集成测试床）与 `imex_test`（list/show 夹具）要留；其余 `imex_ddl`、`imex_ddl4`、`imex_edge`、`imex_edge2`、`imex_env_it`、`imex_hdr`、`imex_map`、`imex_map2`、`imex_other`、`imex_probe_xs`、`imex_sqlecho`、`pi_probe` 是历轮排查留下的。
- **`imex_test` 里的对象**：19 张表（`sh_t` 是 show 夹具，其余为历轮集成/排查残留，含 `big3m_copy` 995,948 行 / 66,699,264 B）、视图 `list_view`/`sh_v`、触发器 `list_trg`/`sh_trg`、事件 `list_evt`/`sh_e`、例程 `list_func`/`list_proc`/`sh_f`/`sh_p`。
- **本机**：`/tmp/wt-0120`、`/tmp/wt-0121`、`/tmp/wt-bench`、`/tmp/wt-pprof` 四个工作树，以及 `scripts/export-all-tables.sh` scratch。

### 4.4 本轮现查发现的一处文档缺陷：仓库引用的基线提交号解析不到

`docs/cli-oracle.txt` 的头注把基线写成「extracted from shipped binary commit **4934dd2**」，全文共 **11** 处引用这串（其中 `## version` 段里那处 `Git commit: 4934dd2` 是当年 `imex version` 的输出快照，按 note 5 属于该保留的内容），`README.md` 另有 2 处、`docs/requirements.md` 8 处。但这串**在本地与远端都解析不到**：`git cat-file -t 4934dd2` → `fatal: Not a valid object name`，`git log --all` 里没有以 `4934` 开头的提交，仓库可见历史只有 69 个提交。`v0.16.0` 及更早的 oracle 就一直这么写着，属长期遗留而非本轮引入。可改的方向是把「基线提交号」换成可解析的引用（旧 13 段体确实能用 `git show v0.16.0:docs/cli-oracle.txt` 取回），而 `## version` 里那处是历史输出快照、动了就等于改契约面。**是否改、怎么改等用户点头**；纯文档轮按纪律不单独打 tag，改的话并入下一轮。

## 五、怎么自证（本轮全部实跑过）

```bash
cd /data/imex
make build                                                   # bin/imex
gofmt -l .                                                   # 期望空
CGO_ENABLED=1 go vet ./...                                   # 期望无输出
CGO_ENABLED=1 go test -race -count=1 ./...                   # 期望 9 个 ok
IMEX_TEST_DSN='gd:gdpass@tcp(127.0.0.1:3306)/imex_it' \
  CGO_ENABLED=1 go test -race -count=1 -v ./test/integration # 期望 0 FAIL / 0 SKIP
grep -c statementCapSlack test/integration/option_matrix_test.go   # 期望 3
```

帮助长度守卫（20 个面，输出必须为空）：

```bash
for c in "--help" "export --help" "import --help" "list --help" "show --help" "version --help" \
         "list databases --help" "list tables --help" "list views --help" "list columns --help" \
         "list indexes --help" "list triggers --help" "list routines --help" "list events --help" \
         "show tab --help" "show vws --help" "show trg --help" "show evt --help" "show prc --help" "show fcn --help"; do
  ./bin/imex $c 2>&1 | awk -v cmd="$c" 'length($0) > 118 {print "OVER118", cmd, NR}'
done
```

两个坑：`test/integration` 的 `TestMain` **只在 `IMEX_TEST_DSN` 存在时**才构建 CLI 二进制，直接驱动二进制的用例必须按 DSN 判空 `t.Skip`（现为 `show_test.go` 两条 help-only 与 `option_matrix_test.go` 一条），否则 `go test ./...` 会报 `exec: no command`；沙箱重启后 3306 不在了，起法：`nohup setsid /usr/sbin/mariadbd --defaults-file=/tmp/mysqld/my.cnf >/tmp/mysqld/runNN.log 2>&1 </dev/null &`，再用 `ss -ltn | grep ':3306 '` 确认。

**QA 现状（2026-11-02 在 `c03f2c9` 上复跑，本机 MariaDB 10.6.23）**：`gofmt -l` 0 行、`go vet` 净、9 包 `-race` 全 ok、集成 **206 通过（70 顶层 + 136 子用例）/ 0 失败 / 0 跳过**（16.331 s）、20 个帮助面 0 行超 118 字节、`statementCapSlack` = 3。

## 六、若要把 0.17.0 发布出去

按顺序、每步都要用户点名：`git push origin main`（把 `c03f2c9` 推上去）→ `git tag -a v0.17.0 -m "<一句话摘要>"` → `git push origin v0.17.0`。打 tag 前复核四件事：提交已 push、`git status --porcelain` 只剩可接受的未跟踪项、上面那串 QA 全绿、`Makefile` 第 1 行确实是 `0.17.0`。产物（源码包 / amd64 二进制）只在用户明确要时才做。

## 七、文档索引

| 要看什么 | 去哪 |
|---|---|
| 命令怎么用、选项含义 | `README.md`（`### list`、`### show`，以及 list 那节里 list/show 共用的「输出通道」表） |
| 设计来龙去脉、实测取证、分级理由 | `docs/requirements.md` 二节（2.3 日志 / 2.5 list / 2.6 show）、3.4.1 SQL 回显、3.6 通道与 show 定稿 |
| 每一轮改了什么、为什么 | `docs/requirements.md` 十四节变更记录（现行最新行 117） |
| 帮助文本的冻结基线 | `docs/cli-oracle.txt`（20 段，头注 note 1–24 记录了每次重基线与漂移处理） |
| 测试矩阵与资产清单 | `docs/requirements.md` 十一节（11.1 资产表、11.2 未自动化场景） |
| 打包/tag/文档纪律 | `/data/AGENTS.md`（工作区约定，不在仓库内） |
