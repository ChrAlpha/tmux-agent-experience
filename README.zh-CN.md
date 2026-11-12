<div align="right"><strong><a href="./README.md">🇬🇧English</a></strong> | <strong>🇨🇳中文</strong></div>

# tmux-agent-experience

让 tmux 知道你的 coding agents 正在做什么。

照常在 tmux panes 中使用 Claude Code、Codex、Pi 和 oh-my-pi。看清谁在工作、等待回应或已经完成，也让 agents 发现彼此、发送任务并读取回复。tmux 还是 tmux，agent 还是 agent；tmag 只是它们之间的那根线。

## Agent 动态，一目了然

- **需要你时，及时提醒。** Agent 等待回应或完成任务时，所在 tmux window 会显示 bell 标记。用 tmux 的 `prefix + M-n` / `M-p` 即可跳转。
- **直接找到下一个需要关注的 agent。** `prefix + A` 跳到等待最久的 agent，优先处理 blocked 状态；`prefix + a` 打开 agent 树。
- **进度就在眼前。** 在状态栏加入 `#{tmag_status}`，显示 `2 blocked · 1 working`；在 pane 边框加入 `#{E:@tmag-pane-status}`，显示 `codex blocked permission` 等标签。
- **让 agents 协作。** Agents 可以发现彼此、查看状态、发送 prompts 并读取回复。请一个 agent 邀请另一个参与当前任务。
- **离开终端，也能操作。** 通过可选的本地 HTTP API 回应 permission requests、发送 prompts 或中断任务；每个客户端均需 owner 授权。也可以将提醒接入自己的通知命令。

没有 daemon，也不包装 agent。不会向忙碌的 agent 输入 prompt。Agent 发出的 prompt 带有经证明的发送方。不存储 prompts 和输出。tmag 缺失或故障，也不会阻塞任何 turn。默认行为均可[配置](https://github.com/ChrAlpha/tmux-agent-experience/wiki/tmux-Integration-zh-CN#配置项)。

## 安装

需要 Linux 或 macOS（amd64 或 arm64）、**tmux 3.2+**、[TPM](https://github.com/tmux-plugins/tpm)、`curl`，以及 Claude Code、Codex、Pi、oh-my-pi 中的任意一个或多个。

### 交给 agent 安装

在 tmux 中运行的 coding agent 里粘贴：

> Read https://raw.githubusercontent.com/ChrAlpha/tmux-agent-experience/main/SKILL.md and follow it to install or update tmag for the AI tool I am using.

Agent 会先征得你同意再修改 tmux 配置，随后安装插件、接入当前使用的 agent 宿主，并验证结果。更新时也使用这段 prompt。

### 手动安装

1. 在 tmux 配置中，将这一行放在 TPM 的 `run` 行之前，然后按 `prefix + I`：

   ```tmux
   set -g @plugin 'ChrAlpha/tmux-agent-experience'
   ```

2. 按 `prefix + M-a`，通过已安装的各个 agent 自己的 plugin manager 接入；Pi 和 oh-my-pi 从插件目录安装 tmag 的 packages。tmag 使用已发布的 binary，并校验 checksum。

3. 在 tmux 中启动新的 agent sessions。Codex 中通过 `/hooks` 批准 tmag 的 hooks，并通过 `/title` 启用 `thread-id`。Codex pane 会在第一个 prompt 后关联到对应 session。

**更新：** 按 `prefix + U`，再按 `prefix + M-a`。每次插件更新后，都需在新的 Codex session 中通过 `/hooks` 重新批准 hooks。

[完整安装、更新和卸载说明 →](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Installation-and-Updates-zh-CN)

## 使用

| 操作 | 默认快捷键 |
| --- | --- |
| 打开 agent 树 | `prefix + a` |
| 跳到等待最久的 agent | `prefix + A` |
| 设置 agent 插件并检查安装 | `prefix + M-a` |
| 在 tmux window 中启动本地 HTTP API | `prefix + M-r` |

在脚本中使用时，将插件的 `bin/` 目录加入 `PATH`：

```console
$ tmag list
TARGET     PANE  AGENT   STATE                FOR  ID        CWD
work:1.0   %3    claude  blocked (permission) 12s  1787a44e  ~/src/api
work:2.0   %7    codex   idle                 4m   01a19fda  ~/src/web

$ tmag send 01a19fda --wait "Review the diff in HEAD and list risky changes"
01a19fda idle
$ tmag read 01a19fda --last
```

设置时，agents 会获得 `tmag-peers` skill。你可以请当前 agent 找另一个 agent review 改动、排查 bug，或比较不同方案。

## 文档

[Wiki](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Home-zh-CN) 提供使用指南与参考：

- [tmux 集成与配置](https://github.com/ChrAlpha/tmux-agent-experience/wiki/tmux-Integration-zh-CN)
- [Agent 协作](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Agent-Collaboration-zh-CN)
- [CLI 参考](https://github.com/ChrAlpha/tmux-agent-experience/wiki/CLI-Reference-zh-CN)
- [HTTP API](https://github.com/ChrAlpha/tmux-agent-experience/wiki/HTTP-API-zh-CN) 与[通知](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Notifications-zh-CN)
- [故障排查与已知限制](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Troubleshooting-zh-CN)
- [架构、JSON/events 与存储](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Architecture-and-Contracts-zh-CN)

## 参与贡献

参见 [CONTRIBUTING.md](CONTRIBUTING.md)。安全问题请按 [SECURITY.md](SECURITY.md) 中的说明私下报告。

## License

[MIT](LICENSE)

<!-- Translation source: README.md @ 9ae794f3e7d0136866be776399f14f203ce7781b -->
