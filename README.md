# 降智检测工具

读取你电脑上 Codex、Claude、CC Switch 的配置，用你自己的 Key 或官方登录一键测试：模型有没有被降智或替换，这次测试花了多少。

由 [PinAI](https://app.pinaic.com) 发布，程序里有标注“推广”的 PinAI 链接。推广不参与任何判定。

下载：[Windows](https://check-api.pinaic.com/downloads/pinai-check-windows-amd64.exe) · [macOS（Apple 芯片）](https://check-api.pinaic.com/downloads/pinai-check-macos-arm64) · [macOS（Intel）](https://check-api.pinaic.com/downloads/pinai-check-macos-amd64) · [Linux](https://check-api.pinaic.com/downloads/pinai-check-linux-amd64) · [校验值](https://check-api.pinaic.com/downloads/SHA256SUMS)

Windows 是窗口程序，可缩到托盘。macOS 和 Linux 目前打开浏览器页面。程序暂未签名，首次打开系统可能提示风险。

## 它会读什么

| 文件 | 用途 |
|---|---|
| `~/.codex/config.toml`、`~/.codex/auth.json` | 接口地址、模型、API Key；官方登录只看登录模式 |
| `~/.claude/settings.json` 和环境变量 `ANTHROPIC_*` | 接口地址、模型、API Key |
| `~/.claude/.credentials.json`、`~/.claude.json` | 只判断是否有官方登录，不使用里面的令牌 |
| `~/.cc-switch/cc-switch.db` | 只读 CC Switch 的供应商列表 |

## 它会发什么

| 发给谁 | 内容 |
|---|---|
| 你配置的接口地址 | 你配置的 Key、检测题目；可能读 `/v1/models` 和 `/v1/usage` |
| `check-api.pinaic.com` | 模型名、题目长度、回答文本；**不含 Key 和接口地址**。也会读取配置、推广文案和牌价 |
| 官方登录 | 调用你电脑上的 `codex`、`claude` 命令行，由它们自己发请求；本工具不读取、不复制、不刷新登录令牌 |

判定放在服务器上，所以回答文本要发到 `check-api.pinaic.com`。检测题目只是让模型写一串随机整数，回答里没有你的内容。判定服务的源码不在这个仓库，指纹库的公开版本在 [modeltrace-bank](https://github.com/SPHINX998/modeltrace-bank)。

Key 不写入本机文件。测试历史和设置加密保存：Windows 用系统 DPAPI，macOS 用钥匙串，Linux 只有文件权限保护。

## 怎么自己确认

1. **看联网记录**：程序里的“联网记录”列出这次运行发出的每个请求，包括地址、路径和内容类型。
2. **抓包或用防火墙**：除了你配置的地址和 `check-api.pinaic.com`，它不应该连别的地方。官方登录的请求由官方命令行发出，属于那两个程序。
3. **读代码**：入口 `cmd/relaycheck-client/main.go`，读配置 `internal/features/relaycheckclient/scan.go` 和 `scan_ccswitch.go`，发请求 `internal/features/relayupstream/grouptest_probe.go`，判定请求 `internal/features/relaycheckclient/remote.go`。
4. **自己编译并比对**：用 `go.mod` 里的 Go 版本运行下面的命令，再和下载页的校验值比对。版本号见 [Releases](https://github.com/SPHINX998/degrade-check/releases)，要先切到同名标签。

   ```bash
   bash build.sh 版本号
   ```

5. **用临时 Key**：不放心就新建一把低额度的 Key，测完删掉。

请求头和 Claude Code 一致，是因为指纹库按这种请求形态建立，而且不少中转站只放行 Claude Code。这不会把你的 Key 发到别处。

## 结论怎么看

结论是回答的统计特征，不是模型身份证明：

- 中转站可能认出题目，只在测试时给真模型，这一点测不出来。
- 背后是号池的渠道每次可能不同，要看多次结果。
- 库外模型只能确认能用。
- Claude 只判断是否换了型号，不判断同型号降智。
- “牌价约”按标准价估算，实际扣费取决于渠道倍率。

## 许可证

LGPL-3.0，请求和判定规则来自 [Sub2API](https://github.com/Wei-Shaw/sub2api) 的二次开发。
