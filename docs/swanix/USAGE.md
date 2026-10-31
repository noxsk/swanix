# 使用指南

本文档按以下顺序说明 swanix 的部署与使用：版本选择、获取软件包、安装、配置、检查、切换与确认、日常运维、典型场景、故障排查、升级与卸载。
各配置项的定义、默认值与取值范围见[配置参考](CONFIG.md)。

## 1. 版本选择

| 项目 | Lite（`swanix-lite`） | Full（`swanix-full`） |
|---|---|---|
| 多线路分流、健康检测、动态权重、粘滞、满载转移、IPv4/IPv6、LuCI | 支持 | 支持 |
| 代理协议、TUN 透明接管、出站 `wan_policy` | 不支持 | 支持（sing-box 完整功能集，含 naive） |
| ipk 大小 / 可执行文件大小（x86_64） | 约 3 MB / 7 MB | 约 32 MB / 91 MB |
| 空闲内存占用（实测） | 约 10 MiB | 约 50 MiB |

仅需多线路功能时应选用 Lite。Full 版体积较大，不适用于闪存容量较小的设备。两个版本互斥，可相互替换，替换时配置保留。

## 2. 获取软件包

### 2.1 GitHub Actions 构建产物

在仓库 Actions 页面中打开最近一次成功的 `build` 运行，构建产物（Artifacts）按「系统版本-平台-变体」命名：

| 产物 | 适用范围 |
|---|---|
| `pkg-24.10-x86-64-lite` / `-full` | OpenWrt 24.10，x86_64（ipk） |
| `pkg-24.10-mediatek-filogic-lite` / `-full` | OpenWrt 24.10，MT798x 等 filogic 平台（aarch64_cortex-a53，ipk） |
| `pkg-25.12-x86-64-lite` / `-full` | OpenWrt 25.12，x86_64（apk） |
| `pkg-25.12-mediatek-filogic-lite` / `-full` | OpenWrt 25.12，filogic 平台（apk） |

软件包位于压缩包内的 `<系统版本>/<平台>/<包架构>/` 目录（例如 `24.10/x86/64/x86_64/`）。Lite 压缩包包含 `swanix-common`、`swanix-lite`、
`luci-app-swanix` 及中文语言包 `luci-i18n-swanix-zh-cn`；Full 压缩包仅包含 `swanix-full`，其余三个软件包取自同平台的 Lite 压缩包。
`meta/` 目录为独立可执行文件与能力清单，安装时不需要。

### 2.2 自行构建

可在 Actions 页面手动运行 `build` 工作流（该工作流不创建 tag，不发布 release），或在本地使用 `ci/build/` 下的脚本，参见 README 的「开发」一节。

## 3. 安装

依赖包（`ip-full`、`nftables-json`、`firewall4`、`kmod-nft-nat` 等）由官方软件源提供，安装前须更新软件源索引。
将软件包上传至路由器（下例为 `/tmp/swanix/`）后执行：

```sh
# OpenWrt 24.10（opkg）
opkg update
opkg install /tmp/swanix/swanix-common_*.ipk /tmp/swanix/swanix-lite_*.ipk \
             /tmp/swanix/luci-app-swanix_*.ipk /tmp/swanix/luci-i18n-swanix-zh-cn_*.ipk

# OpenWrt 25.12（apk；自行构建的软件包未签名，须使用 --allow-untrusted）
apk update
apk add --allow-untrusted /tmp/swanix/swanix-common-*.apk /tmp/swanix/swanix-lite-*.apk \
             /tmp/swanix/luci-app-swanix-*.apk /tmp/swanix/luci-i18n-swanix-zh-cn-*.apk
```

安装完成后，swanix 处于**禁用、仅观察**状态（`enabled 0`、`startup_mode observe`），不修改路由，也不影响 mwan3。
LuCI 入口位于「网络 → Multi-WAN (swanix)」。

## 4. 配置

配置可通过 LuCI 完成，也可直接编辑 `/etc/config/swanix`。接管前至少应确认以下各项：

1. **受管 LAN**：`global.lan_network`，通常为 `lan`；存在访客网络时追加 `guest`。
2. **线路**：`wan.<名称>.network` 须对应 netifd 逻辑接口（即「网络 → 接口」中的接口名，如 `wan`、`wan2`、`wwan`）；
   `id` 取值 1–14 且互不相同；`download_mbps` / `upload_mbps` 须填写**实际可用速率**，该值决定份额分配与满载判定。
3. **探测目标**：`health.<名称>.target` 须为 IP 地址，且须经每条线路均可达。中国大陆网络可使用 `223.5.5.5:443`、`119.29.29.29:443` 等，
   IPv6 可使用 `[2400:3200::1]:443` 等。默认目标 `1.1.1.1`、`9.9.9.9`、`8.8.8.8` 在部分网络中不可达，将导致线路被判定为故障。
4. **防火墙**：所有线路接口须属于 `wan` 防火墙区域，且该区域须启用 IP 动态伪装（masquerade）。
5. **启用**：`global.enabled 1`，`global.startup_mode manage`。

命令行配置示例（两条线路：`wan` 为 PPPoE，`wan2` 为 DHCP）：

```sh
uci set swanix.global.lan_network='lan'
uci set swanix.wan1.network='wan';  uci set swanix.wan1.download_mbps='1000'; uci set swanix.wan1.upload_mbps='50'
uci set swanix.wan2.network='wan2'; uci set swanix.wan2.download_mbps='500';  uci set swanix.wan2.upload_mbps='50'
uci -q delete swanix.health_default.target
uci add_list swanix.health_default.target='223.5.5.5:443'
uci add_list swanix.health_default.target='119.29.29.29:443'
uci set swanix.global.enabled='1'; uci set swanix.global.startup_mode='manage'
uci commit swanix
```

> **仅观察模式。** 将 `enabled` 设为 `1`、`startup_mode` 保持 `observe`，再执行 `/etc/init.d/swanix start`，即可在不接管的前提下查看 swanix 识别的线路状态。
> 该模式不执行探测、不写入内核，可与 mwan3 同时运行。

## 5. 检查与预览

```sh
swanix check      # 严格校验配置；错误信息包含字段路径与修改建议
swanix doctor     # 检查系统：工具、接口解析、防火墙伪装、flow offloading、mark 冲突、rp_filter、mwan3 等
swanix preview    # 输出将要安装的 nftables 规则、ip rule 与路由，不写入任何内容
```

`doctor` 报告的 `fail` 项须在切换前处理，否则 `apply` 将拒绝执行；`warn` 项应逐项确认是否符合预期。

## 6. 切换与确认

```sh
swanix apply            # 切换至 swanix 并进入确认时限（默认 90 秒，可用 --timeout 120s 指定）
swanix confirm          # 在时限内确认，保留切换
swanix rollback         # 主动撤回本次切换
```

`apply` 按以下步骤执行：

1. 备份当前配置及 mwan3 的运行与开机自启状态，并启动独立的回滚守护进程；
2. 停止 mwan3（如正在运行），启动 swanix；
3. **等待真实探测通过**（默认最长 40 秒，处理能力较弱的设备可用 `--verify 90s` 延长），验证失败时立即回滚；
4. 验证通过后开始确认计时。

确认时限内如出现网络中断或无法访问管理界面，无需任何操作：时限届满后，回滚守护进程将撤销 swanix 写入的内核规则，恢复原配置，并重新启动 mwan3。
`confirm` 将 swanix 设为开机自启，并取消 mwan3 的开机自启（mwan3 软件包保留）。

**LuCI。** 各 swanix 页面的「保存并应用」执行相同流程：预检查通过后切换，并显示带倒计时的确认对话框，点击「确认」即保留切换。
预检查未通过时，配置自动还原。

**后续变更。** 修改配置后须再次执行 `swanix apply`（或在 LuCI 中保存并应用），每次变更均有独立的确认时限。

## 7. 日常运维

```sh
swanix status            # 各线路的设备、状态及原因、RTT、探测失败率、各策略中的新连接份额、实时速率、满载状态
swanix status --json     # JSON 格式输出（LuCI 与 ubus 接口使用此格式）
swanix history           # 最近约 1 小时的吞吐记录（保存在内存中，采样间隔 5 秒）
swanix features          # 编译包含的能力（Lite / Full、代理协议列表）
swanix version
logread -e swanix        # 守护进程日志：状态变化、满载、代理连接中断等
ubus call swanix status  # rpcd 接口
```

接口上下线、地址变化、PPPoE 重拨及前缀变化均由 hotplug 与 netlink 事件自动处理。如需立即重新读取接口状态，可执行 `swanix reconcile`。

**线路状态定义：**

| 状态 | 含义 |
|---|---|
| `healthy` | 正常 |
| `degraded` | 可达，但空闲时延迟或探测失败率明显升高；测速等负载造成的排队不算质量下降，份额不因此降低 |
| `down` | 不分配新连接 |
| `recovering` | 已恢复，份额在 `recover_ramp` 内逐步上升 |
| `unknown` | 尚无探测结果 |

## 8. 典型场景

以下配置片段均写入 `/etc/config/swanix`，修改后执行 `swanix apply` 生效。

### 8.1 按带宽比例均衡（默认行为）

为每条线路填写 `download_mbps` / `upload_mbps`，并在策略中列出成员。以下行带宽 1000 Mbps 与 500 Mbps 的两条线路为例，
新连接约按 2:1 分配，并随实际负载、延迟与探测失败率动态调整。

### 8.2 主备线路

```
config wan 'backup4g'
	option id '3'
	option network 'wwan'
	option download_mbps '50'
	option upload_mbps '10'
	option role 'backup'
```

将该线路加入策略成员。仅当策略中全部 `active` 线路均不可用时，新连接才分配至备用线路；主线路恢复后，新连接重新分配至主线路。

### 8.3 线路偏好

`preference '200'` 表示在同等容量条件下获得两倍份额。

### 8.4 满载转移

```
config policy 'balanced'
	list member 'wan1'
	list member 'wan2'
	option saturation_threshold '90'
```

线路实测速率达到其配置带宽的 90%，且策略中存在未达该阈值的线路时，新连接分配至未满载的线路；该线路速率回落至 80% 以下后恢复分配。

### 8.5 连接粘滞

```
	option affinity 'source_destination'   # 同一源地址访问同一目的地址时使用同一线路
	option affinity_timeout '600'
```

`affinity 'source'` 使同一源地址的全部连接使用同一线路，适用于网上银行、对出口地址敏感的网站及在线游戏等业务。

### 8.6 路由器自身 DNS 固定线路

运营商 DNS 服务器通常拒绝来自其他运营商地址的查询，因此应将路由器自身的 DNS 查询固定在对应线路：

```
config policy 'wan1_only'
	list member 'wan1'

config rule 'dns_wan1'
	option priority '10'
	option local '1'
	option proto 'udp'
	list dest_port '53'
	option policy 'wan1_only'
```

wan1 故障期间，上述 DNS 查询将被拒绝，而不会经其他线路发出。如需故障切换，可在该策略中增加第二个成员并将其设为 `role backup`。
DDNS、VPN 控制连接、NTP 等流量可按同样方式固定。

### 8.7 指定主机或网段使用指定线路

```
config rule 'nas_wan2'
	option priority '20'
	list src '192.168.1.10'
	option policy 'wan2_only'
```

规则亦可按目的地址（`dest`）、协议与端口匹配。

### 8.8 多策略的默认策略

存在多个策略时，须指定默认策略：

```
config global 'global'
	option default_policy 'balanced'
```

### 8.9 IPv6

* **运营商委派前缀**（DHCPv6-PD，含 PPPoE 会话上的 PD）：使用 `ipv6.mode native`，并为 LAN 接口设置 `ip6assign`。各线路的前缀经各自线路转发。
* **单一前缀，或要求 IPv6 按连接均衡**：使用 `nat66`，LAN 使用 ULA 前缀，并在 `nat66_source` 中列出这些前缀。
* **不由 swanix 处理 IPv6**：使用 `unmanaged`。

### 8.10 PPPoE 与 DHCP 线路

无需额外配置，`network` 填写对应的 PPPoE 或 DHCP 接口即可。PPPoE 接口设置 `option ipv6 'auto'` 时，netifd 将创建 `wan_6` 形式的动态接口，
`ipv6_network auto` 可自动识别。重拨、地址变化与前缀变化均自动跟进；原地址上的连接由内核清除。

### 8.11 软件流量卸载

```sh
uci set firewall.@defaults[0].flow_offloading='1'; uci commit firewall; /etc/init.d/firewall reload
uci set swanix.global.flow_offload='software'; uci commit swanix; swanix apply
```

不支持硬件流量卸载（`flow_offloading_hw`）。

### 8.12 Full：代理按连接选择线路

1. 执行 `cp /usr/share/swanix/proxy.json.example /etc/swanix/proxy.json`，并在出站上设置 `"wan_policy": "balanced"`；
2. 执行 `uci set swanix.proxy.enabled='1'`、`uci commit swanix`、`swanix check`、`swanix apply`。

代理建立的每个物理连接按该策略的份额选择线路。线路故障时，经该线路建立的代理连接将被关闭，并通过可用线路重新建立（`proxy.interrupt_on_down`）。

### 8.13 Full：TUN 透明接管

在 `proxy.json` 中添加启用 `auto_route` 的 tun 入站（**不得**启用 `auto_redirect`），并在出站上设置 `wan_policy`，
然后执行 `uci set swanix.proxy.transparent_mode='tun'`。规则优先级、路由表编号与 mark 不得与 swanix 冲突，`swanix check` 将逐项报告冲突。
详见[配置参考第 8 节](CONFIG.md#8-proxy-与-proxyjsonfull-版)。

## 9. 故障排查

| 现象 | 处理方法 |
|---|---|
| `apply` 后网络中断 | 等待确认时限届满自动回滚，或执行 `swanix rollback`。 |
| `apply` 提示「doctor 未通过」 | 按输出中 `→` 后的提示修正。常见原因：WAN 区域未启用伪装；启用了 flow offloading 但未在配置中声明。 |
| `apply` 提示「验证失败，已回滚」 | 验证时限内无线路通过探测。应检查探测目标是否可达、是否为 IP 地址，以及 `network` 是否指向正确的接口。 |
| 线路持续为 `down`，但该线路可正常访问网络 | 通过 `swanix status` 查看原因。探测仅经该线路自身的路由表发出，目标须经该线路可达；应改用该运营商网络内可达的目标。 |
| 线路恢复后流量较少 | 属正常现象：`recover_ramp` 期间份额逐步上升。 |
| `rejected_connections` 持续增加 | 某策略在该地址族下暂无可用线路，新连接被拒绝。 |
| 部分网站反复要求重新登录或触发风控 | 对相关源地址或目的地址使用 `affinity source` / `source_destination`，或通过规则固定线路。 |
| 线路负载不均衡 | 核对 `download_mbps` / `upload_mbps` 是否为实际速率。份额仅作用于新连接，大流量连接在建立时所选的线路上持续传输。 |
| 端口转发或远程管理仅能经一条线路访问 | swanix 保证入站连接经入站线路回包。如问题仍存在，应确认所有线路均属于 `wan` 防火墙区域。 |
| 与其他代理或 VPN 冲突 | 执行 `swanix doctor` 检查 mark 冲突。其他组件不得使用 `kernel.mark_mask` 内的位，其策略路由规则不得位于 `kernel.priority_base` 起的区间内。 |

报告问题时，应附上以下命令的输出：`swanix status --json`、`swanix preview`、`logread -e swanix`、`nft list table inet swanix`、`ip rule`。

## 10. 升级、版本替换与卸载

### 10.1 升级

安装新版本软件包即可（`opkg install` / `apk add` 新版本文件）。配置保留，服务自动重启并重新应用规则。

### 10.2 Lite 与 Full 互换

两个版本互斥，替换时配置保留：

```sh
opkg remove luci-i18n-swanix-zh-cn luci-app-swanix swanix-lite
opkg install swanix-full_*.ipk luci-app-swanix_*.ipk luci-i18n-swanix-zh-cn_*.ipk
```

两条命令之间，多线路分流暂停，流量经 `main` 路由表转发。

### 10.3 停用（保留软件包）

以下步骤停用 swanix，并可选择恢复 mwan3：

```sh
uci set swanix.global.enabled='0'; uci commit swanix
/etc/init.d/swanix stop; /etc/init.d/swanix disable
swanix remove                                        # 撤销 nft 表、ip rule 与路由表
/etc/init.d/mwan3 enable; /etc/init.d/mwan3 start    # 如需恢复 mwan3
```

仅执行 `/etc/init.d/swanix stop` 不会撤销内核规则（以保证服务重启期间已有连接不受影响），须配合 `swanix remove`。

### 10.4 卸载

```sh
opkg remove luci-i18n-swanix-zh-cn luci-app-swanix swanix-lite swanix-common   # apk 使用 apk del
```

卸载过程撤销 swanix 的 nft 表、规则与路由，不修改 fw4 及其他组件。已修改的 `/etc/config/swanix` 予以保留。
