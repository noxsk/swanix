# 分配、质量与波动算法：本次验证

## 范围与结论

本次基于 `16a0115` 及开发工作区的已有改动做增量优化，未覆盖或提交原有配置、LuCI、接口历史等改动。
环境：2026-11-08，Apple M4，darwin/arm64，Go 1.27.3；不是路由器。

**已验证**：质量窗口正确性、质量恢复防抖、分配方向一致性、逐连接选择的 CPU/分配开销，以及对比工具的并发测量流程。
**未验证**：真实转发吞吐、真实多连接建立速率和真实满载 p95/p99 延迟的改善。本次没有可用 Linux 测试机（发现的 Linux 机器离线），macOS 上 Linux netns/nft 用例会跳过。不能用微基准或队列模型代替这些实测。

## 最新集成结论：柔性卸载已替代生产硬溢出

本节优先于下文按研究过程保留的历史结论。生产 `Scheduler.Step` 已采用最小柔性方案：候选负载新鲜且有未饱和替代线时，给饱和线原始评分乘以 `clamp((1-u)/(1-threshold), .5, 1)`，不清零、不跳过 EWMA/步长/死区；失效移除仍立即撤回。无新增配置，阈值与默认值不变，行为从“立即最低份额”变为“逐渐有界折扣”。阈值以下因子为1，饱和标志保留10个百分点滞回，不表示实际权重已经变化。状态返回的标志与内部状态隔离。

三组替代策略与历史参考比较：仅保留硬零分目标但遵守平滑，可恢复吞吐，却在 balanced 单线繁忙、1秒反馈下仍有567.67ms队列p95；最终柔性评分为125.44ms，关闭溢出124.96ms。选择柔性评分而不引入V1/V2的额外控制器。

```sh
go test -count=1 -v ./experimental/multiwan/scheduler -run TestSoftSpill
```

| 1秒反馈场景 | 历史硬溢出 | 当前柔性卸载 | 关闭溢出 |
|---|---|---|---|
| capacity 单线繁忙服务量 / 队列p95 | 116.56 Mbps / 833.33ms | 140.00 Mbps / 415.00ms | 140.00 Mbps / 483.23ms |
| capacity 全线过载服务量 / 队列p95 | 142.37 Mbps / 500ms | 199.41 Mbps / 500ms | 199.41 Mbps / 500ms |
| balanced 单线繁忙服务量 / 队列p95 | 114.83 Mbps / 833.33ms | 139.42 Mbps / 125.44ms | 139.42 Mbps / 124.96ms |

这些仍是流体模型，不是TCP实测。1/2/5秒反馈、反序及1ms积分复核覆盖当前生产实现；守护条件为服务量不低于硬溢出且不低于关闭溢出的98%，队列p95不高于关闭溢出的1.1倍+1ms。较慢反馈下延迟不保证改善；全线过载仍需SQM。倍率下限.5是保守工程参数而非最优值。

`hard_spill_reference_test.go` 冻结变更前Step控制流程，与当前实现共享基础评分/取整/步长工具，**不是完整独立历史二进制**；原硬溢出修复与V1/V2研究保留为历史对照。`TestSaturatedFloorSurvivesShareRepair` 现在只针对该参考，当前生产没有硬1%不变量。新测试检查质量/恢复/低偏好接收端、评分折扣上下界、逐步卸载、滞回隔离；既有Down/backup/恢复测试继续覆盖。

配置说明、LuCI中英帮助、运行日志和遥测注释已同步。Linux `TestLiteSaturationSpillover` 改为先等待逐步权重下降，再对照状态份额统计300条连接，不再把饱和标志等同于1%份额。该Linux集成用例本机未执行；历史1/100结果已标为旧版本，不能证明新算法真实吞吐/延迟。尚需实机测试不同容量配置误差、长流、ACK拥塞及稳定健康目标。

## 改动（前期实施记录）

### 1. 分配

`experimental/multiwan/scheduler/scheduler.go`

- 评分 headroom 改用与容量和饱和判断一致的方向：download 看 RX、upload 看 TX、balanced 看两者较大值。旧评分始终看较忙方向，会在下载偏好下再次扣除上传负载的分数。
- 全部候选消失时清空饱和/溢出状态；连续空候选不再报告 Changed，避免无效内核权重更新。
- 保留 EWMA、最大步长、死区、饱和滞回及失效立即撤回策略。
- 再次复查修正 capacity 模式把负载过期线路当成溢出目的地的问题：所有候选负载必须新鲜才启用硬溢出；不完整时清除饱和滞回。普通容量评分及其平滑回退仍保留，不等于完全禁止向未知负载线路分配新连接。`TestCapacitySpillRequiresFreshDestinationLoad` 覆盖过期、恢复到滞回区间和重新饱和。
- 后续复查修正饱和线路份额回流：四 WAN 下其他线路偏好变化会触发步长裁剪后的总和修复，旧修复逻辑把仍饱和线路从 1% 抬至 3%。现在将溢出线路固定在 MinShare，排除出补偿空间及最终缩放。新增 `TestSaturatedFloorSurvivesShareRepair`，覆盖四种目标、连续 20 次更新、总和及步长约束；不是闭环网络延迟证明。

方向偏好不是物理隔离：上传拥塞仍可能阻塞下载 TCP ACK。是否应给反方向保留温和惩罚，需要真实非对称链路数据，不能仅凭模型认定完全忽略反方向始终更好。

### 2. 线路质量与波动

`experimental/multiwan/health/probe.go`、`state.go`

- RTT 基线改成窗口内各轮成功目标 RTT 中位数的最小值，与当前轮 RTT 口径一致。稳定的 `[5,100,110]ms` 不再与单个 5ms 目标比较而误降级。
- 窗口以完成轮数计，失败率以这些轮内实际目标总数计；目标数量不再改变保留轮数。至少 8 个探测才启用失败率降级。
- 有界 ring、增量失败计数和单调最小队列，使查询不必扫描整个历史；每轮 RTT 中位数仍需要按该轮目标排序。
- Degraded 恢复需连续两轮正常；一次正常不再让持续不稳定线路立即升回 Healthy。增加一个探测间隔的恢复等待是明确的防抖代价。
- 保留可达性 Down/Recovering 规则；忙线 RTT 不重复处罚、探测失败仍计入质量。

### 3. 多连接开销

`experimental/multiwan/engine/engine.go`

- Schedule/Reconcile 发布不可变线路选择快照；Pick 原子读取已计算的累计权重、mark、IPv4 源地址。
- 去除热路径上的控制面全局锁、每连接 ID 列表构造与排序、配置及源地址扫描。
- 无候选仍拒绝；未知策略报错、IPv6 不指定 IPv4 源地址；发布前后快照不共享可变数据。
- 成本转移到控制面更新，并非成本消失。测试覆盖控制面持锁时 Pick 不阻塞，以及并发发布的 race 检查。

## 可重复对照与结果

### 确定性功能/模型对照

```sh
go test -count=1 -v ./experimental/multiwan/scheduler \
  -run 'TestCompareDirectionalHeadroom|TestScoreBiasConsistency|TestEmptyPool'
go test -count=1 -v ./experimental/multiwan/health \
  -run 'TestQualityStableMixedTargets|TestQualityWindowCountsRounds|TestQualityStateChurnComparison'
```

输出摘要：

```text
MODEL ONLY fixed shares legacy=map[1:24 2:76] optimized=map[1:50 2:50];
served 143.20 -> 180.00 Mbit/s; queue p95 499.00 -> 0.00 ms
bad bad good × 100: Healthy/Degraded switches legacy=200 optimized=1
```

队列模型仅用于回归：两条 100 Mbit/s 服务队列、180 Mbit/s 等大小短流需求、50 Mbit FIFO、1ms 步长、60s；用固定评分快照隔离方向偏好差异。模型不运行完整调度反馈，不模拟 TCP、ACK、真实 RTT、长流或内核，因此其中 +25.7% 服务量和排队消除**不是实际吞吐/延迟承诺**。p95 是模型中两个队列每个时间步的队列占用换算延迟，不是实际请求延迟。

状态对照中的 bad 是质量异常但 quorum 成功，不是断网；简化旧参考只覆盖 Healthy/Degraded 子状态机。新算法最终维持 Degraded，直到连续两轮正常，不能将减少切换误解为改善了物理线路质量。

### 动态反馈模型：旧硬溢出的负面结果（历史参考）

```sh
go test -count=1 -v ./experimental/multiwan/scheduler -run TestDynamicFeedbackModel
```

新增 `feedback_model_test.go`：两条 100 Mbps 线路、各 50 Mbit 有界队列，等大小短传输按份额注入，已排队数据不迁移。四个阶段各 60 秒：50 Mbps 轻载、140 Mbps 需求且第一线有 40 Mbps 优先背景流量、220 Mbps 全线过载、60 Mbps 排空恢复。采集服务量、丢弃量、按路径/时间等权采样的队列 p95、份额更新、溢出切换及最后 30 秒切换数；阶段末决策计入该阶段，影响下一阶段。恢复阶段服务率包括排空前一阶段积压，因此可能高于当前输入率。

测试比较 capacity/balanced、硬溢出关闭/90%，反馈间隔 1/2/5 秒；负载 EWMA 每样本 .3，反馈间隔也改变平滑时间常数。使用 10ms 和 1ms 两种积分步长复核；测试检查 Mbit 守恒、最低份额、非紧急步长、轻载无丢弃、最终排空，以及输入顺序反转后逐决策份额和溢出标志相同。

修正队列积分为先计算同期服务再限制剩余队列，避免把连续流体人为当成批量到达。修正后，1 秒反馈、capacity 的结果为：

| 阶段 | 硬溢出关闭：服务量 / 队列 p95 | 硬溢出90%：服务量 / 队列 p95 | 硬溢出末30秒切换数 |
|---|---|---|---:|
| 单线有背景负载 | 140.00 Mbps / 483.23 ms | 116.56 Mbps / 833.33 ms | 15 |
| 全线过载 | 199.41 Mbps / 500.00 ms | 142.37 Mbps / 500.00 ms | 13 |

1ms 积分步长得到相同的上述服务量（显示精度）和末段切换数。**PASS 只代表模型守恒/约束等测试通过，不代表硬溢出性能合格**；负面结果没有作为“必须一直变差”的断言固化。当时的硬溢出把饱和线骤降为1%，延迟的负载反馈可引发反复倒流和闲置容量；尚需设计有目的地剩余容量约束的平滑卸载，并用此测试验证，而不是仅增大死区掩盖切换。

模型不含 TCP/ACK、长流、失败重传或探测状态机；背景流量优先、需求不响应丢弃、新份额立即影响新字节的假设会放大某些动态效应。p95 不是请求延迟；不能据此量化真实网络损失，也不能把关闭硬溢出视作普遍最优。全线过载时两种策略均有 500ms 队列 p95，进一步说明调度不是 SQM。当前机器发现结果：本机 macOS 可用，Linux `ubuntu` 为 `offline`，无 SSH 主机，不能执行 Linux 数据面对照。

### 有界卸载候选 V1：模型有收益，但拒绝进入生产

```sh
go test -count=1 -v ./experimental/multiwan/scheduler \
  -run 'TestBoundedSpillFeedbackExperiment|TestBoundedSpillPromotionDiagnostics'
```

新增两个仅测试文件 `bounded_spill_experiment_test.go`、`bounded_spill_limits_test.go`。候选按接收端剩余容量计算转移比例，并限制每轮步长；总配置容量仅用作 Mbps 到连接份额的比例尺，不是新流量需求估计，因此不能称为真实接收容量保证。生产代码、配置与默认值未接入该候选。

在同一闭环模型中加入三组对照：现有硬溢出、有界候选、关闭溢出；不只与较差基线比较。下面是 1 秒反馈的结果（服务量 / 路径时间队列 p95）：

| 场景 | 硬溢出 | 有界候选 | 关闭溢出 |
|---|---|---|---|
| capacity 单线背景负载 | 116.56 Mbps / 833.33 ms | 140.00 Mbps / 299.67 ms | 140.00 Mbps / 483.23 ms |
| capacity 全线过载 | 142.37 Mbps / 500 ms | 199.53 Mbps / 500 ms | 199.41 Mbps / 500 ms |
| balanced 单线背景负载 | 114.83 Mbps / 833.33 ms | 139.42 Mbps / 471.60 ms | 139.42 Mbps / 124.96 ms |

capacity 的单线/全线负载末 30 秒溢出切换从 15/13 次降至 0/0 次；候选最大单步分别为 7/4 点。2/5 秒反馈仍有吞吐收益，但不保证延迟改善。输入顺序反转的逐步轨迹与 1ms 积分复核通过；测试对吞吐、残留切换及排空有边界断言，**没有断言所有延迟分位数均改善**，不代表达到生产门槛。

**拒绝推广的原因**：候选把普通目标替换成从旧份额开始的转移，覆盖健康、偏好、恢复斜坡；balanced 也可能比直接关闭溢出更慢。固定输入 40 次的诊断得到：

| 接收端情况 | 候选接收端份额 | 关闭溢出份额 |
|---|---:|---:|
| 86% 利用率、转移量不足死区 | 50%（冻结） | 53% |
| Degraded 且探测失败率 .5 | 99% | 39% |
| Recovering 斜坡起点 | 99% | 20% |
| Preference=1 | 99% | 7% |

这些诊断只校验合法分配并打印行为，不把错误结果写成必须保留的断言；文件顶部显式标注 REJECTED V1。下一版应约束普通目标而不是抛弃它，并保留死区以下的累计意图。还需多接收端、异构容量、长短流和真实网络验证。此次实验确认了减少骤变的方向有价值，但**没有证明候选优于关闭硬溢出，更没有修复生产硬溢出的振荡**。

### 目标保留型 V2：缺陷回归通过，暂不接入生产

```sh
go test -count=1 -v ./experimental/multiwan/scheduler -run TestConstrainedSpill
```

新增 `constrained_spill_test.go`、`constrained_spill_invariants_test.go`、`constrained_spill_state_test.go`，仅测试。V2 先运行关闭硬溢出的普通评分，再在普通目标确实卸载饱和线时限制接收端正向增量，按比例缩小负向增量以守恒。不改变方向、不超过普通目标、不重置 EWMA；死区只在普通目标处判断，不对受限小步重复过滤。比例尺仍是总配置容量，非实际新流字节率，不能当成接收端容量保证。

已执行的回归：

- 四种目标 × 临界负载/Degraded/Recovering/低偏好，80轮后与普通调度差异不超过3个百分点。capacity 下接收端分别为54%/39%/20%/7%，V1 的后三种均错误到达99%。
- throughput 每轮 .1 点受限增量：100轮从50%推进到60%，保留整数份额未变化时的内部小数进度；`Changed` 对应真实整数份额变化。
- 四种目标各1000轮四WAN异构容量测试，轮换 download/upload/balanced，随机负载、RTT、偏好、失败率；逐步与相同内部状态的普通调度比较，验证不反转/超越普通方向、浮点步长、最低份额和总和。
- 负载过期、缺RTT、零容量、同数量候选替换、Down、备用线路、候选增加、全空均回退到普通决策并清理滞回。
- 返回的卸载标志不与内部滞回共用 map，调用者修改返回值不影响后续决策；接收份额增加的饱和路径不标成卸载来源。这里的标志是受约束步骤中的卸载来源，不是完整饱和状态，不能与旧硬溢出切换次数直接等同。

1秒反馈闭环对照（hard/off/V2）：capacity 单线繁忙服务量116.56/140.00/140.00 Mbps，队列p95 833.33/483.23/483.23ms；全线过载142.37/199.41/199.41 Mbps，队列p95均500ms。balanced 单线繁忙服务量114.83/139.42/139.42 Mbps，队列p95 833.33/124.96/124.96ms。2/5秒反馈、反序输入和1ms积分复核通过；模型验收限制服务量不低于关闭溢出的98%，队列p95不高于其1.1倍加1ms，并明确保留模型不是TCP/真实请求延迟的边界。

**结论**：V2 修复了V1覆盖普通目标及小步冻结的问题，但大部分模型收益来自取消硬溢出跳变，与关闭硬溢出几乎相同。尚未证明新增约束具有足够额外价值，故没有修改生产策略/配置/默认值。下一步应优先比较最小生产改动（保留普通平滑、不再绕过步长）和V2，而非仅因原型测试通过就引入更多控制状态。此为V2阶段的结论；后续生产集成见本文顶部。真实长短混合流及Linux数据面效果仍需验证。

### CPU 微基准

冻结旧 Pick/Quality 实现留在测试中，对相同输入比较；并未用当前实现作为自己的基线。

```sh
go test -run '^$' -bench 'BenchmarkPickComparison/WANs=(2|14)/' \
  -benchmem -benchtime=300ms -count=5 -cpu=1,4 ./experimental/multiwan/engine
go test -run '^$' -bench BenchmarkQualityRing \
  -benchmem -benchtime=300ms -count=5 -cpu=1 ./experimental/multiwan/health
```

本机五次中位数（原始输出见 `perf/2026-11-08/`）：

| 操作 | 旧 ns/op | 新 ns/op | 说明 |
|---|---:|---:|---|
| Pick，2 WAN，1 CPU | 251.1 | 44.10 | 热路径 CPU 耗时 −82.4% |
| Pick，2 WAN，4 CPU 并发 | 222.3 | 12.74 | 聚合每操作耗时 −94.3% |
| Pick，14 WAN，1 CPU | 1128 | 57.94 | 112 B、1 次分配 → 0 |
| Pick，14 WAN，4 CPU 并发 | 771.5 | 20.01 | 不再被全局锁串行化 |
| Quality Push+Impaired，20 轮 | 142.3 | 30.74 | 55 B/op → 0 |
| Quality Push+Impaired，2000 轮 | 7484 | 32.27 | 用于检验窗口扩展性，非默认配置 |

旧 Quality 的 `allocs/op=0` 是摊销整数显示，`B/op>0` 表示仍分配。新旧质量窗口语义已修正，微基准衡量同一探测输入的处理成本，不表示算法行为相同。

这些 Go benchmark 按子用例顺序运行，不是 AB/BA 交替网络实验；环境有负载噪声，不报告统计显著性。并发 ns/op 是总工作量除以时间得出的聚合成本，不是单请求 p99，也不能直接推算网络连接速率。

### 满载测量工具

新增 `ci/test/perf/loaded_latency.py`，详见 [运行说明](../../ci/test/perf/loaded_latency.md)。此前数据路径 connect 与 bulk 顺序运行，不能称为满载延迟。本工具先启动 bulk，预热后运行 connect，检查 bulk 在整个 connect 过程持续存活且时序覆盖；AB/BA 多轮交替，保留原始 JSON、命令、错误、时序及成对比值中位数。

- 本次实际 loopback smoke：两轮配对、顺序 A B B A，4 个有效 run、0 个拒绝；这是工具自检，不是算法 A/B。
- 4 个 Python 测试覆盖统计、无效/错误结果拒绝、覆盖边界和子进程重叠。
- 后续补齐 `perfload` p95 字段和配对统计，增加 Go 分位数/JSON 回归测试，并校验 p50 ≤ p95 ≤ p99。使用排序后下标 floor(p×(n−1)) 的既有经验分位数定义，只统计成功 connect+banner 耗时，不是包 RTT；缺少 p95 的旧二进制会被拒绝，需要重新构建。
- 补齐 p95 后重新运行 loopback AB/BA 自检：4 个有效 run、0 个拒绝；仅验证工具采集链路，不能当成算法 A/B 效果。
- bulk 吞吐覆盖预热+connect+尾部，不是仅 connect 的同窗吞吐；工具不能证明每条流持续活跃，也不能仅凭并发下载证明物理瓶颈满载。
- 两地址不自动部署两个算法版本。请分别固定配置、版本、链路和路由；同一目标只能证明工具可重复运行。

## 本次执行的检查

```sh
go test -count=1 ./experimental/multiwan/...
go test -race -count=1 ./experimental/multiwan/health ./experimental/multiwan/scheduler ./experimental/multiwan/engine
go test -tags mw_lite ./cmd/swanix
go build -tags mw_lite -o /tmp/swanix-algorithm-check ./cmd/swanix
python3 -m unittest discover -s ci/test/perf -p 'test_loaded_latency.py' -v
```

上述检查通过；最初失败的两个现有质量回归用例现已通过。Linux 特有用例的跳过不算数据面验证，Full TUN 也未在本次构建/实测范围内。

## 尚需 Linux / 实机验证的部分

1. **吞吐与满载延迟**：相同路由器上分别部署前/后版本，以至少 6 轮 AB/BA 比较；记录空闲及满载 RTT p50/p95/p99、吞吐、错误、各 WAN 利用率、CPU softirq、重传和权重变动。用流数 1/8/32/128 确认吞吐平台，排除发生器 CPU 瓶颈；当前 connect 工具提供 p50/p90/p95/p99，配对报告汇总 p50/p95/p99；仍需另行采集空闲 RTT 和真实链路满载证据。
2. **非对称链路**：100/10、500/50 等容量，上传满载同时下载、两线全满、一线满另一线空闲，检查忽略反方向 headroom 是否损伤 ACK；balanced/download/upload 分开对照。
3. **真实多连接开销**：Lite 内核路径与 Full 代理路径分别测连接速率、p99、CPU/内存；本次 Pick 优化只影响 Full 逐连接选择，不能据此声称 Lite 转发加速。
4. **SQM 与长流**：AQM/SQM 开关、软硬件 flow offload、不同 MTU、长 TCP/QUIC 流、粘滞和双栈。调度仅控制新连接，不能迁移既有长流或直接消除所有链路满载时的 bufferbloat。
5. **探测/采样对齐的剩余盲区**：ProbeDue 同步等待目标，主循环期间不能按时采样；传输刚启动或计数器过期时，缓存负载可能不对应 RTT 所在时段。20% 配置容量阈值在配置高于真实瓶颈时也可能误判。本次未重构事件循环或动态估计容量，应以假时钟测试和实机时序数据进一步验证。
6. **探测证据边界**：成功目标集合变化仍可能移动 RTT 中位数；稳定坏目标仍会贡献失败率；长期持续变慢最终会进入新基线。保持独立健康目标、检查 quorum 与超时；失败率是 TCP/HTTP 探测失败率，不是链路丢包率。
7. **恢复与历史展示**：Recovering 中单次失败仍立即 Down，保守保护可能在边缘链路上反复撤回；暂不改变这项既有策略。LuCI 30 秒采样的健康占比、峰值和状态色块不是精确在线时长或未采样尖峰，不能拿它们代替逐轮满载测量。
