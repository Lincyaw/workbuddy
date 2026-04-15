# Distributed Topology and CLI

状态：planned

## 目标拓扑

旧需求里关于 coordinator/worker 分离、长轮询通信、多 repo 管理的设想，保留为未来目标态，而不是当前事实。

目标形态：

- coordinator 负责 poll GitHub、状态机、路由、审计和 API
- worker 在不同机器领取任务并执行 agent
- registry 继续按 repo + role 路由
- transport 从当前 channel 演进为 HTTP 长轮询或等价机制

## 目标 CLI

未来可以存在下面这些命令，但当前代码未实现：

- `workbuddy coordinator`
- `workbuddy worker`
- `workbuddy init`
- `workbuddy status`
- `workbuddy run`
- `workbuddy validate`
- `workbuddy logs`

当前这些命令只能作为规划，不可当成已实现功能写进 implemented 文档。

## 设计继承关系

这部分规划不需要推翻当前已有抽象：

- `registry` 仍有价值
- `router` 仍可保留，只是 sender/transport 会替换
- `store`、`eventlog`、`audit`、`webui` 仍可复用

真正需要替换的是：

- 内嵌 worker
- channel 传输
- 本地进程内执行假设

## 多 repo 目标

目标态希望一个 coordinator 管多个 repo，但当前 `serve` 实际仍只消费单个 `repo` 配置。

因此多 repo 能力目前属于设计目标，不能视为当前实现。
