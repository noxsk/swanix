# 异构线路：按业务目标调度

## 设计与边界

不绑定接入类型。线路健康由探测状态机决定，业务适配由策略的 `objective` 决定：高延迟但稳定的路径仍是健康路径，可能适合大吞吐；低延迟但窄带路径适合交互，但也可能被大量小连接压满。

本次实现四种策略目标：

- `capacity`：旧行为，默认不变。
- `latency`：以绝对响应延迟和余量为主，不让高容量直接压过低延迟优势。
- `throughput`：以方向相关的剩余容量为主，可给低延迟路径软预留余量。
- `balanced`：以配置的参考传输量权衡延迟与传输时间，不自动推断连接大小。

公式、参数范围及回退规则见 [配置参考 5.4](CONFIG.md#54-异构线路与业务目标)。公式是工程启发式，不是全局最优或精确 TCP 完成时间模型。没有新增逐包用户态分析；Lite 仍将周期性生成的连接份额交给 nft，Full 仍使用已有原子快照 Pick。

**没有实现也不承诺：** 首包识别最终流大小、普通单流多线路叠加、已建立连接跨 NAT 出口迁移、硬性带宽预留、自动业务学习、逐连接实时负载反馈。HTTP/2、HTTP/3 会话可混合大小业务，策略只能作用于承载它们的连接。

## 配置方式

以下是假定已有 `wan1` / `wan2` 的策略增量示例。不要直接替换整份配置；实际名称与业务地址需自行匹配。

```uci
config policy 'interactive'
 list member 'wan1'
 list member 'wan2'
 option objective 'latency'
 option affinity 'connection'
 option saturation_threshold '90'

config policy 'bulk'
 list member 'wan1'
 list member 'wan2'
 option objective 'throughput'
 option latency_reserve_percent '50'
 option affinity 'connection'
 option saturation_threshold '90'

config policy 'general'
 list member 'wan1'
 list member 'wan2'
 option objective 'balanced'
 option flow_size_kib '256'
 option affinity 'connection'
```

通过现有源/目的地址、协议、端口规则将**已知业务**送入 interactive 或 bulk；Full 可以用出站 `wan_policy` 选择。无法识别的连接可进入 general 或维持原 capacity 默认。不要将所有 TCP/443 一律当作下载或交互。

LuCI「策略与规则」中可选择调度目标；参考传输量仅权衡目标显示，软预留仅吞吐目标显示。切换目标时隐藏的非零预留应清除，后端也会拒绝其他目标上的非零预留。

使用前：

1. WAN 的 `download_mbps` / `upload_mbps` 应接近真实可用容量，不是网口速率；同组 `traffic_bias` 应指向相同方向。
2. 参与目标比较的候选 WAN 应使用相同探测目标与探测方式。新增保护会比较配置目标及最近成功目标集合；不一致时容量回退，而非盲目比较 RTT。
3. `swanix check` 校验后查看 `swanix preview`。预览没有实际 RTT，只能显示容量回退，不代表运行后延迟策略失效。
4. 应用后等待可比的新鲜负载与成功探测，查看各策略 `new_connection_share` 及 `swanix status` JSON 中的 `scheduling_reason`；后者说明目标评分或证据不足回退。份额有 EWMA、最大步长和死区，不保证每轮达到理论目标。
5. 需要硬性保护交互延迟时，仍需 SQM/AQM；软预留无法控制已经分配出去的长流。

IPv4 与 NAT66 使用这些权重；IPv6 native 仍根据源前缀路由。粘滞优先于新连接重新分配，因此粘滞客户端可能继续使用已记录的线路。

## 正确性保护

- 新目标的负载时效按 `3×sample_interval`，最近成功轮次时效按 health 的 `interval+timeout`，不再由调度周期决定探测是否过期。
- 最近轮次失败时不把此前成功 RTT 当成当前证据；任一候选缺少可比证据则整组容量回退，同时暂停饱和溢出，避免未知负载线路被视为安全目的地。
- 只检查当前候选池：待命 backup、Down 线路不妨碍 active 池；active 全不可用时比较 backup 池。
- 新目标不改变健康状态，且保留失败率、偏好、恢复爬坡、失效立即退出。
- 内核权重提交失败保留 dirty 标记，下次即便算法份额未变也重试，避免用户态与内核永久失配。Full 快照仍先发布，短期失配可持续至成功重试。

新目标使用独立基线窗口：仅收录成功轮，成功目标集合变化时重建，避免部分目标失败留下的偏低历史基线让软预留选错线路；原健康/兼容容量窗口不变。回归测试通过实际 ProbeDue→Schedule 覆盖未达 quorum 和已达 quorum 的部分成功后恢复。

共同探测地址也不是完美的业务 RTT：Anycast 路径、服务端处理和特定业务目的地可能不同。不要把探测 RTT 当成所有业务的精确延迟。

## 可重复验证

```sh
go test -count=1 ./experimental/multiwan/...
go test -race -count=1 ./experimental/multiwan/engine ./experimental/multiwan/scheduler ./experimental/multiwan/config
go test -v -count=1 ./experimental/multiwan/scheduler -run 'TestObjective|TestObjectives|TestHeterogeneousFixedSnapshotModel'
go test -v -count=1 ./experimental/multiwan/engine -run 'TestObjectiveRuntime|TestSchedulePublishesDistinctWorkloadPolicies|TestPreviewObjective'
go test ./experimental/multiwan/scheduler -run '^$' -bench BenchmarkObjectives -benchmem -count=3
node ci/test/luci/policy-objectives.js
go test -tags mw_lite ./cmd/swanix
go build -tags mw_lite -o /tmp/swanix-heterogeneous ./cmd/swanix
```

覆盖范围：配置默认兼容/非法值；方向和健康因子；目标反转；排队延迟适配；相等线路与小噪声；stale+saturation；备用与故障；目标集合不一致；独立采样时效；内核失败重试；策略传至 Full 快照和状态输出。前端测试是表单 schema stub，不是完整浏览器交互或路由器实测。

### 固定快照混合业务模型

固定种子 20260421，每组 12,000 个请求（10,781 短请求、1,219 大请求），50 Mbps/10 ms 与 500 Mbps/80 ms 两条路径；负载为配置总容量的 20%/60%/90%。两种策略使用同一到达序列、大小和抽签数，所有请求完整排空，不丢弃慢请求。模型事先知道业务类别，不是算法从首包识别出来的。

对照：旧策略两类均 capacity，新策略短请求 latency、大请求 throughput+50% 软预留。固定初始份额分别为短请求 9:91→98:2、大请求 9:91→5:95。

| 需求负载 | 短请求 p50，旧→新 ms | p95，旧→新 ms | p99，旧→新 ms | 大请求有效吞吐，旧→新 Mbps |
|---|---:|---:|---:|---:|
| 20% | 160.262 → 22.621 | 162.097 → 40.972 | 163.146 → 160.262 | 103.666 → 103.666 |
| 60% | 160.488 → 25.243 | 164.194 → 70.234 | 168.632 → 160.393 | 309.580 → 310.663 |
| 90% | 161.452 → 33.107 | 177.761 → 160.131 | **200.008 → 209.767** | 456.378 → 457.601 |

**高负载 p99 退化约 4.9%，如实保留。** 非零慢路径份额和低延迟窄带拥塞可能拖尾，不能宣称所有分位数均改善。断言要求中位与 p95 改善，大请求吞吐不下降超过 20%、平均完成时间不增长超过 50%；这些是固定模型的宽松回归界限，不是产品 SLA。

这是理想字节公平共享服务模型，加两倍基线 RTT，不模拟 TCP、ACK 竞争、慢启动、丢包、实际队列管理或完整动态反馈；**不是实机吞吐或满载延迟证据**。必须结合高负载反例解读，不能只摘取低负载收益。

### 控制面开销

本机 Apple M4、darwin/arm64、Go 1.27.3，双线路静态输入，3 次中位数：capacity 1.286 µs/op，latency 1.424，throughput 1.481，balanced 1.456。新目标每次多 656 B、5 次分配（35 次 vs 30 次），发生在每策略/每地址族调度周期，不在逐连接热路径。没有宣称新目标比兼容算法计算更快。

原始模型和基准输出保存在 `perf/2026-11-08/heterogeneous-model.txt`、`objectives.txt`。

## 仍需实机验证

- 增加并发连接与大小混合，观察真实短请求 p95/p99、长流吞吐、每线利用率、重传、softirq，以及所有请求的成功率。
- 真实上下行耦合：低上传容量可能堵塞下载 ACK；按方向看余量不能消除这种物理约束。
- 采样到调度更新前的突发：当前按周期计算份额，并非每连接重新估计剩余容量；长连接/粘滞会削弱控制效果。
- 两条都满载、容量随时间变化、探测间歇失败及不同目标集合，应验证回退是否频繁、闭环是否振荡；本次固定快照模型不证明闭环稳定。
- SQM、软硬件卸载、MTU、NAT66、Full TUN 分开比较；实际链路类型不作为分支条件。
- 复用 `ci/test/perf/loaded_latency.py` 做 AB/BA 同时负载测量，并另外使用可区分业务类的发生器；该工具本身不会自动部署策略版本或证明链路满载。

未修改默认出厂策略，也未部署到真实网络。先 opt-in 验证再推广，不以模型结果直接替换所有用户的默认行为。
