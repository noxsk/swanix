# swanix

swanix 是 OpenWrt 的连接级多线路（多 WAN）控制器，用于替代 mwan3，基于 [sing-box](https://github.com/SagerNet/sing-box) `v1.14.4` 的分支开发。

来自受管 LAN 及路由器自身的每一条**新连接**，由 Linux nftables、conntrack 与策略路由分配至一条线路，并在其生命周期内保持在该线路上。
Go 守护进程仅承担控制面职责（健康检测、调度、规则生成与下发），数据包不经过用户态；仅在 Full 版启用代理时，代理流量经由 sing-box 处理。

本项目独立维护，并非 sing-box 官方发行版。上游版权声明与 GPL-3.0-or-later 许可证保留于 [LICENSE](LICENSE)，再分发前请先阅读。

## 功能

### 多线路（Lite 与 Full 均提供）

* 按连接分流。权重依据各线路的带宽、偏好、实时负载、延迟与探测失败率动态计算，并经平滑处理。
* 健康检测：TCP / HTTP(S) 探测（目标须为 IP 地址），按线路、按地址族独立进行；网卡断开与 PPPoE 掉线即时感知；故障线路立即退出分配，恢复后份额逐步回升。
* 主备线路（`role backup`）、偏好权重，以及按源地址、目的地址、协议、端口匹配的分流规则；可单独约束路由器自身流量（例如将 DNS 固定于指定线路）。
* 可选功能：**连接粘滞**（同一源地址，或同一源地址访问同一目的地址时使用同一线路）；**满载转移**（线路接近配置带宽时，新连接优先分配至未满载的线路）。
* 入站连接（端口转发、路由器自身服务）始终经入站线路回包。
* IPv6 支持三种模式：`native`（按委派前缀选择线路）、`nat66`（地址转换后按连接均衡）、`unmanaged`（不处理）。
* 线路类型：静态地址、DHCP、DHCPv6-PD、PPPoE（含会话上的 IPv6 前缀委派）；地址变化、前缀变化与重拨均自动跟进。
* 兼容 fw4 软件流量卸载。
* 安全切换：`apply` 在验证通过后进入确认时限，时限内未确认则由独立的守护进程自动回滚并恢复 mwan3。
* 仅使用自有的 nft 表、mark 位、路由表与规则优先级区间，移除时不影响 fw4 及其他组件。
* 提供 LuCI 界面（含简体中文）、rpcd / ubus 接口与命令行工具。

### 代理（仅 Full 提供）

* sing-box 完整协议功能集（含 naive / cronet）。
* 出站 `wan_policy`：代理建立的每个物理连接按策略份额选择线路；线路故障时关闭其上的代理连接，使长连接会话经可用线路重建。
* TUN 透明接管（`auto_route`），swanix 负责校验其规则、路由表与 mark 不与自身冲突。

### 版本对比

| 项目 | Lite（`swanix-lite`） | Full（`swanix-full`） |
|---|---|---|
| 多线路全部功能、LuCI | 支持 | 支持 |
| 代理协议、TUN、`wan_policy` | 不支持（`mw_lite` 构建，不链接代理代码） | 支持 |
| 可执行文件大小 / 空闲内存（x86_64 实测） | 约 7 MB / 10 MiB | 约 91 MB / 50 MiB |

两个版本使用同一可执行文件名 `/usr/bin/swanix`，互斥安装，共用 `swanix-common`（配置、启动脚本、hotplug、rpcd）与 `luci-app-swanix`。
支持 OpenWrt 24.10（ipk）与 25.12（apk）；CI 构建 x86/64 与 mediatek/filogic 两个平台。

## 设计边界

* 不进行逐包分发，**单条连接无法叠加多条线路的带宽**。权重分配的对象是新连接，而非流量字节数。
* 公网地址变化将中断使用原地址的连接，此为网络层面的固有限制。
* 安装后处于禁用、仅观察状态（`observe` 模式仅读取 netifd 状态，不写入内核），不会自动接管，也不会自动停用 mwan3。

## 快速开始

```sh
# 1. 安装（软件包可从 GitHub Actions 的 build 运行中下载，见使用指南）
opkg update && opkg install ./swanix-common_*.ipk ./swanix-lite_*.ipk ./luci-app-swanix_*.ipk ./luci-i18n-swanix-zh-cn_*.ipk

# 2. 配置受管 LAN、各线路对应的接口与实际带宽，以及可达的探测目标
uci set swanix.global.lan_network='lan'
uci set swanix.wan1.network='wan';  uci set swanix.wan1.download_mbps='1000'; uci set swanix.wan1.upload_mbps='50'
uci set swanix.wan2.network='wan2'; uci set swanix.wan2.download_mbps='500';  uci set swanix.wan2.upload_mbps='50'
uci -q delete swanix.health_default.target
uci add_list swanix.health_default.target='223.5.5.5:443'
uci add_list swanix.health_default.target='119.29.29.29:443'
uci set swanix.global.enabled='1'; uci set swanix.global.startup_mode='manage'
uci commit swanix

# 3. 校验、预览、切换并确认
swanix check && swanix doctor
swanix preview
swanix apply          # 切换；默认须在 90 秒内确认，否则自动回滚
swanix confirm
swanix status
```

上述步骤亦可在 LuCI「网络 → Multi-WAN (swanix)」中完成：执行「保存并应用」后，在倒计时对话框中点击「确认」。

## 文档

| 文档 | 内容 |
|---|---|
| [使用指南](docs/swanix/USAGE.md) | 版本选择、获取与安装、配置、切换与确认、日常运维、典型场景、故障排查、升级与卸载 |
| [配置参考](docs/swanix/CONFIG.md) | `/etc/config/swanix` 全部选项的默认值、取值范围与作用，以及 `proxy.json` 的约束 |
| [性能测试](docs/swanix/PERFORMANCE.md) | 测试方法、数据路径开销、故障收敛、权重更新、代理路径成本与控制面基准的实测结果及配置建议 |
| [验证状态](docs/swanix/STATUS.md) | 已在真实内核、OpenWrt 虚拟机与 CI 中验证的内容及未验证的内容（英文） |
| [架构](docs/swanix/ARCHITECTURE.md) | mark 布局、规则顺序、数据路径（英文） |
| [运维要点](docs/swanix/OPERATIONS.md) | 配置与运维要点的英文摘要 |

**验证范围。** 自动化测试覆盖真实内核（网络命名空间），以及官方 OpenWrt 24.10、25.12 x86 镜像与 24.10 arm64 镜像（QEMU）中的完整流程：
安装、接管、LuCI、IPv4 / IPv6 分流、故障切换、DHCP / PPPoE / 前缀委派、流量卸载、回滚、升级与卸载。
**尚未在真实路由器硬件及运营商设备上验证，不支持硬件流量卸载。** 详见验证状态文档。

## 目录结构

```
cmd/swanix/                  命令行入口：entry_lite.go（构建标签 mw_lite）/ entry_full.go
experimental/multiwan/       config、health、scheduler、kernel、netif、engine、session、doctor、control、wanhook 等
service/multiwan/            Full：sing-box 服务适配    option/multiwan.go  include/multiwan.go
common/dialer/               Full：wan_policy 钩子（对上游的少量修改）
openwrt/                     swanix-common、swanix（lite / full 配方）、luci-app-swanix
ci/                          版本锁定、校验、构建、测试与发布脚本；.github/workflows/
docs/swanix/                 文档
```

## 开发

```sh
ci/build/install-go.sh "$HOME/go-lock"     # 安装 ci/locks/lock.json 锁定的 Go 版本
go test -race ./experimental/multiwan/...
ci/validate/lite-deps.sh                   # 校验 Lite 未链接代理代码
sudo ci/test/netns.sh ./experimental/multiwan/kernel -v                            # 真实内核，一次性网络命名空间
sudo ci/test/netns.sh -tags mw_lite ./cmd/swanix -run 'LiteDaemon|LiteSaturation' -v
sudo ci/test/netns.sh ./cmd/swanix -run 'FullProxy|FullTUN' -v
python3 ci/test/vm/run.py 24.10 <软件包目录> <工作目录>                           # 官方 OpenWrt 镜像中的完整测试
NODE_PATH=... node ci/test/luci/render.js <luci-base resources> <输出目录>          # 在无头 Chromium 中渲染 LuCI 页面
```

GitHub Actions 工作流：
* `ci`：每次推送时运行，包括校验、单元测试、网络命名空间集成测试与 LuCI 渲染；
* `build`：手动触发，构建全部软件包并执行安装测试与虚拟机测试，不创建 tag、不发布；
* `release`：仅在推送 `v*` 版本 tag 时运行。

上游同步须通过经审阅的 Pull Request 更新 `ci/locks/`（tag、commit、`release/DEFAULT_BUILD_TAGS` 副本、预期能力清单），不进行自动拉取。
