# cc-connect `/ccs` 供应商与模型卡片

这是本机定制补丁仓库，不是官方 cc-connect 或 CC Switch 的 fork。

目标：在飞书里用 `/ccs` 走 **选择供应商 → 选择模型 → 确认应用**。完整 Codex 模型列表还依赖 CC Switch 控制接口的模型目录修复。

## 基线

| 组件 | 上游 | 提交 |
| --- | --- | --- |
| cc-connect | [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect) | `17c61062c2f9ce9bcdd45a2082e491f9743a2770` (`v1.5.0`) |
| cc-switch | [farion1231/cc-switch](https://github.com/farion1231/cc-switch) | `3217f72596f2d1c0f879f0a05f83803825d9809f` (`v3.20.1`) |

本仓库不包含上游完整源码、生产 `config.toml`、控制接口令牌、exe 或部署日志。

## 这个补丁做什么

- 内置 `/ccs` / `/cc-switch` 卡片：供应商下拉 → 模型下拉 → 确认后才写入。
- cc-connect 通过本机 loopback 控制接口读取 CC Switch 供应商目录。
- CC Switch `control_api.rs` 返回 Codex 已保存的完整模型目录，而不是只返回默认模型。
- 飞书优先原地更新被点击的卡片；浏览、翻页、取消不会切换供应商或模型。

`/commands add` 和 `[[commands]]` **不能**替代这个实现。自定义命令只能展开 prompt 或跑 shell，不能返回交互卡片并接收结构化回调。官方 `/ccs` 内置命令会覆盖同名自定义命令。

## 仓库结构

| 路径 | 内容 |
| --- | --- |
| [patches/cc-connect.patch](patches/cc-connect.patch) | 应用到 `cc-connect` `v1.5.0` 的完整补丁 |
| [patches/cc-switch.patch](patches/cc-switch.patch) | 应用到 `cc-switch` `v3.20.1` 的完整补丁 |
| [overlay/cc-connect](overlay/cc-connect) | 新增/替换的 cc-connect 源文件 |
| [overlay/cc-switch](overlay/cc-switch) | 新增的 `control_api.rs` |
| [docs/ccs-model-picker.md](docs/ccs-model-picker.md) | `/ccs` 使用说明 |
| [docs/plugin-design.md](docs/plugin-design.md) | 为何不能做成免编译命令插件 |

## 如何打补丁

在干净的上游 checkout 上应用：

```powershell
git clone https://github.com/chenhg5/cc-connect.git
cd cc-connect
git checkout 17c61062c2f9ce9bcdd45a2082e491f9743a2770
git apply --whitespace=nowarn path\to\cc-connect-ccs\patches\cc-connect.patch
```

```powershell
git clone https://github.com/farion1231/cc-switch.git
cd cc-switch
git checkout 3217f72596f2d1c0f879f0a05f83803825d9809f
git apply --whitespace=nowarn path\to\cc-connect-ccs\patches\cc-switch.patch
```

只复制 `overlay/` 不够：`engine.go`、`i18n.go`、`feishu.go` 等现有文件的改动只在 patch 里。

## 运行时依赖

1. 构建并安装打过补丁的 CC Switch。旧控制接口只返回默认模型，只重启 cc-connect 补不齐目录。
2. 构建并安装打过补丁的 cc-connect。
3. 在项目配置里启用 `provider_control`，指向本机 CC Switch 的 `control-api.json`。
4. 飞书卡片保持开启；`ccs`、`provider`、`model` 命令权限允许当前用户。

不要提交生产配置或控制接口令牌。令牌由 CC Switch 生成本地文件，只给 loopback 使用。

## 已知限制

- 供应商切换作用在 CC Switch 的共享代理上，不是单聊私有设置。
- 模型设置沿用当前项目或绑定 workspace 的 `/model`。
- 卡片专项、CUJ 和 scoped vet 已通过；Windows 全量 `go test ./...` 和全量 `go vet` 未通过。
- 这不是官方发布接口。官方 exe 覆盖后，定制 `/ccs` 会丢失。

## 许可

本仓库新增代码与补丁按 MIT 发布。上游 cc-connect 与 cc-switch 仍归原作者，许可证见各自仓库。
