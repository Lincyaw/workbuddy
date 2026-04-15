# Config Schema Drift

状态：mismatch

## 现象

仓库里的 `.github/workbuddy/config.yaml` 写的是一个比当前 loader 更大的 schema：

```yaml
environment: dev
repo: Lincyaw/workbuddy
poll_interval: 30s

server:
  port: 8080
  host: 127.0.0.1

agents_dir: .github/workbuddy/agents
workflows_dir: .github/workbuddy/workflows

log:
  dir: .workbuddy/logs
  format: jsonl
  max_size_mb: 100
```

但当前 `GlobalConfig` 只支持：

- `repo`
- `environment`
- `poll_interval`
- `port`

代码：

- `internal/config/types.go`
- `internal/config/loader.go`
- `cmd/serve.go`

## 具体不一致点

### 1. `server.port` 不会映射到当前 `GlobalConfig.Port`

当前代码读取的是顶层 `port`，不是嵌套的 `server.port`。

### 2. `server.host` 当前没有消费方

代码里没有一个稳定的 host 配置入口。

### 3. `agents_dir` / `workflows_dir` 只是文档表达，不是当前 loader 真能力

当前 loader 仍然按固定目录：

- `<configDir>/agents`
- `<configDir>/workflows`

### 4. `log.*` 当前没有形成对应 config 结构

日志目录和格式没有通过 `GlobalConfig` 暴露成统一配置接口。

## 建议收敛方式

二选一：

1. 扩展 loader 和 `GlobalConfig`，把这些字段真正实现出来。
2. 收窄 `config.yaml` 样例，只保留当前真正支持的字段。

在未做选择前，`config.yaml` 不能被视为当前完整 schema 真源。
