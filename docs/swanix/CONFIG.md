# 配置参考（`/etc/config/swanix`）

swanix 的多线路配置全部保存于 UCI 文件 `/etc/config/swanix`；LuCI「网络 → Multi-WAN (swanix)」页面读写的也是该文件。
Full 版的代理配置另存于 `proxy.json`，见[第 8 节](#8-proxy-与-proxyjsonfull-版)。

修改配置后应先执行 `swanix check`。该命令采用与守护进程相同的规则进行严格校验，每条错误均给出字段路径、原因与修改建议，例如：

```
wan.wan2.upload_mbps: 必须大于 0，单位 Mbps
policy.balanced.member: 引用已存在的 wan section（未找到 wan.wan9）
```

未知字段、重复 ID、无效引用及非法取值均会被拒绝，不会被静默忽略。配置变更通过 `swanix apply`（或 LuCI 的「保存并应用」）生效，
参见[使用指南第 6 节](USAGE.md#6-切换与确认)。

配置文件由以下 section 构成：

| section | 数量 | 用途 |
|---|---|---|
| `global 'global'` | 1 | 总开关、受管 LAN、默认策略、调度周期 |
| `kernel 'kernel'` | 1 | swanix 占用的 mark 位、路由表编号与规则优先级区间（通常无需修改） |
| `wan '<名称>'` | 1–14 | 每条线路一个 |
| `health '<名称>'` | 任意 | 健康检测参数，由线路的 `option health` 引用 |
| `policy '<名称>'` | ≥1 | 线路池：成员、粘滞、满载转移 |
| `rule '<名称>'` | 任意 | 将匹配的流量交由指定策略处理 |
| `ipv6 'ipv6'` | 1 | IPv6 处理方式 |
| `proxy 'proxy'` | 1 | Full 版代理运行时 |

布尔值取 `'0'` 或 `'1'`。下表中「默认」指未设置该选项时采用的值，「—」表示无默认值、须显式设置。

---

## 1. `global`

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `config_version` | `1` | `1` | 配置结构版本，与程序版本无关。 |
| `enabled` | `0` | `0` / `1` | 总开关。安装后为 `0`。 |
| `startup_mode` | `observe` | `observe`、`manage` | `observe`：仅读取并展示 netifd 状态，**不向内核写入任何内容**，可与 mwan3 共存；此模式不执行探测（探测依赖 swanix 的路由表），线路状态仅反映 netifd 链路状态。`manage`：接管路由。 |
| `lan_network` | — | netifd 逻辑接口名（list） | 受管 LAN，例如 `lan`、`guest`。仅从这些接口进入的流量参与分流。`enabled 1` 时至少须指定一个。 |
| `manage_router_traffic` | `1` | `0` / `1` | 是否对路由器自身发起的连接（DNS、NTP、软件包下载等）进行分流。为 `0` 时，路由器自身流量经 `main` 路由表转发。无论取值如何，**经某条线路进入的连接（端口转发，以及路由器上的 SSH、LuCI、VPN 等服务）均经该线路回包**。 |
| `default_policy` | 唯一的策略 | policy 名称 | 未被任何 `rule` 匹配的流量所使用的策略。仅定义一个策略时可省略；定义多个策略时必须指定。 |
| `flow_offload` | `off` | `off`、`software` | 须与 fw4 的 flow offloading 设置一致，由 `doctor` 检查。`software` 已经验证：已卸载的连接沿用其首包选定的线路。`hardware` 未在硬件上验证，不予接受。 |
| `persist_history` | `0` | `0` / `1` | 保留项，当前无作用。吞吐记录仅保存在内存中，约 1 小时。 |
| `confirm_timeout` | `90` | 10–600（秒） | `apply` 后等待 `confirm` 的时限，自切换通过验证时起算。 |
| `update_interval` | `10` | 2–300（秒） | 调度周期，即重新计算各线路新连接份额的间隔。 |
| `sample_interval` | `2` | 1 至 `update_interval`（秒） | 采集网卡收发字节数并计算实际速率的间隔。 |

## 2. `kernel`

swanix 仅使用下列内核资源，移除时亦仅撤销这些资源。默认值不与常见组件冲突，通常无需修改。

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `mark_mask` | `0xff000000` | 连续 8 位的十六进制掩码 | swanix 使用的 fwmark / ct mark 位。其他组件（代理、VPN、QoS 等）不得写入这些位；`doctor` 会报告检测到的冲突。 |
| `table_base` | `1000` | 1–60000 | 路由表编号基数：线路 *N* 的业务表为 `table_base+N`，探测表为 `table_base+256+N`。 |
| `priority_base` | `10000` | 1–30000 | `ip rule` 优先级区间起点，占用 `priority_base` 至 `priority_base+255`。 |
| `nft_table` | `swanix` | 字母、数字、下划线 | nftables 表名（`inet` 族）。 |
| `control_socket` | `/var/run/swanix/control.sock` | `/var/run/` 下的路径 | 命令行工具、rpcd 与守护进程之间的通信套接字。 |

## 3. `wan`

每条线路对应一个 `wan` section。section 名称（如 `wan1`）即该线路在策略、状态输出与日志中的名称。

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `id` | — | 1–14，唯一 | 稳定编号，决定该线路的 mark 与路由表编号。修改后已有连接将失去归属，不应随意更改。 |
| `enabled` | `1` | `0` / `1` | 禁用的线路不参与任何策略。 |
| `network` | — | netifd 逻辑接口名 | 线路的 IPv4 接口，如 `wan`、`wan2`。接口协议不限，静态、DHCP、PPPoE 均可。一个接口只能属于一条线路。 |
| `ipv6_network` | `auto` | `auto`、`none`、接口名 | 线路的 IPv6 接口。`auto`：`network` 自身具有 IPv6 默认路由时使用该接口，否则使用 `<network>6`（通常为 `wan6`）或 `<network>_6`（PPPoE / DHCP 接口设置 `option ipv6 auto` 时由 netifd 动态创建）；存在多个候选时不作推断，在状态中报告问题，须显式指定。`none`：该线路不承载 IPv6。 |
| `download_mbps` | — | > 0 | 线路**实际可用**的下行速率（Mbps）。份额分配与满载判定均以此为依据，须填写实际值。 |
| `upload_mbps` | — | > 0 | 线路实际可用的上行速率（Mbps）。 |
| `preference` | `100` | 1–1000 | 相对偏好系数，作用于容量权重。`200` 表示在同等条件下获得两倍份额。该值不代表容量。 |
| `role` | `active` | `active`、`backup` | `backup` 线路仅在所属策略中**无任何可用的 `active` 线路**时承担流量。 |
| `traffic_bias` | `balanced` | `download`、`upload`、`balanced` | 计算容量与利用率时采用的方向：`download` 取下行，`upload` 取上行；`balanced` 的容量取两者平均值，利用率取两个方向中的较大者。 |
| `health` | 内置默认值 | health 名称 | 引用的健康检测 section。未设置时使用内置默认值（见第 4 节）。 |
| `probe_target` | — | 同 `health.target`（list） | 仅为该线路追加的探测目标，例如该运营商自有的服务器。 |

## 4. `health`

健康检测按线路、按地址族（IPv4 / IPv6）分别进行。探测连接携带该线路的探测 mark，并绑定至该线路的网络设备，仅经该线路的探测路由表发出；
**探测失败时不会改经其他线路发出**。

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `target` | `1.1.1.1:443`、`9.9.9.9:443`、`8.8.8.8:443` | `IP:端口`、`[IPv6]:端口`、`http(s)://IP/路径`（list） | 探测目标，**须为 IP 地址**，以避免域名解析经由其他线路完成。`IP:端口` 形式执行 TCP 握手探测，URL 形式执行 HTTP(S) 请求。IPv4 线路仅使用 IPv4 目标，IPv6 同理，因此两类目标均应配置。 |
| `interval` | `5` | ≥ 1（秒） | 探测轮次间隔。 |
| `timeout` | `3` | 1 至 2×`interval`（秒） | 单个目标的超时时间。 |
| `successes_required` | `1` | 1 至目标数量 | 一轮探测中判定成功所需的最少成功目标数。 |
| `down_rounds` | `3` | ≥ 1 | 连续失败多少轮后判定为故障（`down`）。 |
| `up_rounds` | `3` | ≥ 1 | 故障后连续成功多少轮后判定为恢复。 |
| `recover_ramp` | `30` | 0–3600（秒） | 恢复后份额由低到高逐步上升的时长，避免刚恢复的线路瞬时承接大量新连接。 |

线路状态依次为：`unknown`（尚无结果）→ `healthy` / `degraded`（可达，但空闲时延迟或失败率连续升高；负载下的排队不进入该状态）/ `down` → `recovering` → `healthy`。
网卡断开（netifd `ifdown`、PPPoE 掉线、链路中断）经 netlink 事件立即判定为 `down`，无需等待探测轮次。

故障判定时间约为 `interval × down_rounds`（默认约 15 秒）。缩短该时间可减小上述两个值，但探测流量随之增加，误判概率亦相应升高。

## 5. `policy`

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `member` | — | wan 名称（list），不得为空 | 策略包含的线路。 |
| `affinity` | `connection` | `connection`、`source`、`source_destination` | 粘滞方式，见下文。 |
| `affinity_timeout` | `300` | 10–86400（秒） | 粘滞记录在无新连接后保持的时长。 |
| `saturation_threshold` | 关闭 | `0`，或 50–99（%） | 满载转移阈值，见下文。 |
| `unavailable_action` | `reject` | `reject` | 策略中无可用线路时拒绝新连接，**不回退至 `main` 路由表**，以免流量经非预期线路发出。 |

### 5.1 份额计算

每个调度周期内，每条可用线路得到一个评分，评分为以下因子之积：按 `traffic_bias` 计算的容量、`preference`、余量（当前利用率越高，该因子越小）、
空闲延迟（相对基线 RTT 升高越多，该因子越小；线路正在承载流量时探测 RTT 含有本线路的排队，不再重复降权）、探测成功率。评分按比例换算为新连接份额（百分比），经指数平滑后每次最多调整 10 个百分点；
变化小于 3 个百分点时不作调整，以抑制抖动。每条可用线路至少保留 1% 的份额。线路故障时立即退出分配，不受平滑限制。

**份额仅决定新连接的线路分配。已建立的连接始终保持在原线路上，单条连接不会叠加多条线路的带宽。**

### 5.2 粘滞（`affinity`）

| 取值 | 行为 |
|---|---|
| `connection` | 每条新连接按份额独立分配，均衡效果最佳。 |
| `source` | 同一源地址在 `affinity_timeout` 内的连接使用同一线路，适用于对出口地址敏感的业务（网上银行、在线游戏、带风控的网站等）。 |
| `source_destination` | 同一源地址访问同一目的地址时使用同一线路；访问不同目的地址时仍可分配至不同线路。 |

已记录的线路不可用时立即重新分配，不等待超时。粘滞以客户端为单位生效，会降低相应客户端的均衡程度。

### 5.3 满载转移（`saturation_threshold`）

* 线路的实测速率达到其 `download_mbps` / `upload_mbps`（按 `traffic_bias` 所取方向）的设定百分比，且策略中存在低于该阈值的线路时，
  该线路仅保留 1% 的最小份额，新连接立即分配至未满载的线路，不受每次 10 个百分点的调整限制。
* 速率回落至阈值以下 10 个百分点后恢复分配，份额逐步回升。
* 策略中全部线路均达到阈值时，按正常权重分配。
* 判定依据为网卡计数器（按 `sample_interval` 采样并平滑），在下一个 `update_interval` 生效，响应时间为秒级。
* 与粘滞同时启用时，**粘滞优先**：已记录线路的客户端继续使用该线路，满载转移仅影响新的线路分配。

## 6. `rule`

规则按 `priority` 升序匹配，以第一条匹配的规则所指定的策略为准；均未匹配时使用 `global.default_policy`。未设置的匹配条件视为匹配任意值。

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `enabled` | `1` | `0` / `1` | |
| `priority` | — | 1–9999 | 数值越小，匹配越靠前。 |
| `src` | 任意 | IP 或 CIDR（list） | 源地址。 |
| `dest` | 任意 | IP 或 CIDR（list） | 目的地址。 |
| `proto` | 任意 | `tcp`、`udp`、`icmp`、`icmpv6` | 协议。 |
| `dest_port` | 任意 | `80`、`8000-8100` 形式（list） | 目的端口，要求 `proto` 为 `tcp` 或 `udp`。 |
| `local` | `0` | `0` / `1` | `0`：仅匹配 LAN 发起的流量；`1`：仅匹配路由器自身发起的流量（要求 `manage_router_traffic 1`）。 |
| `policy` | — | policy 名称 | 匹配后使用的策略。 |

IPv6 采用 `native` 模式时，IPv6 流量按源前缀选择线路，规则不作用于 IPv6；`swanix preview` 会对此给出提示。

## 7. `ipv6`

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `mode` | `native` | `native`、`nat66`、`unmanaged` | 见下文。 |
| `nat66_source` | — | IPv6 CIDR（list） | `nat66` 模式下允许进行源地址转换的 LAN 前缀，通常为 ULA（如 `fd00::/64`）。`nat66` 模式下必须设置。 |
| `nat66_router_traffic` | `0` | `0` / `1` | `nat66` 模式下是否同时转换路由器自身发起的 IPv6 流量。 |

| 模式 | 说明 |
|---|---|
| `native` | 适用于各线路均有委派前缀的场景。各线路委派至 LAN 的前缀互不相同，LAN 主机所用源地址的前缀决定出口线路，不进行地址转换。线路故障时，其前缀的流量被拒绝（主机将改用其他前缀的地址），不会改经其他线路转发（否则源地址不合法）。此模式下 IPv6 无法按连接均衡，均衡程度取决于主机的源地址选择。 |
| `nat66` | LAN 使用 ULA 前缀，出口处转换为线路地址。IPv6 可与 IPv4 一样按连接均衡与粘滞。 |
| `unmanaged` | swanix 不处理 IPv6，IPv6 流量经 `main` 路由表转发。 |

## 8. `proxy` 与 `proxy.json`（Full 版）

| 选项 | 默认 | 取值 | 说明 |
|---|---|---|---|
| `enabled` | `0` | `0` / `1` | 启动 sing-box 代理运行时。Lite 版设为 `1` 时校验失败。 |
| `config_file` | `/etc/swanix/proxy.json` | 路径 | sing-box 配置文件。swanix 不改写该文件，仅在加载时注入多线路服务。示例文件位于 `/usr/share/swanix/proxy.json.example`。 |
| `transparent_mode` | `off` | `off`、`tun` | `tun`：由 `proxy.json` 中的 tun 入站接管 LAN 流量，swanix 将校验其规则与 mark，见下文。不支持 `tproxy`。 |
| `interrupt_on_down` | `1` | `0` / `1` | 线路故障时关闭经该线路建立的代理连接，使多路复用、HTTP/2、QUIC 等长连接会话立即经可用线路重建。探测可靠性不足、存在误判可能时可设为 `0`。 |

### 8.1 `wan_policy`

在 `proxy.json` 的出站上设置 `"wan_policy": "<策略名>"` 后，该出站每建立一个连接（TCP 或已连接的 UDP），
或每创建一个未连接的 UDP 套接字（QUIC 类出站使用），即按该策略的份额选择一条线路：

```json
{
  "inbounds":  [{ "type": "socks", "tag": "in", "listen": "192.168.1.1", "listen_port": 1080 }],
  "outbounds": [{ "type": "direct", "tag": "balanced", "wan_policy": "balanced" }],
  "route": { "final": "balanced" }
}
```

约束如下：

* 所引用的策略须在 `/etc/config/swanix` 中定义，由 `swanix check` 校验。
* 不得与 `detour`、`bind_interface`、`routing_mark`、`inet4_bind_address`、`inet6_bind_address`、`netns` 同时使用，因为这些选项会固定出口。
  经上游代理转发时，应将 `wan_policy` 设置在实际建立物理连接的出站上。
* 长连接会话（naive、多路复用、HTTP/2 等）在建立时选定线路，会话内的全部流均经该线路传输。
* `route.default_mark` 及任一出站的 `routing_mark` 不得落在 `kernel.mark_mask` 范围内。

### 8.2 TUN 透明接管（`transparent_mode tun`）

`proxy.json` 中须包含启用 `auto_route` 的 tun 入站，且至少一个出站设置了 `wan_policy`。swanix 在 `check` 与启动时校验以下条件：

* tun 入站的 `iproute2_rule_index`（默认 9000）至其后 10 位须位于 `kernel.priority_base` 之前，以保证 LAN 流量先进入 TUN；
* `iproute2_table_index`（默认 2022）不得落在 swanix 的路由表范围（`table_base` 至 `table_base+511`）内；
* **不得启用 `auto_redirect`**：其 nftables 重定向仅放行 mark 恰为其自身 mark 的连接，会将 swanix 的探测连接与 `wan_policy` 连接重定向回代理；
* 存在启用 `auto_route` 的 tun 入站而 `transparent_mode` 为 `off` 时，校验失败。

```json
{
  "inbounds": [{ "type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30"], "auto_route": true, "stack": "system" }],
  "outbounds": [{ "type": "direct", "tag": "wan", "wan_policy": "balanced" }],
  "route": { "final": "wan" }
}
```

---

## 完整示例：两条线路按带宽均衡，第三条线路备用

```
config global 'global'
	option config_version '1'
	option enabled '1'
	option startup_mode 'manage'
	list lan_network 'lan'
	option default_policy 'balanced'

config wan 'ct'
	option id '1'
	option network 'wan'          # PPPoE
	option download_mbps '1000'
	option upload_mbps '50'
	option health 'fast'

config wan 'cu'
	option id '2'
	option network 'wan2'         # DHCP
	option download_mbps '500'
	option upload_mbps '50'
	option health 'fast'

config wan 'lte'
	option id '3'
	option network 'wwan'
	option download_mbps '50'
	option upload_mbps '10'
	option role 'backup'
	option health 'fast'

config health 'fast'
	list target '223.5.5.5:443'
	list target '119.29.29.29:443'
	list target '[2400:3200::1]:443'
	option interval '3'
	option timeout '2'
	option down_rounds '3'
	option up_rounds '3'

config policy 'balanced'
	list member 'ct'
	list member 'cu'
	list member 'lte'
	option affinity 'source_destination'
	option affinity_timeout '600'
	option saturation_threshold '90'

config ipv6 'ipv6'
	option mode 'native'
```

其他场景（DNS 固定线路、指定主机使用指定线路、主备、PPPoE 上的 IPv6 等）见[使用指南第 8 节](USAGE.md#8-典型场景)。
