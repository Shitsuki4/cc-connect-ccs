# CCS 独立插件方案核查

> **状态：方案核查完成，插件尚未实现或安装。**
> 本轮没有修改 cc-connect 源码、生产配置、供应商、模型、历史或计划任务；没有启动第二个真实机器人。
> 以下结论只针对本地基线 `17c61062c2f9ce9bcdd45a2082e491f9743a2770` 及现有工作区，不代表未来官方版本的能力。

## 结论与选择

如果“插件”指独立目录、独立程序、通过接口接入，**可以设计**；如果指往当前官方 cc-connect 放一个文件，就获得原生 `/ccs` 供应商→模型卡片，**目前没有找到这种运行时命令插件入口**。

不要把“已拆成独立程序”与“官方升级后一定能用”混为一谈。后者还取决于宿主接口和 CC Switch 控制接口兼容。

| 路线 | 主程序需重编译吗 | 能否维持聊天内卡片 | 更新边界 |
|---|---|---|---|
| 现有编译期模块拆包 | 需要 | 可以保留现有实现 | 官方 exe 覆盖仍会丢失定制功能 |
| 外置命令插件 + 薄宿主接口 | 添加宿主接口时需要 | 可设计，尚未实现 | 官方没有该接口前，更新仍需保留适配补丁 |
| 外置飞书 Bridge 适配器 + CCS 插件 | Bridge 接口本身不要求改宿主 | 可设计，需要重新实现及测试 | 独立文件不随宿主 exe 替换；协议、权限和切换接口兼容仍需验证 |
| 自定义命令打开独立网页 | 不需要改卡片接口 | 不是用户要求的聊天内逐步卡片 | 体验改变，不能当作等价交付 |

**若官方 exe 可独立升级是硬要求，优先验证外置 Bridge 路线，而不是先做一个仍依赖私有宿主补丁的“插件”。**
但它会把飞书接入改成另一个常驻进程，需要明确接受这项架构变化后，再做隔离原型；不能直接替换生产。

## Evidence → Finding

源文件指纹保存在 [source-evidence.json](source-evidence.json)，不包含任何生产凭据或配置。

| Evidence | 可复查位置 | Finding |
|---|---|---|
| E1：平台/代理注册表由 Go `init` 导入使用 | [registry.go](../cc-connect/core/registry.go):16；[plugin_platform_feishu.go](../cc-connect/cmd/cc-connect/plugin_platform_feishu.go):5 | F1：此处的“插件架构”是编译期模块，不是可安装命令插件 |
| E2：自定义命令只能展开 prompt 或运行 shell；shell 输出走进度/文本回复 | [command.go](../cc-connect/core/command.go):14；[engine.go](../cc-connect/core/engine.go):14506、14548 | F2：现有 exec 命令不能直接返回任意卡片及接收其结构化回调；给它套个插件清单不能补出该能力 |
| E3：Bridge 支持运行时外部适配器；协议文档标为 draft | [bridge-protocol.md](../cc-connect/docs/bridge-protocol.md):3、8 | F3：存在不必为平台适配器重编译宿主的入口；不等于已保证 CCS 功能或跨版本兼容 |
| E4：普通 Bridge message 保留传入身份/会话；card_action 的命令分支合成管理端身份 | [bridge.go](../cc-connect/core/bridge.go):876、895、898、1014、1021 | F4：不能把真实飞书点击者直接映射成管理端。应在适配层验证事件，再使用带真实身份的普通 message 进入正常权限入口，并测试权限回执等不同动作 |
| E5：能力快照发布命令清单，未按本次点击者计算授权 | [bridge_capabilities.go](../cc-connect/core/bridge_capabilities.go):57 | F5：命令出现在 capabilities_snapshot 中不能作为供应商/模型修改的授权凭证 |
| E6：当前 CCS 应用流程包含目录指纹复核、会话锁、代理选择及模型持久化 | [ccs_picker.go](../cc-connect/core/ccs_picker.go):381、462 | F6：只搬走卡片渲染不等于完成安全的供应商+模型切换；必须验证外置流程如何保留这些保护 |
| E7：供应商控制依赖本地 CC Switch 控制 API | [provider_control.go](../cc-connect/core/provider_control.go):55、75、86 | F7：即使 cc-connect 侧独立，仍不能保证升级 CC Switch 后该定制控制 API 一直存在 |

F1–F7 均为本地源码观察。F3 的外置路线是候选架构，并非已经完成的集成验证；F6 的宿主外部事务接口是否足够仍待专门验证。

## Path：候选外置调用路径

1. 飞书事件由**唯一**外置适配器接收（E3/F3）。正式迁移前不能同时运行第二个使用相同应用的真实机器人。
2. 普通聊天、原生命令进入 cc-connect Bridge，保留原 `session_key`、点击者 `user_id` 和准确项目路由（E4/F4）。不重命名或重建旧会话。
3. CCS 插件管理供应商→模型→确认/返回/取消界面；更新准确的源卡片，不把 JSON 当文本发送（E2/F2）。
4. 每次写入前仍须走正常授权；仅有 Bridge token 或能力清单不构成用户授权（E4、E5/F4、F5）。如何使外置 CCS 写入满足宿主授权要求，是原型必须解决的接口问题。
5. 真正切换前复核目录、当前模型、代理状态、忙碌会话和原生会话绑定；冲突或不兼容时停止，不自动降级为直接改数据库（E6/F6）。
6. 宿主升级后先做接口和用户旅程测试；通过后再正常切换服务。CC Switch 控制 API 另行检查（E3、E7/F3、F7）。

## 隔离原型与上线门槛

原型只能使用临时配置、模拟平台/代理进程和本地测试控制器，不读取真实消息内容、不启动真实 bot、不执行生产 select/apply/switch。

必须覆盖：

- 供应商→模型→确认→返回→取消→重新打开的完整卡片流程；超时只更新源消息一次。
- 点击者、群、项目、工作区隔离；旧卡片、重放及他人点击拒绝；命令禁用和角色权限不得绕过。
- 普通聊天及卡片命令仍进正常 Engine 权限入口，不能以 `web-admin` 代替真实用户。
- 不忙碌检查与真正应用之间的竞态；先切供应商后模型失败必须明确报告部分成功，不盲目回滚共享代理。
- 旧会话历史前缀、原生 agent session ID、项目及工作区映射保持；新消息允许自然追加。
- 插件断连、主程序重启、协议不匹配时不得触发隐式切换。
- 官方无定制基线上的验证。只对当前定制 `/ccs` 做透传不算独立插件成功。
- 定制 CC Switch 控制 API 的兼容性。不能只测试 cc-connect 一侧。

正式接入仍需遵循已有部署约束：保存最终回复、自然空闲、正常管理接口重启、无强制终止、无历史恢复、无重复旧 Queue。

## 如何复查源码证据

从 `cc-provider-integration\cc-connect` 目录执行以下只读命令：

```powershell
git rev-parse HEAD
Get-Content .\core\registry.go
Get-Content .\cmd\cc-connect\plugin_platform_feishu.go
rg -n 'executeCustomCommand|executeShellCommand|runShellWithProgress' core/engine.go
rg -n 'handleMessage|dispatchAsMessage|web-admin' core/bridge.go
rg -n 'GetBridgePublishedCommands|disabledCmds' core/bridge_capabilities.go
rg -n 'TryLock|fingerprint|applyCCSProjectModel' core/ccs_picker.go
```

本轮只进行了源码核查和文档编写，未新增或运行插件测试，未宣称官方版兼容或真实飞书客户端验收通过。
