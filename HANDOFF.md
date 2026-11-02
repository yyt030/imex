# HANDOFF — imex v0.17.0 发布收尾

> 写于 2026-11-02。面向接手者：目标、已完成、当前卡点、决策与下一步。
> **状态：已完成（2026-11-02）**——`main` 已推到 `29f8b25`，附注 tag `v0.17.0` 已打并推送；
> 第三节的 push 失败与第五节的前两步保留为过程记录。
> 详细状态见同目录 `CONTEXT.md`（141 行，不进 append-only 约束）。

## 一、当前任务目标

把 v0.17.0 这一轮交付发布出去：**commit → 打附注 tag `v0.17.0` → push（含 tag）**。
用户 2026-11-02 指令原文：「commit tag push」。代码与文档已提交，**卡在 `git push`**。

## 二、已完成

| 项 | 结果 |
|---|---|
| 功能提交 | `c03f2c9` feat: show 六 kind + list/show 输出通道模型 + Z 档修复（19 files，+2,842/−128） |
| 文档提交 | `67b06e5` docs: 新增 `CONTEXT.md` 工程状态快照（本文件的前置） |
| 版本号 | `Makefile` 第 1 行 `VERSION ?= 0.17.0`，与待打 tag 名一致 |
| 工作树 | 仅 `?? scripts/`（114 轮裁定移除后的本机 scratch，7,834 B，未入提交） |
| QA | `gofmt -l` 0 行、`go vet` 净、9 包 `-race` ok、真库集成 **206 通过（70 顶层 + 136 子用例）/ 0 失败 / 0 跳过**（13.178 s，MariaDB 10.6.23）、20 个帮助面 0 行超 118 字节、`statementCapSlack` = 3 |
| tag | `v0.17.0` 已打并**已推送**：附注标签，打在 `29f8b25`（tag 对象 `2106d934`，peeled 到 `29f8b25`） |
| 远端 | `main...origin/main` 已同步，origin 顶端 `29f8b25` |

## 三、当时的卡点（已解决：重试后 push 成功）

- `git push origin main` → `Error in the HTTP2 framing layer`。
- `git -c http.version=HTTP/1.1 push` → `GnuTLS recv error (-110): The TLS connection was non-properly terminated`。
- 加 `http.postBuffer=524288000` 同样报 GnuTLS -110 → 不是包体积/分块问题，更像出口代理掐断 git 的 POST。
- 对照：`curl https://api.github.com` 返回 **200**，说明 HTTPS 出网本身通。
- **未验证**：`git ls-remote` / `git fetch`（读路径）是否正常——上一条诊断命令被取消，重跑即可判定是「整个 git smart HTTP 被封」还是「仅 push 的 POST 被封」。
- 认证应无问题：历史提交 `03b4828` 就是这样推上去的。

## 四、关键决策（本轮及长期）

1. **tag 打在 `67b06e5`（HEAD）**：v0.17.0 是同一轮的提交，CONTEXT.md 作为收尾文档一并纳入 tag；`c03f2c9` 不单独打 tag。
2. **tag 形式**：`git tag -a v0.17.0 -m "<一句话摘要>"`；名字 = `Makefile` 第 1 行；附注标签，不用 lightweight。
3. **不进位**：CONTEXT.md 是纯文档，按 AGENTS.md 第 7 条「纯文档/纯测试轮不单独打 tag」，并入 v0.17.0，版本号仍 0.17.0。
4. **顺序**：先 push main（让 tag 前置的「提交已 push」成立）→ 再 tag → 再单独 `git push origin v0.17.0`。用户已一次性授权这三步。
5. **`?? scripts/` 不处理**：不加 `.gitignore`（属待裁定项），不删除（scratch 可能仍有参考价值），只在报告里点名。
6. **历史只追加**：本轮不改 `README.md` 功能记录与 `docs/requirements.md` 变更记录原行；若要补记 tag/push 结果，按惯例在 117 行内写带日期的「注」，或另起 docs 提交。
7. **发布后不要自动打包**：产物只在用户点名时做（AGENTS.md 第 1 条）。

## 五、下一步（按序）

1. 诊断读路径：`git ls-remote --heads origin`、`git fetch --dry-run origin`（30–60 s 超时）。
2. 若仅 POST 被封：重试 2–3 次（代理偶发）、试 `GIT_HTTP_VERSION=HTTP/1.1`、试 SSH（需密钥与 22/443 放行）；仍不行则回报用户并附完整错误，不要改用破坏性手段。
3. push 成功后：`git tag -a v0.17.0 -m "show 对象定义导出 + list/show 输出通道模型 + Z 档修复"`，再 `git push origin v0.17.0`。
4. 验证：`git ls-remote --tags origin v0.17.0` 有值、`git cat-file -t v0.17.0` = tag、tag 指向 `67b06e5`。
5. （可选，按惯例）在变更记录 117 行内加一句 tag/push 结果，再 push 一次 main；tag 不动。
6. 向用户报告：commit / tag / push 的实际结果，以及 `?? scripts/` 与卡点经过。

## 六、涉及文件路径

- 提交内容：`CONTEXT.md`（新增）、`Makefile`（0.17.0，已在 `c03f2c9`）
- 本轮代码：`cmd/show.go`、`cmd/output_channel.go`、`internal/show/show.go`、`cmd/list.go`、`cmd/validate.go`
- 本轮测试：`test/integration/{show,channel,list}_test.go`、`cmd/{show,output_channel}_test.go`、`internal/show/show_test.go`
- 契约与记录：`docs/cli-oracle.txt`（20 段/1,079 行）、`docs/requirements.md`（3.6、2.6、117 行）、`README.md`（list/show 与输出通道表）
- 工作区约定：`/data/AGENTS.md`（打包、tag、帮助长度、文档纪律）

## 七、QA 复跑命令

```bash
cd /data/imex && make build
gofmt -l .; CGO_ENABLED=1 go vet ./...; CGO_ENABLED=1 go test -race -count=1 ./...
IMEX_TEST_DSN='gd:gdpass@tcp(127.0.0.1:3306)/imex_it' CGO_ENABLED=1 go test -race -count=1 ./test/integration
grep -c statementCapSlack test/integration/option_matrix_test.go   # 期望 3
```

沙箱重启后 3306 会掉：`nohup setsid /usr/sbin/mariadbd --defaults-file=/tmp/mysqld/my.cnf >/tmp/mysqld/runNN.log 2>&1 </dev/null &`。

## 八、仍需用户裁定（勿擅自处理）

枚举记法统一；`-?` 是否扩到父级/root；`## encrypt` 是否入 oracle 及守卫扩面；import 的 `DELIMITER` 与非建表 DDL 回放（单独一轮）；父级误导性的 `unknown shorthand flag`；`scripts/` 是否入 `.gitignore`；`export` 对标识符的朴素加引号。另有本轮现查的遗留：`docs/cli-oracle.txt` 头注引用的基线提交 `4934dd2` 在本地与远端都解析不到（oracle 11 处、requirements 8 处、README 2 处），是否改为可解析引用待定。
